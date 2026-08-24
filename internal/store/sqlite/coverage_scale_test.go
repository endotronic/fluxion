package sqlite

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"fluxion/internal/models"
)

// TestCoverageScale is the test that justifies `coverage` existing.
//
// The diff engine holds a unified tree at roughly 1 KiB per node, which is what
// made the author need 200 GB of swap on real snapshots
// (knowledge/diff-memory.md). Coverage answers the delete question without a
// tree at all, so its memory must be flat in snapshot size - and "must be flat"
// is only a claim until something measures it.
//
// Skipped by default because building the database takes minutes. Run with:
//
//	FLUXION_SCALE_FILES=2000000 go test ./internal/store/sqlite/ -run Scale -v -timeout 60m
//
// Measured 2026-08-23 at that size (2M rows per side, 4M total, 200k uncovered,
// 956 MiB database): scan took 21.9s and peak heap during the scan was 1.3 MiB
// against a 1.0 MiB baseline. Flat, not merely bounded - nothing accumulates per
// row. The equivalent diff would want roughly 4 GiB.
func TestCoverageScale(t *testing.T) {
	nStr := os.Getenv("FLUXION_SCALE_FILES")
	if nStr == "" {
		t.Skip("set FLUXION_SCALE_FILES to run the scale test (e.g. 2000000)")
	}
	n, err := strconv.Atoi(nStr)
	if err != nil {
		t.Fatalf("FLUXION_SCALE_FILES: %v", err)
	}

	// The ceiling is what the test actually asserts. It is generous compared to
	// what a tree-based diff of the same input would need (~2n KiB), and any
	// per-row retention would blow through it long before the run ends.
	const heapCeiling = 256 << 20

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "scale.db")
	s, err := NewSqliteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Every tenth file in the candidate is unique to it; the rest also exist in
	// the keeper, at unrelated paths.
	const uncoveredEvery = 10
	candID := fillSnapshot(t, s, "candidate", n, func(i int) (string, string) {
		if i%uncoveredEvery == 0 {
			return fmt.Sprintf("/cand/%03d/%05d/only_here_%d.bin", i%997, i%89, i), fmt.Sprintf("%040x", i*2+1)
		}
		return fmt.Sprintf("/cand/%03d/%05d/shared_%d.bin", i%997, i%89, i), fmt.Sprintf("%040x", i*2)
	})
	keepID := fillSnapshot(t, s, "keeper", n, func(i int) (string, string) {
		return fmt.Sprintf("/keep/%04d/elsewhere_%d.bin", i%1231, i), fmt.Sprintf("%040x", i*2)
	})

	plan, err := s.ExplainUncovered(candID, []int64{keepID}, "sha1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("query plan:\n%s", plan)

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	var rows int64
	var peakHeap uint64
	start := time.Now()
	err = s.IterateUncovered(candID, []int64{keepID}, "sha1", 0, func(f models.FileRecord) error {
		rows++
		if rows%50000 == 0 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peakHeap {
				peakHeap = m.HeapInuse
			}
		}
		return nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}

	wantRows := int64((n + uncoveredEvery - 1) / uncoveredEvery)
	if rows != wantRows {
		t.Errorf("want %d uncovered rows, got %d", wantRows, rows)
	}

	fi, _ := os.Stat(dbPath)
	t.Logf("n=%d per side, db=%.1f MiB, uncovered=%d, elapsed=%s, peak heap during scan=%.1f MiB (baseline %.1f MiB)",
		n, float64(fi.Size())/(1<<20), rows, elapsed.Round(time.Millisecond),
		float64(peakHeap)/(1<<20), float64(base.HeapInuse)/(1<<20))

	if peakHeap > heapCeiling {
		t.Errorf("peak heap %.1f MiB exceeds the %d MiB ceiling: something is retaining per-row state",
			float64(peakHeap)/(1<<20), heapCeiling>>20)
	}
}

func fillSnapshot(t *testing.T, s *SqliteStore, name string, n int, gen func(i int) (path, sha1 string)) int64 {
	t.Helper()
	snap, err := s.CreateSnapshot("/"+name, name, "scale")
	if err != nil {
		t.Fatal(err)
	}

	const batchSize = 20000
	now := time.Now()
	batch := make([]*models.FileRecord, 0, batchSize)
	for i := 0; i < n; i++ {
		p, h := gen(i)
		batch = append(batch, &models.FileRecord{
			SnapshotID: snap.ID,
			Path:       p,
			Filename:   filepath.Base(p),
			SizeBytes:  int64(i%100000 + 1),
			ModTime:    now,
			SHA1:       h,
		})
		if len(batch) == batchSize {
			if err := s.BatchAddFiles(batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := s.BatchAddFiles(batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CompleteSnapshot(snap.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	return snap.ID
}
