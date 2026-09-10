package diff

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"strings"

	"fluxion/internal/models"
)

// Phase 2 of knowledge/diff-memory.md: produce the diff from a depth-first
// stream instead of a materialised tree. Phase 3 (external.go) drives this same
// walk two or more times to add move/copy detection.
//
// Where the memory goes instead: one streamFrame per directory on the current
// path, so O(depth), plus each frame's pending output lines, which --max-lines
// caps at the budget - O(depth x budget) in total, kilobytes rather than
// gigabytes. Nothing is retained per file.
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

// errStreamUnsupported reports options or input shapes the streaming engine
// cannot honour, and is the signal CompareSnapshots falls back to the tree
// engine on.
var errStreamUnsupported = errors.New("diff: streaming engine cannot answer this query")

// errStreamMatchedTwin reports the one tree-engine behaviour a stream cannot
// reproduce; see the comment on the check in closeTop.
var errStreamMatchedTwin = fmt.Errorf("%w: matched directory with a file twin", errStreamUnsupported)

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
	sizeA, sizeB int64

	ord    int64
	status Status // resolved in finalizeLeaf, once decisions can be applied
	source string // Move/Copy source, from the matcher
}

// rawStatus is the leaf's status before move/copy matching - stage 5's
// fileStatus, reached from the two sides alone.
func (l *streamLeaf) rawStatus() Status {
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

	ord int64 // pre-order ordinal; see streamEngine.ord

	dirA, dirB bool
	acc        rollupAccum
	stats      DiffResult

	// twin is the file aspect of this directory's own path, when the same path
	// is a file on one side and a directory on the other.
	twin       *streamLeaf
	twinStatus Status

	// matched records a Move/Copy the matcher established for this directory by
	// content. Like Node.matched it overrides the rollup rather than being
	// derived from it: the match is a fact about the data.
	matched     bool
	matchedStat Status
	source      string

	// demoting is set when this frame, or an ancestor, is a move source the
	// output failed to mention - see external.go's fixed point. Every
	// MovedSource beneath it reverts to Removed.
	demoting bool

	// candOff is where this frame's subtree started writing move-source
	// candidate records, so that a frame which turns out to be a move source
	// itself can replace all of them with one.
	candOff int64

	// Running directory digests. computeMerkleHashes sorts its entries; a
	// stream cannot, so it relies on children arriving in the order that sort
	// would have produced - name order, host before twin - which is exactly DFS
	// order. That is what makes a fixed-size running hash equivalent to
	// digestEntries over a materialised child list.
	digA, digB hash.Hash
	nA, nB     int

	digestA, digestB hashVal

	// pendingLeaf is the last leaf added here, still unfinalised because the
	// very next record may reveal it to be a directory. DFS-key order is what
	// makes one slot enough: the claiming record, if any, is adjacent.
	pendingLeaf *streamLeaf

	// out is this subtree's output so far, already budget-capped. trunc holds
	// the summary standing in for everything past the budget.
	out      []DiffResult
	trunc    DiffResult
	hasTrunc bool
}

type streamEngine struct {
	opts  Options
	stack []*streamFrame

	prevKey string
	started bool
	err     error
	prog    progressReporter

	// ord numbers every node in pre-order - directory, then its file twin, then
	// its children in name order - which is exactly the order detectMovesCopies
	// indexes and matches in. Carrying that order into the external matcher is
	// what makes it attribute a move to the same source the tree engine would.
	ord        int64
	rootStatus Status

	// The Phase 3 hooks. All nil on the Phase 2 path, which is why a
	// --no-moves --no-copies diff still costs one pass and no temp files.
	nodes      func(nodeView) error // pass 1: every node, for the matcher
	decisions  *decisionCursor      // pass 2: what the matcher decided
	demotions  *ordinalCursor       // pass 2: move sources to reinstate
	candidates *spillFile           // pass 2: move sources found, for the next round
	needDigest bool

	scratch, frame []byte // reused encoding buffers for candidate records

	// High-water marks of what the engine retains, so the O(depth x budget)
	// bound can be asserted structurally rather than by sampling the heap -
	// which is both noisy and, done at any useful rate, slower than the work
	// being measured. Two ints; not worth gating behind a build tag.
	peakFrames int
	peakLines  int
}

