package diff

import (
	"errors"
	"fmt"
	"strings"

	"fluxion/internal/models"
)

// Phase 2 of knowledge/diff-memory.md: produce the diff from a depth-first
// stream instead of a materialised tree.
//
// Where the memory goes instead: one streamFrame per directory on the current
// path, so O(depth), plus each frame's pending output lines, which --max-lines
// caps at the budget - O(depth x budget) in total, kilobytes rather than
// gigabytes. Nothing is retained per file.
//
// # What this engine does not do yet
//
// It handles NoMoves && NoCopies only, and streamCompare refuses anything else.
// That is not a shortcut, it is where the phase boundary genuinely falls:
// detectMovesCopies (stage 6) needs a hash-keyed index over every node in both
// snapshots, which is exactly the global view a stream does not have. Phase 3
// replaces it with an external sort over a spine file; until then, a stream can
// answer "what changed" but not "where did it go".
//
// The restriction is worth having on its own. The cases that actually blow up
// today - two scans of the same multi-million-file tree, the stale-replica
// comparisons in knowledge/fleet.md - are mostly-static trees where moves are
// rare and the answer wanted is "what does the replica lack".
//
// # Why the input must be DFS-ordered
//
// Plain path order is not enough, for the reason diff-memory.md's "Fact 2" gives:
// given a file `a`, a file `a.txt` and a file `a/x`, byte order is `a`, `a.txt`,
// `a/x`, because '.' (0x2E) sorts below '/' (0x2F). A frame would then see its
// children out of name order and would have to re-sort its output, and a leaf
// could turn out to be a directory long after its siblings had been finalised.
// Ordering by the DFS key - the path with '/' rewritten to 0x01 - makes `a` and
// `a\x01x` adjacent, so a leaf is either claimed by the very next record or
// never.
//
// 0x01 is a legal byte in a filename, so a pathological name could still sort
// wrong. streamCompare therefore verifies the order as it walks rather than
// trusting it, and returns errStreamOutOfOrder if it is violated. Under
// goals.md's severity rule a mis-nested node is a wrong answer, not a cosmetic
// one, so refusing to answer is the only acceptable response - the caller falls
// back to the tree engine.

// errStreamUnsupported reports options the streaming engine cannot honour.
var errStreamUnsupported = errors.New("diff: streaming engine requires --no-moves and --no-copies")

// errStreamOutOfOrder reports input that was not in DFS-key order, which the
// streaming engine cannot process correctly and will not guess at.
var errStreamOutOfOrder = errors.New("diff: input not in DFS-key order")

// dfsKey maps a path to the order the streaming engine needs: the same as byte
// order on the path, except that '/' sorts below every other legal byte, so a
// directory's contents immediately follow the directory itself.
func dfsKey(path string) string {
	return strings.ReplaceAll(path, "/", "\x01")
}

// streamLeaf is a file record's two sides, held only until it is known whether
// the path is also a directory.
type streamLeaf struct {
	name         string
	inA, inB     bool
	hashA, hashB hashVal
}

func (l *streamLeaf) status() Status {
	switch {
	case l.inA && !l.inB:
		return StatusRemoved
	case l.inB && !l.inA:
		return StatusAdded
	case l.hashA.empty() || l.hashB.empty():
		// Present on both sides but at least one carries no hash of the compared
		// type, so the contents cannot be claimed to match. Modified, never
		// Unchanged - goals.md.
		return StatusModified
	case l.hashA == l.hashB:
		return StatusUnchanged
	default:
		return StatusModified
	}
}

// statsOf returns the leaf's contribution to a collapsed ancestor's counts.
func statsOf(s Status) DiffResult {
	var r DiffResult
	switch s {
	case StatusAdded:
		r.AddedCount = 1
	case StatusRemoved:
		r.RemovedCount = 1
	case StatusModified:
		r.ModifiedCount = 1
	case StatusCopy:
		r.CopyCount = 1
	case StatusMove:
		r.MoveCount = 1
	case StatusUnchanged:
		r.UnchangedFileCount = 1
	}
	return r
}

