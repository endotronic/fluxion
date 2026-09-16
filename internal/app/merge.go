package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fluxion/internal/consts"
	"fluxion/internal/models"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"

	"github.com/schollz/progressbar/v3"
	"github.com/sirupsen/logrus"
)

type MergeConfig struct {
	DBPath    string
	Name      string
	Hostname  string
	Snapshots []string
}

func RunMerge(cfg MergeConfig) error {
	if cfg.DBPath == "" {
		return fmt.Errorf("DB path is required")
	}
	if len(cfg.Snapshots) < 2 {
		return fmt.Errorf("at least 2 snapshots are required to merge")
	}
	if cfg.Name == "" {
		return fmt.Errorf("a name for the new merged snapshot is required")
	}

	// Open DB
	var dbStore store.Store
	var err error
	dbStore, err = sqlite.NewSqliteStore(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("error opening DB: %w", err)
	}
	defer dbStore.Close()

	// 1. Resolve Input Snapshots
	var inputSnaps []*models.Snapshot
	for _, query := range cfg.Snapshots {
		s, err := dbStore.FindSnapshot(query)
		if err != nil {
			return fmt.Errorf("error finding snapshot '%s': %w", query, err)
		}
		// Confirm it is completed?
		// We could merge incomplete ones, but let's warn? No, strict is better?
		// Let's allow it but log.
		if s.Status != models.StatusCompleted {
			logrus.Warnf("Snapshot '%s' (ID: %d) is not marked as completed. Merging anyway.", s.Name, s.ID)
		}
		inputSnaps = append(inputSnaps, s)
	}

	// 2. Create New Snapshot
	// Determine LCA of root paths
	var rootPaths []string
	for _, s := range inputSnaps {
		rootPaths = append(rootPaths, s.RootPath)
	}
	rootPath := findLCA(rootPaths)

	hostname := cfg.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}

	finalName, err := getUniqueSnapshotName(dbStore, cfg.Name, true)
	if err != nil {
		return fmt.Errorf("error resolving name: %w", err)
	}

	newSnap, err := dbStore.CreateSnapshot(rootPath, finalName, hostname)
	if err != nil {
		return fmt.Errorf("error creating new snapshot: %w", err)
	}

	logrus.Infof("Created new snapshot ID %d (Name: %s) for merge of %d snapshots.", newSnap.ID, finalName, len(inputSnaps))

	// 3. Merge Files
	//
	// A snapshot holds one row per path - a path cannot have two contents at the
	// same instant - so when several inputs contain the same path they collapse
	// into a single record and the last input listed wins. Collapsing identical
	// content is silent; collapsing *different* content discards a version, so
	// that is reported.
	//
	// Detecting a collision needs a map of every path merged so far, which costs
	// memory proportional to the whole run - unaffordable at fleet scale (a
	// union of a fleet's zfs-scan snapshots can be tens of millions of files).
	// But it is also unnecessary whenever rootsDisjoint(rootPaths) holds: every
	// stored path is guaranteed prefixed by its own snapshot's root, so inputs
	// with disjoint roots cannot possibly share a path. That is the normal case
	// for a fleet merge - independent zfs-scan datasets - so the common case
	// pays no memory for collision tracking at all, and only the rarer
	// overlapping-roots case pays the map. Note this does not cover a single
	// input snapshot that already holds two rows for the same path on its own
	// (schema allows it - see the missing (snapshot_id, path) constraint in
	// knowledge/data-model.md) - that is a pre-existing data-quality issue in
	// the source, not something a merge across disjoint trees can introduce.
	trackCollisions := !rootsDisjoint(rootPaths)

	var seenHash map[string]string
	if trackCollisions {
		seenHash = make(map[string]string)
	}
	var collapsed, conflicts, uniquePaths int
	var conflictSamples []string

	totalImported := 0

	for _, s := range inputSnaps {
		logrus.Infof("Merging files from snapshot '%s' (ID: %d)...", s.Name, s.ID)

		count, _ := dbStore.GetFileCount(s.ID)
		bar := progressbar.Default(count, fmt.Sprintf("Reading %s", s.Name))

		batch := make([]*models.FileRecord, 0, consts.DBBatchSize)
		snapImported := 0
		iterErr := dbStore.IterateFiles(s.ID, func(f models.FileRecord) error {
			hash := f.SHA1
			if hash == "" {
				hash = f.MD5
			}
			if trackCollisions {
				if prev, ok := seenHash[f.Path]; ok {
					collapsed++
					if prev != hash {
						conflicts++
						if len(conflictSamples) < 10 {
							conflictSamples = append(conflictSamples, f.Path)
						}
					}
				} else {
					uniquePaths++
				}
				seenHash[f.Path] = hash
			} else {
				uniquePaths++
			}

			newRec := &models.FileRecord{
				SnapshotID: newSnap.ID,
				Path:       f.Path,
				Filename:   f.Filename,
				SizeBytes:  f.SizeBytes,
				ModTime:    f.ModTime,
				SHA1:       f.SHA1,
				MD5:        f.MD5,
			}
			batch = append(batch, newRec)
			snapImported++

			if len(batch) >= consts.DBBatchSize {
				if err := dbStore.BatchAddFiles(batch); err != nil {
					logrus.Errorf("Error writing batch: %v", err)
				}
				batch = batch[:0]
			}
			bar.Add(1)
			return nil
		})
		if iterErr != nil {
			logrus.Errorf("Error reading files from '%s': %v", s.Name, iterErr)
			continue
		}
		if len(batch) > 0 {
			if err := dbStore.BatchAddFiles(batch); err != nil {
				logrus.Errorf("Error writing batch: %v", err)
			}
		}
		bar.Finish()
		fmt.Println() // newline because logrus might not handle progressbar newline well? or bar does.
		totalImported += snapImported
	}

	// 4. Complete
	if err := dbStore.CompleteSnapshot(newSnap.ID, time.Time{}); err != nil {
		logrus.Errorf("Error completing snapshot: %v", err)
	}

	if collapsed > 0 {
		logrus.Infof("%d record(s) repeated a path already seen in an earlier input and were combined.", collapsed)
	}
	if conflicts > 0 {
		logrus.Warnf("%d path(s) held different content in different inputs; the last input listed won:", conflicts)
		for _, p := range conflictSamples {
			logrus.Warnf("  %s", p)
		}
		if conflicts > len(conflictSamples) {
			logrus.Warnf("  ... and %d more", conflicts-len(conflictSamples))
		}
	}

	logrus.Infof("Successfully merged %d records into snapshot '%s' (%d unique paths).",
		totalImported, finalName, uniquePaths)

	return nil
}

func findLCA(paths []string) string {
	if len(paths) == 0 {
		return "/"
	}

	common := paths[0]
	// If it doesn't end in separator and isn't root, ideally we treat it as directory.
	// But snapshots root_paths are usually directories.
	// We'll trust the string manipulation.

	for _, p := range paths[1:] {
		// Shrink common until it is a prefix of p
		// Note: We must respect path boundaries. "/data" is not a prefix of "/data2".
		// It must be "/data" prefix of "/data/subdir" OR "/data" == "/data".

		for {
			if common == p || strings.HasPrefix(p, common+string(os.PathSeparator)) || (common == string(os.PathSeparator) && strings.HasPrefix(p, common)) {
				break
			}
			// Special case: if common is "/" and p doesn't match, well p must be relative?
			// But assumption is absolute paths.
			if common == "/" || common == "" || common == "." {
				common = "/"
				break
			}

			common = filepath.Dir(common)
		}
	}

	if common == "" {
		return "/"
	}
	return common
}
