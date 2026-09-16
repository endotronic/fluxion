package app

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"fluxion/internal/diff"
	"fluxion/internal/models"
	"fluxion/internal/store/sqlite"
)

// TestMultiSourceDiff_EquivalentToMergeThenDiff is the equivalence proof this
// package's own conventions call for (see merge_test.go and
// internal/diff/mergejoin_test.go): combining several disjoint-root snapshots
// on the fly for one side of a diff must produce exactly the same
// []diff.DiffResult as physically merging them first and diffing the result -
// the thing multiSnapshotIter exists to avoid paying for.
func TestMultiSourceDiff_EquivalentToMergeThenDiff(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	ds1 := writeSnapshotFiles(t, dbPath, "ds1", "/pool/ds1", map[string]string{
		"/pool/ds1/a.txt": "hash-a",
		"/pool/ds1/b.txt": "hash-b",
	})
	ds2 := writeSnapshotFiles(t, dbPath, "ds2", "/pool/ds2", map[string]string{
		"/pool/ds2/c.txt": "hash-c",
	})
	baseline := writeSnapshotFiles(t, dbPath, "baseline", "/pool", map[string]string{
		"/pool/ds1/a.txt": "hash-a",     // unchanged
		"/pool/ds1/b.txt": "hash-b-old", // modified
		"/pool/ds2/d.txt": "hash-d",     // removed (not in ds1/ds2 union)
	})

	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer s.Close()

	multiResults := runCompare(t, s, []string{ds1, ds2}, []string{baseline})

	// Oracle: physically merge ds1+ds2, then diff the merged snapshot against
	// the same baseline the normal, already-trusted single-vs-single way.
	if err := RunMerge(MergeConfig{DBPath: dbPath, Name: "merged-oracle", Snapshots: []string{ds1, ds2}}); err != nil {
		t.Fatalf("RunMerge: %v", err)
	}
	oracleResults := runCompare(t, s, []string{"merged-oracle"}, []string{baseline})

	if !reflect.DeepEqual(multiResults, oracleResults) {
		t.Fatalf("multi-source diff disagreed with merge-then-diff.\nmulti:  %+v\noracle: %+v", multiResults, oracleResults)
	}
	if len(multiResults) == 0 {
		t.Fatal("test produced no diff output at all - it isn't testing anything")
	}
}

// TestRunDiff_AllowsNestedRootsWithDisjointContent pins the real bug found
// running this against the author's actual fleet (2026-09-13): a parent ZFS
// dataset's root ("luna/kevin") is a normal *string* prefix of a child
// dataset's root ("luna/kevin/archives/2016-2020"), because `zfs-scan` scans
// each dataset with `--cross-mounts=false` - the child is a separate mounted
// filesystem the parent's own scan never descended into, so the two never
// share a file despite the nested names. An earlier version of this feature
// refused this pair outright as "overlapping," which made it unusable for
// exactly the fleet hierarchy it was built for. It must be allowed, and the
// content from both must actually show up.
func TestRunDiff_AllowsNestedRootsWithDisjointContent(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	parent := writeSnapshotFiles(t, dbPath, "parent", "luna/kevin", map[string]string{
		"luna/kevin/toplevel.txt": "h1",
	})
	child := writeSnapshotFiles(t, dbPath, "child", "luna/kevin/archives/2016-2020", map[string]string{
		"luna/kevin/archives/2016-2020/photo.jpg": "h2",
	})
	other := writeSnapshotFiles(t, dbPath, "other", "luna/other", map[string]string{
		"luna/other/z": "h3",
	})

	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer s.Close()

	results := runCompare(t, s, []string{parent, child}, []string{other})

	// The collector is free to collapse the child's whole subtree into one
	// "archives/" line rather than naming photo.jpg explicitly - that's
	// ordinary, correct collapsing, not data loss. What must hold is that
	// every real file is accounted for somewhere (the same invariant
	// property_test.go asserts): parent's toplevel.txt and child's photo.jpg
	// are both Removed relative to "other", and "other"'s z is Added.
	var removed, added int64
	for _, r := range results {
		removed += r.RemovedCount
		added += r.AddedCount
	}
	if removed != 2 {
		t.Errorf("expected 2 removed files total (parent's + child's), got %d; results: %+v", removed, results)
	}
	if added != 1 {
		t.Errorf("expected 1 added file (other's z), got %d; results: %+v", added, results)
	}
}

