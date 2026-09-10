package diff

import "fluxion/internal/models"

// This file holds the two ways the unified tree can be built from A's and B's
// record streams. They must produce identical trees; mergejoin_test.go asserts
// exactly that over the same random inputs the property test uses.
//
// Phase 1 of knowledge/diff-memory.md. The tree is still fully materialised
// afterwards, so this buys no memory on its own - the point is to establish
// that the tree CAN be built by co-walking two ordered streams, since the later
// phases replace the tree with a stack and will have no map to fall back on.

// progressReporter throttles the per-record progress callback to roughly every
// 1000 records without requiring the count to land exactly on a multiple, which
// a merge join cannot promise: it consumes two records in a single step
// whenever both sides carry the same path.
type progressReporter struct {
	onProgress   func(int)
	count        int
	lastReported int
}

func (p *progressReporter) add(n int) {
	p.count += n
	if p.onProgress != nil && p.count-p.lastReported >= 1000 {
		p.onProgress(p.count)
		p.lastReported = p.count
	}
}

func (p *progressReporter) finish() {
	if p.onProgress != nil {
		p.onProgress(p.count)
	}
}

// twoPassInsert builds the tree the original way: every record of A, then every
// record of B, each located independently.
//
// Retained as the oracle for the merge-join builder rather than deleted. It is
// the behaviour every golden test and every one of property_test.go's 400,000
// seeds was validated against, which makes it the only trustworthy definition
// of "the tree we are supposed to get".
func twoPassInsert(root *Node, iterA, iterB FileIterator, hashType string, onProgress func(int)) error {
	prog := &progressReporter{onProgress: onProgress}

	if err := iterA(func(path string, record models.FileRecord) error {
		insertNode(root, path, record, true, hashType)
		prog.add(1)
		return nil
	}); err != nil {
		return err
	}

	if err := iterB(func(path string, record models.FileRecord) error {
		insertNode(root, path, record, false, hashType)
		prog.add(1)
		return nil
	}); err != nil {
		return err
	}

	prog.finish()
	return nil
}

// mergeJoinInsert builds the same tree by co-walking both streams, advancing
// whichever side holds the smaller path and locating a path shared by both
// exactly once instead of twice.
//
// **Correct even if the input is not actually sorted.** The loop's only real
// invariant is that every iteration consumes at least one record and the loop
// runs until both sides are exhausted, so every record is applied exactly once
// no matter what order it arrives in. Order affects only whether a path's two
// sides are handled in one step or two, and locateNode is idempotent, so both
// routes leave the identical node. This matters because sorted-at-SQL does not
// imply sorted-at-yield: app/diff.go relativises paths against root_path and
// falls back to the absolute path for any record not underneath it, so a
// snapshot holding both kinds arrives interleaved. Ordering is therefore an
// optimisation here (one tree walk instead of two for shared paths), never a
// correctness requirement - which is the property the later, stack-based phases
// will NOT enjoy, and the reason to establish this one first.
func mergeJoinInsert(root *Node, iterA, iterB FileIterator, hashType string, onProgress func(int)) error {
	pa := newPullIter(iterA)
	defer pa.stop()
	pb := newPullIter(iterB)
	defer pb.stop()

	prog := &progressReporter{onProgress: onProgress}

	okA := pa.advance()
	okB := pb.advance()

	for okA || okB {
		switch {
		case okA && okB && pa.cur.path == pb.cur.path:
			// Shared path: locate once, record both sides.
			n := locateNode(root, pa.cur.path)
			applySide(n, pa.cur.rec, true, hashType)
			applySide(n, pb.cur.rec, false, hashType)
			prog.add(2)
			okA = pa.advance()
			okB = pb.advance()

		case okA && (!okB || pa.cur.path < pb.cur.path):
			applySide(locateNode(root, pa.cur.path), pa.cur.rec, true, hashType)
			prog.add(1)
			okA = pa.advance()

		default: // okB, and B's path sorts first (or A is exhausted)
			applySide(locateNode(root, pb.cur.path), pb.cur.rec, false, hashType)
			prog.add(1)
			okB = pb.advance()
		}
	}

	// A source failure is only observable once its side reports exhaustion,
	// which the loop above guarantees for both.
	if err := pa.err(); err != nil {
		return err
	}
	if err := pb.err(); err != nil {
		return err
	}

	prog.finish()
	return nil
}
