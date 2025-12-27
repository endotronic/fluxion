package sqlite

import (
	"fluxion/internal/models"
	"fmt"
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
	snapName := "test-snapshot-1"
	snap, err := s.CreateSnapshot(rootPath, snapName, "test-host")
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}
	if snap.ID == 0 {
		t.Error("Snapshot ID should not be 0")
	}
	if snap.RootPath != rootPath {
		t.Errorf("Expected root path %s, got %s", rootPath, snap.RootPath)
	}
	if snap.Name != snapName {
		t.Errorf("Expected name %s, got %s", snapName, snap.Name)
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
	
	// 3. FindSnapshot
	// By ID
	found, err := s.FindSnapshot(fmt.Sprintf("%d", snap.ID))
	if err != nil {
		t.Fatalf("FindSnapshot(ID) failed: %v", err)
	}
	if found.ID != snap.ID {
		t.Errorf("Found wrong snapshot by ID")
	}
	
	// By Name
	foundByName, err := s.FindSnapshot(snapName)
	if err != nil {
		t.Fatalf("FindSnapshot(Name) failed: %v", err)
	}
	if foundByName.ID != snap.ID {
		t.Errorf("Found wrong snapshot by Name")
	}

	// 4. ListSnapshots
	list, err := s.ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots failed: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("Expected 1 snapshot, got %d", len(list))
	}
	if list[0].Name != snapName {
		t.Errorf("List snapshot name mismatch")
	}

	// 5. Complete Snapshot (Implicit Time)
	err = s.CompleteSnapshot(snap.ID, time.Time{})
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

	// 6. Complete Snapshot (Explicit Time)
	// Create another snapshot
	snap2, _ := s.CreateSnapshot(rootPath, "test-snap-2", "host")
	fixedTime := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	err = s.CompleteSnapshot(snap2.ID, fixedTime)
	if err != nil {
		t.Fatalf("CompleteSnapshot(Explicit) failed: %v", err)
	}

	updated2, _ := s.FindSnapshot(fmt.Sprintf("%d", snap2.ID))
	if updated2.FinishedAt == nil || !updated2.FinishedAt.Equal(fixedTime) {
		t.Errorf("Expected FinishedAt to be %v, got %v", fixedTime, updated2.FinishedAt)
	}
}

func TestSqliteStore_Files(t *testing.T) {
	s, err := NewSqliteStore(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	snap, err := s.CreateSnapshot("/tmp/test", "test_snap", "test-host")
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}

	files := []*models.FileRecord{
		{SnapshotID: snap.ID, Path: "/test/root/file1", SHA1: "hash1", MD5: "md5_1", SizeBytes: 100, ModTime: time.Now()},
		{SnapshotID: snap.ID, Path: "/test/root/file2", SHA1: "hash2", MD5: "md5_2", SizeBytes: 200, ModTime: time.Now()},
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
	if r, ok := retrieved["/test/root/file1"]; !ok || r.MD5 != "md5_1" {
		t.Error("Failed to retrieve MD5 for file1")
	}
}

func TestSqliteStore_ListSnapshots_Sorting(t *testing.T) {
	s, err := NewSqliteStore(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer s.Close()

	// Helper to create valid time
	mkTime := func(h int) time.Time {
		return time.Date(2023, 1, 1, h, 0, 0, 0, time.UTC)
	}

	// 1. Create Snapshots
	// ID 1
	s1, _ := s.CreateSnapshot("/", "snap1", "host") 
	// ID 2
	s2, _ := s.CreateSnapshot("/", "snap2", "host") 
	// ID 3
	s3, _ := s.CreateSnapshot("/", "snap3", "host") 
	// ID 4
	s4, _ := s.CreateSnapshot("/", "snap4", "host") 

	// 2. Complete a subset
	// S1: Completed at 10:00 (Oldest result)
	s.CompleteSnapshot(s1.ID, mkTime(10)) 

	// S2: Completed at 12:00 (Newest result)
	s.CompleteSnapshot(s2.ID, mkTime(12))

	// S3: In Progress (NULL)
	// S4: In Progress (NULL)

	// 3. List
	snaps, err := s.ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots failed: %v", err)
	}

	if len(snaps) != 4 {
		t.Fatalf("Expected 4 snapshots, got %d", len(snaps))
	}

	// Expected Order:
	// 1. S2 (Finished 12:00)
	// 2. S1 (Finished 10:00)
	// 3. S4 (NULL Finished, ID 4 > ID 3)
	// 4. S3 (NULL Finished, ID 3)

	order := []int64{snaps[0].ID, snaps[1].ID, snaps[2].ID, snaps[3].ID}
	expected := []int64{s2.ID, s1.ID, s4.ID, s3.ID}

	for i := range order {
		if order[i] != expected[i] {
			t.Errorf("Order mismatch at index %d. Want ID %d, got %d. Full list: %v", i, expected[i], order[i], order)
		}
	}
}