// TestMultiSnapshotIter_UsesDFSOrderWhenStreamable pins the bug found running
// the real fleet comparison (2026-09-14): the merge must compare cursors using
// whatever key each source is actually sorted by, not always plain path
// order. A streamable source is read via IterateFilesDFS, which sorts by
// replace(path, '/', char(1)) - '/' sorting below every other byte, so a
// directory's contents are adjacent to it - and comparing with plain string
// order instead disagrees for exactly this classic case (see dfsKey's doc
// comment and internal/diff/streaming.go): "a" < "a.txt" < "a/x" in plain
// order, but "a" < "a/x" < "a.txt" in DFS order, because '\x01' (what '/'
// becomes) sorts below '.'. The old plain-order comparator would have
// silently emitted the wrong order here instead of erroring - worse than the
// loud false-positive collision it happened to produce on the real fleet's
// data, and not something a small case like this necessarily reproduces as
// an error either way, so this test asserts the actual output order directly.
func TestMultiSnapshotIter_UsesDFSOrderWhenStreamable(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	// "a" and "a.txt" from one source, "a/x" from another - split across
	// sources is what exposes a merge-level comparator bug; a single source's
	// own IterateFilesDFS call would get this right on its own. "two"'s root
	// is deliberately the same string as "one"'s own file "a" - the ordinary
	// shape of a file/directory name collision across independently-scanned
	// trees, and what makes their relative paths interleave at exactly the
	// point plain and DFS order disagree.
	one := writeSnapshotFiles(t, dbPath, "one", "/root", map[string]string{
		"/root/a":     "h1",
		"/root/a.txt": "h2",
	})
	two := writeSnapshotFiles(t, dbPath, "two", "/root/a", map[string]string{
		"/root/a/x": "h3",
	})

	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer s.Close()

	snaps, err := resolveSnapshots(s, []string{one, two})
	if err != nil {
		t.Fatalf("resolveSnapshots: %v", err)
	}

	iter, _ := multiSnapshotIter(s, snaps, true, nil) // streamable=true: DFS order required
	var got []string
	if err := iter(func(path string, _ models.FileRecord) error {
		got = append(got, path)
		return nil
	}); err != nil {
		t.Fatalf("multiSnapshotIter (streamable): %v", err)
	}

	want := []string{"a", "a/x", "a.txt"} // DFS order, not plain order ("a", "a.txt", "a/x")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("streamable merge order = %v, want %v (DFS order)", got, want)
	}

	// The non-streamable case reads via IterateFiles (plain path order), so
	// plain order is what it must produce.
	iterPlain, _ := multiSnapshotIter(s, snaps, false, nil)
	var gotPlain []string
	if err := iterPlain(func(path string, _ models.FileRecord) error {
		gotPlain = append(gotPlain, path)
		return nil
	}); err != nil {
		t.Fatalf("multiSnapshotIter (plain): %v", err)
	}
	wantPlain := []string{"a", "a.txt", "a/x"}
	if !reflect.DeepEqual(gotPlain, wantPlain) {
		t.Fatalf("plain merge order = %v, want %v", gotPlain, wantPlain)
	}
}

// TestMultiSnapshotIter_CatchesGenuineCollision: the one thing multiSnapshotIter
// still cannot handle is two sources actually holding the same relative path -
// that needs merge's last-input-wins precedence, which this does not attempt.
// It must be caught during the merge, not silently resolved by picking
// whichever source's cursor happened to win the tie.
func TestMultiSnapshotIter_CatchesGenuineCollision(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	one := writeSnapshotFiles(t, dbPath, "one", "/root/one", map[string]string{"/root/one/a": "h1"})
	two := writeSnapshotFiles(t, dbPath, "two", "/root/two", map[string]string{"/root/one/a": "h1"})

	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer s.Close()

	snaps, err := resolveSnapshots(s, []string{one, two})
	if err != nil {
		t.Fatalf("resolveSnapshots: %v", err)
	}

	iter, _ := multiSnapshotIter(s, snaps, false, nil)
	walkErr := iter(func(string, models.FileRecord) error { return nil })
	if walkErr == nil {
		t.Fatal("expected the merge to catch the collision, got none")
	}
	if !errors.Is(walkErr, errMultiSourceOverlap) {
		t.Fatalf("expected errMultiSourceOverlap, got: %v", walkErr)
	}
}

// runCompare drives a diff exactly the way RunDiff does internally, but
// returns the raw results instead of printing them, for equivalence
// assertions.
func runCompare(t *testing.T, s *sqlite.SqliteStore, oldQueries, newQueries []string) []diff.DiffResult {
	t.Helper()

	snapsA, err := resolveSnapshots(s, oldQueries)
	if err != nil {
		t.Fatalf("resolveSnapshots(old): %v", err)
	}
	snapsB, err := resolveSnapshots(s, newQueries)
	if err != nil {
		t.Fatalf("resolveSnapshots(new): %v", err)
	}

	iterA, rootA := buildSideIter(s, snapsA, false, nil)
	iterB, rootB := buildSideIter(s, snapsB, false, nil)

	results, err := diff.CompareSnapshots(iterA, iterB, diff.Options{
		RootA:    rootA,
		RootB:    rootB,
		HashType: "sha1",
		Engine:   diff.EngineTree,
	})
	if err != nil {
		t.Fatalf("CompareSnapshots: %v", err)
	}
	return results
}

// writeSnapshotFiles creates a completed snapshot with the given root and
// files (path -> sha1), returning its name.
func writeSnapshotFiles(t *testing.T, dbPath, name, root string, files map[string]string) string {
	t.Helper()
	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer s.Close()

	snap, err := s.CreateSnapshot(root, name, "host1")
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	var batch []*models.FileRecord
	for path, hash := range files {
		batch = append(batch, &models.FileRecord{
			SnapshotID: snap.ID,
			Path:       path,
			Filename:   filepath.Base(path),
			SizeBytes:  100,
			ModTime:    time.Now(),
			SHA1:       hash,
		})
	}
	if err := s.BatchAddFiles(batch); err != nil {
		t.Fatalf("BatchAddFiles: %v", err)
	}
	if err := s.CompleteSnapshot(snap.ID, time.Now()); err != nil {
		t.Fatalf("CompleteSnapshot: %v", err)
	}
	return name
}
