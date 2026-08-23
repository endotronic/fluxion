package sqlite

import (
	"fluxion/internal/models"
	"testing"
	"time"
)

// Timestamps written by the previous cgo driver (mattn/go-sqlite3) use a layout
// that the current pure-Go driver does not produce. Databases created before the
// driver swap must keep loading, so this pins the compatibility both ways.
func TestTimestampCompatibility(t *testing.T) {
	layouts := map[string]string{
		"legacy cgo driver": "2024-03-01 09:15:30.123456789-07:00",
		"go String() form":  "2024-03-01 09:15:30.123456789 -0700 MST",
		"current RFC3339":   "2024-03-01T16:15:30.123456789Z",
		"bare sqlite":       "2024-03-01 16:15:30",
	}

	for name, stored := range layouts {
		t.Run(name, func(t *testing.T) {
			s, err := NewSqliteStore(":memory:")
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer s.Close()

			snap, err := s.CreateSnapshot("/root", "snap", "host")
			if err != nil {
				t.Fatalf("CreateSnapshot: %v", err)
			}
			if err := s.AddFile(&models.FileRecord{
				SnapshotID: snap.ID, Path: "/root/f", Filename: "f",
				SizeBytes: 1, ModTime: time.Now(), SHA1: "abc",
			}); err != nil {
				t.Fatalf("AddFile: %v", err)
			}

			// Rewrite both timestamp columns into the layout under test.
			if _, err := s.db.Exec(`UPDATE snapshots SET started_at = ?`, stored); err != nil {
				t.Fatalf("update snapshots: %v", err)
			}
			if _, err := s.db.Exec(`UPDATE files SET mod_time = ?`, stored); err != nil {
				t.Fatalf("update files: %v", err)
			}

			got, err := s.FindSnapshot("snap")
			if err != nil {
				t.Fatalf("FindSnapshot: %v", err)
			}
			if got.StartedAt.IsZero() {
				t.Errorf("started_at %q did not parse", stored)
			}

			var seen int
			err = s.IterateFiles(snap.ID, func(f models.FileRecord) error {
				seen++
				if f.ModTime.IsZero() {
					t.Errorf("mod_time %q did not parse", stored)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("IterateFiles: %v", err)
			}
			if seen != 1 {
				t.Errorf("expected 1 file, got %d", seen)
			}
		})
	}
}

// Stored timestamps must be free of the monotonic-clock suffix that Go's
// String() form carries, and must be readable by SQLite's own date functions.
func TestStoredTimestampIsPortable(t *testing.T) {
	s, err := NewSqliteStore(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.CreateSnapshot("/root", "snap", "host"); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	var raw string
	if err := s.db.QueryRow(`SELECT started_at FROM snapshots`).Scan(&raw); err != nil {
		t.Fatalf("scan raw: %v", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, raw); err != nil {
		t.Errorf("stored timestamp %q is not RFC3339: %v", raw, err)
	}

	var viaSQLite string
	if err := s.db.QueryRow(`SELECT datetime(started_at) FROM snapshots`).Scan(&viaSQLite); err != nil {
		t.Fatalf("datetime(): %v", err)
	}
	if viaSQLite == "" {
		t.Errorf("SQLite could not interpret stored timestamp %q", raw)
	}
}
