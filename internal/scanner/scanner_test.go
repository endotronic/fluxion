package scanner

import (
	"crypto/sha1"
	"encoding/hex"
	"file-hasher/internal/models"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestRunScan(t *testing.T) {
	// Setup Temp Dir
	tmpDir, err := ioutil.TempDir("", "scanner_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test structure
	// root/file1.txt (content: "hello")
	// root/sub/file2.txt (content: "world")
	
	file1Path := filepath.Join(tmpDir, "file1.txt")
	err = ioutil.WriteFile(file1Path, []byte("hello"), 0644)
	if err != nil {
		t.Fatalf("Failed to write file1: %v", err)
	}

	subDir := filepath.Join(tmpDir, "sub")
	err = os.Mkdir(subDir, 0755)
	if err != nil {
		t.Fatalf("Failed to create subdir: %v", err)
	}

	file2Path := filepath.Join(subDir, "file2.txt")
	err = ioutil.WriteFile(file2Path, []byte("world"), 0644)
	if err != nil {
		t.Fatalf("Failed to write file2: %v", err)
	}

	// Expected Hashes
	hash1 := sha1Sum("hello")
	hash2 := sha1Sum("world")

	// Config
	resultsFn := make(chan ScanResult)
	cfg := ScannerConfig{
		RootPath:   tmpDir,
		SnapshotID: 1,
		NumWorkers: 2,
		OnFileFound: func(path string, size int64) {},
	}

	// Run Scan
	go RunScan(cfg, resultsFn)

	// Collect Results
	var files []*models.FileRecord
	for res := range resultsFn {
		if res.Error != nil {
			t.Errorf("Scan error: %v", res.Error)
			continue
		}
		files = append(files, res.File)
	}

	if len(files) != 2 {
		t.Errorf("Expected 2 files, got %d", len(files))
	}

	// Sort for deterministic validation
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})

	// Verify File 1 (file1.txt comes before sub/file2.txt alphabetically?)
	// path/file1.txt vs path/sub/file2.txt
	// file1.txt < sub/...
	
	// Note: Readdir order isn't guaranteed, but we sorted 'files'.
	// Verify content
	found1 := false
	found2 := false

	for _, f := range files {
		if f.Path == file1Path {
			found1 = true
			if f.SHA1 != hash1 {
				t.Errorf("File1 hash mismatch. Want %s, got %s", hash1, f.SHA1)
			}
		} else if f.Path == file2Path {
			found2 = true
			if f.SHA1 != hash2 {
				t.Errorf("File2 hash mismatch. Want %s, got %s", hash2, f.SHA1)
			}
		} else {
			t.Errorf("Unexpected file path: %s", f.Path)
		}
	}

	if !found1 {
		t.Error("file1.txt not found")
	}
	if !found2 {
		t.Error("file2.txt not found")
	}
}

func sha1Sum(content string) string {
	h := sha1.New()
	h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}
