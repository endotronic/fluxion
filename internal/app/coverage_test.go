package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"fluxion/internal/models"
	"fluxion/internal/store/sqlite"
)

// createHashedSnapshot builds a completed snapshot from path->sha1 pairs, so a
// test can control content identity independently of paths.
func createHashedSnapshot(t *testing.T, dbPath, name, root string, files map[string]string) *models.Snapshot {
	t.Helper()
	s, err := sqlite.NewSqliteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	defer s.Close()

	snap, err := s.CreateSnapshot(root, name, "host1")
	if err != nil {
		t.Fatalf("failed to create snapshot: %v", err)
	}

	batch := make([]*models.FileRecord, 0, len(files))
	for p, h := range files {
		batch = append(batch, &models.FileRecord{
			SnapshotID: snap.ID,
			Path:       p,
			Filename:   filepath.Base(p),
			SizeBytes:  1000,
			ModTime:    time.Now(),
			SHA1:       h,
		})
	}
	if err := s.BatchAddFiles(batch); err != nil {
		t.Fatalf("failed to add files: %v", err)
	}
	if err := s.CompleteSnapshot(snap.ID, time.Now()); err != nil {
		t.Fatalf("failed to complete snapshot: %v", err)
	}
	return snap
}

func runCoverageQuiet(t *testing.T, cfg CoverageConfig) CoverageResult {
	t.Helper()
	// RunCoverage reports to stdout; a test only wants the result.
	realStdout := os.Stdout
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devNull
	defer func() {
		os.Stdout = realStdout
		devNull.Close()
	}()

	res, err := RunCoverage(cfg)
	if err != nil {
		t.Fatalf("RunCoverage: %v", err)
	}
	return res
}

// The headline case: a "deprecated" copy whose every file exists on the keeper
// under entirely different paths is safe to delete, and the tool must say so.
func TestRunCoverage_ContentCoveredAtDifferentPaths(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	createHashedSnapshot(t, dbPath, "deprecated", "/artemis/deprecated", map[string]string{
		"/artemis/deprecated/old/photo1.jpg": "aaa",
		"/artemis/deprecated/old/photo2.jpg": "bbb",
	})
	createHashedSnapshot(t, dbPath, "luna", "/luna/kevin", map[string]string{
		"/luna/kevin/photos/2019/img_0001.jpg": "aaa",
		"/luna/kevin/photos/2020/img_0002.jpg": "bbb",
		"/luna/kevin/photos/2021/img_0003.jpg": "ccc",
	})

	res := runCoverageQuiet(t, CoverageConfig{
		DBPath:         dbPath,
		CandidateQuery: "deprecated",
		KeeperQueries:  []string{"luna"},
	})

	if !res.Covered() {
		t.Errorf("expected full coverage, got %d uncovered / %d unhashed", res.UncoveredFiles, res.NoHashFiles)
	}
	if res.TotalFiles != 2 {
		t.Errorf("expected 2 files checked, got %d", res.TotalFiles)
	}
}

func TestRunCoverage_ReportsWhatWouldBeLost(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	createHashedSnapshot(t, dbPath, "cand", "/cand", map[string]string{
		"/cand/kept.bin":   "aaa",
		"/cand/unique.bin": "zzz",
	})
	createHashedSnapshot(t, dbPath, "keeper", "/keeper", map[string]string{
		"/keeper/elsewhere.bin": "aaa",
	})

	res := runCoverageQuiet(t, CoverageConfig{
		DBPath:         dbPath,
		CandidateQuery: "cand",
		KeeperQueries:  []string{"keeper"},
	})

	if res.Covered() {
		t.Fatal("expected a gap to be reported")
	}
	if res.UncoveredFiles != 1 || res.UncoveredBytes != 1000 {
		t.Errorf("want 1 uncovered file / 1000 bytes, got %d / %d", res.UncoveredFiles, res.UncoveredBytes)
	}
}

// Coverage is a union across keepers: no single keeper holds everything, but
// together they do.
func TestRunCoverage_UnionOfKeepers(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	createHashedSnapshot(t, dbPath, "cand", "/cand", map[string]string{
		"/cand/a": "aaa",
		"/cand/b": "bbb",
	})
	createHashedSnapshot(t, dbPath, "k1", "/k1", map[string]string{"/k1/x": "aaa"})
	createHashedSnapshot(t, dbPath, "k2", "/k2", map[string]string{"/k2/y": "bbb"})

	if res := runCoverageQuiet(t, CoverageConfig{
		DBPath: dbPath, CandidateQuery: "cand", KeeperQueries: []string{"k1"},
	}); res.Covered() {
		t.Error("k1 alone should not cover the candidate")
	}

	if res := runCoverageQuiet(t, CoverageConfig{
		DBPath: dbPath, CandidateQuery: "cand", KeeperQueries: []string{"k1", "k2"},
	}); !res.Covered() {
		t.Error("k1 and k2 together should cover the candidate")
	}
}

func TestRunCoverage_RejectsSelfCheck(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	createHashedSnapshot(t, dbPath, "only", "/only", map[string]string{"/only/a": "aaa"})

	if _, err := RunCoverage(CoverageConfig{
		DBPath: dbPath, CandidateQuery: "only", KeeperQueries: []string{"only"},
	}); err == nil {
		t.Error("checking a snapshot against itself is always 'covered' and always wrong; want an error")
	}
}

func TestRunCoverage_MinSizeSkips(t *testing.T) {
	dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	createHashedSnapshot(t, dbPath, "cand", "/cand", map[string]string{"/cand/small": "zzz"})
	createHashedSnapshot(t, dbPath, "keeper", "/keeper", map[string]string{"/keeper/other": "aaa"})

	res := runCoverageQuiet(t, CoverageConfig{
		DBPath: dbPath, CandidateQuery: "cand", KeeperQueries: []string{"keeper"},
		MinSize: 2000,
	})
	if !res.Covered() {
		t.Error("the only file is below the floor, so nothing was checked and nothing is uncovered")
	}
	if res.TotalFiles != 0 {
		t.Errorf("want 0 files checked, got %d", res.TotalFiles)
	}
}
