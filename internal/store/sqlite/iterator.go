package sqlite

import (
	"fluxion/internal/models"
)

func (s *SqliteStore) IterateFiles(snapshotID int64, onFile func(models.FileRecord) error) error {
	rows, err := s.db.Query(`SELECT id, snapshot_id, path, filename, size_bytes, mod_time, sha1, md5 FROM files WHERE snapshot_id = ?`, snapshotID)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var f models.FileRecord
		if err := rows.Scan(&f.ID, &f.SnapshotID, &f.Path, &f.Filename, &f.SizeBytes, &f.ModTime, &f.SHA1, &f.MD5); err != nil {
			return err
		}
		if err := onFile(f); err != nil {
			return err
		}
	}
	return rows.Err()
}
