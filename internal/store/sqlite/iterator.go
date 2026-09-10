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

// IterateFilesDFS streams every file of a snapshot in DFS-key order: byte order
// on the path, except that '/' is treated as sorting below every other legal
// byte, so a directory's contents follow the directory itself immediately.
//
// This is what internal/diff's streaming engine requires, and plain path order
// is not it - given a file `a`, a file `a.txt` and a file `a/x`, byte order puts
// `a.txt` between the other two because '.' (0x2E) sorts below '/' (0x2F). See
// the ordering note at the top of internal/diff/streaming.go.
//
// Unlike IterateFiles this cannot be served from the index, because the sort key
// is a computed expression: the plan is a covering index scan plus a temp
// b-tree. Measured on a real 1,158,320-row snapshot that costs about the same as
// the plain ordering (1.45s vs 1.83s) - SQLite sorts short strings quickly and
// spills to disk when it must, which is the trade knowledge/diff-memory.md's
// plan assumes ("temp storage unconstrained").
func (s *SqliteStore) IterateFilesDFS(snapshotID int64, onFile func(models.FileRecord) error) error {
	rows, err := s.db.Query(`SELECT id, snapshot_id, path, filename, size_bytes, mod_time, sha1, md5 FROM files WHERE snapshot_id = ? ORDER BY replace(path, '/', char(1))`, snapshotID)
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
