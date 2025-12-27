package app

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"fluxion/internal/models"
	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
)

type DeleteConfig struct {
	DBPath    string
	SnapQuery string
	Yes       bool
}

func RunDelete(cfg DeleteConfig) error {
	if cfg.DBPath == "" {
		return fmt.Errorf("DB path is required")
	}
	if cfg.SnapQuery == "" {
		return fmt.Errorf("snapshot name or ID is required")
	}

	var dbStore store.Store
	var err error
	dbStore, err = sqlite.NewSqliteStore(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("error opening DB: %w", err)
	}
	defer dbStore.Close()
	
	snap, err := dbStore.FindSnapshot(cfg.SnapQuery)
	if err != nil {
		return fmt.Errorf("error finding snapshot: %w", err)
	}
	
	// Check if already deleted
	if snap.Status == models.StatusDeleted {
		fmt.Printf("Snapshot '%s' (ID: %d) is already deleted.\n", snap.Name, snap.ID)
		return nil
	}
	
	// Get stats
	count, _ := dbStore.GetFileCount(snap.ID)
	
	if !cfg.Yes {
		fmt.Printf("About to delete snapshot:\n")
		fmt.Printf("  ID:   %d\n", snap.ID)
		fmt.Printf("  Name: %s\n", snap.Name)
		fmt.Printf("  Path: %s\n", snap.RootPath)
		fmt.Printf("  Date: %s\n", snap.StartedAt.Format(time.RFC822))
		fmt.Printf("  Files: %d\n", count)
		fmt.Printf("\nThis will permanently remove the %d file records associated with this snapshot.\n", count)
		fmt.Printf("The snapshot entry itself will remain as a tombstone.\n")
		fmt.Printf("Are you sure? [y/N]: ")
		
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			response := strings.ToLower(strings.TrimSpace(scanner.Text()))
			if response != "y" && response != "yes" {
				fmt.Println("Cancelled.")
				return nil
			}
		}
	}
	
	fmt.Printf("Deleting snapshot '%s' (ID: %d)...\n", snap.Name, snap.ID)
	err = dbStore.DeleteSnapshot(snap.ID)
	if err != nil {
		return fmt.Errorf("error deleting snapshot: %w", err)
	}
	
	fmt.Println("Success.")
	return nil
}
