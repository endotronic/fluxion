package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"fluxion/internal/models"
	"fluxion/internal/store/sqlite"

	_ "modernc.org/sqlite"
)

// oldStyleDB builds a database shaped the way a pre-2026-09-10 zfs-scan left
// one: root_path is the throwaway mount, and every file path sits under it.
func oldStyleDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")

	st, err := sqlite.NewSqliteStore(path)
	if err != nil {
		t.Fatalf("create db: %v", err)
	}
	defer st.Close()

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
				SizeBytes: 1, SHA1: fmt.Sprintf("%040x", len(p)),
			})
		}
		if len(recs) > 0 {
			if err := st.BatchAddFiles(recs); err != nil {
				t.Fatalf("BatchAddFiles: %v", err)
			}
		}
	}

	add("/tmp/fluxion-zfsscan-111", "luna/mike/archives",
		[]string{"Archives/one.txt", "two.txt", "odd/100%_done_a_b.txt"})
	add("/tmp/fluxion-zfsscan-222", "luna/mike/unsorted", []string{"three.txt"})
	// An ordinary snapshot, which must be left completely alone.
	add("/mnt/backup", "manual-backup", []string{"keep.txt"})
	// A zfs-scan-looking root whose name cannot serve as a path: repairing it
	// would be guessing, so it is reported and skipped.
	add("/tmp/fluxion-zfsscan-333", "/absolute-name", []string{"four.txt"})

	return path
}

func snapshotState(t *testing.T, dbPath string) map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	out := map[string][]string{}
	rows, err := db.Query(`SELECT s.name, s.root_path, f.path FROM snapshots s
	                       LEFT JOIN files f ON f.snapshot_id = s.id ORDER BY s.id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, root string
		var p sql.NullString
		if err := rows.Scan(&name, &root, &p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		key := name + " @ " + root
		if p.Valid {
			out[key] = append(out[key], p.String)
		} else if _, ok := out[key]; !ok {
			out[key] = nil
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func TestRepair(t *testing.T) {
	dbPath := oldStyleDB(t)

	before := snapshotState(t, dbPath)

	// A dry run must change nothing at all.
	if err := run(dbPath, false, 2); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := snapshotState(t, dbPath); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("dry run modified the database\n got: %v\nwant: %v", got, before)
	}

	// A batch size below the row count, so the loop really iterates.
	if err := run(dbPath, true, 2); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got := snapshotState(t, dbPath)
	want := map[string][]string{
		"luna/mike/archives @ luna/mike/archives": {
			"luna/mike/archives/Archives/one.txt",
			"luna/mike/archives/odd/100%_done_a_b.txt",
			"luna/mike/archives/two.txt",
		},
		"luna/mike/unsorted @ luna/mike/unsorted": {"luna/mike/unsorted/three.txt"},
		// Untouched: not a zfs-scan mount.
		"manual-backup @ /mnt/backup": {"/mnt/backup/keep.txt"},
		// Untouched: the name cannot serve as a root path.
		"/absolute-name @ /tmp/fluxion-zfsscan-333": {"/tmp/fluxion-zfsscan-333/four.txt"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after repair\n got: %v\nwant: %v", got, want)
	}

	// Running it again must be a no-op, not a double rewrite.
	if err := run(dbPath, true, 2); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if got2 := snapshotState(t, dbPath); fmt.Sprint(got2) != fmt.Sprint(want) {
		t.Fatalf("re-running the repair changed the result\n got: %v\nwant: %v", got2, want)
	}
}

// Files are converted before the snapshot's root_path is, so a run killed
// halfway leaves the snapshot still matching and the next run finishes it. This
// simulates the interruption by converting a single batch by hand and then
// letting the tool loose on the half-done database.
func TestRepair_ResumesAfterAnInterruption(t *testing.T) {
	dbPath := oldStyleDB(t)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := affected{name: "luna/mike/archives", oldRoot: "/tmp/fluxion-zfsscan-111"}
	if err := db.QueryRow(`SELECT id FROM snapshots WHERE name = ?`, s.name).Scan(&s.id); err != nil {
		t.Fatalf("find snapshot: %v", err)
	}
	// One row's worth of progress, then "die" before root_path is touched.
	if _, err := db.Exec(`
		UPDATE files SET path = ? || substr(path, ?)
		 WHERE rowid IN (SELECT rowid FROM files WHERE snapshot_id = ? AND path LIKE ? LIMIT 1)`,
		s.name+"/", len(s.oldRoot)+2, s.id, s.oldRoot+"/%"); err != nil {
		t.Fatalf("partial update: %v", err)
	}
	db.Close()

	if err := run(dbPath, true, 100); err != nil {
		t.Fatalf("resume: %v", err)
	}

	got := snapshotState(t, dbPath)["luna/mike/archives @ luna/mike/archives"]
	want := []string{
		"luna/mike/archives/Archives/one.txt",
		"luna/mike/archives/odd/100%_done_a_b.txt",
		"luna/mike/archives/two.txt",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after resuming\n got: %v\nwant: %v", got, want)
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
