package sqlite

import (
	"database/sql"
	"fmt"
	"strconv"
)

// Current Schema Version
const CurrentSchemaVersion = 2

// Migrations
// We define them as function closures so we can execute logic if needed, or just SQL.
var migrations = []func(*sql.DB) error{
	// Version 1: Baseline (Pre-existing)
	// We don't actually run SQL for v1 because we assume "InitSchema" sets up the base,
	// OR if it's a legacy DB, it's already in v1 state.
	// However, for a *fresh* DB, we need to create the base tables.
	// So, let's include the base creation here.
	func(db *sql.DB) error {
		queries := []string{
			`CREATE TABLE IF NOT EXISTS snapshots (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				name TEXT NOT NULL,
				root_path TEXT NOT NULL,
				started_at DATETIME NOT NULL,
				finished_at DATETIME,
				status TEXT NOT NULL,
				hashes TEXT DEFAULT ''
			);`,
			`CREATE TABLE IF NOT EXISTS files (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				snapshot_id INTEGER NOT NULL,
				path TEXT NOT NULL,
				filename TEXT NOT NULL,
				size_bytes INTEGER NOT NULL,
				mod_time DATETIME NOT NULL,
				sha1 TEXT NOT NULL,
				md5 TEXT NOT NULL,
				FOREIGN KEY(snapshot_id) REFERENCES snapshots(id),
				CHECK (length(sha1) > 0 OR length(md5) > 0)
			);`,
			`CREATE INDEX IF NOT EXISTS idx_files_snapshot_path ON files(snapshot_id, path);`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_snapshots_name ON snapshots(name);`,
		}
		for _, q := range queries {
			if _, err := db.Exec(q); err != nil {
				return err
			}
		}
		return nil
	},
	// Version 2: Add computer_name to snapshots
	func(db *sql.DB) error {
		_, err := db.Exec(`ALTER TABLE snapshots ADD COLUMN computer_name TEXT DEFAULT '';`)
		return err
	},
}

func (s *SqliteStore) migrate() error {
	// 1. Ensure metadata table exists
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS db_metadata (key TEXT PRIMARY KEY, value TEXT)`)
	if err != nil {
		return fmt.Errorf("failed to create metadata table: %w", err)
	}

	// 2. Get current version
	version, err := s.getSchemaVersion()
	if err != nil {
		return err
	}

	// 3. Handle Legacy/Fresh States
	if version == 0 {
		// Detect if this is a legacy DB (tables exist but no metadata)
		if s.tableExists("snapshots") {
			// It is legacy. Assume it is Version 1.
			// We skip Migration 0 (which is index 0, so v1).
			// We just set version to 1 and proceed.
			if err := s.setSchemaVersion(1); err != nil {
				return err
			}
			version = 1
			fmt.Println("Detected legacy database. Marked as Schema Version 1.")
		} else {
			// Fresh DB. Version 0.
			// Proceed to apply all migrations.
		}
	}
	
	if version >= CurrentSchemaVersion {
		return nil // Up to date
	}

	fmt.Printf("Migrating database from version %d to %d...\n", version, CurrentSchemaVersion)

	// 4. Apply Migrations
	// migrations[0] upgrades v0 -> v1
	// migrations[1] upgrades v1 -> v2
	// So if version is 1, we start at index 1.
	for i := version; i < CurrentSchemaVersion; i++ {
		fmt.Printf("Applying migration %d...\n", i+1)
		err := migrations[i](s.db)
		if err != nil {
			return fmt.Errorf("migration %d failed: %w", i+1, err)
		}
		
		// Update version
		if err := s.setSchemaVersion(i + 1); err != nil {
			return fmt.Errorf("failed to update schema version to %d: %w", i+1, err)
		}
	}
	
	return nil
}

func (s *SqliteStore) getSchemaVersion() (int, error) {
	var val string
	err := s.db.QueryRow(`SELECT value FROM db_metadata WHERE key = 'schema_version'`).Scan(&val)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("invalid schema_version: %v", val)
	}
	return v, nil
}

func (s *SqliteStore) setSchemaVersion(v int) error {
	val := strconv.Itoa(v)
	_, err := s.db.Exec(`INSERT INTO db_metadata (key, value) VALUES ('schema_version', ?) 
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, val)
	return err
}

func (s *SqliteStore) tableExists(name string) bool {
	var n string
	err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return err == nil
}