func addStats(dst *DiffResult, src DiffResult) {
	dst.AddedCount += src.AddedCount
	dst.RemovedCount += src.RemovedCount
	dst.ModifiedCount += src.ModifiedCount
	dst.CopyCount += src.CopyCount
	dst.MoveCount += src.MoveCount
	dst.UnchangedFileCount += src.UnchangedFileCount
	dst.UnchangedDirCount += src.UnchangedDirCount
}

// streamFrame is one open directory. It holds what the rollup decision needs
// about its children and the output they produced - never the children.
type streamFrame struct {
	name string
	path string // full path, built once on open

	dirA, dirB bool
	acc        rollupAccum
	stats      DiffResult

	// twin is the file aspect of this directory's own path, when the same path
	// is a file on one side and a directory on the other.
	twin *streamLeaf

	// pendingLeaf is the last leaf added here, still unfinalised because the
	// very next record may reveal it to be a directory. DFS-key order is what
	// makes one slot enough: the claiming record, if any, is adjacent.
	pendingLeaf *streamLeaf

	// out is this subtree's output so far, already budget-capped by the frames
	// beneath it. Capped again when this frame closes.
	out []DiffResult
}

type streamEngine struct {
	opts  Options
	stack []*streamFrame

	prevKey string
	started bool

	// High-water marks of what the engine retains, so the O(depth x budget)
	// bound can be asserted structurally rather than by sampling the heap -
	// which is both noisy and, done at any useful rate, slower than the work
	// being measured. Two ints; not worth gating behind a build tag.
	peakFrames int
	peakLines  int
}

// observeRetention records the current retained size. Called where it changes
// meaningfully - on push and on close - which is per directory, not per file.
func (e *streamEngine) observeRetention() {
	if len(e.stack) > e.peakFrames {
		e.peakFrames = len(e.stack)
	}
	lines := 0
	for _, f := range e.stack {
		lines += len(f.out)
	}
	if lines > e.peakLines {
		e.peakLines = lines
	}
}

// streamCompare is the streaming counterpart to compareSnapshotsWith. It
// produces byte-identical output to the tree engine for the options it accepts;
// streaming_test.go asserts that against the same corpus property_test.go uses.
func streamCompare(iterA, iterB FileIterator, opts Options) ([]DiffResult, error) {
	if !opts.NoMoves || !opts.NoCopies {
		return nil, errStreamUnsupported
	}

	e := &streamEngine{opts: opts}
	root := &streamFrame{name: "", path: "", acc: newRollupAccum()}
	e.stack = []*streamFrame{root}

	err := mergeJoinStreamsBy(iterA, iterB, dfsKey, func(path string, a, b *models.FileRecord) error {
		return e.add(path, a, b)
	})
	if err != nil {
		return nil, err
	}

	// Close everything still open, innermost first.
	for len(e.stack) > 1 {
		e.closeTop()
	}
	e.finalizeLeaf(root)

	results := root.out
	if opts.ShowUnchanged {
		stats := root.stats
		if stats.UnchangedFileCount > 0 || stats.UnchangedDirCount > 0 {
			results = append(results, DiffResult{
				Path:               ".",
				Status:             StatusMixed,
				UnchangedFileCount: stats.UnchangedFileCount,
				UnchangedDirCount:  stats.UnchangedDirCount,
			})
		}
	}
	return results, nil
}

// add folds one merged record into the stack.
func (e *streamEngine) add(path string, a, b *models.FileRecord) error {
	key := dfsKey(path)
	if e.started && key <= e.prevKey {
		return fmt.Errorf("%w: %q did not follow %q", errStreamOutOfOrder, path, e.prevPath())
	}
	e.prevKey, e.started = key, true

	parts := splitPath(path)
	if len(parts) == 0 {
		// A record at "" or "/". The tree engine marks the root itself a file;
		// there is no sensible line for it and none is emitted, matching.
		return nil
	}

	dirs, leafName := parts[:len(parts)-1], parts[len(parts)-1]

	// Unwind to the deepest frame still on this record's path.
	common := 1 // the root frame is always common
	for common < len(e.stack) && common-1 < len(dirs) && e.stack[common].name == dirs[common-1] {
		common++
	}
	for len(e.stack) > common {
		e.closeTop()
	}

	// Descend, opening frames for the components not yet on the stack.
	for i := common - 1; i < len(dirs); i++ {
		e.open(dirs[i])
	}

	top := e.stack[len(e.stack)-1]
	e.finalizeLeaf(top)

	leaf := &streamLeaf{name: leafName}
	if a != nil {
		leaf.inA = true
		leaf.hashA = compactHash(hashOfRecord(*a, e.opts.HashType))
	}
	if b != nil {
		leaf.inB = true
		leaf.hashB = compactHash(hashOfRecord(*b, e.opts.HashType))
	}
	top.pendingLeaf = leaf
	return nil
}

