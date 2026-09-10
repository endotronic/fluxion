package sqlite

import (
	"fluxion/internal/models"
)

// IterateFiles streams every file of a snapshot, in path order.
//
// The ORDER BY is load-bearing, not cosmetic: `diff`'s merge-join tree builder
// (mergeJoinInsert in internal/diff) pairs A's and B's records by advancing
// whichever side has the smaller path, and the later streaming phases planned
// in knowledge/diff-memory.md need every subtree's rows contiguous. It costs
// nothing here - idx_files_snapshot_path is (snapshot_id, path), so an
// equality match on snapshot_id already yields path order from the index, with
// no temp b-tree. Without the clause SQLite is merely *likely* to return that
// order rather than obliged to.
//
// Note for callers relativising paths afterwards (app/diff.go does): stripping
// a common prefix preserves this order, but app/diff.go's fallback branch for
// records *not* under root_path yields them at their absolute path instead, so
// a snapshot holding both kinds is no longer sorted by the time it reaches the
// consumer. Anything depending on order must tolerate that - see
// mergeJoinInsert, which is robust to it by construction.
func (s *SqliteStore) IterateFiles(snapshotID int64, onFile func(models.FileRecord) error) error {
	rows, err := s.db.Query(`SELECT id, snapshot_id, path, filename, size_bytes, mod_time, sha1, md5 FROM files WHERE snapshot_id = ? ORDER BY path`, snapshotID)
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
