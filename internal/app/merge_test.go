package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"fluxion/internal/models"
	"fluxion/internal/store/sqlite"
)

func setupTestDB(t *testing.T) (string, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "fluxion_merge_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	dbPath := filepath.Join(tmpDir, "test.db")

	// Create DB to ensure schema is init
	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to init db: %v", err)
	}
	s.Close()

	return dbPath, func() {
		os.RemoveAll(tmpDir)
	}
}

func createDummySnapshot(t *testing.T, dbPath, names, root string, files []string) *models.Snapshot {
	t.Helper()
	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	defer s.Close()

	if root == "" {
		root = "/tmp"
	}

	snap, err := s.CreateSnapshot(root, names, "host1")
	if err != nil {
		t.Fatalf("failed to create snapshot: %v", err)
	}

	filesBatch := make([]*models.FileRecord, 0)
	for _, f := range files {
		filesBatch = append(filesBatch, &models.FileRecord{
			SnapshotID: snap.ID,
			Path:       f,
			Filename:   filepath.Base(f),
			SizeBytes:  100,
			ModTime:    time.Now(),
			SHA1:       "sha1",
			MD5:        "md5",
		})
	}

	if err := s.BatchAddFiles(filesBatch); err != nil {
		t.Fatalf("failed to add files: %v", err)
	}

	if err := s.CompleteSnapshot(snap.ID, time.Now()); err != nil {
		t.Fatalf("failed to complete snapshot: %v", err)
	}

	return snap
}

func TestRunMerge_Success(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	// Create source snapshots
	createDummySnapshot(t, dbPath, "snap1", "", []string{"/a/file1", "/a/file2"})
	createDummySnapshot(t, dbPath, "snap2", "", []string{"/b/file3"})

	cfg := MergeConfig{
		DBPath:    dbPath,
		Name:      "merged_snap",
		Snapshots: []string{"snap1", "snap2"},
	}

	err := RunMerge(cfg)
	if err != nil {
		t.Fatalf("RunMerge failed: %v", err)
	}

	// Verify
	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	defer s.Close()

	merged, err := s.FindSnapshot("merged_snap")
	if err != nil {
		t.Fatalf("failed to find merged snapshot: %v", err)
	}

	count, err := s.GetFileCount(merged.ID)
	if err != nil {
		t.Fatalf("failed to get file count: %v", err)
	}

	if count != 3 {
		t.Errorf("expected 3 files, got %d", count)
	}
}

// A path can only hold one thing at a time, so a path present in several inputs
// yields one record in the merged snapshot - not one per input. Preserving the
// duplicates (the old behaviour) inflated `size` and made `dupes` report the
// file as a copy of itself.
func TestRunMerge_OverlappingPathsCollapse(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	// Overlapping files
	createDummySnapshot(t, dbPath, "snap1", "", []string{"/common/file"})
	createDummySnapshot(t, dbPath, "snap2", "", []string{"/common/file"})

	cfg := MergeConfig{
		DBPath:    dbPath,
		Name:      "merged_dupes",
		Snapshots: []string{"snap1", "snap2"},
	}

	err := RunMerge(cfg)
	if err != nil {
		t.Fatalf("RunMerge failed: %v", err)
	}

	// Verify
	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	defer s.Close()

	merged, err := s.FindSnapshot("merged_dupes")
	if err != nil {
		t.Fatalf("failed to find merged snapshot: %v", err)
	}

	count, err := s.GetFileCount(merged.ID)
	if err != nil {
		t.Fatalf("failed to get file count: %v", err)
	}

	if count != 1 {
		t.Errorf("expected 1 file (one row per path), got %d", count)
	}
}

func TestRunMerge_Hostname(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	createDummySnapshot(t, dbPath, "snap1", "", []string{"/a/1"})
	createDummySnapshot(t, dbPath, "snap2", "", []string{"/b/2"})

	expectedHost := "custom-server"
	cfg := MergeConfig{
		DBPath:    dbPath,
		Name:      "merged_host",
		Hostname:  expectedHost,
		Snapshots: []string{"snap1", "snap2"},
	}

	if err := RunMerge(cfg); err != nil {
		t.Fatalf("RunMerge failed: %v", err)
	}

	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	defer s.Close()

	merged, err := s.FindSnapshot("merged_host")
	if err != nil {
		t.Fatalf("failed to find merged snapshot: %v", err)
	}

	if merged.ComputerName != expectedHost {
		t.Errorf("expected hostname %s, got %s", expectedHost, merged.ComputerName)
	}
}

func TestRunMerge_LCA(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	// Create snapshots with different roots
	// Snap 1: /home/user/docs/work
	// Snap 2: /home/user/pictures
	// LCA: /home/user

	createDummySnapshot(t, dbPath, "snap1", "/home/user/docs/work", []string{"/home/user/docs/work/file1"})
	createDummySnapshot(t, dbPath, "snap2", "/home/user/pictures", []string{"/home/user/pictures/pic1"})

	cfg := MergeConfig{
		DBPath:    dbPath,
		Name:      "merged_lca",
		Snapshots: []string{"snap1", "snap2"},
	}

	if err := RunMerge(cfg); err != nil {
		t.Fatalf("RunMerge failed: %v", err)
	}

	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	defer s.Close()

	merged, err := s.FindSnapshot("merged_lca")
	if err != nil {
		t.Fatalf("failed to find merged snapshot: %v", err)
	}

	expectedRoot := "/home/user"
	if merged.RootPath != expectedRoot {
		t.Errorf("expected root path '%s', got '%s'", expectedRoot, merged.RootPath)
	}
}

// When two inputs disagree about what lives at a path, the last input listed
// wins. The merged snapshot cannot represent both, so the point of the test is
// that the rule is deterministic and follows the order given on the command line.
func TestRunMerge_ConflictingContentTakesLastInput(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	writeSnapshotWithHash(t, dbPath, "snapA", "/common/file", "aaaa")
	writeSnapshotWithHash(t, dbPath, "snapB", "/common/file", "bbbb")

	cfg := MergeConfig{
		DBPath:    dbPath,
		Name:      "merged_conflict",
		Snapshots: []string{"snapA", "snapB"},
	}
	if err := RunMerge(cfg); err != nil {
		t.Fatalf("RunMerge failed: %v", err)
	}

	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	defer s.Close()

	merged, err := s.FindSnapshot("merged_conflict")
	if err != nil {
		t.Fatalf("failed to find merged snapshot: %v", err)
	}

	var got []models.FileRecord
	if err := s.IterateFiles(merged.ID, func(f models.FileRecord) error {
		got = append(got, f)
		return nil
	}); err != nil {
		t.Fatalf("failed to iterate files: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 file, got %d", len(got))
	}
	if got[0].SHA1 != "bbbb" {
		t.Errorf("expected the last input's content (bbbb), got %q", got[0].SHA1)
	}
}

func writeSnapshotWithHash(t *testing.T, dbPath, name, path, sha1 string) {
	t.Helper()
	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	defer s.Close()

	snap, err := s.CreateSnapshot("/tmp", name, "host1")
	if err != nil {
		t.Fatalf("failed to create snapshot: %v", err)
	}
	if err := s.AddFile(&models.FileRecord{
		SnapshotID: snap.ID,
		Path:       path,
		Filename:   filepath.Base(path),
		SizeBytes:  100,
		ModTime:    time.Now(),
		SHA1:       sha1,
		MD5:        "",
	}); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	if err := s.CompleteSnapshot(snap.ID, time.Time{}); err != nil {
		t.Fatalf("failed to complete snapshot: %v", err)
	}
}
