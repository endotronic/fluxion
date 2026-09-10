package diff

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"fluxion/internal/models"
)

// The acceptance test for every stage knowledge/diff-memory.md replaces is
// EQUIVALENCE, not inspection: the new implementation must produce byte-identical
// []DiffResult to the one the golden tests and property_test.go's seeds were
// validated against. This file establishes that harness for Phase 1's tree
// builder; Phases 2-5 should extend it rather than start over.

func runBuilder(t *testing.T, build treeBuilder, a, b map[string]models.FileRecord, maxLines int) []DiffResult {
	t.Helper()
	results, err := compareSnapshotsWith(build, mapToIter(a), mapToIter(b), Options{
		RootA: "/", RootB: "/", HashType: "sha1", MaxLinesPerDir: maxLines,
	})
	if err != nil {
		t.Fatalf("compareSnapshotsWith failed: %v", err)
	}
	return results
}

func requireSameResults(t *testing.T, name string, got, want []DiffResult) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	t.Errorf("%s: merge-join builder disagreed with two-pass oracle\n got (%d lines): %s\nwant (%d lines): %s",
		name, len(got), formatResults(got), len(want), formatResults(want))
}

func TestEquivalence_MergeJoinMatchesTwoPass(t *testing.T) {
	for seed := int64(0); seed < randomTreeSeeds; seed++ {
		a, b := generateTreePair(rand.New(rand.NewSource(seed)))
		name := fmt.Sprintf("seed %d", seed)

		got := runBuilder(t, mergeJoinInsert, a, b, 0)
		want := runBuilder(t, twoPassInsert, a, b, 0)
		requireSameResults(t, name, got, want)

		if t.Failed() {
			t.FailNow() // one disagreement is enough; the rest is noise
		}
	}
}

// The line budget is the only mechanism that removes lines on purpose, so it is
// the one most able to expose a builder difference as lost output rather than
// merely different output.
func TestEquivalence_MergeJoinMatchesTwoPass_Budgeted(t *testing.T) {
	for seed := int64(0); seed < randomTreeSeeds; seed++ {
		a, b := generateTreePair(rand.New(rand.NewSource(seed)))
		name := fmt.Sprintf("seed %d (budget 1)", seed)

		got := runBuilder(t, mergeJoinInsert, a, b, 1)
		want := runBuilder(t, twoPassInsert, a, b, 1)
		requireSameResults(t, name, got, want)

		if t.Failed() {
			t.FailNow()
		}
	}
}

// shuffledIter yields the same records in an order chosen by the seed, breaking
// the sorted-input assumption on purpose.
func shuffledIter(m map[string]models.FileRecord, r *rand.Rand) FileIterator {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic starting point, then shuffle
	r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })

	return func(yield func(string, models.FileRecord) error) error {
		for _, k := range keys {
			if err := yield(k, m[k]); err != nil {
				return err
			}
		}
		return nil
	}
}

