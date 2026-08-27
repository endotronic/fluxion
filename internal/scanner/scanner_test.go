package scanner

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fluxion/internal/models"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
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

	// Create a Symlink (should be skipped)
	symlinkPath := filepath.Join(tmpDir, "symlink.txt")
	err = os.Symlink(file1Path, symlinkPath)
	if err != nil {
		t.Fatalf("Failed to create symlink: %v", err)
	}

	// Expected Hashes
	hash1 := sha1Sum("hello")
	hash2 := sha1Sum("world")

	// Config
	resultsFn := make(chan ScanResult)
	cfg := ScannerConfig{
		RootPath:    tmpDir,
		SnapshotID:  1,
		NumWorkers:  2,
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
		} else if f.Path == symlinkPath {
			t.Errorf("Scanner included symlink: %s", f.Path)
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

func md5Sum(content string) string {
	h := md5.New()
	h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

func TestRunScan_MD5(t *testing.T) {
	// Setup Temp Dir
	tmpDir, err := ioutil.TempDir("", "scanner_md5_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	file1Path := filepath.Join(tmpDir, "file1.txt")
	err = ioutil.WriteFile(file1Path, []byte("hello"), 0644)
	if err != nil {
		t.Fatalf("Failed to write file1: %v", err)
	}

	hash1 := sha1Sum("hello")
	md51 := md5Sum("hello")

	// Config
	resultsFn := make(chan ScanResult)
	cfg := ScannerConfig{
		RootPath:   tmpDir,
		SnapshotID: 2,
		NumWorkers: 1,
		ComputeMD5: true,
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

	if len(files) != 1 {
		t.Errorf("Expected 1 file, got %d", len(files))
	} else {
		f := files[0]
		if f.SHA1 != hash1 {
			t.Errorf("SHA1 mismatch. Want %s, got %s", hash1, f.SHA1)
		}
		if f.MD5 != md51 {
			t.Errorf("MD5 mismatch. Want %s, got %s", md51, f.MD5)
		}
	}
}
func TestRunScan_Resume(t *testing.T) {
	// Setup Temp Dir
	tmpDir, err := ioutil.TempDir("", "scanner_resume_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	file1Path := filepath.Join(tmpDir, "file1.txt")
	err = ioutil.WriteFile(file1Path, []byte("hello"), 0644)
	if err != nil {
		t.Fatalf("Failed to write file1: %v", err)
	}

	// Create Resume Map with file1 already processed
	resumeMap := make(map[string]models.FileRecord)
	resumeMap[file1Path] = models.FileRecord{
		Path:      file1Path,
		SHA1:      "precomputed_sha1",
		SizeBytes: 5,
	}

	// Config
	resultsFn := make(chan ScanResult)
	cfg := ScannerConfig{
		RootPath:   tmpDir,
		SnapshotID: 3,
		NumWorkers: 1,
		ResumeMap:  resumeMap,
	}

	// Run Scan
	go RunScan(cfg, resultsFn)

	// Collect Results
	var files []*models.FileRecord
	var fromResumeCount int
	for res := range resultsFn {
		if res.Error != nil {
			t.Errorf("Scan error: %v", res.Error)
			continue
		}
		if res.FromResume {
			fromResumeCount++
		}
		files = append(files, res.File)
	}

	if len(files) != 1 {
		t.Errorf("Expected 1 file, got %d", len(files))
	} else {
		f := files[0]
		// Should match resume map data, NOT re-hashed data (though checking hash equality is tricky if logic differs)
		// But FromResume should be true
		if f.SHA1 != "precomputed_sha1" {
			t.Errorf("SHA1 mismatch. Want from resume 'precomputed_sha1', got %s", f.SHA1)
		}
	}

	if fromResumeCount != 1 {
		t.Errorf("Expected 1 result from resume, got %d", fromResumeCount)
	}
}

// TestRunScan_StopChStopsPromptly proves closing StopCh actually unblocks a
// walker that's stuck mid-send into a full `paths` buffer, rather than
// deadlocking, AND that a worker stops after at most the one file it already
// had in hand rather than continuing to drain whatever the walker had queued
// up ahead of it. The latter used to be probabilistic, not guaranteed: a
// `select` with both `paths` (non-empty) and StopCh (closed) ready picks
// between them uniformly at random each iteration, so a worker only gave up
// after happening to win that coin flip, which - across a large queued
// backlog - could let real, visible scanning work continue for a while after
// StopCh closed instead of stopping essentially immediately. This is the
// scenario that mattered for zfs-scan's SIGINT handling: closing StopCh has
// to make an in-flight scan stand down within a bounded time so a caller
// (zfs-scan's unmount retry) can rely on the mount actually becoming idle,
// not race a scan that never stops.
func TestRunScan_StopChStopsPromptly(t *testing.T) {
	tmpDir := t.TempDir()

	// NumWorkers(1) * consts.ScannerChannelBufferMultiplier(1000) = a 1000
	// entry buffer. More files than that forces the walker to block on
	// `paths <- path` well before it finishes, which is the only way to
	// actually exercise the select-based deadlock avoidance around that
	// send (a buffer that never fills wouldn't touch that code path at all).
	// It also means the buffer is holding hundreds of already-queued paths
	// by the time StopCh closes below - the scenario the random-choice bug
	// needed to show up in.
	const numFiles = 1500
	for i := 0; i < numFiles; i++ {
		p := filepath.Join(tmpDir, fmt.Sprintf("f%d", i))
		if err := os.WriteFile(p, nil, 0644); err != nil {
			t.Fatalf("write file %d: %v", i, err)
		}
	}

	stopCh := make(chan struct{})
	results := make(chan ScanResult)
	cfg := ScannerConfig{
		RootPath:   tmpDir,
		SnapshotID: 4,
		NumWorkers: 1,
		StopCh:     stopCh,
	}

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		RunScan(cfg, results)
	}()

	// Read a few results - by the time these arrive, the walker has almost
	// certainly already filled the 1000-entry buffer and is blocked trying
	// to send the next one, since the walker enqueues far faster than one
	// worker can hash-and-report.
	const readBeforeStop = 5
	got := 0
	for i := 0; i < readBeforeStop; i++ {
		if _, ok := <-results; !ok {
			t.Fatalf("results closed after only %d items, before StopCh was even closed", i)
		}
		got++
	}
	close(stopCh)

	// Drain whatever's left in flight so RunScan's own send(s) don't block on
	// a test that stopped reading, while counting it - with the deterministic
	// priority check, the single worker here can process at most one more
	// file after StopCh closes (whichever it already won the race for right
	// as the close happened), not a random slice of the ~1000-deep backlog.
	for range results {
		got++
	}

	const maxAfterStop = 3 // readBeforeStop + generous slack for the one in-flight file
	if got > readBeforeStop+maxAfterStop {
		t.Errorf("processed %d files total (%d read before close) - StopCh should stop the worker after at most one more in-flight file, not let it keep draining the queued backlog", got, readBeforeStop)
	}

	select {
	case <-scanDone:
	case <-time.After(5 * time.Second):
		t.Fatal("RunScan did not return within 5s of StopCh closing - likely deadlocked on a blocked paths<- send")
	}
}

// TestRunScan_StopChAlreadyClosed proves a StopCh that's closed before
// RunScan is even called stops the walk essentially immediately, rather than
// scanning at least a first batch anyway.
func TestRunScan_StopChAlreadyClosed(t *testing.T) {
	tmpDir := t.TempDir()
	for i := 0; i < 20; i++ {
		p := filepath.Join(tmpDir, fmt.Sprintf("f%d", i))
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("write file %d: %v", i, err)
		}
	}

	stopCh := make(chan struct{})
	close(stopCh)

	results := make(chan ScanResult)
	cfg := ScannerConfig{
		RootPath:   tmpDir,
		SnapshotID: 5,
		NumWorkers: 2,
		StopCh:     stopCh,
	}

	scanDone := make(chan struct{})
	var got int
	go func() {
		defer close(scanDone)
		for range results {
			got++
		}
	}()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		RunScan(cfg, results)
	}()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("RunScan did not return within 5s with an already-closed StopCh")
	}
	<-scanDone

	if got != 0 {
		t.Errorf("expected 0 files processed with StopCh already closed, got %d", got)
	}
}
