package diff

import (
	"fmt"
	"runtime"
	"testing"

	"fluxion/internal/models"
)

// TestMemory_TwoIdenticalSnapshots measures retained heap for a synthetic
// 200,000-file tree diffed against itself, matching the exact scenario
// knowledge/diff-algo.md cites as the pre-digest baseline: "Two identical
// 200,000-file snapshots: 200 MiB retained heap." That number came from the
// old concatenated-merkle-string scheme (knowledge/diff-memory.md's Phase 0);
// this asserts a ceiling well above the ~40 MiB the fixed-width digest change
// should produce, so a regression back toward string concatenation fails a
// test instead of surfacing as a multi-GB swap storm on real fleet data.
func TestMemory_TwoIdenticalSnapshots(t *testing.T) {
	const n = 200_000
	files := make(map[string]models.FileRecord, n)
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("/dir%d/subdir%d/file%d.txt", i%50, i%500, i)
		files[path] = models.FileRecord{
			Path: path,
			SHA1: fmt.Sprintf("%040x", i), // distinct, well-formed hex, like a real SHA-1
		}
	}

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	results, err := CompareSnapshots(mapToIter(files), mapToIter(files), Options{
		RootA: "/", RootB: "/", HashType: "sha1",
	})
	if err != nil {
		t.Fatalf("CompareSnapshots: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no differences between identical snapshots, got %d", len(results))
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	retainedMiB := float64(after.HeapAlloc) / (1024 * 1024)
	t.Logf("retained heap after diffing %d identical files: %.1f MiB (pre-digest baseline was ~200 MiB)", n, retainedMiB)

	const ceilingMiB = 100.0 // well above the ~40 MiB expected, well below the 200 MiB pre-digest baseline
	if retainedMiB > ceilingMiB {
		t.Errorf("retained heap %.1f MiB exceeds %.1f MiB ceiling - the fixed-width digest change (diff-memory.md Phase 0) may have regressed", retainedMiB, ceilingMiB)
	}

	runtime.KeepAlive(results)
}