// nodeView is everything the external matcher needs to know about one node.
// It is deliberately a value: nothing about a node outlives the frame it came
// from.
type nodeView struct {
	ord          int64
	isFile       bool
	status       Status
	hashA, hashB hashVal
	sizeA, sizeB int64
	presentInA   bool
	path         string
}

// newStreamEngine builds a walker. needDigest turns on the running directory
// digests, which only the matcher's pass consumes; it is a constructor argument
// rather than a field the caller sets afterwards because the root frame's own
// digest is one of the things the matcher needs.
func newStreamEngine(opts Options, needDigest bool) *streamEngine {
	e := &streamEngine{opts: opts, ord: 1, needDigest: needDigest}
	e.prog.onProgress = opts.OnProgress
	e.stack = []*streamFrame{e.newFrame("", "", 0)}
	return e
}

func (e *streamEngine) newFrame(name, path string, ord int64) *streamFrame {
	f := &streamFrame{name: name, path: path, ord: ord, acc: newRollupAccum()}
	if e.needDigest {
		f.digA, f.digB = sha1.New(), sha1.New()
	}
	return f
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
		if f.hasTrunc {
			lines++
		}
	}
	if lines > e.peakLines {
		e.peakLines = lines
	}
}

// streamCompare is the streaming counterpart to compareSnapshotsWith. It
// produces byte-identical output to the tree engine; streaming_test.go asserts
// that against the same corpus property_test.go uses.
func streamCompare(iterA, iterB FileIterator, opts Options) ([]DiffResult, error) {
	if !opts.NoMoves || !opts.NoCopies {
		// Move/copy detection needs a global, content-keyed view of both
		// snapshots, which one pass cannot have. externalCompare drives this
		// same walk several times with a matcher in between.
		return externalCompare(iterA, iterB, opts)
	}

	e := newStreamEngine(opts, false)
	if err := e.run(iterA, iterB); err != nil {
		return nil, err
	}
	return e.results(), nil
}

// run walks both streams once and leaves the answer in the root frame.
func (e *streamEngine) run(iterA, iterB FileIterator) error {
	// The root has ordinal 0 and can itself be a suppressed move source: a diff
	// whose every file moved away rolls it up to MovedSource, and no line can
	// ever name the root, so the whole thing reverts to Removed.
	e.stack[0].demoting = e.demoted(0)

	err := mergeJoinStreamsBy(iterA, iterB, dfsKey, func(path string, a, b *models.FileRecord) error {
		return e.add(path, a, b)
	})
	if err != nil {
		return err
	}

	// Close everything still open, innermost first.
	for len(e.stack) > 1 && e.err == nil {
		e.closeTop()
	}
	if e.err != nil {
		return e.err
	}
	root := e.stack[0]
	e.finalizeLeaf(root)
	if e.err != nil {
		return e.err
	}

	// The root's own rollup never prints - collectNode recurses straight past
	// it - but stage 8 asks about it: a diff whose every file moved away rolls
	// the root itself up to MovedSource, and the root's path is named by
	// nothing, so the whole thing reverts to Removed.
	e.prog.finish()
	e.rootStatus, _, _ = decideRollup(root.acc, root.dirA)

	// The root is a node like any other to the matcher: it carries a directory
	// digest, it can be indexed as a copy source, and a tree whose whole content
	// sits one level down can legitimately be reported as a copy of it. Leaving
	// it out cost exactly that - a "Copy c/ <- /" the stream reported as a plain
	// move of the file underneath instead (seed 6084).
	root.digestA, root.digestB = root.digest()
	if err := e.emitNode(nodeView{
		ord: 0, isFile: false, status: e.rootStatus,
		hashA: root.digestA, hashB: root.digestB,
		presentInA: root.dirA, path: "",
	}); err != nil {
		return err
	}

	if e.candidates != nil && e.rootStatus == StatusMovedSource {
		if err := e.candidates.truncate(0); err != nil {
			return err
		}
		if err := e.writeCandidate(0, ""); err != nil {
			return err
		}
	}
	return e.err
}

