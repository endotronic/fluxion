package diff

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
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

		x := newExternalDiff(streamOpts(budget))
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

// Legacy-imported snapshots carry MD5 and no SHA-1 (knowledge/goals.md,
// "Lineage"), so --md5 is how a current scan is compared against the author's
// years of dupe-finder flat files - the case the fleet work actually starts
// from. The engines have to agree there too, and the matcher keys on whichever
// hash was selected, so it is a distinct path and not a relabelling.
func TestExternal_MD5MatchesTreeEngine(t *testing.T) {
	md5rec := func(path, hash string) models.FileRecord {
		return models.FileRecord{Path: path, Filename: path, SizeBytes: 10, MD5: hash}
	}

	for seed := int64(0); seed < 3000; seed++ {
		a, b := generateTreePair(rand.New(rand.NewSource(seed)))
		// Re-key the generated pair onto MD5, leaving SHA-1 empty as an
		// import-legacy snapshot does.
		for p, r := range a {
			a[p] = md5rec(p, r.SHA1)
		}
		for p, r := range b {
			b[p] = md5rec(p, r.SHA1)
		}

		opts := streamOpts(0)
		opts.HashType = "md5"

		got, err := streamCompare(dfsIter(a), dfsIter(b), opts)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		want, err := compareSnapshotsWith(mergeJoinInsert, mapToIter(a), mapToIter(b), opts)
		if err != nil {
			t.Fatalf("seed %d: tree: %v", seed, err)
		}
		if !reflect.DeepEqual(finalizeResults(got, "/", "/"), want) {
			t.Fatalf("seed %d: disagreed comparing by MD5\n got: %s\nwant: %s", seed,
				formatResults(finalizeResults(got, "/", "/")), formatResults(want))
		}
	}
}

// Stage 8's loop is capped at 32 rounds and returns whatever the 32nd produced.
// That guard is the difference between a bug that fails a test and one that
// quietly hands back a diff which never reached its fixed point - and each round
// is a full re-walk of both snapshots, so at fleet scale it is also the
// difference between one DB scan and several. Both are worth pinning with a
// measurement rather than an assumption.
func TestExternal_FixedPointConverges(t *testing.T) {
	rounds := map[int]int{}
	worst, worstSeed := 0, int64(-1)

	for seed := int64(0); seed < 20000; seed++ {
		a, b := generateTreePair(rand.New(rand.NewSource(seed)))
		for _, budget := range []int{0, 1} {
			x := newExternalDiff(streamOpts(budget))
			if _, err := x.compare(dfsIter(a), dfsIter(b)); err != nil {
				t.Fatalf("seed %d: %v", seed, err)
			}
			n := x.passes - 1 // pass 1 is the matcher, the rest are output rounds
			rounds[n]++
			if n > worst {
				worst, worstSeed = n, seed
			}
		}
	}

	t.Logf("output rounds: %v (worst %d, seed %d)", rounds, worst, worstSeed)
	// Measured over 100,000 runs of this corpus: 76%% need one round, 24%% need
	// two, 236 need three and 3 need four. Well clear of the guard - if this
	// starts approaching it, the loop is no longer converging for the reason
	// diff-algo.md's stage 8 says it does.
	if worst > 8 {
		t.Errorf("fixed point needed %d rounds (seed %d); the guard is 32, so it is "+
			"no longer converging in the two the rules predict", worst, worstSeed)
	}
	if rounds[1] == 0 {
		t.Error("no input finished in a single round - the fixed point is now always paying an extra full re-walk")
	}
}

