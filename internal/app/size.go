package app

import (
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
	"fluxion/internal/util"
	"fmt"
)

type SizeConfig struct {
	DBPath     string
	SnapQuery  string
	TotalBytes bool
}

func RunSize(cfg SizeConfig) error {
	if cfg.DBPath == "" {
		return fmt.Errorf("DB path is required")
	}
	if cfg.SnapQuery == "" {
		return fmt.Errorf("snapshot ID or name is required")
	}

	// Open DB
	var dbStore store.Store
	var err error
	dbStore, err = sqlite.NewSqliteStore(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("error opening DB: %w", err)
	}
	defer dbStore.Close()

	// Find Snapshot to ensure it exists and get ID
	snap, err := dbStore.FindSnapshot(cfg.SnapQuery)
	if err != nil {
		return fmt.Errorf("could not find snapshot '%s': %w", cfg.SnapQuery, err)
	}

	// Get Size
	sizeBytes, err := dbStore.GetSnapshotBytes(snap.ID)
	if err != nil {
		return fmt.Errorf("error getting snapshot size: %w", err)
	}

	if cfg.TotalBytes {
		fmt.Println(sizeBytes)
	} else {
		fmt.Println(util.FormatBytes(sizeBytes))
	}

	return nil
}
