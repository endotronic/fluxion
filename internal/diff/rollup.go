package diff

// The rollup decision - "given what this directory's children turned out to be,
// what does the directory itself say?" - lives here rather than inline in
// propagateNodeStatus so that a second engine can reach the same verdicts by
// construction instead of by reimplementation.
//
// knowledge/diff-algo.md documents these rules as "reverse-engineered from
// individual test cases, not derived from a stated principle", with several
// branches named after the test that motivated them. That is exactly the kind of
// logic that must not be transcribed twice: a streaming engine that got one
// branch subtly wrong would produce a diff that looks plausible and quietly
// disagrees, and the disagreements that matter are the ones where content stops
// being mentioned at all. So both engines feed the same accumulator and ask the
// same function.

// rollupAccum gathers what propagateNodeStatus needs to know about a directory's
// children. The tree engine fills it by walking child nodes; the streaming
// engine fills it as children close, without keeping them.
type rollupAccum struct {
	sawChild bool

	allUnchanged            bool
	allMovedSource          bool
	allRemovedOrMovedSource bool
	allAddedLike            bool // Added, Copy, Move

	hasMove     bool
	hasCopy     bool
	hasAdded    bool
	hasModified bool
	// hasMovedSource is why allAddedLike alone cannot justify an Added/Move/Copy
	// summary: content that left the directory is not content arriving in it.
	hasMovedSource bool

	hasUnchangedContent bool

	// First source by child name, for a rolled-up Move/Copy. "First" must mean
	// first by name - see the sortedChildren note in propagateNodeStatus.
	firstMoveSource string
	firstCopySource string

	changeCount int
}

func newRollupAccum() rollupAccum {
	return rollupAccum{
		allUnchanged:            true,
		allMovedSource:          true,
		allRemovedOrMovedSource: true,
		allAddedLike:            true,
	}
}

// observe folds one child's outcome in. Children must be observed in name
// order, because firstMoveSource/firstCopySource depend on it.
func (a *rollupAccum) observe(s Status, childHasUnchanged bool, childSourcePath string) {
	a.sawChild = true

	if childHasUnchanged {
		a.hasUnchangedContent = true
	}

	if s != StatusUnchanged {
		a.allUnchanged = false
	}
	if s != StatusMovedSource {
		a.allMovedSource = false
	}
	if s != StatusRemoved && s != StatusMovedSource {
		a.allRemovedOrMovedSource = false
	}
	if s != StatusAdded && s != StatusCopy && s != StatusMove && s != StatusMovedSource {
		a.allAddedLike = false
	}

	switch s {
	case StatusAdded:
		a.hasAdded = true
	case StatusMovedSource:
		a.hasMovedSource = true
	case StatusModified:
		a.hasModified = true
	case StatusMove:
		a.hasMove = true
		if a.firstMoveSource == "" {
			a.firstMoveSource = childSourcePath
		}
	case StatusCopy:
		a.hasCopy = true
		if a.firstCopySource == "" {
			a.firstCopySource = childSourcePath
		}
	case StatusMixed:
		a.allUnchanged = false
		a.allMovedSource = false
		a.allRemovedOrMovedSource = false
		a.allAddedLike = false
	}

	if isChangeStatus(s) {
		a.changeCount++
	}
}

func isChangeStatus(s Status) bool {
	return s == StatusAdded || s == StatusRemoved || s == StatusModified ||
		s == StatusCopy || s == StatusMove
}

// decideRollup returns the directory's status, the source path it should carry
// (empty unless the status is a rolled-up Move/Copy) and whether its subtree
// holds unchanged content.
//
// dirA is the node's DirA: whether anything at or under this path existed in A.
// It is checked directly rather than inferred from hashes, because a directory
// whose A-side children all lack the compared hash has an empty digest and is
// still very much present.
func decideRollup(a rollupAccum, dirA bool) (status Status, sourcePath string, hasUnchanged bool) {
	if !a.sawChild {
		return StatusUnchanged, "", true
	}

	if a.allUnchanged {
		return StatusUnchanged, "", true
	}
	if a.allMovedSource {
		return StatusMovedSource, "", false
	}
	if a.allRemovedOrMovedSource {
		return StatusRemoved, "", false
	}

	// New in B and holding only added-like things.
	if !dirA && a.allAddedLike {
		if !a.hasMove && !a.hasCopy {
			return StatusAdded, "", false // pure additions
		}
		if a.changeCount > 1 {
			return StatusAdded, "", false
		}
		// A single Move/Copy falls through, so the detail is shown.
	}

	canRollup := !a.hasUnchangedContent && (a.allAddedLike || a.changeCount >= 2)
	if canRollup {
		if a.hasModified {
			return StatusModified, "", a.hasUnchangedContent
		}

		// allAddedLike counts MovedSource as added-like so a directory emptied
		// by a move still rolls up. But summarising a directory that holds a
		// MovedSource as Added/Move/Copy would describe only what arrived and
		// silently drop what left, so those require !hasMovedSource.
		if a.allAddedLike && !a.hasMovedSource {
			typesFound := 0
			if a.hasMove {
				typesFound++
			}
			if a.hasCopy {
				typesFound++
			}
			if a.hasAdded {
				typesFound++
			}
			// More than one kind of add-like change: refuse to pick a winner.
			if typesFound > 1 {
				return StatusModified, "", a.hasUnchangedContent
			}

			// Preference: Move > Added > Copy.
			if a.hasMove {
				if a.changeCount == 1 {
					return StatusMixed, "", a.hasUnchangedContent
				}
				return StatusMove, a.firstMoveSource, false
			}
			if a.hasAdded {
				return StatusAdded, "", false
			}
			if a.hasCopy {
				if a.changeCount == 1 {
					return StatusMixed, "", a.hasUnchangedContent
				}
				return StatusCopy, a.firstCopySource, false
			}
		}

		// Mixed kinds of change, but nothing unchanged to protect.
		if !a.hasUnchangedContent {
			if !dirA {
				return StatusAdded, "", false
			}
			return StatusModified, "", false
		}
	}

	return StatusMixed, "", a.hasUnchangedContent
}
