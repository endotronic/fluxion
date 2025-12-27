package app

import (
	"fmt"
	"os"
	"time"

	"fluxion/internal/store"
	"fluxion/internal/store/sqlite"
	"fluxion/internal/table"
)

type ListConfig struct {
	DBPath string
}

func RunList(cfg ListConfig) error {
	if cfg.DBPath == "" {
		return fmt.Errorf("DB path is required")
	}

	var dbStore store.Store
	var err error
	dbStore, err = sqlite.NewSqliteStore(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("error opening DB: %w", err)
	}
	defer dbStore.Close()

	snaps, err := dbStore.ListSnapshots()
	if err != nil {
		return fmt.Errorf("error listing snapshots: %w", err)
	}

	if len(snaps) == 0 {
		fmt.Println("No snapshots found.")
		return nil
	}

	tbl := table.New([]string{"ID", "Name", "Status", "Started At", "Finished At", "Computer", "Root Path"})

	for _, s := range snaps {
		finished := "N/A"
		if s.FinishedAt != nil {
			finished = s.FinishedAt.Format(time.RFC3339)
		}
		
		tbl.AddRow([]string{
			fmt.Sprintf("%d", s.ID),
			s.Name,
			string(s.Status),
			s.StartedAt.Format(time.RFC3339),
			finished,
			s.ComputerName,
			s.RootPath,
		})
	}
	
	tbl.Print(os.Stdout)
	return nil
}
