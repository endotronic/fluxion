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

func createDummySnapshot(t *testing.T, dbPath, name string, files []string) *models.Snapshot {
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
	createDummySnapshot(t, dbPath, "snap1", []string{"/a/file1", "/a/file2"})
	createDummySnapshot(t, dbPath, "snap2", []string{"/b/file3"})
	
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

func TestRunMerge_Duplicates(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()
	
	// Overlapping files
	createDummySnapshot(t, dbPath, "snap1", []string{"/common/file"})
	createDummySnapshot(t, dbPath, "snap2", []string{"/common/file"})
	
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
	
	// Should be 2, because duplicates are preserved
	if count != 2 {
		t.Errorf("expected 2 files (duplicates preserved), got %d", count)
	}
}

func TestRunMerge_Hostname(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()
	
	createDummySnapshot(t, dbPath, "snap1", []string{"/a/1"})
	createDummySnapshot(t, dbPath, "snap2", []string{"/b/2"})
	
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
