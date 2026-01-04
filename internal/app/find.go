package app

import (
	"fmt"
	"os"
	"regexp"

	"fluxion/internal/models"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"

	"github.com/schollz/progressbar/v3"
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

	// Get total file count for progress bar (useful for Regex mode, or general context)
	count, err := dbStore.GetFileCount(snap.ID)
	if err != nil {
		// Non-fatal, just default to -1 (indeterminate) if failed
		count = -1
	}

	// Initialize Progress Bar
	var bar *progressbar.ProgressBar
	if cfg.IsRegex && count > 0 {
		bar = progressbar.NewOptions64(count,
			progressbar.OptionSetWriter(os.Stderr),
			progressbar.OptionSetWidth(15),
			progressbar.OptionSetDescription("Scanning"),
			progressbar.OptionShowCount(),
			progressbar.OptionClearOnFinish(),
		)
	} else {
		// SQL mode or unknown count: Indeterminate spinner
		bar = progressbar.NewOptions64(-1,
			progressbar.OptionSetWriter(os.Stderr),
			progressbar.OptionSetDescription("Found"),
			progressbar.OptionClearOnFinish(),
			progressbar.OptionShowCount(),
		)
	}

	// Helper to print matches without breaking the bar
	printMatch := func(path string) {
		bar.Clear()
		fmt.Println(path)
		// For regex mode, the bar updates frequently so it will redraw.
		// For SQL mode, we manually force a redraw if needed, or the next Add(1) does it.
	}

	if cfg.IsRegex {
		// Regex Mode: Iterate in Go
		re, err := regexp.Compile(cfg.Pattern)
		if err != nil {
			return fmt.Errorf("invalid regex pattern: %w", err)
		}

		err = dbStore.IterateFiles(snap.ID, func(f models.FileRecord) error {
			bar.Add(1)
			if re.MatchString(f.Filename) {
				printMatch(f.Path)
			}
			return nil
		})
	} else {
		// SQL Search Mode
		err = dbStore.SearchFiles(snap.ID, cfg.Pattern, cfg.CaseSensitive, func(f models.FileRecord) error {
			printMatch(f.Path)
			bar.Add(1) // Increment found count
			return nil
		})
	}

	// Finish bar to clear it
	bar.Finish()

	if err != nil {
		return fmt.Errorf("error searching files: %w", err)
	}

	return nil
}
