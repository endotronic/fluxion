package diff

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"fluxion/internal/models"
)

// The Phase 3 claim is that move/copy detection stops needing a global index:
// what the engine retains must still be bounded by tree depth and the line
// budget, not by the number of files. Asserted the same way Phase 2's is - the
// passes record their own high-water marks - because sampling the heap for this
// is noisy and, at any useful rate, slower than the work being measured.
func TestExternal_RetentionIsBoundedWithMovesOn(t *testing.T) {
	// Every file moves from one directory tree to another, so the matcher has
	// the maximum amount of work to do and every move source is a candidate the
	// fixed point has to consider.
	build := func(n int, dir string) map[string]models.FileRecord {
		m := make(map[string]models.FileRecord, n)
		for i := 0; i < n; i++ {
			p := fmt.Sprintf("/%s/sub%d/file%d.txt", dir, i%500, i)
			m[p] = models.FileRecord{Path: p, SizeBytes: 10, SHA1: fmt.Sprintf("%040x", i)}
		}
		return m
	}

	const budget = DefaultMaxLinesPerDir
	var prev struct{ frames, lines, passes int }

	for _, n := range []int{20_000, 200_000} {
		a, b := build(n, "old"), build(n, "new")

		x := &externalDiff{opts: streamOpts(budget)}
		if _, err := x.compare(dfsIter(a), dfsIter(b)); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		t.Logf("%7d files/side: %d passes, peak frames %d, peak retained lines %d",
			n, x.passes, x.peakFrames, x.peakLines)

		// /root + old|new + sub = 3 frames; the leaf is a record, not a frame.
		if x.peakFrames > 4 {
			t.Errorf("n=%d: peak frames %d exceeds the tree's depth", n, x.peakFrames)
		}
		if maxLines := 4 * (budget + 1); x.peakLines > maxLines {
			t.Errorf("n=%d: peak retained lines %d exceeds depth x (budget+1) = %d",
				n, x.peakLines, maxLines)
		}

		if prev.frames != 0 {
			// The decisive check: 10x the files must not move any of these.
			if x.peakFrames != prev.frames || x.peakLines != prev.lines || x.passes != prev.passes {
				t.Errorf("cost changed with input size: %d files gave (frames %d, lines %d, passes %d), "+
					"previously (frames %d, lines %d, passes %d) - something scales with the file count",
					n, x.peakFrames, x.peakLines, x.passes, prev.frames, prev.lines, prev.passes)
			}
		}
		prev.frames, prev.lines, prev.passes = x.peakFrames, x.peakLines, x.passes
	}
}

// Everything above runs entirely in memory, because the intermediates for a
// test-sized diff never reach the spill limit - which means the disk-backed
// halves of spillFile, recReader and the sorter's run merge would otherwise
// never be executed by any test. Shrinking the limits to a few kilobytes puts
// the same corpus through them and asserts the same equivalence.
func TestExternal_SpilledIntermediatesGiveTheSameAnswer(t *testing.T) {
	defer swapLimits(2<<10, 1<<10)()

	for seed := int64(0); seed < 400; seed++ {
		a, b := generateTreePair(rand.New(rand.NewSource(seed)))
		// Random pairs are small; pad them out so the intermediates outgrow the
		// limits above rather than only brushing them.
		for i := 0; i < 200; i++ {
			p := fmt.Sprintf("/pad%d/f%d", i%7, i)
			a[p] = rec(p, fmt.Sprintf("pad%d", i), 10)
			q := fmt.Sprintf("/moved%d/f%d", i%7, i)
			b[q] = rec(q, fmt.Sprintf("pad%d", i), 10)
		}

		for _, budget := range []int{0, 3} {
			opts := streamOpts(budget)
			opts.TempDir = t.TempDir()

			got, err := streamCompare(dfsIter(a), dfsIter(b), opts)
			if err != nil {
				t.Fatalf("seed %d: streamCompare: %v", seed, err)
			}
			want, err := compareSnapshotsWith(mergeJoinInsert, mapToIter(a), mapToIter(b), opts)
			if err != nil {
				t.Fatalf("seed %d: tree: %v", seed, err)
			}
			if !reflect.DeepEqual(finalizeResults(got, "/", "/"), want) {
				t.Fatalf("seed %d (budget %d): spilled intermediates changed the answer\n got: %s\nwant: %s",
					seed, budget, formatResults(finalizeResults(got, "/", "/")), formatResults(want))
			}
		}
	}
}

// The pathology the plan calls out: one digest can cover an enormous group,
// because every directory holding nothing but zero-byte files hashes the same.
// The tree engine survives it by having already lost - it holds every member in
// a map. The group scan must not.
func TestExternal_HugeContentGroup(t *testing.T) {
	a := make(map[string]models.FileRecord)
	b := make(map[string]models.FileRecord)
	for i := 0; i < 4000; i++ {
		// Non-zero size, one shared hash: indistinguishable content at 4,000
		// distinct paths, so the group holds every one of them.
		p := fmt.Sprintf("/a/d%d/f", i)
		a[p] = rec(p, "same", 10)
		q := fmt.Sprintf("/b/d%d/f", i)
		b[q] = rec(q, "same", 10)
	}

	opts := streamOpts(0)
	got, err := streamCompare(dfsIter(a), dfsIter(b), opts)
	if err != nil {
		t.Fatalf("streamCompare: %v", err)
	}
	want, err := compareSnapshotsWith(mergeJoinInsert, mapToIter(a), mapToIter(b), opts)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if !reflect.DeepEqual(finalizeResults(got, "/", "/"), want) {
		t.Errorf("disagreed on a %d-member content group\n got: %s\nwant: %s",
			len(a), formatResults(finalizeResults(got, "/", "/")), formatResults(want))
	}
}

