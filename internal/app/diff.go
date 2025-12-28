package app

import (
	"fmt"
	"path/filepath"
	"strings"

	"fluxion/internal/diff"
	"fluxion/internal/models"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"

	"github.com/schollz/progressbar/v3"
)

type DiffConfig struct {
	DBPath     string
	OldQuery   string
	NewQuery   string
	UpdateMode bool
	Excludes   []string
}

func RunDiff(cfg DiffConfig) error {
	if cfg.DBPath == "" {
		return fmt.Errorf("DB path is required")
	}

	// Open DB
	var dbStore store.Store
	var err error
	dbStore, err = sqlite.NewSqliteStore(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("error opening DB: %w", err)
	}
	defer dbStore.Close()

	// 1. Find Snapshots
	snapA, err := dbStore.FindSnapshot(cfg.OldQuery)
	if err != nil {
		return fmt.Errorf("could not find 'old' snapshot '%s': %w", cfg.OldQuery, err)
	}

	snapB, err := dbStore.FindSnapshot(cfg.NewQuery)
	if err != nil {
		return fmt.Errorf("could not find 'new' snapshot '%s': %w", cfg.NewQuery, err)
	}

	oldID := snapA.ID
	newID := snapB.ID

	// Determine Hash Strategy
	hasSHA1A, hasMD5A := false, false
	for _, h := range snapA.Hashes {
		if h == "sha1" {
			hasSHA1A = true
		}
		if h == "md5" {
			hasMD5A = true
		}
	}

	hasSHA1B, hasMD5B := false, false
	for _, h := range snapB.Hashes {
		if h == "sha1" {
			hasSHA1B = true
		}
		if h == "md5" {
			hasMD5B = true
		}
	}

	commonSHA1 := hasSHA1A && hasSHA1B
	commonMD5 := hasMD5A && hasMD5B

	var strategy string
	if commonSHA1 {
		strategy = "sha1"
	} else if commonMD5 {
		strategy = "md5"
	} else {
		fmt.Printf("Error: Incompatible hash types.\n")
		fmt.Printf("Snapshot A Hashes: %v\n", snapA.Hashes)
		fmt.Printf("Snapshot B Hashes: %v\n", snapB.Hashes)
		return fmt.Errorf("snapshots must share at least one common hash algorithm")
	}

	fmt.Printf("Comparing using strategy: %s\n", strings.ToUpper(strategy))

	// 2. Load Maps with Progress
	fmt.Printf("Loading Snapshot %d...\n", oldID)
	countA, _ := dbStore.GetFileCount(oldID)
	barA := progressbar.Default(countA)
	filesA, err := dbStore.GetFilesForSnapshot(oldID, func(c int) { barA.Set(c) })
	if err != nil {
		return err
	}

	fmt.Printf("\nLoading Snapshot %d...\n", newID)
	countB, _ := dbStore.GetFileCount(newID)
	barB := progressbar.Default(countB)
	filesB, err := dbStore.GetFilesForSnapshot(newID, func(c int) { barB.Set(c) })
	if err != nil {
		return err
	}
	fmt.Println()

	// 3. Compare with Progress
	// Total ops = len(A) + len(B)
	fmt.Println("Computing Diff...")

	// Prepare relative maps and apply exclusions
	relFilesA := make(map[string]models.FileRecord, len(filesA))
	for k, v := range filesA {
		if isExcluded(k, snapA.RootPath, cfg.Excludes) {
			continue
		}

		rel, err := filepath.Rel(snapA.RootPath, k)
		if err == nil {
			// Also check relative path against exclusion just in case
			if isExcluded(rel, "", cfg.Excludes) {
				continue
			}
			relFilesA[rel] = v
		} else {
			relFilesA[k] = v // Fallback
		}
	}

	relFilesB := make(map[string]models.FileRecord, len(filesB))
	for k, v := range filesB {
		if isExcluded(k, snapB.RootPath, cfg.Excludes) {
			continue
		}

		rel, err := filepath.Rel(snapB.RootPath, k)
		if err == nil {
			if isExcluded(rel, "", cfg.Excludes) {
				continue
			}
			relFilesB[rel] = v
		} else {
			relFilesB[k] = v // Fallback
		}
	}

	barDiff := progressbar.Default(int64(len(filesA) + len(filesB)))

	results, err := diff.CompareSnapshots(relFilesA, relFilesB, snapA.RootPath, snapB.RootPath, strategy, func(curr, total int) {
		barDiff.Set(curr)
	})
	if err != nil {
		return fmt.Errorf("error during diff: %w", err)
	}
	fmt.Println()

	// 4. Print Results
	if len(results) == 0 {
		fmt.Println("No differences found.")
		return nil
	}

	for _, res := range results {
		// Update Mode Filtering
		if cfg.UpdateMode {
			// Ignore Added (Not in A)
			if res.Status == diff.StatusAdded {
				continue
			}
			// Ignore Move/Copy (Content exists in B)
			if res.Status == diff.StatusMove || res.Status == diff.StatusCopy {
				continue
			}
			// Show: StatusRemoved (Missing in B), StatusModified (Changed in B)
		}

		symbol := "?"
		path := res.Path
		switch res.Status {
		case diff.StatusAdded:
			symbol = "[+]"
		case diff.StatusRemoved:
			symbol = "[-]"
		case diff.StatusModified:
			symbol = "[M]"
		case diff.StatusMove:
			symbol = "[>]"
			path = fmt.Sprintf("%s -> %s", res.SourcePath, res.Path)
		case diff.StatusCopy:
			symbol = "[C]"
			path = fmt.Sprintf("%s -> %s", res.SourcePath, res.Path)
		}

		if res.FileCount > 0 {
			path = fmt.Sprintf("%s (and %d files)", path, res.FileCount)
		}

		fmt.Printf("%s %s\n", symbol, path)
	}
	return nil
}

func isExcluded(path, root string, excludes []string) bool {
	if len(excludes) == 0 {
		return false
	}
	// Normalize path
	path = filepath.Clean(path)

	for _, excl := range excludes {
		// 1. If exclude matches absolute path prefix (if path is absolute)
		if filepath.IsAbs(excl) {
			if strings.HasPrefix(path, excl) {
				return true
			}
		} else {
			// 2. Relative exclude
			// If we have a root, we can check if path is under root/excl
			if root != "" {
				absExcl := filepath.Join(root, excl)
				if strings.HasPrefix(path, absExcl) {
					return true
				}
			}

			// 3. Or check if path itself starts with exclude (relative match)
			// This matches "node_modules" against "node_modules/foo"
			if strings.HasPrefix(path, excl) {
				return true
			}

			// 4. Also check for path component match? (e.g. "foo/node_modules/bar")
			// Requirement says: "If relative, it is applied to each snapshot from the snapshot's root path."
			// So "node_modules" means "$ROOT/node_modules". It does NOT mean "anywhere/node_modules".
		}
	}
	return false
}