// results renders the root frame's accumulated output.
func (e *streamEngine) results() []DiffResult {
	root := e.stack[0]
	results := root.lines(e.opts)
	if e.opts.ShowUnchanged {
		stats := root.stats
		if e.rootStatus == StatusUnchanged {
			// accumulateStats counts a directory whose status is Unchanged as
			// one unchanged directory, and it is called on the root itself, so
			// a diff with nothing to report still says "1 directory unchanged".
			// The frame never counts itself - closeTop does that, and the root
			// never closes - so it is added here.
			stats.UnchangedDirCount++
		}
		if stats.UnchangedFileCount > 0 || stats.UnchangedDirCount > 0 {
			results = append(results, DiffResult{
				Path:               ".",
				Status:             StatusMixed,
				UnchangedFileCount: stats.UnchangedFileCount,
				UnchangedDirCount:  stats.UnchangedDirCount,
			})
		}
	}
	return results
}

// add folds one merged record into the stack.
func (e *streamEngine) add(path string, a, b *models.FileRecord) error {
	if e.err != nil {
		return e.err
	}

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
		leaf.sizeA = a.SizeBytes
	}
	if b != nil {
		leaf.inB = true
		leaf.hashB = compactHash(hashOfRecord(*b, e.opts.HashType))
		leaf.sizeB = b.SizeBytes
	}
	top.pendingLeaf = leaf

	// Both sides of a shared path arrive as one record, so the count does not
	// land on tidy multiples; progressReporter throttles on the difference.
	if a != nil && b != nil {
		e.prog.add(2)
	} else {
		e.prog.add(1)
	}
	return e.err
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
	f := e.newFrame(name, parent.path+"/"+name, e.nextOrd())
	f.twin = twin
	f.demoting = parent.demoting || e.demoted(f.ord)
	if dec := e.decisionFor(f.ord); dec.status == StatusMove || dec.status == StatusCopy {
		f.matched, f.matchedStat, f.source = true, dec.status, dec.source
	}
	if e.candidates != nil {
		f.candOff = e.candidates.size
	}

	if twin != nil {
		twin.ord = e.nextOrd()
		e.resolveLeaf(twin, f.demoting)
		f.twinStatus = twin.status
		if twin.status == StatusMovedSource {
			e.setErr(e.writeCandidate(twin.ord, f.path))
		}
	}

	e.stack = append(e.stack, f)
	e.observeRetention()
}

func (e *streamEngine) nextOrd() int64 {
	o := e.ord
	e.ord++
	return o
}

// resolveLeaf settles a leaf's final status: its own two sides, then whatever
// the matcher decided about it, then whether the fixed point has reinstated it.
func (e *streamEngine) resolveLeaf(l *streamLeaf, demoting bool) {
	l.status = l.rawStatus()
	dec := e.decisionFor(l.ord)
	switch dec.status {
	case StatusMove, StatusCopy:
		l.status, l.source = dec.status, dec.source
	case StatusMovedSource:
		l.status = StatusMovedSource
	}
	if l.status == StatusMovedSource && (demoting || e.demoted(l.ord)) {
		// Nothing in the output named this source, so the move it belongs to
		// tells the reader nothing about it. Report the loss.
		l.status = StatusRemoved
	}
}

// finalizeLeaf folds a frame's pending leaf in, now that nothing can claim it as
// a directory.
func (e *streamEngine) finalizeLeaf(f *streamFrame) {
	leaf := f.pendingLeaf
	if leaf == nil {
		return
	}
	f.pendingLeaf = nil

	leaf.ord = e.nextOrd()
	e.resolveLeaf(leaf, f.demoting)
	status := leaf.status

	f.acc.observe(status, status == StatusUnchanged, leaf.source)
	stats := statsOf(status)
	addStats(&f.stats, stats)
	if leaf.inA {
		f.dirA = true
	}
	if leaf.inB {
		f.dirB = true
	}
	f.addDigestEntry(leaf.name, false, leaf.hashA, leaf.hashB)

	path := f.path + "/" + leaf.name
	e.setErr(e.emitNode(nodeView{
		ord: leaf.ord, isFile: true, status: leaf.rawStatus(),
		hashA: leaf.hashA, hashB: leaf.hashB, sizeA: leaf.sizeA, sizeB: leaf.sizeB,
		presentInA: leaf.inA, path: path,
	}))

	if status == StatusMovedSource {
		e.setErr(e.writeCandidate(leaf.ord, path))
		return // reports nothing of its own
	}
	if status == StatusUnchanged {
		return
	}

	line := DiffResult{Path: path, Status: status, SourcePath: leaf.source}
	addStats(&line, stats)
	// A leaf line never carries unchanged counts, even with --show-unchanged:
	// an unchanged leaf produces no line at all.
	line.UnchangedFileCount, line.UnchangedDirCount = 0, 0
	f.appendOut(e.opts, line)
}

