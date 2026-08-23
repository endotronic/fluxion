package sqlite

import (
	"database/sql"
	"fluxion/internal/models"
	"fluxion/internal/store"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type SqliteStore struct {
	db *sql.DB
}

// Ensure SqliteStore implements store.Store
var _ store.Store = (*SqliteStore)(nil)

// buildDSN turns a filesystem path into a SQLite URI with our connection pragmas.
//
// The pragmas matter: journal_mode=WAL and synchronous=NORMAL are what make the
// bulk inserts during `snapshot` tolerable, and busy_timeout stops concurrent
// readers from failing outright with SQLITE_BUSY.
func buildDSN(dbPath string) string {
	// SQLite URI parsing percent-decodes the path, and splits parameters at '?'.
	// Escape the three characters that would otherwise change the meaning of the
	// URI so that paths containing them still open the file the caller asked for.
	r := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")
	return "file:" + r.Replace(dbPath) +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
}

func NewSqliteStore(dbPath string) (*SqliteStore, error) {
	db, err := sql.Open("sqlite", buildDSN(dbPath))
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

// dbTime renders a time for storage.
//
// The driver's default rendering of a time.Time is Go's String() form, which
// includes a monotonic-clock reading ("m=+0.002088029") and a zone abbreviation
// that no SQLite date function can parse. We store RFC3339 with nanoseconds
// instead: unambiguous, sortable as text, and understood by SQLite's own date
// helpers. Reads stay lenient so databases written by the previous (cgo) driver
// still load - see the scan path, which accepts several layouts.
func dbTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func (s *SqliteStore) Close() error {
	return s.db.Close()
}

func (s *SqliteStore) CreateSnapshot(rootPath, name, computerName string) (*models.Snapshot, error) {
	now := time.Now()
	res, err := s.db.Exec(`INSERT INTO snapshots (name, root_path, computer_name, started_at, status) VALUES (?, ?, ?, ?, ?)`,
		name, rootPath, computerName, dbTime(now), models.StatusInProgress)
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
	row := s.db.QueryRow(`SELECT id, name, root_path, computer_name, started_at, finished_at, status, hashes, error_count FROM snapshots WHERE root_path = ? ORDER BY id DESC LIMIT 1`, rootPath)
	var snap models.Snapshot
	var hashStr sql.NullString
	if err := row.Scan(&snap.ID, &snap.Name, &snap.RootPath, &snap.ComputerName, &snap.StartedAt, &snap.FinishedAt, &snap.Status, &hashStr, &snap.ErrorCount); err != nil {
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

	row := s.db.QueryRow(`SELECT id, name, root_path, computer_name, started_at, finished_at, status, hashes, error_count FROM snapshots WHERE id = ?`, query)
	err := row.Scan(&snap.ID, &snap.Name, &snap.RootPath, &snap.ComputerName, &snap.StartedAt, &snap.FinishedAt, &snap.Status, &hashStr, &snap.ErrorCount)
	if err == nil {
		if hashStr.Valid && hashStr.String != "" {
			snap.Hashes = strings.Split(hashStr.String, ",")
		}
		return &snap, nil
	}

	// If failed, try Name
	row = s.db.QueryRow(`SELECT id, name, root_path, computer_name, started_at, finished_at, status, hashes, error_count FROM snapshots WHERE name = ? ORDER BY id DESC LIMIT 1`, query)
	err = row.Scan(&snap.ID, &snap.Name, &snap.RootPath, &snap.ComputerName, &snap.StartedAt, &snap.FinishedAt, &snap.Status, &hashStr, &snap.ErrorCount)
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
	rows, err := s.db.Query(`SELECT id, name, root_path, computer_name, started_at, finished_at, status, hashes, error_count FROM snapshots ORDER BY finished_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []*models.Snapshot
	for rows.Next() {
		var snap models.Snapshot
		var finishedAt sql.NullTime
		var hashStr sql.NullString
		if err := rows.Scan(&snap.ID, &snap.Name, &snap.RootPath, &snap.ComputerName, &snap.StartedAt, &finishedAt, &snap.Status, &hashStr, &snap.ErrorCount); err != nil {
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

// detectHashes reports which hash algorithms actually made it into the snapshot.
func (s *SqliteStore) detectHashes(id int64) string {
	var hashes []string

	var dum string
	if err := s.db.QueryRow(`SELECT sha1 FROM files WHERE snapshot_id = ? AND length(sha1) > 0 LIMIT 1`, id).Scan(&dum); err == nil {
		hashes = append(hashes, "sha1")
	}
	if err := s.db.QueryRow(`SELECT md5 FROM files WHERE snapshot_id = ? AND length(md5) > 0 LIMIT 1`, id).Scan(&dum); err == nil {
		hashes = append(hashes, "md5")
	}

	return strings.Join(hashes, ",")
}

func (s *SqliteStore) finishSnapshot(id int64, status models.SnapshotStatus, finishedAt time.Time, errorCount int64) error {
	if finishedAt.IsZero() {
		finishedAt = time.Now()
	}
	_, err := s.db.Exec(
		`UPDATE snapshots SET status = ?, finished_at = ?, hashes = ?, error_count = ? WHERE id = ?`,
		status, dbTime(finishedAt), s.detectHashes(id), errorCount, id)
	return err
}

func (s *SqliteStore) CompleteSnapshot(id int64, finishedAt time.Time) error {
	return s.finishSnapshot(id, models.StatusCompleted, finishedAt, 0)
}

// FailSnapshot closes out a scan that could not read everything it walked.
//
// The rows that were collected are kept - a partial snapshot is still useful -
// but the status marks it as untrustworthy so that a later diff does not present
// the files it never managed to read as deletions.
func (s *SqliteStore) FailSnapshot(id int64, finishedAt time.Time, errorCount int64) error {
	return s.finishSnapshot(id, models.StatusFailed, finishedAt, errorCount)
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
	_, err := s.db.Exec(`INSERT INTO files (snapshot_id, path, filename, size_bytes, mod_time, sha1, md5) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(snapshot_id, path) DO UPDATE SET
			filename=excluded.filename, size_bytes=excluded.size_bytes,
			mod_time=excluded.mod_time, sha1=excluded.sha1, md5=excluded.md5`,
		f.SnapshotID, f.Path, f.Filename, f.SizeBytes, dbTime(f.ModTime), f.SHA1, f.MD5)
	return err
}

func (s *SqliteStore) BatchAddFiles(files []*models.FileRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	// Upsert: re-recording a path (a resumed scan that re-hashes files whose
	// MD5 was missing, for example) must replace the earlier row, not add a
	// second one for the same file.
	stmt, err := tx.Prepare(`INSERT INTO files (snapshot_id, path, filename, size_bytes, mod_time, sha1, md5) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(snapshot_id, path) DO UPDATE SET
			filename=excluded.filename, size_bytes=excluded.size_bytes,
			mod_time=excluded.mod_time, sha1=excluded.sha1, md5=excluded.md5`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, f := range files {
		_, err := stmt.Exec(f.SnapshotID, f.Path, f.Filename, f.SizeBytes, dbTime(f.ModTime), f.SHA1, f.MD5)
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

func (s *SqliteStore) GetFileList(snapshotID int64, onProgress func(int)) ([]*models.FileRecord, error) {
	rows, err := s.db.Query(`SELECT id, snapshot_id, path, filename, size_bytes, mod_time, sha1, md5 FROM files WHERE snapshot_id = ?`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*models.FileRecord
	count := 0
	for rows.Next() {
		var f models.FileRecord
		if err := rows.Scan(&f.ID, &f.SnapshotID, &f.Path, &f.Filename, &f.SizeBytes, &f.ModTime, &f.SHA1, &f.MD5); err != nil {
			return nil, err
		}
		result = append(result, &f)
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

func (s *SqliteStore) HasSizes(snapshotID int64) (bool, error) {
	var dum int
	err := s.db.QueryRow(`SELECT 1 FROM files WHERE snapshot_id = ? AND size_bytes > 0 LIMIT 1`, snapshotID).Scan(&dum)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *SqliteStore) GetSnapshotBytes(snapshotID int64) (int64, error) {
	var totalBytes *int64
	// COALESCE returns 0 if SUM is NULL (empty snapshot)
	row := s.db.QueryRow("SELECT SUM(size_bytes) FROM files WHERE snapshot_id = ?", snapshotID)
	if err := row.Scan(&totalBytes); err != nil {
		return 0, fmt.Errorf("failed to get snapshot bytes: %w", err)
	}
	if totalBytes == nil {
		return 0, nil
	}
	return *totalBytes, nil
}
