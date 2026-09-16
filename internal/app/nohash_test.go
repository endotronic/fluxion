package app

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fluxion/internal/models"
	"fluxion/internal/store/sqlite"

	_ "modernc.org/sqlite"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, contents := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A metadata-only scan records what the filesystem already knows and never opens
// a file. The point is triage: on a 185T fleet, hashing everything to find out
// which trees even overlap costs weeks, while size and name are enough to rule
// most pairs out.
func TestSnapshot_NoHash(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	dir := writeTree(t, map[string]string{
		"a.txt":     "hello",
		"sub/b.txt": "world!!",
	})

	if err := RunSnapshot(SnapshotConfig{
		TargetDir: dir, DBPath: dbPath, Name: "meta", Threads: 2,
		SkipHashing: true, SkipEstimation: true, NonInteractive: true,
	}); err != nil {
		t.Fatalf("RunSnapshot: %v", err)
	}

	st, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	snap, err := st.FindSnapshot("meta")
	if err != nil {
		t.Fatalf("FindSnapshot: %v", err)
	}
	if len(snap.Hashes) != 0 {
		t.Errorf("Hashes = %v, want none - nothing was hashed", snap.Hashes)
	}

	seen := map[string]models.FileRecord{}
	if err := st.IterateFiles(snap.ID, func(f models.FileRecord) error {
		seen[filepath.Base(f.Path)] = f
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("recorded %d files, want 2: %v", len(seen), seen)
	}
	for name, f := range seen {
		if f.SHA1 != "" || f.MD5 != "" {
			t.Errorf("%s: recorded a hash (%q/%q) despite --no-hash", name, f.SHA1, f.MD5)
		}
		if f.SizeBytes == 0 {
			t.Errorf("%s: size not recorded; metadata is the whole point", name)
		}
		if f.ModTime.IsZero() {
			t.Errorf("%s: mtime not recorded", name)
		}
	}
	if got := seen["a.txt"].SizeBytes; got != 5 {
		t.Errorf("a.txt size = %d, want 5", got)
	}
	if got := seen["b.txt"].SizeBytes; got != 7 {
		t.Errorf("b.txt size = %d, want 7", got)
	}
}

// The severity rule in knowledge/goals.md, applied to a snapshot that carries no
// hash at all: it may report that trees differ, and must never report that they
// match. This is the property that makes metadata-only scans safe to have.
func TestSnapshot_NoHash_NeverReportsFilesAsMatching(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	// Byte-for-byte identical trees, so a hashing scan would call every file
	// Unchanged and coverage would call every file covered.
	same := map[string]string{"a.txt": "identical", "sub/b.txt": "identical too"}
	dirA, dirB := writeTree(t, same), writeTree(t, same)

	for name, dir := range map[string]string{"A": dirA, "B": dirB} {
		if err := RunSnapshot(SnapshotConfig{
			TargetDir: dir, DBPath: dbPath, Name: name, Threads: 2,
			SkipHashing: true, SkipEstimation: true, NonInteractive: true,
		}); err != nil {
			t.Fatalf("RunSnapshot(%s): %v", name, err)
		}
	}

	// diff refuses outright: with no hash on either side there is no algorithm
	// in common, and guessing "unchanged" from size and mtime is exactly the
	// claim goals.md forbids.
	err := RunDiff(DiffConfig{DBPath: dbPath, OldQueries: []string{"A"}, NewQueries: []string{"B"}})
	if err == nil {
		t.Fatal("diff of two hash-less snapshots succeeded; it must refuse rather than " +
			"report files as unchanged on the strength of size and mtime")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "hash") {
		t.Errorf("diff error should say the hashes are the problem, got: %v", err)
	}
}

// An older database still constrains every row to carry a hash. Nothing removes
// that constraint (see the note in schema.go), so the scan has to say so
// clearly rather than fail on the first insert with a constraint violation.
func TestSnapshot_NoHash_RefusesOnADatabaseThatCannotStoreIt(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	st, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.SupportsHashlessFiles(); err != nil || !ok {
		t.Fatalf("a fresh database should accept hash-less rows: ok=%v err=%v", ok, err)
	}
	st.Close()

	// Put the old constraint back, the way a pre-2026-09-10 database has it.
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		PRAGMA writable_schema = ON;
		UPDATE sqlite_master SET sql = replace(sql,
			'FOREIGN KEY(snapshot_id) REFERENCES snapshots(id)',
			'FOREIGN KEY(snapshot_id) REFERENCES snapshots(id), CHECK (length(sha1) > 0 OR length(md5) > 0)')
		WHERE type = 'table' AND name = 'files';
		PRAGMA writable_schema = RESET;`); err != nil {
		t.Fatalf("re-creating the old constraint: %v", err)
	}
	raw.Close()

	st2, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st2.SupportsHashlessFiles(); err != nil || ok {
		t.Fatalf("expected the old constraint to be detected: ok=%v err=%v", ok, err)
	}
	st2.Close()

	dir := writeTree(t, map[string]string{"a.txt": "hello"})
	err = RunSnapshot(SnapshotConfig{
		TargetDir: dir, DBPath: dbPath, Name: "meta", Threads: 1,
		SkipHashing: true, SkipEstimation: true, NonInteractive: true,
	})
	if err == nil {
		t.Fatal("expected a refusal on a database that cannot store hash-less rows")
	}
	if !strings.Contains(err.Error(), "convert-db") {
		t.Errorf("the refusal should say what to do about it, got: %v", err)
	}
}
