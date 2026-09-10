package diff

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"fluxion/internal/models"
)

// dfsIter yields records in DFS-key order, which is what streamCompare requires
// and what a store-side iterator would have to produce for it.
func dfsIter(m map[string]models.FileRecord) FileIterator {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return dfsKey(keys[i]) < dfsKey(keys[j]) })

	return func(yield func(string, models.FileRecord) error) error {
		for _, k := range keys {
			if err := yield(k, m[k]); err != nil {
				return err
			}
		}
		return nil
	}
}

func streamOpts(maxLines int) Options {
	return Options{
		RootA: "/", RootB: "/", HashType: "sha1",
		NoMoves: true, NoCopies: true,
		MaxLinesPerDir: maxLines,
	}
}

// runStreaming and runTree produce the two answers that must agree.
func runStreaming(t *testing.T, a, b map[string]models.FileRecord, maxLines int) []DiffResult {
	t.Helper()
	got, err := streamCompare(dfsIter(a), dfsIter(b), streamOpts(maxLines))
	if err != nil {
		t.Fatalf("streamCompare: %v", err)
	}
	return finalizeResults(got, "/", "/")
}

func runTree(t *testing.T, a, b map[string]models.FileRecord, maxLines int) []DiffResult {
	t.Helper()
	got, err := compareSnapshotsWith(mergeJoinInsert, mapToIter(a), mapToIter(b), streamOpts(maxLines))
	if err != nil {
		t.Fatalf("compareSnapshotsWith: %v", err)
	}
	return got
}

func requireStreamMatches(t *testing.T, name string, a, b map[string]models.FileRecord, maxLines int) {
	t.Helper()
	got := runStreaming(t, a, b, maxLines)
	want := runTree(t, a, b, maxLines)
	if reflect.DeepEqual(got, want) {
		return
	}
	t.Fatalf("%s: streaming engine disagreed with the tree engine\n"+
		"A: %v\nB: %v\n got (%d): %s\nwant (%d): %s",
		name, sortedKeys(a), sortedKeys(b), len(got), formatResults(got), len(want), formatResults(want))
}

func sortedKeys(m map[string]models.FileRecord) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func TestStreaming_MatchesTreeEngine(t *testing.T) {
	for seed := int64(0); seed < randomTreeSeeds; seed++ {
		a, b := generateTreePair(rand.New(rand.NewSource(seed)))
		requireStreamMatches(t, fmt.Sprintf("seed %d", seed), a, b, 0)
	}
}

// The line budget is the only mechanism that removes output on purpose, so it is
// where a streaming engine is most likely to lose something the tree engine kept.
func TestStreaming_MatchesTreeEngine_Budgeted(t *testing.T) {
	for _, budget := range []int{1, 3} {
		for seed := int64(0); seed < randomTreeSeeds; seed++ {
			a, b := generateTreePair(rand.New(rand.NewSource(seed)))
			requireStreamMatches(t, fmt.Sprintf("seed %d (budget %d)", seed, budget), a, b, budget)
		}
	}
}

func TestStreaming_MatchesTreeEngine_HandBuilt(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]models.FileRecord
	}{
		{
			name: "file becomes a directory",
			a:    map[string]models.FileRecord{"/data": rec("/data", "h1", 10)},
			b: map[string]models.FileRecord{
				"/data/one.txt": rec("/data/one.txt", "h2", 10),
				"/data/two.txt": rec("/data/two.txt", "h3", 10),
			},
		},
		{
			name: "directory becomes a file",
			a: map[string]models.FileRecord{
				"/data/one.txt": rec("/data/one.txt", "h2", 10),
			},
			b: map[string]models.FileRecord{"/data": rec("/data", "h1", 10)},
		},
		{
			// The ordering hazard: '.' sorts below '/', so plain path order gives
			// a, a.txt, a/x - the DFS key is what keeps a and a/x adjacent.
			name: "sibling sorts between a file and its directory form",
			a: map[string]models.FileRecord{
				"/a":     rec("/a", "h1", 1),
				"/a.txt": rec("/a.txt", "h2", 1),
			},
			b: map[string]models.FileRecord{
				"/a/x":   rec("/a/x", "h3", 1),
				"/a.txt": rec("/a.txt", "h2", 1),
			},
		},
		{
			name: "everything removed",
			a: map[string]models.FileRecord{
				"/d/one": rec("/d/one", "h1", 1),
				"/d/two": rec("/d/two", "h2", 1),
			},
			b: map[string]models.FileRecord{},
		},
		{
			name: "everything added",
			a:    map[string]models.FileRecord{},
			b: map[string]models.FileRecord{
				"/d/one": rec("/d/one", "h1", 1),
				"/d/two": rec("/d/two", "h2", 1),
			},
		},
		{
			name: "one modified among unchanged siblings",
			a: map[string]models.FileRecord{
				"/d/one": rec("/d/one", "h1", 1),
				"/d/two": rec("/d/two", "h2", 1),
			},
			b: map[string]models.FileRecord{
				"/d/one": rec("/d/one", "h1", 1),
				"/d/two": rec("/d/two", "CHANGED", 1),
			},
		},
		{
			name: "deep nesting, single change at the bottom",
			a: map[string]models.FileRecord{
				"/a/b/c/d/e": rec("/a/b/c/d/e", "h1", 1),
				"/a/b/other": rec("/a/b/other", "h2", 1),
			},
			b: map[string]models.FileRecord{
				"/a/b/c/d/e": rec("/a/b/c/d/e", "CHANGED", 1),
				"/a/b/other": rec("/a/b/other", "h2", 1),
			},
		},
		{
			name: "empty on both sides",
			a:    map[string]models.FileRecord{},
			b:    map[string]models.FileRecord{},
		},
		{
			name: "missing hash on one side is Modified, never Unchanged",
			a:    map[string]models.FileRecord{"/f": {Path: "/f", SizeBytes: 1}},
			b:    map[string]models.FileRecord{"/f": rec("/f", "h1", 1)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, budget := range []int{0, 1} {
				requireStreamMatches(t, fmt.Sprintf("%s (budget %d)", tc.name, budget), tc.a, tc.b, budget)
			}
		})
	}
}

