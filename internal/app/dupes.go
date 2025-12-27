package app

import (
	"fmt"

	"fluxion/internal/dupes"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
	"fluxion/internal/util"

	"github.com/sirupsen/logrus"
)

type DupesConfig struct {
	DBPath    string
	SnapQuery string
	MinSize   int64
}

func RunDupes(cfg DupesConfig) error {
	if cfg.DBPath == "" {
		return fmt.Errorf("DB path is required")
	}
	if cfg.SnapQuery == "" {
		return fmt.Errorf("snapshot name or ID is required")
	}

	// Open DB
	var dbStore store.Store
	var err error
	dbStore, err = sqlite.NewSqliteStore(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("error opening DB: %w", err)
	}
	defer dbStore.Close()

	// Find Snapshot
	snap, err := dbStore.FindSnapshot(cfg.SnapQuery)
	if err != nil {
		return fmt.Errorf("error finding snapshot: %w", err)
	}

	logrus.Infof("Finding duplicates in snapshot '%s' (ID: %d)...", snap.Name, snap.ID)

	// Determine strategy
	// If snapshot has specific hashes, prioritize stronger ones?
	// The dupes package usually receives a map or list.
	// But `internal/dupes` might need `GetFilesForSnapshot`.
	
	// Load files
	logrus.Info("Loading file list...")
	files, err := dbStore.GetFilesForSnapshot(snap.ID, nil) // No progress bar for now or simple
	if err != nil {
		return fmt.Errorf("error loading files: %w", err)
	}
	
	
	logrus.Infof("Analyze %d files for duplicates (Min Size: %s)...", len(files), util.FormatBytes(cfg.MinSize))
	
	// Optimization Note:
	// We pass the file map directly to FindDuplicates to avoid expensive slice conversion/allocation.
	// The `files` map comes from `GetFilesForSnapshot` which loads the entire snapshot into memory.
	// For very large snapshots (millions of files), this might need to be refactored to a streaming approach
	// or database-side duplicate detection, but for now this is efficient enough for typical use cases.
	
	// Run Dupes Analysis
	// We use `FindDuplicates` from `internal/dupes/dupes.go`.
	groups, err := dupes.FindDuplicates(files, cfg.MinSize, snap.RootPath)
	if err != nil {
		return fmt.Errorf("error finding duplicates: %w", err)
	}
	
	if len(groups) == 0 {
		logrus.Info("No duplicates found.")
		return nil
	}
	
	// We need to print them manually as `dupes` package might not have `PrintResults` export?
	// Checking dupes/util.go...
	// If it doesn't, we implement print here.
	
	var totalWasted int64 = 0
	for _, g := range groups {
		redundantCount := int64(len(g.Paths) - 1)
		wasted := redundantCount * g.Size
		totalWasted += wasted
		
		fmt.Printf("[DUPE] Size: %s | Count: %d | Wasted: %s\n", util.FormatBytes(g.Size), len(g.Paths), util.FormatBytes(wasted))
		for _, p := range g.Paths {
			fmt.Printf("  - %s\n", p)
		}
		fmt.Println()
	}
	fmt.Printf("Total Wasted Space: %s\n", util.FormatBytes(totalWasted))
	
	return nil
}