func (e *streamEngine) prevPath() string {
	return strings.ReplaceAll(e.prevKey, "\x01", "/")
}

// open pushes a frame for a child directory, claiming the parent's pending leaf
// as a FileTwin when the same name was seen as a file first. DFS-key order is
// what makes a single pending slot enough: the claiming record, if there is one,
// is adjacent to the leaf.
func (e *streamEngine) open(name string) {
	parent := e.stack[len(e.stack)-1]

	var twin *streamLeaf
	if parent.pendingLeaf != nil && parent.pendingLeaf.name == name {
		twin = parent.pendingLeaf
		parent.pendingLeaf = nil
	} else {
		e.finalizeLeaf(parent)
	}

	// Note what is deliberately NOT done here: the twin's presence does not set
	// the new frame's dirA/dirB. A directory's DirA means "something at or under
	// this path existed in A" as computed from its *children* - the tree engine
	// derives it in computeMerkleHashes from child.presentInA(), after
	// splitFileDirCollisions has already moved the file aspect out to the twin.
	// The twin's own presence reaches the *parent* instead, via presentInA().
	f := &streamFrame{
		name: name,
		path: parent.path + "/" + name,
		acc:  newRollupAccum(),
		twin: twin,
	}
	e.stack = append(e.stack, f)
	e.observeRetention()
}

// finalizeLeaf folds a frame's pending leaf in, now that nothing can claim it as
// a directory.
func (e *streamEngine) finalizeLeaf(f *streamFrame) {
	leaf := f.pendingLeaf
	if leaf == nil {
		return
	}
	f.pendingLeaf = nil

	status := leaf.status()
	f.acc.observe(status, status == StatusUnchanged, "")
	stats := statsOf(status)
	addStats(&f.stats, stats)
	if leaf.inA {
		f.dirA = true
	}
	if leaf.inB {
		f.dirB = true
	}

	if status == StatusUnchanged || status == StatusMovedSource {
		return // reports nothing of its own
	}

	line := DiffResult{Path: f.path + "/" + leaf.name, Status: status}
	addStats(&line, stats)
	// A leaf line never carries unchanged counts, even with --show-unchanged:
	// an unchanged leaf produces no line at all.
	line.UnchangedFileCount, line.UnchangedDirCount = 0, 0
	f.out = append(f.out, line)
}