// closeTop finalises the innermost frame and folds its verdict into its parent.
func (e *streamEngine) closeTop() {
	f := e.stack[len(e.stack)-1]
	e.stack = e.stack[:len(e.stack)-1]
	parent := e.stack[len(e.stack)-1]

	e.finalizeLeaf(f)

	f.digestA, f.digestB = f.digest()

	dirStatus, sourcePath, hasUnchanged := decideRollup(f.acc, f.dirA)
	if f.matched {
		// A Move/Copy established by content matching is a fact about the data,
		// not an inference from children, and propagateNodeStatus keeps it
		// as-is - including its "no unchanged content beneath" answer.
		dirStatus, sourcePath, hasUnchanged = f.matchedStat, f.source, false
	}

	// Reconcile with the file aspect, exactly as propagateStatus does: an
	// unchanged twin only contributes "there is unchanged content here", while a
	// twin whose fate differs from the directory's forces Mixed - a single
	// summary status would necessarily hide one of the two halves.
	finalStatus := dirStatus
	if f.twin != nil {
		if f.matched {
			// The one tree-engine behaviour a stream cannot reproduce. A matched
			// node freezes its subtree's statuses at stage 5 (propagateStatus
			// returns without recursing into it), which is invisible while the
			// node collapses to one line - but a twin that disagrees forces
			// Mixed, and the collector then prints those frozen children. A
			// stream has already rolled them up with the move statuses included.
			//
			// It needs a path that is a file *and* a directory within the same
			// snapshot, which no filesystem produces and the scanner cannot
			// record; 200,000 generated tree pairs contain none. Refusing costs
			// a fall back to the tree engine on input that should not exist,
			// where guessing would cost a wrong answer.
			e.setErr(errStreamMatchedTwin)
			return
		}
		if f.twinStatus == StatusUnchanged {
			hasUnchanged = true
		} else if f.twinStatus != dirStatus {
			finalStatus = StatusMixed
		}
		addStats(&f.stats, statsOf(f.twinStatus))
	}

	if finalStatus == StatusUnchanged {
		f.stats.UnchangedDirCount++
	}

	presentInA := f.dirA || (f.twin != nil && f.twin.inA)
	e.setErr(e.emitNode(nodeView{
		ord: f.ord, isFile: false, status: finalStatus,
		hashA: f.digestA, hashB: f.digestB,
		presentInA: presentInA, path: f.path,
	}))
	if f.twin != nil {
		e.setErr(e.emitNode(nodeView{
			ord: f.twin.ord, isFile: true, status: f.twin.rawStatus(),
			hashA: f.twin.hashA, hashB: f.twin.hashB,
			sizeA: f.twin.sizeA, sizeB: f.twin.sizeB,
			presentInA: f.twin.inA, path: f.path,
		}))
	}

	// Counts and presence reach the parent however this directory is reported.
	addStats(&parent.stats, f.stats)
	if presentInA {
		parent.dirA = true
	}
	if f.dirB || (f.twin != nil && f.twin.inB) {
		parent.dirB = true
	}
	parent.addDigestEntry(f.name, false, f.digestA, f.digestB)
	if f.twin != nil {
		parent.addDigestEntry(f.name, true, f.twin.hashA, f.twin.hashB)
	}

	if finalStatus == StatusMovedSource {
		// The whole subtree moved away, so the single record for this directory
		// replaces every candidate record beneath it: stage 8 asks about the
		// highest suppressed source, not each leaf.
		if e.candidates != nil {
			e.setErr(e.candidates.truncate(f.candOff))
			e.setErr(e.writeCandidate(f.ord, f.path))
		}
	}

	var lines []DiffResult
	switch finalStatus {
	case StatusUnchanged, StatusMovedSource:
		// Reports nothing itself.

	case StatusMixed:
		lines = f.lines(e.opts)
		if e.opts.ShowUnchanged && (f.stats.UnchangedFileCount > 0 || f.stats.UnchangedDirCount > 0) {
			// Post-order context row, after the children it contextualises, and
			// deliberately outside the budget applied to them.
			lines = append(lines, DiffResult{
				Path:               f.path + "/",
				Status:             StatusMixed,
				SourcePath:         sourcePath,
				UnchangedFileCount: f.stats.UnchangedFileCount,
				UnchangedDirCount:  f.stats.UnchangedDirCount,
			})
		}

	default:
		// Collapses: one line standing in for the whole subtree.
		line := DiffResult{
			Path:          f.path + "/",
			Status:        finalStatus,
			SourcePath:    sourcePath,
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
	if f.twin != nil && f.twinStatus != StatusUnchanged && f.twinStatus != StatusMovedSource {
		twinLine := DiffResult{Path: f.path, Status: f.twinStatus, SourcePath: f.twin.source}
		addStats(&twinLine, statsOf(f.twinStatus))
		twinLine.UnchangedFileCount, twinLine.UnchangedDirCount = 0, 0
		if f.twinStatus == StatusRemoved {
			lines = append([]DiffResult{twinLine}, lines...)
		} else {
			lines = append(lines, twinLine)
		}
	}

	parent.acc.observe(finalStatus, hasUnchanged, sourcePath)
	parent.appendOut(e.opts, lines...)
	e.observeRetention()
}

func (e *streamEngine) setErr(err error) {
	if err != nil && e.err == nil {
		e.err = err
	}
}

func (e *streamEngine) emitNode(v nodeView) error {
	if e.nodes == nil {
		return nil
	}
	return e.nodes(v)
}

// appendOut adds lines to a directory's pending output, folding everything past
// the budget into one Truncated summary as it goes rather than after the fact.
//
// Mirrors collector.applyBudget: keep the first N lines and replace the rest
// with one summary carrying their combined counts, so nothing is dropped
// silently. Doing it incrementally is what bounds a frame's retained output to
// O(budget) even for a directory with a million changed children, and with it
// the whole engine to O(depth x budget).
//
// MaxLinesPerDir 0 means unlimited, which is unbounded by construction - the one
// place the line budget stops being a readability feature and becomes the memory
// bound (diff-memory.md, "Fact 1").
func (f *streamFrame) appendOut(opts Options, lines ...DiffResult) {
	// The virtual root is exempt, in both engines. The budget exists to stop one
	// directory drowning the output, not to cap the run - and since a truncated
	// directory always emits budget+1 lines, a capped root would immediately
	// re-truncate the summary it had just produced, collapsing the entire diff
	// into a single line.
	unlimited := opts.MaxLinesPerDir <= 0 || f.name == ""
	for _, line := range lines {
		if unlimited || len(f.out) < opts.MaxLinesPerDir {
			f.out = append(f.out, line)
			continue
		}
		addStats(&f.trunc, line)
		// A nested summary already stands in for lines of its own; this one
		// stands in for those too.
		f.trunc.HiddenCount += line.HiddenCount + 1
		f.hasTrunc = true
	}
}

// lines is what this directory's children produced, with the truncation summary
// appended if there was one.
func (f *streamFrame) lines(opts Options) []DiffResult {
	if !f.hasTrunc {
		return f.out
	}
	sum := f.trunc
	sum.Path = f.path + "/"
	sum.Status = StatusTruncated
	return append(f.out, sum)
}

// addDigestEntry folds one child's contribution into this directory's running
// digests, matching digestEntries' length-prefixed encoding byte for byte.
func (f *streamFrame) addDigestEntry(name string, twin bool, hA, hB hashVal) {
	if f.digA == nil {
		return
	}
	if !hA.empty() {
		writeDigestEntry(f.digA, name, twin, hA)
		f.nA++
	}
	if !hB.empty() {
		writeDigestEntry(f.digB, name, twin, hB)
		f.nB++
	}
}

func writeDigestEntry(h hash.Hash, name string, twin bool, hv hashVal) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(name)))
	h.Write(lenBuf[:])
	h.Write([]byte(name))
	if twin {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	b := hv.bytes()
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
	h.Write(lenBuf[:])
	h.Write(b)
}

// digest closes the running hashes. An empty entry set yields no hash at all,
// not a hash of zero entries - "empty" and "absent" are tracked separately, by
// DirA/DirB.
func (f *streamFrame) digest() (hashVal, hashVal) {
	if f.digA == nil {
		return hashVal{}, hashVal{}
	}
	var a, b hashVal
	if f.nA > 0 {
		a.n = uint8(copy(a.b[:], f.digA.Sum(nil)))
	}
	if f.nB > 0 {
		b.n = uint8(copy(b.b[:], f.digB.Sum(nil)))
	}
	return a, b
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
