package app

import (
	"fmt"

	"fluxion/internal/dupes"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
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

	fmt.Printf("Finding duplicates in snapshot '%s' (ID: %d)...\n", snap.Name, snap.ID)

	// Determine strategy
	// If snapshot has specific hashes, prioritize stronger ones?
	// The dupes package usually receives a map or list.
	// But `internal/dupes` might need `GetFilesForSnapshot`.
	
	// Load files
	fmt.Println("Loading file list...")
	files, err := dbStore.GetFilesForSnapshot(snap.ID, nil) // No progress bar for now or simple
	if err != nil {
		return fmt.Errorf("error loading files: %w", err)
	}
	
	
	fmt.Printf("Analyze %d files for duplicates (Min Size: %d)...\n", len(files), cfg.MinSize)
	
	// Run Dupes Analysis
	// We use `FindDuplicates` from `internal/dupes/dupes.go`.
	groups, err := dupes.FindDuplicates(files, cfg.MinSize, snap.RootPath)
	if err != nil {
		return fmt.Errorf("error finding duplicates: %w", err)
	}
	
	if len(groups) == 0 {
		fmt.Println("No duplicates found.")
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
		
		fmt.Printf("[DUPE] Size: %d | Count: %d | Wasted: %d\n", g.Size, len(g.Paths), wasted)
		for _, p := range g.Paths {
			fmt.Printf("  - %s\n", p)
		}
		fmt.Println()
	}
	fmt.Printf("Total Wasted Space: %d\n", totalWasted)
	
	return nil
}