func TestStreaming_RefusesMoveDetection(t *testing.T) {
	a := map[string]models.FileRecord{"/x": rec("/x", "h1", 1)}
	for _, opts := range []Options{
		{HashType: "sha1", NoMoves: true},  // copies still on
		{HashType: "sha1", NoCopies: true}, // moves still on
		{HashType: "sha1"},                 // both on
	} {
		if _, err := streamCompare(mapToIter(a), mapToIter(a), opts); !errors.Is(err, errStreamUnsupported) {
			t.Errorf("opts %+v: err = %v, want errStreamUnsupported", opts, err)
		}
	}
}

// Out-of-order input cannot be diffed correctly by a stack walk, and guessing
// would mis-nest nodes - a wrong answer, not a cosmetic one, under goals.md's
// severity rule. It must refuse, so the caller can fall back to the tree engine.
func TestStreaming_RefusesOutOfOrderInput(t *testing.T) {
	backwards := FileIterator(func(yield func(string, models.FileRecord) error) error {
		if err := yield("/b", rec("/b", "h1", 1)); err != nil {
			return err
		}
		return yield("/a", rec("/a", "h2", 1))
	})
	empty := mapToIter(map[string]models.FileRecord{})

	if _, err := streamCompare(backwards, empty, streamOpts(0)); !errors.Is(err, errStreamOutOfOrder) {
		t.Errorf("err = %v, want errStreamOutOfOrder", err)
	}
}

// The whole point of Phase 2: what the engine retains must be bounded by tree
// depth and the line budget, not by the number of files. Asserted structurally -
// the engine records its own high-water marks - because sampling the heap for
// this is both noisy and, at any useful sample rate, far slower than the work
// being measured.
func TestStreaming_RetentionIsBoundedByDepthAndBudget(t *testing.T) {
	build := func(n int, tag string) map[string]models.FileRecord {
		m := make(map[string]models.FileRecord, n)
		for i := 0; i < n; i++ {
			// Depth 3 under the root: /dirN/subN/fileN.txt
			p := fmt.Sprintf("/dir%d/sub%d/file%d.txt", i%50, i%500, i)
			m[p] = models.FileRecord{Path: p, SHA1: fmt.Sprintf("%s%040x", tag, i)}
		}
		return m
	}

	const budget = DefaultMaxLinesPerDir
	var prev struct{ frames, lines int }

	for _, n := range []int{20_000, 200_000} {
		a, b := build(n, "a"), build(n, "b") // every file differs: nothing collapses to Unchanged

		e := &streamEngine{opts: streamOpts(budget)}
		root := &streamFrame{name: "", path: "", acc: newRollupAccum()}
		e.stack = []*streamFrame{root}
		err := mergeJoinStreamsBy(dfsIter(a), dfsIter(b), dfsKey,
			func(path string, ra, rb *models.FileRecord) error { return e.add(path, ra, rb) })
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		for len(e.stack) > 1 {
			e.closeTop()
		}

		t.Logf("%7d files/side: peak frames %d, peak retained lines %d", n, e.peakFrames, e.peakLines)

		// /root + dir + sub = 3 frames; the leaf is a record, not a frame.
		if e.peakFrames > 4 {
			t.Errorf("n=%d: peak frames %d exceeds the tree's depth", n, e.peakFrames)
		}
		// Each open frame retains at most budget+1 lines (the +1 being the
		// truncation summary), across at most depth frames.
		if maxLines := 4 * (budget + 1); e.peakLines > maxLines {
			t.Errorf("n=%d: peak retained lines %d exceeds depth x (budget+1) = %d",
				n, e.peakLines, maxLines)
		}

		if prev.frames != 0 {
			// The decisive check: 10x the files must not move these at all.
			if e.peakFrames != prev.frames || e.peakLines != prev.lines {
				t.Errorf("retention changed with input size: %d files gave (frames %d, lines %d), "+
					"previously (frames %d, lines %d) - the stream is holding per-file state",
					n, e.peakFrames, e.peakLines, prev.frames, prev.lines)
			}
		}
		prev.frames, prev.lines = e.peakFrames, e.peakLines
	}
}
