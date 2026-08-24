package sqlite

import (
	"fmt"
	"strings"

	"fluxion/internal/models"
)

// hashColumn maps a strategy name to its column. Callers pass a value that came
// from a snapshot's recorded hash list, so anything else is a programming error
// rather than bad user input.
func hashColumn(hashType string) (string, error) {
	switch hashType {
	case "sha1":
		return "sha1", nil
	case "md5":
		return "md5", nil
	default:
		return "", fmt.Errorf("unsupported hash type %q", hashType)
	}
}

// SnapshotTotals reports how many files a snapshot holds at or above minSize,
// and how many bytes they account for.
func (s *SqliteStore) SnapshotTotals(snapshotID int64, minSize int64) (int64, int64, error) {
	var count, bytes int64
	err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM files
		 WHERE snapshot_id = ? AND size_bytes >= ?`,
		snapshotID, minSize).Scan(&count, &bytes)
	return count, bytes, err
}

// IterateUncovered streams, in path order, every file of candidateID whose
// content hash does not appear in any of keeperIDs.
//
// This is the whole of the "is it safe to delete this copy?" question, asked
// without building a diff tree: content is what matters for the answer, so
// paths are carried for reporting only and never compared. Memory is bounded by
// the driver's row buffer regardless of snapshot size - see
// knowledge/diff-memory.md for why that mattered enough to add a command.
//
// Files whose hash of the chosen type is empty cannot be compared, so they are
// yielded too, with that hash still empty. Reporting them as covered would be
// the exact failure goals.md ranks worst; it is the caller's job to distinguish
// them and say so.
func (s *SqliteStore) IterateUncovered(candidateID int64, keeperIDs []int64, hashType string, minSize int64, onFile func(models.FileRecord) error) error {
	col, err := hashColumn(hashType)
	if err != nil {
		return err
	}
	if len(keeperIDs) == 0 {
		return fmt.Errorf("at least one snapshot to check against is required")
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keeperIDs)), ",")
	args := []any{candidateID, minSize}
	for _, id := range keeperIDs {
		args = append(args, id)
	}

	// The `g.<col> != ''` term is not redundant with the equality above it: it
	// is what lets SQLite prove the partial index from migration 5 applies.
	// Without it the subquery falls back to a scan of `files` per candidate row.
	query := fmt.Sprintf(`
		SELECT f.path, f.filename, f.size_bytes, f.sha1, f.md5
		FROM files f
		WHERE f.snapshot_id = ?
		  AND f.size_bytes >= ?
		  AND (
		    f.%[1]s = ''
		    OR NOT EXISTS (
		      SELECT 1 FROM files g
		      WHERE g.%[1]s = f.%[1]s AND g.%[1]s != '' AND g.snapshot_id IN (%[2]s)
		    )
		  )
		ORDER BY f.path`, col, placeholders)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var f models.FileRecord
		f.SnapshotID = candidateID
		if err := rows.Scan(&f.Path, &f.Filename, &f.SizeBytes, &f.SHA1, &f.MD5); err != nil {
			return err
		}
		if err := onFile(f); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ExplainUncovered returns SQLite's query plan for the IterateUncovered query.
// It exists so a test can assert the plan stays a pair of index lookups: a
// regression to a table scan or a temp b-tree sort is invisible on small
// databases and fatal on real ones.
func (s *SqliteStore) ExplainUncovered(candidateID int64, keeperIDs []int64, hashType string) (string, error) {
	col, err := hashColumn(hashType)
	if err != nil {
		return "", err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keeperIDs)), ",")
	args := []any{candidateID, int64(0)}
	for _, id := range keeperIDs {
		args = append(args, id)
	}

	query := fmt.Sprintf(`EXPLAIN QUERY PLAN
		SELECT f.path, f.filename, f.size_bytes, f.sha1, f.md5
		FROM files f
		WHERE f.snapshot_id = ?
		  AND f.size_bytes >= ?
		  AND (
		    f.%[1]s = ''
		    OR NOT EXISTS (
		      SELECT 1 FROM files g
		      WHERE g.%[1]s = f.%[1]s AND g.%[1]s != '' AND g.snapshot_id IN (%[2]s)
		    )
		  )
		ORDER BY f.path`, col, placeholders)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id, parent, notused int64
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			return "", err
		}
		out = append(out, detail)
	}
	return strings.Join(out, "\n"), rows.Err()
}
