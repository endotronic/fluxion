package sqlite

import (
	"fluxion/internal/models"
	"testing"
	"time"
)

func TestSqliteStore_Snapshots(t *testing.T) {
	// Use in-memory DB
	s, err := NewSqliteStore(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// 1. Create Snapshot
	rootPath := "/test/root"
	snap, err := s.CreateSnapshot(rootPath)
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}
	if snap.ID == 0 {
		t.Error("Snapshot ID should not be 0")
	}
	if snap.RootPath != rootPath {
		t.Errorf("Expected root path %s, got %s", rootPath, snap.RootPath)
	}
	if snap.Status != "in_progress" {
		t.Errorf("Expected status 'in_progress', got %s", snap.Status)
	}

	// 2. GetLastSnapshot
	last, err := s.GetLastSnapshot(rootPath)
	if err != nil {
		t.Fatalf("GetLastSnapshot failed: %v", err)
	}
	if last.ID != snap.ID {
		t.Errorf("GetLastSnapshot ID mismatch. Want %d, got %d", snap.ID, last.ID)
	}

	// 3. ListSnapshots
	list, err := s.ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots failed: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("Expected 1 snapshot, got %d", len(list))
	}

	// 4. Complete Snapshot
	err = s.CompleteSnapshot(snap.ID)
	if err != nil {
		t.Fatalf("CompleteSnapshot failed: %v", err)
	}
	
	updated, err := s.GetLastSnapshot(rootPath)
	if err != nil {
		t.Fatalf("GetLastSnapshot failed: %v", err)
	}
	if updated.Status != "completed" {
		t.Errorf("Expected status 'completed', got %s", updated.Status)
	}
	if updated.FinishedAt == nil {
		t.Error("Expected FinishedAt to be set")
	}
}

func TestSqliteStore_Files(t *testing.T) {
	s, err := NewSqliteStore(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	snap, err := s.CreateSnapshot("/test/root")
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}

	files := []*models.FileRecord{
		{SnapshotID: snap.ID, Path: "/test/root/file1", SHA1: "hash1", SizeBytes: 100, ModTime: time.Now()},
		{SnapshotID: snap.ID, Path: "/test/root/file2", SHA1: "hash2", SizeBytes: 200, ModTime: time.Now()},
	}

	// 1. Batch Add
	err = s.BatchAddFiles(files)
	if err != nil {
		t.Fatalf("BatchAddFiles failed: %v", err)
	}

	// 2. GetFileCount
	count, err := s.GetFileCount(snap.ID)
	if err != nil {
		t.Fatalf("GetFileCount failed: %v", err)
	}
	if count != 2 {
		t.Errorf("Expected 2 files, got %d", count)
	}

	// 3. GetFilesForSnapshot
	retrieved, err := s.GetFilesForSnapshot(snap.ID, nil)
	if err != nil {
		t.Fatalf("GetFilesForSnapshot failed: %v", err)
	}
	if len(retrieved) != 2 {
		t.Errorf("Expected 2 files in map, got %d", len(retrieved))
	}
	if r, ok := retrieved["/test/root/file1"]; !ok || r.SHA1 != "hash1" {
		t.Error("Failed to retrieve file1 correctly")
	}
}
