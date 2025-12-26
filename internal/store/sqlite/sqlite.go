package sqlite

import (
	"database/sql"
	"file-hasher/internal/models"
	"file-hasher/internal/store"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type SqliteStore struct {
	db *sql.DB
}

// Ensure SqliteStore implements store.Store
var _ store.Store = (*SqliteStore)(nil)

func NewSqliteStore(dbPath string) (*SqliteStore, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		return nil, err
	}

	s := &SqliteStore{db: db}
	if err := s.initSchema(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *SqliteStore) Close() error {
	return s.db.Close()
}

func (s *SqliteStore) initSchema() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			root_path TEXT NOT NULL,
			started_at DATETIME NOT NULL,
			finished_at DATETIME,
			status TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS files (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			snapshot_id INTEGER NOT NULL,
			path TEXT NOT NULL,
			filename TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			mod_time DATETIME NOT NULL,
			sha1 TEXT NOT NULL,
			FOREIGN KEY(snapshot_id) REFERENCES snapshots(id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_files_snapshot_path ON files(snapshot_id, path);`,
	}

	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("init schema error: %w", err)
		}
	}
	return nil
}

func (s *SqliteStore) CreateSnapshot(rootPath string) (*models.Snapshot, error) {
	now := time.Now()
	res, err := s.db.Exec(`INSERT INTO snapshots (root_path, started_at, status) VALUES (?, ?, ?)`,
		rootPath, now, models.StatusInProgress)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &models.Snapshot{
		ID:        id,
		RootPath:  rootPath,
		StartedAt: now,
		Status:    models.StatusInProgress,
	}, nil
}

func (s *SqliteStore) GetLastSnapshot(rootPath string) (*models.Snapshot, error) {
	row := s.db.QueryRow(`SELECT id, root_path, started_at, finished_at, status FROM snapshots WHERE root_path = ? ORDER BY id DESC LIMIT 1`, rootPath)
	
	var snap models.Snapshot
	var finishedAt sql.NullTime
	err := row.Scan(&snap.ID, &snap.RootPath, &snap.StartedAt, &finishedAt, &snap.Status)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		snap.FinishedAt = &t
	}
	return &snap, nil
}

func (s *SqliteStore) ListSnapshots() ([]*models.Snapshot, error) {
	rows, err := s.db.Query(`SELECT id, root_path, started_at, finished_at, status FROM snapshots ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []*models.Snapshot
	for rows.Next() {
		var snap models.Snapshot
		var finishedAt sql.NullTime
		if err := rows.Scan(&snap.ID, &snap.RootPath, &snap.StartedAt, &finishedAt, &snap.Status); err != nil {
			return nil, err
		}
		if finishedAt.Valid {
			t := finishedAt.Time
			snap.FinishedAt = &t
		}
		snaps = append(snaps, &snap)
	}
	return snaps, nil
}

func (s *SqliteStore) CompleteSnapshot(id int64) error {
	now := time.Now()
	_, err := s.db.Exec(`UPDATE snapshots SET status = ?, finished_at = ? WHERE id = ?`, models.StatusCompleted, now, id)
	return err
}

func (s *SqliteStore) AddFile(f *models.FileRecord) error {
	_, err := s.db.Exec(`INSERT INTO files (snapshot_id, path, filename, size_bytes, mod_time, sha1) VALUES (?, ?, ?, ?, ?, ?)`,
		f.SnapshotID, f.Path, f.Filename, f.SizeBytes, f.ModTime, f.SHA1)
	return err
}

func (s *SqliteStore) BatchAddFiles(files []*models.FileRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO files (snapshot_id, path, filename, size_bytes, mod_time, sha1) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, f := range files {
		_, err := stmt.Exec(f.SnapshotID, f.Path, f.Filename, f.SizeBytes, f.ModTime, f.SHA1)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *SqliteStore) GetFileCount(snapshotID int64) (int64, error) {
	var count int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM files WHERE snapshot_id = ?`, snapshotID).Scan(&count)
	return count, err
}

func (s *SqliteStore) GetFilesForSnapshot(snapshotID int64, onProgress func(int)) (map[string]models.FileRecord, error) {
	rows, err := s.db.Query(`SELECT id, snapshot_id, path, filename, size_bytes, mod_time, sha1 FROM files WHERE snapshot_id = ?`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]models.FileRecord)
	count := 0
	for rows.Next() {
		var f models.FileRecord
		if err := rows.Scan(&f.ID, &f.SnapshotID, &f.Path, &f.Filename, &f.SizeBytes, &f.ModTime, &f.SHA1); err != nil {
			return nil, err
		}
		result[f.Path] = f
		count++
		if onProgress != nil && count%1000 == 0 {
			onProgress(count)
		}
	}
	if onProgress != nil {
		onProgress(count)
	}
	return result, nil
}
