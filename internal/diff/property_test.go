package diff

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"fluxion/internal/models"
)

// The invariant this file exists to protect:
//
//	Every file that differs between the two snapshots must be *accounted for*
//	somewhere in the output - either reported at its own path, or covered by a
//	reported ancestor directory that subsumes it.
//
// The severity rule in knowledge/goals.md is why this is the invariant worth
// asserting above all others: a file that silently vanishes from the diff reads
// as "nothing changed here", and acting on that answer can cost the user data.
// Over-reporting only costs reading time. So these tests never assert that
// output is minimal - only that it is complete.

// collapsing lists, per kind of change, the ancestor statuses that legitimately
// stand in for a whole subtree. A directory only collapses when nothing beneath
// it is unchanged, and the line carries per-kind counts, so it does account for
// what it hides - but only in the right direction. "Added dir/" must never be
// what accounts for a file that was *removed* from underneath it.
var collapsing = map[string][]Status{
	"removed":  {StatusRemoved, StatusModified, StatusMove, StatusMovedSource},
	"added":    {StatusAdded, StatusModified, StatusMove, StatusCopy},
	"modified": {StatusModified, StatusAdded, StatusRemoved, StatusMove, StatusCopy},
}

// accountedFor reports whether some result covers path.
// Results carry paths relative to the snapshot root; the scenarios are written
// with absolute ones, so normalise before comparing.
func accountedFor(results []DiffResult, path, kind string) bool {
	path = strings.TrimPrefix(path, "/")

	for _, r := range results {
		if covers(r.RelPath, path, r.Status, collapsing[kind]) {
			return true
		}
		// A move reports the destination as its path and the origin as its
		// source. The origin's files are gone from where they were, and the
		// user can see that from the "<- old/path" half of the line.
		if kind == "removed" && r.Status == StatusMove &&
			covers(r.SourceRelPath, path, r.Status, collapsing[kind]) {
			return true
		}
	}
	return false
}

// covers reports whether a result at resultPath accounts for a change to path.
func covers(resultPath, path string, status Status, subsuming []Status) bool {
	rel := strings.TrimSuffix(resultPath, "/")
	if rel == "" || rel == "." {
		return false
	}

	// At the file's own path, any non-unchanged status is an acknowledgement
	// that something happened here; the user will see the line and look.
	if rel == path {
		return status != StatusUnchanged
	}

	if !strings.HasPrefix(path, rel+"/") {
		return false
	}
	for _, s := range subsuming {
		if status == s {
			return true
		}
	}
	return false
}

// checkSound asserts that no collapsed line claims something the snapshots
// contradict. A directory reported as wholly Added must not contain files that
// were already there, and one reported as wholly Removed must not contain files
// that still exist - either would send the user looking in the wrong direction.
func checkSound(t *testing.T, name string, a, b map[string]models.FileRecord, results []DiffResult) {
	t.Helper()

	under := func(m map[string]models.FileRecord, dir string) []string {
		var hits []string
		for p := range m {
			if strings.HasPrefix(strings.TrimPrefix(p, "/"), dir+"/") {
				hits = append(hits, p)
			}
		}
		sort.Strings(hits)
		return hits
	}

	// Content that left via a move is named on the move's own line, so it does
	// not make an enclosing "Added"/"Removed" summary a lie. Whether such a
	// directory should be summarised that way at all is a rollup question, not
	// a correctness one.
	movedAway := make(map[string]bool)
	for _, r := range results {
		if r.Status == StatusMove && r.SourceRelPath != "" {
			movedAway["/"+strings.TrimSuffix(r.SourceRelPath, "/")] = true
		}
	}
	accountedElsewhere := func(paths []string) []string {
		var rest []string
		for _, p := range paths {
			if !movedAway[p] {
				rest = append(rest, p)
			}
		}
		return rest
	}

	for _, r := range results {
		if !strings.HasSuffix(r.RelPath, "/") {
			continue // not a collapsed directory
		}
		dir := strings.TrimSuffix(r.RelPath, "/")

		switch r.Status {
		case StatusAdded:
			if stale := accountedElsewhere(under(a, dir)); len(stale) > 0 {
				t.Errorf("%s: %q reported as wholly Added, but these existed in A already: %v\n\nfull output:\n%s",
					name, r.RelPath, stale, formatResults(results))
			}
		case StatusRemoved:
			if survivors := accountedElsewhere(under(b, dir)); len(survivors) > 0 {
				t.Errorf("%s: %q reported as wholly Removed, but these still exist in B: %v\n\nfull output:\n%s",
					name, r.RelPath, survivors, formatResults(results))
			}
		}
	}
}

