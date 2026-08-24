package sqlite

import (
	"fluxion/internal/models"
	"sort"
	"strings"
	"testing"
	"time"
)

// mkSnap creates a completed snapshot holding the given path->hash pairs.
// A hash of "" records a file with no SHA-1 at all, which the schema only
// permits alongside an MD5, so those rows get a placeholder MD5.
func mkSnap(t *testing.T, s *SqliteStore, name string, files map[string]string) *models.Snapshot {
	t.Helper()
	snap, err := s.CreateSnapshot("/root/"+name, name, "host")
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	var recs []*models.FileRecord
	for p, h := range files {
		r := &models.FileRecord{
			SnapshotID: snap.ID,
			Path:       "/root/" + name + p,
			Filename:   p[strings.LastIndex(p, "/")+1:],
			SizeBytes:  100,
			ModTime:    time.Now(),
			SHA1:       h,
		}
		if h == "" {
			r.MD5 = "md5-placeholder"
		}
		recs = append(recs, r)
	}
	if err := s.BatchAddFiles(recs); err != nil {
		t.Fatalf("BatchAddFiles: %v", err)
	}
	if err := s.CompleteSnapshot(snap.ID, time.Time{}); err != nil {
		t.Fatalf("CompleteSnapshot: %v", err)
	}
	got, err := s.FindSnapshot(name)
	if err != nil {
		t.Fatalf("FindSnapshot: %v", err)
	}
	return got
}

func uncovered(t *testing.T, s *SqliteStore, cand int64, keepers []int64, minSize int64) []string {
	t.Helper()
	var out []string
	err := s.IterateUncovered(cand, keepers, "sha1", minSize, func(f models.FileRecord) error {
		out = append(out, f.Path)
		return nil
	})
	if err != nil {
		t.Fatalf("IterateUncovered: %v", err)
	}
	return out
}

func TestIterateUncovered(t *testing.T) {
	s, err := NewSqliteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// The candidate's content, and where (if anywhere) it also lives.
	cand := mkSnap(t, s, "cand", map[string]string{
		"/a.txt":         "hash-a",
		"/b.txt":         "hash-b",
		"/deep/c.txt":    "hash-c",
		"/moved/d.txt":   "hash-d",
		"/only-here.txt": "hash-z",
	})
	// keeper1 holds two of them, deliberately at completely different paths:
	// coverage is a content question, so the paths must not matter.
	keep1 := mkSnap(t, s, "keep1", map[string]string{
		"/somewhere/else.txt": "hash-a",
		"/x/y/z.bin":          "hash-d",
	})
	// keeper2 holds two more, so only hash-z is genuinely missing.
	keep2 := mkSnap(t, s, "keep2", map[string]string{
		"/p.txt": "hash-b",
		"/q.txt": "hash-c",
	})

	t.Run("single keeper", func(t *testing.T) {
		got := uncovered(t, s, cand.ID, []int64{keep1.ID}, 0)
		want := []string{"/root/cand/b.txt", "/root/cand/deep/c.txt", "/root/cand/only-here.txt"}
		assertPaths(t, got, want)
	})

	t.Run("multiple keepers union", func(t *testing.T) {
		got := uncovered(t, s, cand.ID, []int64{keep1.ID, keep2.ID}, 0)
		assertPaths(t, got, []string{"/root/cand/only-here.txt"})
	})

	t.Run("results arrive in path order", func(t *testing.T) {
		got := uncovered(t, s, cand.ID, []int64{keep2.ID}, 0)
		if !sort.StringsAreSorted(got) {
			t.Errorf("expected path order, got %v", got)
		}
	})

	t.Run("min size skips small files", func(t *testing.T) {
		got := uncovered(t, s, cand.ID, []int64{keep1.ID, keep2.ID}, 101)
		if len(got) != 0 {
			t.Errorf("every file is 100 bytes, so a 101 floor should skip all; got %v", got)
		}
	})

	t.Run("self comparison covers everything", func(t *testing.T) {
		got := uncovered(t, s, cand.ID, []int64{cand.ID}, 0)
		if len(got) != 0 {
			t.Errorf("a snapshot covers itself; got %v", got)
		}
	})
}

// A file carrying no hash of the compared type cannot be shown to exist
// anywhere. goals.md ranks a false "present" as the worst failure the tool can
// have, so it must come back as uncovered rather than be quietly dropped - and
// it must not be matched against other hashless rows by their shared emptiness.
func TestIterateUncovered_EmptyHashIsNeverCovered(t *testing.T) {
	s, err := NewSqliteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	cand := mkSnap(t, s, "cand", map[string]string{"/nohash.bin": ""})
	keep := mkSnap(t, s, "keep", map[string]string{"/also-nohash.bin": ""})

	got := uncovered(t, s, cand.ID, []int64{keep.ID}, 0)
	assertPaths(t, got, []string{"/root/cand/nohash.bin"})
}

// The query plan is the whole point of migration 5. On a toy database a table
// scan is invisible; on a fleet-sized one it is the difference between minutes
// and never finishing, and a temp b-tree sort is what puts the row set back in
// memory. Assert both stay gone.
func TestIterateUncovered_QueryPlanUsesIndexes(t *testing.T) {
	s, err := NewSqliteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	cand := mkSnap(t, s, "cand", map[string]string{"/a": "h1"})
	keep := mkSnap(t, s, "keep", map[string]string{"/b": "h1"})

	plan, err := s.ExplainUncovered(cand.ID, []int64{keep.ID}, "sha1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("query plan:\n%s", plan)

	if !strings.Contains(plan, "idx_files_sha1") {
		t.Errorf("content lookup is not using idx_files_sha1; it would scan `files` per candidate row.\nplan:\n%s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("plan sorts into a temp b-tree, which materialises the result set.\nplan:\n%s", plan)
	}
	if !strings.Contains(plan, "idx_files_snapshot_path") {
		t.Errorf("path ordering is not coming from idx_files_snapshot_path.\nplan:\n%s", plan)
	}
}

func TestSnapshotTotals(t *testing.T) {
	s, err := NewSqliteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	snap := mkSnap(t, s, "snap", map[string]string{"/a": "h1", "/b": "h2", "/c": "h3"})

	count, bytes, err := s.SnapshotTotals(snap.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || bytes != 300 {
		t.Errorf("want 3 files / 300 bytes, got %d / %d", count, bytes)
	}

	count, bytes, err = s.SnapshotTotals(snap.ID, 101)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || bytes != 0 {
		t.Errorf("want 0 files above the floor, got %d / %d", count, bytes)
	}
}

func assertPaths(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}