// Stage 8's fixed point is what stops a move source going unmentioned when the
// line that was supposed to name it is collapsed or truncated away - the
// false-unchanged failure goals.md ranks worst. These are the shapes that
// exercise it, checked against the tree engine rather than against an expected
// listing, so the assertion is "the same answer" and not "an answer I wrote
// down".
func TestExternal_MoveScenariosMatchTreeEngine(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]models.FileRecord
	}{
		{
			name: "directory moved wholesale",
			a: map[string]models.FileRecord{
				"/old/1": rec("/old/1", "h1", 10),
				"/old/2": rec("/old/2", "h2", 10),
			},
			b: map[string]models.FileRecord{
				"/new/1": rec("/new/1", "h1", 10),
				"/new/2": rec("/new/2", "h2", 10),
			},
		},
		{
			name: "two files swap contents",
			a: map[string]models.FileRecord{
				"/d/x": rec("/d/x", "hx", 10),
				"/d/y": rec("/d/y", "hy", 10),
			},
			b: map[string]models.FileRecord{
				"/d/x": rec("/d/x", "hy", 10),
				"/d/y": rec("/d/y", "hx", 10),
			},
		},
		{
			name: "copy alongside the original",
			a: map[string]models.FileRecord{
				"/keep/x": rec("/keep/x", "h1", 10),
			},
			b: map[string]models.FileRecord{
				"/keep/x": rec("/keep/x", "h1", 10),
				"/dup/x":  rec("/dup/x", "h1", 10),
			},
		},
		{
			name: "one source, many copies",
			a: map[string]models.FileRecord{
				"/src": rec("/src", "h1", 10),
			},
			b: map[string]models.FileRecord{
				"/src":  rec("/src", "h1", 10),
				"/c1/a": rec("/c1/a", "h1", 10),
				"/c2/a": rec("/c2/a", "h1", 10),
				"/c3/a": rec("/c3/a", "h1", 10),
			},
		},
		{
			name: "three removals absorb at most three moves",
			a: map[string]models.FileRecord{
				"/old/1": rec("/old/1", "same", 10),
				"/old/2": rec("/old/2", "same", 10),
				"/old/3": rec("/old/3", "same", 10),
			},
			b: map[string]models.FileRecord{
				"/new/1": rec("/new/1", "same", 10),
				"/new/2": rec("/new/2", "same", 10),
				"/new/3": rec("/new/3", "same", 10),
				"/new/4": rec("/new/4", "same", 10),
				"/new/5": rec("/new/5", "same", 10),
			},
		},
		{
			name: "move out of a directory that also gains content",
			a: map[string]models.FileRecord{
				"/d/gone":  rec("/d/gone", "h1", 10),
				"/d/stays": rec("/d/stays", "h2", 10),
			},
			b: map[string]models.FileRecord{
				"/elsewhere/gone": rec("/elsewhere/gone", "h1", 10),
				"/d/stays":        rec("/d/stays", "h2", 10),
				"/d/fresh":        rec("/d/fresh", "h3", 10),
			},
		},
		{
			name: "everything moves away, so the root itself is the source",
			a: map[string]models.FileRecord{
				"/1": rec("/1", "h1", 10),
				"/2": rec("/2", "h2", 10),
			},
			b: map[string]models.FileRecord{
				"/sub/1": rec("/sub/1", "h1", 10),
				"/sub/2": rec("/sub/2", "h2", 10),
			},
		},
		{
			name: "move into a file that became a directory",
			a: map[string]models.FileRecord{
				"/x":     rec("/x", "h1", 10),
				"/old/y": rec("/old/y", "h2", 10),
			},
			b: map[string]models.FileRecord{
				"/x/y": rec("/x/y", "h2", 10),
			},
		},
		{
			name: "zero-byte files never move to each other",
			a: map[string]models.FileRecord{
				"/d/a": rec("/d/a", "e3b0", 0),
				"/d/b": rec("/d/b", "e3b0", 0),
			},
			b: map[string]models.FileRecord{
				"/e/a": rec("/e/a", "e3b0", 0),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, budget := range []int{0, 1, 3} {
				requireStreamMatches(t, fmt.Sprintf("%s (budget %d)", tc.name, budget), tc.a, tc.b, budget)
			}
		})
	}
}

// --no-moves and --no-copies still have to mean what they say once the engine
// is capable of both, and each half has its own path through the matcher.
func TestExternal_HonoursMoveAndCopySwitches(t *testing.T) {
	for _, sw := range []struct {
		name              string
		noMoves, noCopies bool
	}{
		{"moves only", false, true},
		{"copies only", true, false},
	} {
		t.Run(sw.name, func(t *testing.T) {
			for seed := int64(0); seed < 2000; seed++ {
				a, b := generateTreePair(rand.New(rand.NewSource(seed)))
				opts := streamOpts(0)
				opts.NoMoves, opts.NoCopies = sw.noMoves, sw.noCopies

				got, err := streamCompare(dfsIter(a), dfsIter(b), opts)
				if err != nil {
					t.Fatalf("seed %d: %v", seed, err)
				}
				want, err := compareSnapshotsWith(mergeJoinInsert, mapToIter(a), mapToIter(b), opts)
				if err != nil {
					t.Fatalf("seed %d: tree: %v", seed, err)
				}
				if !reflect.DeepEqual(finalizeResults(got, "/", "/"), want) {
					t.Fatalf("seed %d: %s disagreed\n got: %s\nwant: %s", seed, sw.name,
						formatResults(finalizeResults(got, "/", "/")), formatResults(want))
				}
			}
		})
	}
}
