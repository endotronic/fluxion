package sqlite

import (
	"database/sql"
	"fluxion/internal/models"
	"fluxion/internal/store"
	"fmt"
	"strings"
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
	if err := s.migrate(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *SqliteStore) Close() error {
	return s.db.Close()
}

func (s *SqliteStore) CreateSnapshot(rootPath, name, computerName string) (*models.Snapshot, error) {
	now := time.Now()
	res, err := s.db.Exec(`INSERT INTO snapshots (name, root_path, computer_name, started_at, status) VALUES (?, ?, ?, ?, ?)`,
		name, rootPath, computerName, now, models.StatusInProgress)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &models.Snapshot{
		ID:           id,
		RootPath:     rootPath,
		Name:         name,
		ComputerName: computerName,
		StartedAt:    now,
		Status:       models.StatusInProgress,
	}, nil
}

func (s *SqliteStore) GetLastSnapshot(rootPath string) (*models.Snapshot, error) {
	row := s.db.QueryRow(`SELECT id, name, root_path, computer_name, started_at, finished_at, status, hashes FROM snapshots WHERE root_path = ? ORDER BY id DESC LIMIT 1`, rootPath)
	var snap models.Snapshot
	var hashStr sql.NullString
	if err := row.Scan(&snap.ID, &snap.Name, &snap.RootPath, &snap.ComputerName, &snap.StartedAt, &snap.FinishedAt, &snap.Status, &hashStr); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // No previous snapshot
		}
		// If column missing, it might error. But we assume schema up to date.
		// Actually, if we just migrated, we are good.
		return nil, err
	}
	if hashStr.Valid && hashStr.String != "" {
		snap.Hashes = strings.Split(hashStr.String, ",")
	}
	return &snap, nil
}

func (s *SqliteStore) FindSnapshot(query string) (*models.Snapshot, error) {
	// 1. Try as ID
	var snap models.Snapshot
	query = strings.TrimSpace(query)
	var hashStr sql.NullString
	
	row := s.db.QueryRow(`SELECT id, name, root_path, computer_name, started_at, finished_at, status, hashes FROM snapshots WHERE id = ?`, query)
	err := row.Scan(&snap.ID, &snap.Name, &snap.RootPath, &snap.ComputerName, &snap.StartedAt, &snap.FinishedAt, &snap.Status, &hashStr)
	if err == nil {
		if hashStr.Valid && hashStr.String != "" {
			snap.Hashes = strings.Split(hashStr.String, ",")
		}
		return &snap, nil
	}
	
	// If failed, try Name
	row = s.db.QueryRow(`SELECT id, name, root_path, computer_name, started_at, finished_at, status, hashes FROM snapshots WHERE name = ? ORDER BY id DESC LIMIT 1`, query)
	err = row.Scan(&snap.ID, &snap.Name, &snap.RootPath, &snap.ComputerName, &snap.StartedAt, &snap.FinishedAt, &snap.Status, &hashStr)
	if err == nil {
		if hashStr.Valid && hashStr.String != "" {
			snap.Hashes = strings.Split(hashStr.String, ",")
		}
		return &snap, nil
	}
	
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("snapshot not found for query: %s", query)
	}
	return nil, err
}

func (s *SqliteStore) ListSnapshots() ([]*models.Snapshot, error) {
	rows, err := s.db.Query(`SELECT id, name, root_path, computer_name, started_at, finished_at, status, hashes FROM snapshots ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []*models.Snapshot
	for rows.Next() {
		var snap models.Snapshot
		var finishedAt sql.NullTime
		var hashStr sql.NullString
		if err := rows.Scan(&snap.ID, &snap.Name, &snap.RootPath, &snap.ComputerName, &snap.StartedAt, &finishedAt, &snap.Status, &hashStr); err != nil {
			return nil, err
		}
		if finishedAt.Valid {
			t := finishedAt.Time
			snap.FinishedAt = &t
		}
		if hashStr.Valid && hashStr.String != "" {
			snap.Hashes = strings.Split(hashStr.String, ",")
		}
		snaps = append(snaps, &snap)
	}
	return snaps, nil
}

func (s *SqliteStore) CompleteSnapshot(id int64) error {
	// Determine available hashes
	var hashes []string
	
	var dum string
	if err := s.db.QueryRow(`SELECT sha1 FROM files WHERE snapshot_id = ? AND length(sha1) > 0 LIMIT 1`, id).Scan(&dum); err == nil {
		hashes = append(hashes, "sha1")
	}
	if err := s.db.QueryRow(`SELECT md5 FROM files WHERE snapshot_id = ? AND length(md5) > 0 LIMIT 1`, id).Scan(&dum); err == nil {
		hashes = append(hashes, "md5")
	}
	
	hashesStr := strings.Join(hashes, ",")

	now := time.Now()
	_, err := s.db.Exec(`UPDATE snapshots SET status = ?, finished_at = ?, hashes = ? WHERE id = ?`, models.StatusCompleted, now, hashesStr, id)
	return err
}

func (s *SqliteStore) DeleteSnapshot(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	
	// Delete file records
	_, err = tx.Exec(`DELETE FROM files WHERE snapshot_id = ?`, id)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete files: %w", err)
	}
	
	// Update snapshot status (Soft delete / Tombstone)
	_, err = tx.Exec(`UPDATE snapshots SET status = ? WHERE id = ?`, models.StatusDeleted, id)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to update snapshot status: %w", err)
	}
	
	return tx.Commit()
}

func (s *SqliteStore) AddFile(f *models.FileRecord) error {
	_, err := s.db.Exec(`INSERT INTO files (snapshot_id, path, filename, size_bytes, mod_time, sha1, md5) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		f.SnapshotID, f.Path, f.Filename, f.SizeBytes, f.ModTime, f.SHA1, f.MD5)
	return err
}

func (s *SqliteStore) BatchAddFiles(files []*models.FileRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO files (snapshot_id, path, filename, size_bytes, mod_time, sha1, md5) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, f := range files {
		_, err := stmt.Exec(f.SnapshotID, f.Path, f.Filename, f.SizeBytes, f.ModTime, f.SHA1, f.MD5)
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
	rows, err := s.db.Query(`SELECT id, snapshot_id, path, filename, size_bytes, mod_time, sha1, md5 FROM files WHERE snapshot_id = ?`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]models.FileRecord)
	count := 0
	for rows.Next() {
		var f models.FileRecord
		if err := rows.Scan(&f.ID, &f.SnapshotID, &f.Path, &f.Filename, &f.SizeBytes, &f.ModTime, &f.SHA1, &f.MD5); err != nil {
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
