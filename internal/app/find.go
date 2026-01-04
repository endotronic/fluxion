package app

import (
	"fmt"
	"regexp"

	"fluxion/internal/models"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
)

type FindConfig struct {
	DBPath        string
	SnapQuery     string
	Pattern       string
	CaseSensitive bool
	IsRegex       bool
}

func RunFind(cfg FindConfig) error {
	if cfg.DBPath == "" {
		return fmt.Errorf("DB path is required")
	}
	if cfg.SnapQuery == "" {
		return fmt.Errorf("snapshot name or ID is required")
	}
	if cfg.Pattern == "" {
		return fmt.Errorf("pattern is required")
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

	if cfg.IsRegex {
		// Regex Mode: Iterate in Go
		re, err := regexp.Compile(cfg.Pattern)
		if err != nil {
			return fmt.Errorf("invalid regex pattern: %w", err)
		}

		err = dbStore.IterateFiles(snap.ID, func(f models.FileRecord) error {
			if re.MatchString(f.Filename) {
				fmt.Println(f.Path)
			}
			return nil
		})
	} else {
		// SQL Search Mode
		err = dbStore.SearchFiles(snap.ID, cfg.Pattern, cfg.CaseSensitive, func(f models.FileRecord) error {
			fmt.Println(f.Path)
			return nil
		})
	}

	if err != nil {
		return fmt.Errorf("error searching files: %w", err)
	}

	return nil
}
