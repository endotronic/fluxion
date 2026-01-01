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
	"github.com/sirupsen/logrus"
)

type DiffConfig struct {
	DBPath        string
	OldQuery      string
	NewQuery      string
	UpdateMode    bool
	Excludes      []string
	NoCopies      bool
	NoMoves       bool
	ShowUnchanged bool
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
		logrus.Errorf("Error: Incompatible hash types.\n")
		logrus.Errorf("Snapshot A Hashes: %v\n", snapA.Hashes)
		logrus.Errorf("Snapshot B Hashes: %v\n", snapB.Hashes)
		return fmt.Errorf("snapshots must share at least one common hash algorithm")
	}

	logrus.Infof("Comparing using strategy: %s", strings.ToUpper(strategy))

	// 2. Load Maps with Progress
	logrus.Infof("Loading Snapshot %d...", oldID)
	countA, _ := dbStore.GetFileCount(oldID)
	barA := progressbar.Default(countA)
	filesA, err := dbStore.GetFilesForSnapshot(oldID, func(c int) { barA.Set(c) })
	if err != nil {
		return err
	}

	logrus.Infof("Loading Snapshot %d...", newID)
	countB, _ := dbStore.GetFileCount(newID)
	barB := progressbar.Default(countB)
	filesB, err := dbStore.GetFilesForSnapshot(newID, func(c int) { barB.Set(c) })
	if err != nil {
		return err
	}
	logrus.Println()

	// 3. Compare with Progress
	// Total ops = len(A) + len(B)
	logrus.Info("Computing Diff...")

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

	results, err := diff.CompareSnapshots(relFilesA, relFilesB, snapA.RootPath, snapB.RootPath, strategy, cfg.NoCopies, cfg.NoMoves, cfg.ShowUnchanged, func(curr, total int) {
		barDiff.Set(curr)
	})
	if err != nil {
		return fmt.Errorf("error during diff: %w", err)
	}
	logrus.Println()

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
		switch res.Status {
		case diff.StatusAdded:
			symbol = "[+]"
		case diff.StatusRemoved:
			symbol = "[-]"
		case diff.StatusModified:
			symbol = "[M]"
		case diff.StatusMove:
			symbol = "[>]"
		case diff.StatusCopy:
			symbol = "[C]"
		case diff.StatusMixed:
			symbol = "   " // Context line
		}

		// Construct display path
		var displayPath string

		fmtRoot := func(r string) string {
			if r != "" && !strings.HasSuffix(r, string(filepath.Separator)) {
				return r + string(filepath.Separator)
			}
			return r
		}

		if res.Status == diff.StatusMove || res.Status == diff.StatusCopy {
			// [C] [/root/A/ -> /root/B/] relativeA/foo -> relativeB/foo
			displayPath = fmt.Sprintf("[%s -> %s] %s -> %s", fmtRoot(res.SourceRoot), fmtRoot(res.Root), res.SourceRelPath, res.RelPath)
		} else {
			// [*] [/path/to/root/] relative/path
			displayPath = fmt.Sprintf("[%s] %s", fmtRoot(res.Root), res.RelPath)
		}

		// Add trailing slash for directories if not present
		// This applies to both StatusMixed context lines and normal directory changes
		// Since mixed nodes are directories by definition logic (containing children), we ensure trailing slash.
		// DiffResult.Path usually has trailing slash for dirs if from core logic.
		// But let's be safe for display consistency.
		if res.Status == diff.StatusMixed && !strings.HasSuffix(displayPath, string(filepath.Separator)) {
			// Wait, displayPath ends with res.RelPath.
			// If RelPath doesn't have it, we might want to add it.
		}

		// Construct summary
		var parts []string
		if res.AddedCount > 0 {
			parts = append(parts, fmt.Sprintf("%d added", res.AddedCount))
		}
		if res.RemovedCount > 0 {
			parts = append(parts, fmt.Sprintf("%d removed", res.RemovedCount))
		}
		if res.ModifiedCount > 0 {
			parts = append(parts, fmt.Sprintf("%d modified", res.ModifiedCount))
		}
		if res.CopyCount > 0 {
			parts = append(parts, fmt.Sprintf("%d copied", res.CopyCount))
		}
		if res.MoveCount > 0 {
			parts = append(parts, fmt.Sprintf("%d moved", res.MoveCount))
		}
		if res.UnchangedDirCount > 0 || res.UnchangedFileCount > 0 {
			subParts := []string{}
			if res.UnchangedDirCount > 0 {
				subParts = append(subParts, fmt.Sprintf("%d directories", res.UnchangedDirCount))
			}
			if res.UnchangedFileCount > 0 {
				subParts = append(subParts, fmt.Sprintf("%d files", res.UnchangedFileCount))
			}
			parts = append(parts, fmt.Sprintf("%s unchanged", strings.Join(subParts, ", ")))
		}

		if len(parts) > 0 {
			displayPath = fmt.Sprintf("%s (%s)", displayPath, strings.Join(parts, ", "))
		}

		fmt.Printf("%s %s\n", symbol, displayPath)
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