// checkComplete asserts the invariant for every differing file in a scenario.
func checkComplete(t *testing.T, name string, a, b map[string]models.FileRecord, results []DiffResult) {
	t.Helper()

	var missing []string

	for path, ra := range a {
		rb, inB := b[path]
		switch {
		case !inB:
			if !accountedFor(results, path, "removed") {
				missing = append(missing, fmt.Sprintf("%s (in A only) is not reported as removed", path))
			}
		case ra.SHA1 != rb.SHA1:
			if !accountedFor(results, path, "modified") {
				missing = append(missing, fmt.Sprintf("%s (content differs) is not reported as modified", path))
			}
		}
	}
	for path := range b {
		if _, inA := a[path]; !inA {
			if !accountedFor(results, path, "added") {
				missing = append(missing, fmt.Sprintf("%s (in B only) is not reported as added", path))
			}
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%s: %d file(s) unaccounted for in the diff:\n  %s\n\nfull output:\n%s",
			name, len(missing), strings.Join(missing, "\n  "), formatResults(results))
	}
}

func formatResults(results []DiffResult) string {
	if len(results) == 0 {
		return "  (empty)"
	}
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "  %-12s %s", r.Status, r.RelPath)
		if r.SourceRelPath != "" {
			fmt.Fprintf(&b, "  <- %s", r.SourceRelPath)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func rec(path, hash string, size int64) models.FileRecord {
	return models.FileRecord{Path: path, Filename: path, SizeBytes: size, SHA1: hash}
}

func runDiff(t *testing.T, a, b map[string]models.FileRecord) []DiffResult {
	t.Helper()
	results, err := CompareSnapshots(mapToIter(a), mapToIter(b), "/", "/", "sha1", false, false, false, nil)
	if err != nil {
		t.Fatalf("CompareSnapshots failed: %v", err)
	}
	return results
}

// TestInvariant_HandBuiltScenarios covers the shapes that are easy to get wrong,
// including the file/directory transitions that motivated this file.
func TestInvariant_HandBuiltScenarios(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]models.FileRecord
	}{
		{
			name: "file becomes a directory",
			a: map[string]models.FileRecord{
				"/data": rec("/data", "h1", 10),
			},
			b: map[string]models.FileRecord{
				"/data/one.txt": rec("/data/one.txt", "h2", 10),
				"/data/two.txt": rec("/data/two.txt", "h3", 10),
			},
		},
		{
			name: "directory becomes a file",
			a: map[string]models.FileRecord{
				"/data/one.txt": rec("/data/one.txt", "h2", 10),
				"/data/two.txt": rec("/data/two.txt", "h3", 10),
			},
			b: map[string]models.FileRecord{
				"/data": rec("/data", "h1", 10),
			},
		},
		{
			name: "file becomes a directory containing a file of the same content",
			a: map[string]models.FileRecord{
				"/x": rec("/x", "same", 10),
			},
			b: map[string]models.FileRecord{
				"/x/x": rec("/x/x", "same", 10),
			},
		},
		{
			name: "deep file to directory transition with siblings",
			a: map[string]models.FileRecord{
				"/a/b/c":     rec("/a/b/c", "h1", 10),
				"/a/sibling": rec("/a/sibling", "h9", 10),
			},
			b: map[string]models.FileRecord{
				"/a/b/c/inner": rec("/a/b/c/inner", "h2", 10),
				"/a/sibling":   rec("/a/sibling", "h9", 10),
			},
		},
		{
			name: "everything removed",
			a: map[string]models.FileRecord{
				"/d/1": rec("/d/1", "h1", 10),
				"/d/2": rec("/d/2", "h2", 10),
			},
			b: map[string]models.FileRecord{},
		},
		{
			name: "everything added",
			a:    map[string]models.FileRecord{},
			b: map[string]models.FileRecord{
				"/d/1": rec("/d/1", "h1", 10),
				"/d/2": rec("/d/2", "h2", 10),
			},
		},
		{
			name: "single file modified among unchanged siblings",
			a: map[string]models.FileRecord{
				"/d/1": rec("/d/1", "h1", 10),
				"/d/2": rec("/d/2", "h2", 10),
				"/d/3": rec("/d/3", "h3", 10),
			},
			b: map[string]models.FileRecord{
				"/d/1": rec("/d/1", "h1", 10),
				"/d/2": rec("/d/2", "CHANGED", 10),
				"/d/3": rec("/d/3", "h3", 10),
			},
		},
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
			name: "empty-hash files at distinct paths",
			a: map[string]models.FileRecord{
				"/d/a": rec("/d/a", "", 0),
				"/d/b": rec("/d/b", "", 0),
			},
			b: map[string]models.FileRecord{
				"/d/a": rec("/d/a", "", 0),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := runDiff(t, tc.a, tc.b)
			checkComplete(t, tc.name, tc.a, tc.b, results)
			checkSound(t, tc.name, tc.a, tc.b, results)
		})
	}
}