// TestEquivalence_MergeJoinToleratesUnsortedInput is the one that earns its
// keep. IterateFiles now promises path order, but app/diff.go relativises paths
// against root_path afterwards and falls back to the absolute path for any
// record not underneath it - so a snapshot holding both kinds reaches the
// builder interleaved, no matter what the SQL did. A merge join that silently
// dropped or double-applied a record in that case would corrupt the diff in
// precisely the direction goals.md's severity rule forbids.
//
// It cannot: every iteration consumes at least one record, the loop runs until
// both sides are exhausted, and locateNode is idempotent. This asserts that
// against the oracle rather than trusting the argument.
func TestEquivalence_MergeJoinToleratesUnsortedInput(t *testing.T) {
	for seed := int64(0); seed < 2000; seed++ {
		r := rand.New(rand.NewSource(seed))
		a, b := generateTreePair(r)
		name := fmt.Sprintf("seed %d (shuffled input)", seed)

		shuffled, err := compareSnapshotsWith(mergeJoinInsert,
			shuffledIter(a, r), shuffledIter(b, r),
			Options{RootA: "/", RootB: "/", HashType: "sha1"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		want := runBuilder(t, twoPassInsert, a, b, 0)
		requireSameResults(t, name, shuffled, want)

		if t.Failed() {
			t.FailNow()
		}
	}
}

// TestDeterminism_RepeatedRunsAgree guards the bug the equivalence tests above
// found on their first run, which predated them and had nothing to do with the
// merge join: propagateNodeStatus captured "the first Move/Copy child" while
// iterating node.Children, a map, so a rolled-up Move/Copy line named a
// different source on each run of the same input (`Move d/ <- c/c/b/c` one run,
// `Move d/ <- e` the next). The property test never caught it because both
// answers are complete and sound - every differing file is still accounted for -
// so it is "wrong or unstable", severity 2 by known-issues.md's taxonomy, not
// the data-loss class.
//
// Comparing a builder against an oracle is what exposed it: any equivalence
// check is worthless while the thing being checked disagrees with itself.
func TestDeterminism_RepeatedRunsAgree(t *testing.T) {
	const repeats = 4
	for seed := int64(0); seed < 2000; seed++ {
		a, b := generateTreePair(rand.New(rand.NewSource(seed)))
		first := runBuilder(t, mergeJoinInsert, a, b, 0)

		for rep := 0; rep < repeats; rep++ {
			again := runBuilder(t, mergeJoinInsert, a, b, 0)
			if !reflect.DeepEqual(first, again) {
				t.Fatalf("seed %d, repeat %d: identical input produced different output\nfirst: %s\nagain: %s",
					seed, rep, formatResults(first), formatResults(again))
			}
		}
	}
}

// A shared path must be located once, not twice - that is the whole efficiency
// argument for the merge join. Counting node creations is indirect, so this
// checks the observable consequence instead: identical trees, and the shared
// path carrying both sides.
func TestMergeJoin_SharedPathRecordsBothSides(t *testing.T) {
	a := map[string]models.FileRecord{"/x": {SHA1: "aaa", SizeBytes: 1}}
	b := map[string]models.FileRecord{"/x": {SHA1: "bbb", SizeBytes: 2}}

	root := &Node{Name: "", Path: "", Children: make(map[string]*Node), Status: StatusUnchanged}
	if err := mergeJoinInsert(root, mapToIter(a), mapToIter(b), "sha1", nil); err != nil {
		t.Fatalf("mergeJoinInsert: %v", err)
	}

	n := root.Children["x"]
	if n == nil {
		t.Fatal("expected node /x")
	}
	if !n.InA || !n.InB {
		t.Errorf("InA=%v InB=%v, want both true", n.InA, n.InB)
	}
	if n.SizeA != 1 || n.SizeB != 2 {
		t.Errorf("SizeA=%d SizeB=%d, want 1 and 2", n.SizeA, n.SizeB)
	}
	if n.Status != StatusModified {
		t.Errorf("Status=%v, want %v", n.Status, StatusModified)
	}
}

func TestMergeJoin_PropagatesIteratorErrors(t *testing.T) {
	boom := fmt.Errorf("boom")
	failing := FileIterator(func(yield func(string, models.FileRecord) error) error {
		return boom
	})
	ok := mapToIter(map[string]models.FileRecord{"/a": {SHA1: "1"}})

	root := &Node{Name: "", Path: "", Children: make(map[string]*Node), Status: StatusUnchanged}
	if err := mergeJoinInsert(root, failing, ok, "sha1", nil); err == nil {
		t.Error("expected the A-side error to surface")
	}

	root = &Node{Name: "", Path: "", Children: make(map[string]*Node), Status: StatusUnchanged}
	if err := mergeJoinInsert(root, ok, failing, "sha1", nil); err == nil {
		t.Error("expected the B-side error to surface")
	}
}
