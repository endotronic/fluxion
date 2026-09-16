package app

import (
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"strings"

	"fluxion/internal/diff"
	"fluxion/internal/models"
	"fluxion/internal/store"
)

// errMultiSourceOverlap is returned when two of the snapshots combined into
// one side of a diff turn out to hold the same relative path - the same
// last-input-wins collision `merge` resolves with a map, which multiSnapshotIter
// does not attempt (see its doc comment). There is no cheap, static way to
// predict this from snapshots' root paths alone (see resolveSnapshots's
// history in git blame / knowledge/known-issues.md for why a root-string
// check was tried and abandoned), so it can only be discovered by walking the
// actual data - which is exactly where this is raised.
var errMultiSourceOverlap = errors.New("snapshots combined into one side of a diff overlap")

// resolveSnapshots looks up each query, in order, failing on the first that
// does not resolve.
func resolveSnapshots(dbStore store.Store, queries []string) ([]*models.Snapshot, error) {
	snaps := make([]*models.Snapshot, len(queries))
	for i, q := range queries {
		s, err := dbStore.FindSnapshot(q)
		if err != nil {
			return nil, fmt.Errorf("could not find snapshot '%s': %w", q, err)
		}
		snaps[i] = s
	}
	return snaps, nil
}

// sideHashes reports which hash types every one of snaps recorded. A side
// built from several snapshots can only be compared using a hash type all of
// its members share - the same rule RunDiff already applies between the two
// sides, extended to within one side.
func sideHashes(snaps []*models.Snapshot) (hasSHA1, hasMD5 bool) {
	hasSHA1, hasMD5 = true, true
	for _, s := range snaps {
		snapSHA1, snapMD5 := false, false
		for _, h := range s.Hashes {
			if h == "sha1" {
				snapSHA1 = true
			}
			if h == "md5" {
				snapMD5 = true
			}
		}
		hasSHA1 = hasSHA1 && snapSHA1
		hasMD5 = hasMD5 && snapMD5
	}
	return hasSHA1, hasMD5
}

// singleSnapshotIter builds the FileIterator for one snapshot, relativizing
// its files to root and applying excludes exactly as RunDiff always has.
// root is normally the snapshot's own RootPath; a multi-source side passes
// the combined sources' common ancestor instead (see multiSnapshotIter), so
// this one function serves both the N=1 case and every source of an N>1 one.
func singleSnapshotIter(dbStore store.Store, id int64, root string, streamable bool, excludes []string) diff.FileIterator {
	return func(yield func(string, models.FileRecord) error) error {
		iterate := dbStore.IterateFiles
		if streamable {
			iterate = dbStore.IterateFilesDFS
		}
		return iterate(id, func(f models.FileRecord) error {
			if isExcluded(f.Path, root, excludes) {
				return nil
			}

			var rel string
			var err error
			if pathHasPrefix(f.Path, root) {
				rel, err = filepath.Rel(root, f.Path)
				if err != nil {
					// Should not happen if prefix matches, but fallback
					rel = f.Path
				}
			} else {
				// Not under the root: keep the absolute path. A plain
				// HasPrefix said /mnt/database/x was under /mnt/data, and
				// filepath.Rel then handed back "../database/x", which the
				// diff engine would nest under a directory literally named
				// "..".
				rel = f.Path
			}

			if err == nil {
				if isExcluded(rel, "", excludes) {
					return nil
				}
				return yield(rel, f)
			}
			// If error in Rel, default to full path (legacy behavior)
			return yield(f.Path, f)
		})
	}
}

