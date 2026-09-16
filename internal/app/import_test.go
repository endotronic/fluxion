package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"fluxion/internal/models"
	"fluxion/internal/store/sqlite"
)

// TestRunImportDB_CopiesFilesBetweenDBs pins RunImportDB's streaming fix
// (2026-09-13): it used to load a whole source snapshot into a
// map[string]FileRecord via GetFilesForSnapshot before writing any of it out,
// the same "materialise the whole thing" defect merge's read path had before
// its own 2026-09-11 fix (knowledge/known-issues.md 3.5). This only asserts
// correctness - IterateFiles vs GetFilesForSnapshot is not something a small
// test can tell apart on memory, but a regression back to materialising would
// still have to pass this to ship.
func TestRunImportDB_CopiesFilesBetweenDBs(t *testing.T) {
	srcDir, err := os.MkdirTemp("", "fluxion_import_src")
	if err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	defer os.RemoveAll(srcDir)
	srcPath := filepath.Join(srcDir, "src.db")

	dstDir, err := os.MkdirTemp("", "fluxion_import_dst")
	if err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	defer os.RemoveAll(dstDir)
	dstPath := filepath.Join(dstDir, "dst.db")

	src, err := sqlite.NewSqliteStore(srcPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	snap, err := src.CreateSnapshot("/data", "source-snap", "host1")
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	files := []*models.FileRecord{
		{SnapshotID: snap.ID, Path: "/data/a.txt", Filename: "a.txt", SizeBytes: 10, ModTime: time.Now(), SHA1: "aaaa"},
		{SnapshotID: snap.ID, Path: "/data/b.txt", Filename: "b.txt", SizeBytes: 20, ModTime: time.Now(), SHA1: "bbbb"},
		{SnapshotID: snap.ID, Path: "/data/c.txt", Filename: "c.txt", SizeBytes: 30, ModTime: time.Now(), SHA1: "cccc"},
	}
	if err := src.BatchAddFiles(files); err != nil {
		t.Fatalf("BatchAddFiles: %v", err)
	}
	if err := src.CompleteSnapshot(snap.ID, time.Now()); err != nil {
		t.Fatalf("CompleteSnapshot: %v", err)
	}
	src.Close()

	// Destination DB must exist with the schema before import opens it.
	dst, err := sqlite.NewSqliteStore(dstPath)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	dst.Close()

	if err := RunImportDB(ImportDBConfig{
		SourceDBPath: srcPath,
		DestDBPath:   dstPath,
		ImportAll:    true,
	}); err != nil {
		t.Fatalf("RunImportDB: %v", err)
	}

	dst, err = sqlite.NewSqliteStore(dstPath)
	if err != nil {
		t.Fatalf("reopen dst: %v", err)
	}
	defer dst.Close()

	imported, err := dst.FindSnapshot("source-snap")
	if err != nil {
		t.Fatalf("FindSnapshot: %v", err)
	}

	count, err := dst.GetFileCount(imported.ID)
	if err != nil {
		t.Fatalf("GetFileCount: %v", err)
	}
	if count != int64(len(files)) {
		t.Fatalf("expected %d files, got %d", len(files), count)
	}

	got := make(map[string]models.FileRecord)
	if err := dst.IterateFiles(imported.ID, func(f models.FileRecord) error {
		got[f.Path] = f
		return nil
	}); err != nil {
		t.Fatalf("IterateFiles: %v", err)
	}
	for _, want := range files {
		f, ok := got[want.Path]
		if !ok {
			t.Errorf("missing imported file %q", want.Path)
			continue
		}
		if f.SHA1 != want.SHA1 || f.SizeBytes != want.SizeBytes {
			t.Errorf("file %q: got {sha1=%q size=%d}, want {sha1=%q size=%d}",
				want.Path, f.SHA1, f.SizeBytes, want.SHA1, want.SizeBytes)
		}
	}
}
