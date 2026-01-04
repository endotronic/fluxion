package sqlite

import (
	"path/filepath"
	"strings"

	"fluxion/internal/models"
)

func (s *SqliteStore) SearchFiles(snapshotID int64, pattern string, caseSensitive bool, onFile func(models.FileRecord) error) error {
	// 1. Convert Glob pattern to SQL LIKE pattern for pre-filtering
	// * -> %
	// ? -> _
	// Escape %, _, \
	likePattern := globToLike(pattern)

	// query: Case-insensitive LIKE is default in SQLite (ASCII)
	// We use LIKE as a coarser filter to reduce data transfer, then verify with Go's filepath.Match
	query := "SELECT id, snapshot_id, path, filename, size_bytes, mod_time, sha1, md5 FROM files WHERE snapshot_id = ? AND filename LIKE ? ESCAPE '/'"

	rows, err := s.db.Query(query, snapshotID, likePattern)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var f models.FileRecord
		if err := rows.Scan(&f.ID, &f.SnapshotID, &f.Path, &f.Filename, &f.SizeBytes, &f.ModTime, &f.SHA1, &f.MD5); err != nil {
			return err
		}

		// 2. Precise Filtering in Go
		var matched bool
		var matchErr error
		if caseSensitive {
			matched, matchErr = filepath.Match(pattern, f.Filename)
		} else {
			// filepath.Match is case-sensitive, so convert both to lower case for case-insensitive matching
			matched, matchErr = filepath.Match(strings.ToLower(pattern), strings.ToLower(f.Filename))
		}

		if matchErr != nil {
			return matchErr
		}

		if matched {
			if err := onFile(f); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

func globToLike(glob string) string {
	var sb strings.Builder
	for _, r := range glob {
		switch r {
		case '*':
			sb.WriteRune('%')
		case '?':
			sb.WriteRune('_')
		case '%', '_', '/': // Characters that need escaping in LIKE
			sb.WriteRune('/')
			sb.WriteRune(r)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
