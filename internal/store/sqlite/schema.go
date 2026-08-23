package sqlite

import (
	"database/sql"
	"fmt"
	"strconv"
)

// Current Schema Version
const CurrentSchemaVersion = 4

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
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_files_snapshot_path ON files(snapshot_id, path);`,
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
	// Version 3: Record how many files a scan failed to read.
	//
	// Before this column existed a scan that could not read part of the tree was
	// marked "completed" and was indistinguishable from a clean one - which is the
	// worst failure this tool can have, because the missing files then look deleted.
	// Snapshots taken before this migration report 0 because the information was
	// never recorded, not because they were error-free.
	func(db *sql.DB) error {
		_, err := db.Exec(`ALTER TABLE snapshots ADD COLUMN error_count INTEGER DEFAULT 0;`)
		return err
	},
	// Version 4: Enforce one row per (snapshot_id, path).
	//
	// The index on these columns was never unique, so nothing stopped the same
	// file being recorded twice in one snapshot. Resuming a scan with --md5
	// newly enabled did exactly that: records lacking an MD5 were re-hashed and
	// inserted alongside the originals. Duplicates double-count in `size` and
	// make `dupes` report a file as a copy of itself.
	func(db *sql.DB) error {
		// The de-duplicating DELETE is expensive on a large table, so only run it
		// when duplicates actually exist. The check rides the existing index.
		var dup int
		err := db.QueryRow(
			`SELECT 1 FROM files GROUP BY snapshot_id, path HAVING COUNT(*) > 1 LIMIT 1`,
		).Scan(&dup)
		if err != nil && err != sql.ErrNoRows {
			return err
		}

		if err == nil {
			fmt.Println("Removing duplicate file records (keeping the most recent of each)...")
			if _, err := db.Exec(
				`DELETE FROM files WHERE id NOT IN (SELECT MAX(id) FROM files GROUP BY snapshot_id, path)`,
			); err != nil {
				return err
			}
		}

		// Replace the non-unique index rather than adding a second one covering
		// the same columns.
		if _, err := db.Exec(`DROP INDEX IF EXISTS idx_files_snapshot_path;`); err != nil {
			return err
		}
		_, err = db.Exec(`CREATE UNIQUE INDEX idx_files_snapshot_path ON files(snapshot_id, path);`)
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
