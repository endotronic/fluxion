package app

import (
	"path/filepath"
	"strings"
)

// pathHasPrefix reports whether path is prefix itself or lies underneath it,
// with the match required to land on a path boundary.
//
// A plain strings.HasPrefix does not: it makes "--exclude data" also drop
// "data2/" and "database/", and "--exclude secrets.txt" also drop
// "secrets.txt.bak". In a tool whose job is proving a tree is safe to destroy,
// that is the failure knowledge/goals.md ranks worst - the diff comes back clean
// because content was excluded, not because it is covered, and the user deletes
// on the strength of it. Confirmed as issue 1.3 in knowledge/known-issues.md.
//
// Both sides are cleaned first, so "--exclude data/" and "--exclude ./data"
// mean what they look like. An empty prefix matches nothing rather than
// everything: "--exclude ”" silently excluding the entire snapshot is the same
// severity-1 direction.
func pathHasPrefix(path, prefix string) bool {
	if path == "" || prefix == "" {
		return false
	}
	path, prefix = filepath.Clean(path), filepath.Clean(prefix)

	if path == prefix {
		return true
	}
	// Clean leaves a trailing separator only on a root ("/" here, "C:\" on
	// Windows), where the boundary is already part of the prefix.
	if prefix[len(prefix)-1] == filepath.Separator {
		return strings.HasPrefix(path, prefix)
	}
	return strings.HasPrefix(path, prefix+string(filepath.Separator))
}