// multiSnapshotIter combines several snapshots into one side of a diff,
// standing in for the single snapshot a real `merge` of them would produce -
// without merge's extra read-then-write pass, because diff only ever needs to
// read this side once anyway.
//
// It relativizes every source to their common ancestor (findLCA, the same
// computation `merge` uses to name its output) and merges them by relative
// path with a k-way merge over one pull cursor per source (see pullIter,
// mirroring internal/diff's own - a merge-join is exactly the shape this
// project already trusts for two sources; this is the same idea over N).
//
// A first version of this sorted sources by root path and concatenated their
// whole streams instead of truly merging them, on the reasoning that disjoint
// roots make root order a valid global path order. That reasoning has a hole
// a real ZFS fleet hits immediately: `zfs-scan` scans each dataset with
// `--cross-mounts=false`, so a parent dataset's own root (`luna/kevin`) is a
// perfectly normal *string* prefix of a child dataset's root
// (`luna/kevin/archives/2016-2020`) despite the two never sharing a single
// file - the child is a separate mounted filesystem the parent's own scan
// never descended into. Refusing that pair as "overlapping" was a false
// positive that made the feature unusable for the fleet hierarchy it exists
// for. Worse, concatenation would have been wrong even without the false
// positive: the parent's own files can sort anywhere relative to the child's
// entire subtree, so emitting the parent's whole block before the child's can
// violate global order even with zero real collisions.
//
// There is no cheap, static fix - whether two sources' *content* collides
// depends on the data, not on their root paths, so it cannot be decided
// before reading them. A genuine k-way merge sidesteps the question rather
// than answering it: it interleaves sources correctly regardless of how their
// roots relate, and the only thing that can still go wrong - two sources
// actually producing the same relative path - surfaces as two cursors tied
// for the next record, caught below and reported as errMultiSourceOverlap.
// That is still a real, if now much rarer, limitation: a genuine collision
// needs merge's last-input-wins precedence to resolve, which this does not
// attempt. See "Future work" in knowledge/diff-algo.md.
func multiSnapshotIter(dbStore store.Store, snaps []*models.Snapshot, streamable bool, excludes []string) (diff.FileIterator, string) {
	roots := make([]string, len(snaps))
	for i, s := range snaps {
		roots[i] = s.RootPath
	}
	commonRoot := findLCA(roots)

	// The comparison key must match whatever order each source is actually
	// sorted by, or the merge can pick the wrong winner and misreport a false
	// collision (found running this against the real fleet, 2026-09-14: a
	// path under a directory whose name interacts with '/' differently under
	// plain byte order than under DFS order compared as smaller than a path
	// already taken, tripping the "out of order" guard on a false positive).
	// streamable sources are read via IterateFilesDFS, which orders by
	// replace(path, '/', char(1)) (internal/store/sqlite/iterator.go) - dfsKey
	// mirrors that in Go, the same way internal/diff/streaming.go's own dfsKey
	// does for the two-source case. Non-streamable sources are read via
	// IterateFiles (plain path order), so the identity key is correct there.
	key := func(p string) string { return p }
	if streamable {
		key = dfsKey
	}

	iter := func(yield func(string, models.FileRecord) error) error {
		cursors := make([]*pullIter, 0, len(snaps))
		defer func() {
			for _, c := range cursors {
				c.stop()
			}
		}()

		active := make([]*pullIter, 0, len(snaps))
		for _, s := range snaps {
			c := newPullIter(singleSnapshotIter(dbStore, s.ID, commonRoot, streamable, excludes))
			cursors = append(cursors, c)
			if c.advance() {
				active = append(active, c)
			} else if err := c.err(); err != nil {
				return err
			}
		}

		var prevKey string
		havePrev := false
		for len(active) > 0 {
			winner := 0
			winnerKey := key(active[0].cur.path)
			for i := 1; i < len(active); i++ {
				k := key(active[i].cur.path)
				if k < winnerKey {
					winner, winnerKey = i, k
				}
			}
			cur := active[winner]

			if havePrev && winnerKey <= prevKey {
				return fmt.Errorf("%w: %q - two of the combined snapshots both hold this path",
					errMultiSourceOverlap, cur.cur.path)
			}
			prevKey, havePrev = winnerKey, true

			if err := yield(cur.cur.path, cur.cur.rec); err != nil {
				return err
			}

			if cur.advance() {
				continue
			}
			if err := cur.err(); err != nil {
				return err
			}
			active = append(active[:winner], active[winner+1:]...)
		}
		return nil
	}

	return iter, commonRoot
}

// dfsKey mirrors internal/diff/streaming.go's unexported function of the same
// name (see its doc comment there for why DFS order needs '/' to sort below
// every other byte). Duplicated rather than shared across the package
// boundary for the same reason as pullIter: it's a small, self-contained
// transform, and internal/diff deliberately keeps its testability seam
// (FileIterator) minimal.
func dfsKey(path string) string {
	return strings.ReplaceAll(path, "/", "\x01")
}

// buildSideIter dispatches to the N=1 or N>1 case. The N>1 case has no
// upfront validation to run first - see multiSnapshotIter for why a
// collision can only be discovered by walking the data, not predicted from
// snapshot metadata.
func buildSideIter(dbStore store.Store, snaps []*models.Snapshot, streamable bool, excludes []string) (diff.FileIterator, string) {
	if len(snaps) == 1 {
		return singleSnapshotIter(dbStore, snaps[0].ID, snaps[0].RootPath, streamable, excludes), snaps[0].RootPath
	}
	return multiSnapshotIter(dbStore, snaps, streamable, excludes)
}

// sumFileCounts adds up GetFileCount across snaps, for sizing the progress
// bar of a combined side. Errors are ignored the same way the single-snapshot
// path already did - a bad count only throws off the progress bar, never the
// diff itself.
func sumFileCounts(dbStore store.Store, snaps []*models.Snapshot) int64 {
	var total int64
	for _, s := range snaps {
		count, _ := dbStore.GetFileCount(s.ID)
		total += count
	}
	return total
}

// snapshotNames joins snapshots' names for a log line naming a combined side.
func snapshotNames(snaps []*models.Snapshot) string {
	names := make([]string, len(snaps))
	for i, s := range snaps {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}

// pullRecord is one yielded (path, record) pair.
type pullRecord struct {
	path string
	rec  models.FileRecord
}

// pullIter adapts a push-based diff.FileIterator to the pull semantics a
// k-way merge needs: look at the head of each source, compare, and advance
// only the one just consumed. Mirrors internal/diff's own pullIter
// (internal/diff/pulliter.go) - see its doc comment for why iter.Pull rather
// than a goroutine and channel. Duplicated rather than shared across the
// package boundary because it is small and internal/diff deliberately keeps
// its testability seam (FileIterator) minimal rather than exporting its
// internals.
type pullIter struct {
	next func() (pullRecord, bool)
	stop func()

	cur   pullRecord
	valid bool

	srcErr error
}

var errPullStopped = errors.New("app: pull iterator stopped")

func newPullIter(it diff.FileIterator) *pullIter {
	p := &pullIter{}

	seq := func(yield func(pullRecord) bool) {
		err := it(func(path string, rec models.FileRecord) error {
			if !yield(pullRecord{path: path, rec: rec}) {
				return errPullStopped
			}
			return nil
		})
		if err != nil && !errors.Is(err, errPullStopped) {
			p.srcErr = err
		}
	}

	p.next, p.stop = iter.Pull(seq)
	return p
}

// advance loads the next record, reporting whether one arrived. When it
// reports false the source is exhausted or failed - check err().
func (p *pullIter) advance() bool {
	p.cur, p.valid = p.next()
	return p.valid
}

// err reports a failure from the underlying FileIterator. Only meaningful
// after advance() has returned false.
func (p *pullIter) err() error { return p.srcErr }