// EstimateTempBytes is what a caller refuses to start on, so the only property
// that matters is the direction of its error: it must never come in under what
// the engine actually occupies. Checked against the measured peak on shapes that
// really do spill, including one that forces a multi-run merge - the case where
// the sorted output coexists with the runs it came from and temp usage roughly
// doubles.
func TestExternal_TempEstimateIsNotOptimistic(t *testing.T) {
	const pathLen = 20 // "/old/d0000/f0000.txt"

	for _, n := range []int{100_000, 400_000} {
		for _, sortMem := range []int{64 << 20, 4 << 20} { // one run, then many
			defer swapLimits(sortMem, spillMemLimit)()

			gen := func(prefix string) FileIterator {
				return func(yield func(string, models.FileRecord) error) error {
					for i := 0; i < n; i++ {
						p := fmt.Sprintf("/%s/d%04d/f%04d.txt", prefix, i/1000, i%1000)
						if err := yield(p, models.FileRecord{
							Path: p, SizeBytes: 10, SHA1: fmt.Sprintf("%040x", i),
						}); err != nil {
							return err
						}
					}
					return nil
				}
			}

			x := newExternalDiff(Options{
				RootA: "/", RootB: "/", HashType: "sha1",
				MaxLinesPerDir: DefaultMaxLinesPerDir, TempDir: t.TempDir(),
			})
			if _, err := x.compare(gen("old"), gen("new")); err != nil {
				t.Fatalf("n=%d: %v", n, err)
			}

			// Every file, every directory, on both sides.
			nodes := int64(2 * (n + n/1000 + 1))
			est := EstimateTempBytes(nodes, pathLen)
			t.Logf("n=%d sortMem=%dMiB: peak %d B (%.1f B/node), estimate %d B (%.1f B/node)",
				n, sortMem>>20, x.meter.peak, float64(x.meter.peak)/float64(nodes),
				est, float64(est)/float64(nodes))

			if est < x.meter.peak {
				t.Errorf("n=%d sortMem=%dMiB: estimate %d is below the measured peak %d - "+
					"a caller sizing a run against this would fill the filesystem",
					n, sortMem>>20, est, x.meter.peak)
			}
		}
	}
}

// The guard has to stop a run that would fill the temp filesystem, and it has to
// stop it as an error rather than as a short diff - a partial answer that looks
// complete is the failure goals.md ranks worst. Driven through the statfs hook
// rather than by actually filling a filesystem.
func TestExternal_RefusesWhenTempSpaceRunsOut(t *testing.T) {
	// One run's worth of intermediates, then the filesystem "fills".
	var calls int
	restore := tempFreeBytes
	tempFreeBytes = func(string) (int64, error) {
		calls++
		if calls > 1 {
			return 1 << 20, nil // 1 MiB left, below any sane reserve
		}
		return 100 << 30, nil
	}
	defer func() { tempFreeBytes = restore }()

	defer swapLimits(sortMemLimit, 1<<10)() // force everything onto "disk" early

	const n = 60_000
	gen := func(prefix string) FileIterator {
		return func(yield func(string, models.FileRecord) error) error {
			for i := 0; i < n; i++ {
				p := fmt.Sprintf("/%s/d%03d/f%04d", prefix, i/500, i%500)
				if err := yield(p, models.FileRecord{
					Path: p, SizeBytes: 10, SHA1: fmt.Sprintf("%040x", i),
				}); err != nil {
					return err
				}
			}
			return nil
		}
	}

	opts := Options{RootA: "/", RootB: "/", HashType: "sha1",
		MaxLinesPerDir: DefaultMaxLinesPerDir, TempDir: t.TempDir()}
	x := newExternalDiff(opts)
	x.meter.checkEvery = 1 << 20 // look often, so the test does not need to be huge

	results, err := x.compare(gen("old"), gen("new"))
	if !errors.Is(err, errTempSpaceExhausted) {
		t.Fatalf("err = %v, want errTempSpaceExhausted", err)
	}
	if results != nil {
		t.Errorf("got %d results alongside the error; a partial diff must not be returned", len(results))
	}
	if msg := err.Error(); !strings.Contains(msg, "--temp-dir") {
		t.Errorf("error does not tell the user what to do about it: %q", msg)
	}

	// A negative reserve is the caller saying the filesystem is theirs to fill.
	opts.MinFreeTempBytes = -1
	if _, err := newExternalDiff(opts).compare(gen("old"), gen("new")); err != nil {
		t.Errorf("MinFreeTempBytes < 0 should disable the guard, got %v", err)
	}
}
