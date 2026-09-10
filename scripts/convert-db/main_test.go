package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"fluxion/internal/models"
	"fluxion/internal/store/sqlite"

	_ "modernc.org/sqlite"
)

// oldStyleDB builds a database shaped the way a pre-2026-09-10 zfs-scan left
// one: the throwaway mount as root_path, every file path under it, and a CHECK
// on files that forbids a hash-less row.
func oldStyleDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")

	st, err := sqlite.NewSqliteStore(path)
	if err != nil {
		t.Fatalf("create db: %v", err)
	}
	add := func(root, name string, files []string) {
		snap, err := st.CreateSnapshot(root, name, "terra")
		if err != nil {
			t.Fatalf("CreateSnapshot: %v", err)
		}
		var recs []*models.FileRecord
		for _, f := range files {
			p := root + "/" + f
			recs = append(recs, &models.FileRecord{
				SnapshotID: snap.ID, Path: p, Filename: filepath.Base(p),
				SizeBytes: int64(len(p)), SHA1: fmt.Sprintf("%040x", len(p)),
			})
		}
		if err := st.BatchAddFiles(recs); err != nil {
			t.Fatalf("BatchAddFiles: %v", err)
		}
		if err := st.CompleteSnapshot(snap.ID, snap.StartedAt); err != nil {
			t.Fatalf("CompleteSnapshot: %v", err)
		}
	}
	add("/tmp/fluxion-zfsscan-111", "luna/mike/archives", []string{"Archives/one.txt", "two.txt"})
	add("/tmp/fluxion-zfsscan-222", "luna/mike/unsorted", []string{"three.txt"})
	add("/mnt/backup", "manual-backup", []string{"keep.txt"})
	add("/tmp/fluxion-zfsscan-333", "/absolute-name", []string{"four.txt"})
	st.Close()

	// Put the old CHECK back: NewSqliteStore no longer creates it, and the
	// point of this fixture is a database that still has it.
	raw, err := sql.Open("sqlite", "file:"+path)
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
	return path
}

func state(t *testing.T, dbPath string) map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	out := map[string][]string{}
	rows, err := db.Query(`SELECT s.name, s.root_path, s.status, f.path, f.sha1, f.size_bytes
	                       FROM snapshots s LEFT JOIN files f ON f.snapshot_id = s.id ORDER BY s.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, root, status string
		var p, sha sql.NullString
		var size sql.NullInt64
		if err := rows.Scan(&name, &root, &status, &p, &sha, &size); err != nil {
			t.Fatal(err)
		}
		key := name + " @ " + root + " [" + status + "]"
		if p.Valid {
			out[key] = append(out[key], fmt.Sprintf("%s sha1=%s size=%d", p.String, sha.String, size.Int64))
		} else if _, ok := out[key]; !ok {
			out[key] = nil
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func TestConvert(t *testing.T) {
	src := oldStyleDB(t)
	before := state(t, src)
	out := filepath.Join(t.TempDir(), "new.db")

	if err := convert(src, out, false, 1); err != nil { // batch 1, so the loop really iterates
		t.Fatalf("convert: %v", err)
	}

	got := state(t, out)
	want := map[string][]string{
		"luna/mike/archives @ luna/mike/archives [completed]": {
			"luna/mike/archives/Archives/one.txt sha1=" + fmt.Sprintf("%040x", len("/tmp/fluxion-zfsscan-111/Archives/one.txt")) +
				fmt.Sprintf(" size=%d", len("/tmp/fluxion-zfsscan-111/Archives/one.txt")),
			"luna/mike/archives/two.txt sha1=" + fmt.Sprintf("%040x", len("/tmp/fluxion-zfsscan-111/two.txt")) +
				fmt.Sprintf(" size=%d", len("/tmp/fluxion-zfsscan-111/two.txt")),
		},
		"luna/mike/unsorted @ luna/mike/unsorted [completed]": {
			"luna/mike/unsorted/three.txt sha1=" + fmt.Sprintf("%040x", len("/tmp/fluxion-zfsscan-222/three.txt")) +
				fmt.Sprintf(" size=%d", len("/tmp/fluxion-zfsscan-222/three.txt")),
		},
		// Not a zfs-scan mount: untouched.
		"manual-backup @ /mnt/backup [completed]": {
			"/mnt/backup/keep.txt sha1=" + fmt.Sprintf("%040x", len("/mnt/backup/keep.txt")) +
				fmt.Sprintf(" size=%d", len("/mnt/backup/keep.txt")),
		},
		// Name unusable as a root: reported and left alone rather than guessed at.
		"/absolute-name @ /tmp/fluxion-zfsscan-333 [completed]": {
			"/tmp/fluxion-zfsscan-333/four.txt sha1=" + fmt.Sprintf("%040x", len("/tmp/fluxion-zfsscan-333/four.txt")) +
				fmt.Sprintf(" size=%d", len("/tmp/fluxion-zfsscan-333/four.txt")),
		},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after conversion\n got: %v\nwant: %v", got, want)
	}

	// The source must be untouched - it is the only copy of a scan that took days.
	if after := state(t, src); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("the source database was modified\n got: %v\nwant: %v", after, before)
	}

	// And the destination must be able to hold what the source could not.
	st, err := sqlite.NewSqliteStore(out)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if ok, err := st.SupportsHashlessFiles(); err != nil || !ok {
		t.Errorf("converted database still refuses hash-less rows: ok=%v err=%v", ok, err)
	}
}

func TestConvert_RefusesToOverwriteAndResumes(t *testing.T) {
	src := oldStyleDB(t)
	out := filepath.Join(t.TempDir(), "new.db")

	if err := convert(src, out, false, 100); err != nil {
		t.Fatalf("first convert: %v", err)
	}
	want := state(t, out)

	// A second run without --resume must not touch an existing file.
	if err := convert(src, out, false, 100); err == nil {
		t.Fatal("expected a refusal to overwrite an existing --out")
	}

	// With --resume, every snapshot is already complete, so it is a no-op.
	if err := convert(src, out, true, 100); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := state(t, out); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("resuming a finished conversion changed it\n got: %v\nwant: %v", got, want)
	}

	// A half-copied snapshot is discarded and redone, not left short.
	db, err := sql.Open("sqlite", "file:"+out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM files WHERE path LIKE 'luna/mike/archives/%' LIMIT 1`); err != nil {
		// SQLite without SQLITE_ENABLE_UPDATE_DELETE_LIMIT: delete one by rowid.
		if _, err2 := db.Exec(`DELETE FROM files WHERE rowid = (
			SELECT rowid FROM files WHERE path LIKE 'luna/mike/archives/%' LIMIT 1)`); err2 != nil {
			t.Fatalf("simulating a partial copy: %v / %v", err, err2)
		}
	}
	db.Close()

	if err := convert(src, out, true, 100); err != nil {
		t.Fatalf("resume after partial: %v", err)
	}
	if got := state(t, out); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("resuming a partial snapshot did not restore it\n got: %v\nwant: %v", got, want)
	}
}

func TestIsZFSScanTempMount(t *testing.T) {
	cases := map[string]bool{
		"/tmp/fluxion-zfsscan-3181317525": true,
		"/var/tmp/fluxion-zfsscan-1":      true,
		"/tmp/fluxion-zfsscan-":           false,
		"/tmp/fluxion-zfsscan-abc":        false,
		"/tmp/fluxion-zfsscan-12ab":       false,
		"/mnt/backup":                     false,
		"/luna/mike/archives":             false,
		"":                                false,
	}
	for in, want := range cases {
		if got := isZFSScanTempMount(in); got != want {
			t.Errorf("isZFSScanTempMount(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
