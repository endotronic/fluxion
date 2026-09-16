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

// rebasePath moves p from under oldRoot to under newRoot, leaving it alone if
// it is not under oldRoot at all.
//
// This is how a scan records where a file *is* rather than where it happened to
// be readable from. zfs-scan walks a dataset through a throwaway mount point and
// must not record that: the directory is gone by the time anyone reads the
// snapshot, and a ZFS mountpoint is a property that can change under you
// anyway. What is stable is the dataset's own name and the path within it.
// rootsDisjoint reports whether no root in roots can contain a path also
// reachable through another root in the list: none are equal, and none is a
// path-boundary prefix of another.
//
// Every file's stored path is guaranteed to begin with its own snapshot's
// root path (rebasePath above feeds CreateSnapshot in snapshot.go, and the
// scanner's own walk root does the same for an ordinary snapshot), so when a
// set of inputs' roots are pairwise disjoint by this definition, their file
// paths cannot collide - a merge across them needs no collision bookkeeping
// at all. Per-dataset zfs-scan snapshots are exactly this case: each is
// rooted at its own dataset name, or, before issue 2.8 was fixed, its own
// randomly generated scan mount - either way, disjoint.
func rootsDisjoint(roots []string) bool {
	for i := 0; i < len(roots); i++ {
		for j := i + 1; j < len(roots); j++ {
			if pathHasPrefix(roots[i], roots[j]) || pathHasPrefix(roots[j], roots[i]) {
				return false
			}
		}
	}
	return true
}

func rebasePath(p, oldRoot, newRoot string) string {
	if oldRoot == newRoot || !pathHasPrefix(p, oldRoot) {
		return p
	}
	rel, err := filepath.Rel(filepath.Clean(oldRoot), filepath.Clean(p))
	if err != nil {
		return p
	}
	if rel == "." {
		return newRoot
	}
	return filepath.Join(newRoot, rel)
}