// randomTreeSeeds is how many generated pairs the routine test run covers. Every
// bug this test has found so far surfaced within the first few thousand, but the
// count is worth raising by hand (400000 takes ~20s) after changing the engine.
const randomTreeSeeds = 5000

// TestInvariant_RandomTrees generates random pairs of trees and asserts the same
// property. Failures print the seed and the whole diff, which is enough to
// reproduce by hand.
func TestInvariant_RandomTrees(t *testing.T) {
	for seed := int64(0); seed < randomTreeSeeds; seed++ {
		a, b := generateTreePair(rand.New(rand.NewSource(seed)))
		results := runDiff(t, a, b)
		name := fmt.Sprintf("seed %d", seed)
		checkComplete(t, name, a, b, results)
		checkSound(t, name, a, b, results)
	}
}

// generateTreePair builds two related trees, deliberately allowing a path to be
// a file in one snapshot and a directory in the other.
func generateTreePair(r *rand.Rand) (map[string]models.FileRecord, map[string]models.FileRecord) {
	names := []string{"a", "b", "c", "d", "e"}
	var paths []string

	// Build a pool of candidate paths, some of which are prefixes of others -
	// which is exactly how a file/directory collision arises. Shape varies with
	// the seed so the corpus contains both wide shallow trees and deep narrow
	// ones; the interesting bugs have all involved a rollup several levels up.
	count := 4 + r.Intn(16)
	maxDepth := 2 + r.Intn(3)
	for i := 0; i < count; i++ {
		depth := 1 + r.Intn(maxDepth)
		var parts []string
		for d := 0; d < depth; d++ {
			parts = append(parts, names[r.Intn(len(names))])
		}
		paths = append(paths, "/"+strings.Join(parts, "/"))
	}

	a := make(map[string]models.FileRecord)
	b := make(map[string]models.FileRecord)

	for _, p := range paths {
		hash := fmt.Sprintf("h%d", r.Intn(4))
		switch r.Intn(4) {
		case 0: // A only
			addIfNoConflict(a, p, hash)
		case 1: // B only
			addIfNoConflict(b, p, hash)
		case 2: // both, same content
			if addIfNoConflict(a, p, hash) {
				addIfNoConflict(b, p, hash)
			}
		default: // both, differing content
			if addIfNoConflict(a, p, hash) {
				addIfNoConflict(b, p, hash+"x")
			}
		}
	}
	return a, b
}

// addIfNoConflict refuses a path that would be both a file and a directory
// *within the same snapshot*, which a real filesystem cannot produce either.
// Collisions *across* snapshots are the interesting case and are left alone.
func addIfNoConflict(m map[string]models.FileRecord, path, hash string) bool {
	for existing := range m {
		if strings.HasPrefix(existing, path+"/") || strings.HasPrefix(path, existing+"/") {
			return false
		}
	}
	m[path] = rec(path, hash, 10)
	return true
}