// closeTop finalises the innermost frame and folds its verdict into its parent.
func (e *streamEngine) closeTop() {
	f := e.stack[len(e.stack)-1]
	e.stack = e.stack[:len(e.stack)-1]
	parent := e.stack[len(e.stack)-1]

	e.finalizeLeaf(f)

	// Cap what this directory's children produced, before it is decided whether
	// any of it survives. The tree engine budgets at the same point
	// (collectChildren -> applyBudget), and the root is exempt in both.
	f.applyBudget(e.opts)

	dirStatus, _, hasUnchanged := decideRollup(f.acc, f.dirA)

	// Reconcile with the file aspect, exactly as propagateStatus does: an
	// unchanged twin only contributes "there is unchanged content here", while a
	// twin whose fate differs from the directory's forces Mixed - a single
	// summary status would necessarily hide one of the two halves.
	finalStatus := dirStatus
	var twinStatus Status
	if f.twin != nil {
		twinStatus = f.twin.status()
		if twinStatus == StatusUnchanged {
			hasUnchanged = true
		} else if twinStatus != dirStatus {
			finalStatus = StatusMixed
		}
		addStats(&f.stats, statsOf(twinStatus))
	}

	if finalStatus == StatusUnchanged {
		f.stats.UnchangedDirCount++
	}

	// Counts and presence reach the parent however this directory is reported.
	addStats(&parent.stats, f.stats)
	if f.dirA || (f.twin != nil && f.twin.inA) {
		parent.dirA = true
	}
	if f.dirB || (f.twin != nil && f.twin.inB) {
		parent.dirB = true
	}

	var lines []DiffResult
	switch finalStatus {
	case StatusUnchanged, StatusMovedSource:
		// Reports nothing itself.

	case StatusMixed:
		lines = f.out
		if e.opts.ShowUnchanged && (f.stats.UnchangedFileCount > 0 || f.stats.UnchangedDirCount > 0) {
			// Post-order context row, after the children it contextualises, and
			// deliberately outside the budget applied above.
			lines = append(lines, DiffResult{
				Path:               f.path + "/",
				Status:             StatusMixed,
				UnchangedFileCount: f.stats.UnchangedFileCount,
				UnchangedDirCount:  f.stats.UnchangedDirCount,
			})
		}

	default:
		// Collapses: one line standing in for the whole subtree.
		line := DiffResult{
			Path:          f.path + "/",
			Status:        finalStatus,
			AddedCount:    f.stats.AddedCount,
			RemovedCount:  f.stats.RemovedCount,
			ModifiedCount: f.stats.ModifiedCount,
			CopyCount:     f.stats.CopyCount,
			MoveCount:     f.stats.MoveCount,
		}
		if e.opts.ShowUnchanged {
			line.UnchangedFileCount = f.stats.UnchangedFileCount
			line.UnchangedDirCount = f.stats.UnchangedDirCount
		}
		lines = []DiffResult{line}
	}

	// A path that is a file on one side and a directory on the other produces
	// two lines. Whichever half existed in A goes first, so the pair reads in
	// the order things happened: the old thing goes, the new thing arrives.
	if f.twin != nil && twinStatus != StatusUnchanged && twinStatus != StatusMovedSource {
		twinLine := DiffResult{Path: f.path, Status: twinStatus}
		addStats(&twinLine, statsOf(twinStatus))
		twinLine.UnchangedFileCount, twinLine.UnchangedDirCount = 0, 0
		if twinStatus == StatusRemoved {
			lines = append([]DiffResult{twinLine}, lines...)
		} else {
			lines = append(lines, twinLine)
		}
	}

	parent.acc.observe(finalStatus, hasUnchanged, "")
	parent.out = append(parent.out, lines...)
	e.observeRetention()
}

// applyBudget caps what this frame accumulated from its children, mirroring
// collector.applyBudget: keep the first N lines and replace the rest with one
// Truncated summary carrying their combined counts, so nothing is dropped
// silently. This is what bounds a frame's retained output to O(budget), and with
// it the whole engine to O(depth x budget).
//
// MaxLinesPerDir 0 means unlimited, which is unbounded by construction - the one
// place the line budget stops being a readability feature and becomes the memory
// bound (diff-memory.md, "Fact 1").
func (f *streamFrame) applyBudget(opts Options) {
	if opts.MaxLinesPerDir <= 0 || len(f.out) <= opts.MaxLinesPerDir {
		return
	}
	var sum DiffResult
	for _, r := range f.out[opts.MaxLinesPerDir:] {
		addStats(&sum, r)
		// A nested summary already stands in for lines of its own; this one
		// stands in for those too.
		sum.HiddenCount += r.HiddenCount
	}
	sum.HiddenCount += int64(len(f.out) - opts.MaxLinesPerDir)
	sum.Path = f.path + "/"
	sum.Status = StatusTruncated
	f.out = append(f.out[:opts.MaxLinesPerDir], sum)
}

func splitPath(path string) []string {
	clean := strings.TrimPrefix(path, "/")
	if clean == "" {
		return nil
	}
	parts := strings.Split(clean, "/")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func hashOfRecord(rec models.FileRecord, hashType string) string {
	if hashType == "md5" {
		return rec.MD5
	}
	return rec.SHA1
}
