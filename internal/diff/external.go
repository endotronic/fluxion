package diff

import (
	"encoding/binary"
)

// Phase 3 of knowledge/diff-memory.md: move/copy matching without a tree.
//
// Stage 6 (detectMovesCopies, knowledge/diff-algo.md) is the one stage that
// genuinely needs a global view - a hash-keyed index over every node in both
// snapshots - and it is the only thing that kept the streaming engine restricted
// to --no-moves --no-copies. It is replaced here by an external sort:
//
//  1. Pass 1 walks the stream and writes one fixed-shape record per node per
//     role: a *source* record keyed on the node's A-side hash for every node the
//     tree engine would have indexed, and a *target* record keyed on its B-side
//     hash for every node it would have tried to match. Directory digests are
//     computed on the way past (streamFrame.digest), so a whole directory can
//     still match as one move.
//
//  2. Sorting those by (hash, ordinal) puts each content group together with its
//     members in pre-order - which is the order detectMovesCopies indexed and
//     matched in, and therefore the order that decides which source a move is
//     attributed to. Each group is then scanned with four independent cursors,
//     one per map the tree engine kept (removed, modified, existing) plus one
//     for the targets. A cursor only ever moves forwards, so a group of ten
//     million identical empty directories costs a linear scan and no memory,
//     where the tree engine's map held every one of them.
//
//  3. The decisions - "node 4,201 is a Move whose source is /old/x", "node 900 is
//     that source" - are sorted back into ordinal order and merge-joined into a
//     second walk, which produces the output.
//
// Stage 8's fixed point (hidden move sources) rides on the same machinery, since
// it is the same shape of problem: collect a trial output, ask it which sources
// it failed to mention, demote those, and walk again. The tree engine mutates
// and re-collects; here each round is another walk with a larger demotion set
// merge-joined in. The set is cumulative, exactly as the tree's mutations are:
// a source demoted in one round emits a Removed line, which would make the next
// round think it had been mentioned all along and put it back.

const (
	// A hash record's key is the 21-byte hash followed by the node's ordinal, so
	// byte order groups by content and orders each group by pre-order position.
	hashKeyLen = 21 + 8
	ordKeyLen  = 8

	roleSource = byte(0)
	roleTarget = byte(1)
)

// decision is what the matcher concluded about one node.
type decision struct {
	status Status // StatusMove, StatusCopy, StatusMovedSource, or zero for none
	source string
}

func externalCompare(iterA, iterB FileIterator, opts Options) ([]DiffResult, error) {
	return (&externalDiff{opts: opts}).compare(iterA, iterB)
}

// externalDiff is the multi-pass driver. It exists as a struct so its cost is
// observable: what the passes retain and how many of them stage 8 needed are
// the two claims Phase 3 makes, and a test can read them off here rather than
// sampling the heap.
type externalDiff struct {
	opts Options

	passes     int
	peakFrames int
	peakLines  int
}

func (x *externalDiff) note(e *streamEngine) {
	x.passes++
	if e.peakFrames > x.peakFrames {
		x.peakFrames = e.peakFrames
	}
	if e.peakLines > x.peakLines {
		x.peakLines = e.peakLines
	}
}

func (x *externalDiff) compare(iterA, iterB FileIterator) ([]DiffResult, error) {
	opts := x.opts
	decisions, err := x.matchExternally(iterA, iterB)
	if err != nil {
		return nil, err
	}
	defer decisions.close()

	// Stage 8. Round 0 suppresses every move source, as the tree engine's first
	// trial collection does; each round after it reinstates the ones the
	// previous round's output turned out not to mention.
	var demotions *spillFile
	defer func() {
		if demotions != nil {
			demotions.close()
		}
	}()

	for round := 0; ; round++ {
		e := newStreamEngine(opts, false)
		e.decisions = newDecisionCursor(decisions)
		if demotions != nil {
			e.demotions = newOrdinalCursor(demotions)
		}
		candidates := newSpill(opts.TempDir)
		e.candidates = candidates

		if err := e.run(iterA, iterB); err != nil {
			candidates.close()
			return nil, err
		}
		x.note(e)
		results := e.results()

		// The bound is a guard, not an expected limit: each round only ever
		// turns MovedSource into Removed, so the set grows monotonically and
		// converges in one or two rounds in practice.
		if round >= 32 {
			candidates.close()
			return results, nil
		}

		reinstated, n, err := unnamedSources(candidates, accountedPaths(results), opts.TempDir)
		candidates.close()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			reinstated.close()
			return results, nil
		}

		merged, err := mergeOrdinals(demotions, reinstated, opts.TempDir)
		reinstated.close()
		if err != nil {
			return nil, err
		}
		if demotions != nil {
			demotions.close()
		}
		demotions = merged
	}
}

// matchExternally runs pass 1 and the group scan, returning the decisions in
// ordinal order. The caller owns the result.
func (x *externalDiff) matchExternally(iterA, iterB FileIterator) (*spillFile, error) {
	opts := x.opts
	sorter := newExtSorter(opts.TempDir, hashKeyLen)

	e := newStreamEngine(opts, true)
	var scratch []byte
	e.nodes = func(v nodeView) error {
		var err error
		scratch, err = writeHashRecords(sorter, scratch, v, opts)
		return err
	}
	if err := e.run(iterA, iterB); err != nil {
		return nil, err
	}
	x.note(e)

	sorted, err := sorter.finish()
	if err != nil {
		return nil, err
	}
	defer sorted.close()

	return matchGroups(sorted, opts)
}

// writeHashRecords emits a node's source and target records - the two halves of
// detectMovesCopies' index and match passes, with their guards applied here so
// that a node the tree engine would never have indexed never reaches the sort.
func writeHashRecords(s *extSorter, scratch []byte, v nodeView, opts Options) ([]byte, error) {
	// Index side. index() prunes any subtree not present in A, and add() skips
	// an empty hash and every zero-byte file - one hash shared by every empty
	// file would otherwise make each of them a move of all the others. An Added
	// node goes into no map at all.
	if v.presentInA && v.status != StatusAdded && !v.hashA.empty() && !(v.isFile && v.sizeA == 0) {
		// With copies off, the existing-node map is never consulted, so only
		// Removed and Modified nodes can still be the source of anything.
		useful := !opts.NoCopies || v.status == StatusRemoved || v.status == StatusModified
		if useful {
			scratch = buildHashRecord(scratch, v.hashA, v.ord, roleSource, v.status, v.isFile, v.path)
			if err := s.add(scratch); err != nil {
				return scratch, err
			}
		}
	}

	// Match side. A directory that already held something in A is refused:
	// "this directory is a copy of that one" describes only what it holds now,
	// and the node then stops being recomputed, so whatever it used to hold and
	// has since lost would never be reported.
	if (v.status == StatusAdded || v.status == StatusModified) &&
		!v.hashB.empty() && !(v.isFile && v.sizeB == 0) &&
		!(!v.isFile && v.presentInA) {
		scratch = buildHashRecord(scratch, v.hashB, v.ord, roleTarget, v.status, v.isFile, "")
		if err := s.add(scratch); err != nil {
			return scratch, err
		}
	}
	return scratch, nil
}

// A hash record: hash(21) ordinal(8) role(1) status(1) isFile(1) path.
func buildHashRecord(dst []byte, h hashVal, ord int64, role byte, status Status, isFile bool, path string) []byte {
	dst = dst[:0]
	dst = append(dst, h.b[:]...)
	dst = append(dst, h.n)
	dst = appendOrd(dst, ord)
	dst = append(dst, role, byte(status), boolByte(isFile))
	return append(dst, path...)
}

type hashRecord struct {
	ord    int64
	role   byte
	status Status
	isFile bool
	path   string
}

func parseHashRecord(rec []byte) hashRecord {
	return hashRecord{
		ord:    int64(binary.BigEndian.Uint64(rec[21:29])),
		role:   rec[29],
		status: Status(rec[30]),
		isFile: rec[31] == 1,
		path:   string(rec[32:]),
	}
}

// matchGroups scans the sorted records one content group at a time and writes
// the decisions, sorted by ordinal. The caller owns the result.
func matchGroups(sorted *spillFile, opts Options) (*spillFile, error) {
	out := newExtSorter(opts.TempDir, ordKeyLen)

	main := newRecReader(sorted, 0, sorted.size)
	var cur [21]byte
	start, have := int64(0), false

	for {
		off := main.offset()
		rec, err := main.next()
		if err != nil {
			return nil, err
		}
		if rec == nil {
			if have {
				if err := matchGroup(sorted, start, off, out, opts); err != nil {
					return nil, err
				}
			}
			break
		}
		var h [21]byte
		copy(h[:], rec[:21])
		switch {
		case !have:
			cur, start, have = h, off, true
		case h != cur:
			if err := matchGroup(sorted, start, off, out, opts); err != nil {
				return nil, err
			}
			cur, start = h, off
		}
	}
	return out.finish()
}

// roleCursor hands out the members of one content group that belong to one of
// detectMovesCopies' three maps, in pre-order, consuming each at most once.
// Where the tree engine held a slice per hash and popped its head, this walks
// forwards through the group's byte range - so the pathological group (every
// zero-byte-file directory shares a digest) costs a scan rather than a map
// holding every member.
type roleCursor struct {
	r    *recReader
	want func(hashRecord) bool
	err  error
	done bool
}

func newRoleCursor(s *spillFile, start, end int64, want func(hashRecord) bool) *roleCursor {
	return &roleCursor{r: newRecReader(s, start, end), want: want}
}

// take returns the next matching record, or nil once the group is exhausted.
func (c *roleCursor) take() *hashRecord {
	if c.done || c.err != nil {
		return nil
	}
	for {
		rec, err := c.r.next()
		if err != nil {
			c.err, c.done = err, true
			return nil
		}
		if rec == nil {
			c.done = true
			return nil
		}
		h := parseHashRecord(rec)
		if c.want(h) {
			return &h
		}
	}
}

func matchGroup(s *spillFile, start, end int64, out *extSorter, opts Options) error {
	removed := newRoleCursor(s, start, end, func(h hashRecord) bool {
		return h.role == roleSource && h.status == StatusRemoved
	})
	modified := newRoleCursor(s, start, end, func(h hashRecord) bool {
		return h.role == roleSource && h.status == StatusModified
	})
	// Every source record that reached the sort is a non-Added node, which is
	// exactly the tree engine's existingMap membership test.
	existing := newRoleCursor(s, start, end, func(h hashRecord) bool {
		return h.role == roleSource
	})

	targets := newRecReader(s, start, end)
	var scratch []byte
	for {
		rec, err := targets.next()
		if err != nil {
			return err
		}
		if rec == nil {
			break
		}
		t := parseHashRecord(rec)
		if t.role != roleTarget {
			continue
		}

		sourcePath := func(src *hashRecord) string {
			p := src.path
			if !t.isFile && !hasTrailingSlash(p) {
				p += "/"
			}
			return p
		}

		matched := false
		if !opts.NoMoves {
			// Removed first: the strongest match, and consumed, so N removals
			// can absorb at most N moves.
			if src := removed.take(); src != nil {
				scratch = appendDecision(scratch[:0], t.ord, StatusMove, sourcePath(src))
				if err := out.add(scratch); err != nil {
					return err
				}
				// Only a Removed node ever reaches this map, so the source is
				// always suppressed - the tree engine's src.Status check can
				// never fail here.
				scratch = appendDecision(scratch[:0], src.ord, StatusMovedSource, "")
				if err := out.add(scratch); err != nil {
					return err
				}
				matched = true
			} else if t.status == StatusModified {
				// File swap / rewrite: the target is Modified and a Modified
				// node elsewhere holds the content that used to be here. Swaps
				// are 1:1, so the source is consumed and not suppressed.
				if src := modified.take(); src != nil {
					scratch = appendDecision(scratch[:0], t.ord, StatusMove, sourcePath(src))
					if err := out.add(scratch); err != nil {
						return err
					}
					matched = true
				}
			}
		}

		if !matched && !opts.NoCopies {
			// Prefer a modified source over an unchanged one. Copies are not
			// consumed: one source can father many copies.
			src := modified.take()
			if src == nil {
				src = existing.take()
			}
			if src != nil {
				scratch = appendDecision(scratch[:0], t.ord, StatusCopy, sourcePath(src))
				if err := out.add(scratch); err != nil {
					return err
				}
			}
		}
	}

	for _, c := range []*roleCursor{removed, modified, existing} {
		if c.err != nil {
			return c.err
		}
	}
	return nil
}

// A decision record: ordinal(8) status(1) source path.
func appendDecision(dst []byte, ord int64, status Status, source string) []byte {
	dst = appendOrd(dst, ord)
	dst = append(dst, byte(status))
	return append(dst, source...)
}

// decisionCursor merge-joins the matcher's verdicts into a walk. Both sides are
// in ordinal order, so one forward pass serves every node.
type decisionCursor struct {
	r    *recReader
	ord  int64
	dec  decision
	ok   bool
	err  error
	done bool
}

func newDecisionCursor(s *spillFile) *decisionCursor {
	c := &decisionCursor{r: newRecReader(s, 0, s.size)}
	c.advance()
	return c
}

func (c *decisionCursor) advance() {
	rec, err := c.r.next()
	if err != nil {
		c.err, c.ok, c.done = err, false, true
		return
	}
	if rec == nil {
		c.ok, c.done = false, true
		return
	}
	c.ord = int64(binary.BigEndian.Uint64(rec[:8]))
	c.dec = decision{status: Status(rec[8]), source: string(rec[9:])}
	c.ok = true
}

// at returns the decision for ord, if there is one, consuming it.
func (c *decisionCursor) at(ord int64) decision {
	for c.ok && c.ord < ord {
		c.advance()
	}
	if c.ok && c.ord == ord {
		d := c.dec
		c.advance()
		return d
	}
	return decision{}
}

// ordinalCursor is the same merge join over a bare list of ordinals.
type ordinalCursor struct {
	r   *recReader
	ord int64
	ok  bool
	err error
}

func newOrdinalCursor(s *spillFile) *ordinalCursor {
	c := &ordinalCursor{r: newRecReader(s, 0, s.size)}
	c.advance()
	return c
}

func (c *ordinalCursor) advance() {
	rec, err := c.r.next()
	if err != nil {
		c.err, c.ok = err, false
		return
	}
	if rec == nil {
		c.ok = false
		return
	}
	c.ord, c.ok = int64(binary.BigEndian.Uint64(rec[:8])), true
}

func (c *ordinalCursor) at(ord int64) bool {
	for c.ok && c.ord < ord {
		c.advance()
	}
	if c.ok && c.ord == ord {
		c.advance()
		return true
	}
	return false
}

// unnamedSources reads the move sources this round suppressed and returns those
// the output never mentioned - the ones stage 8 has to reinstate as plain
// removals, because a file that is gone from B currently reads as untouched.
//
// The candidates arrive in ordinal order already: they are written during the
// walk, and a directory that turns out to be a suppressed source itself replaces
// its subtree's records with its own, which is a truncation back to where the
// subtree started.
func unnamedSources(candidates *spillFile, named map[string]bool, tmpDir string) (*spillFile, int, error) {
	if err := candidates.flush(); err != nil {
		return nil, 0, err
	}
	out := newSpill(tmpDir)
	r := newRecReader(candidates, 0, candidates.size)
	n := 0
	var scratch []byte
	for {
		rec, err := r.next()
		if err != nil {
			out.close()
			return nil, 0, err
		}
		if rec == nil {
			break
		}
		ord := int64(binary.BigEndian.Uint64(rec[:8]))
		if sourceNamed(named, string(rec[8:])) {
			continue
		}
		scratch = appendRecord(scratch[:0], appendOrd(nil, ord))
		if _, err := out.Write(scratch); err != nil {
			out.close()
			return nil, 0, err
		}
		n++
	}
	if err := out.flush(); err != nil {
		out.close()
		return nil, 0, err
	}
	return out, n, nil
}

// mergeOrdinals unions two ordinal-ordered lists. The demotion set is
// cumulative, so this is how a round's findings are added to it.
func mergeOrdinals(a, b *spillFile, tmpDir string) (*spillFile, error) {
	out := newSpill(tmpDir)
	var ca, cb *ordinalCursor
	if a != nil {
		ca = newOrdinalCursor(a)
	}
	if b != nil {
		cb = newOrdinalCursor(b)
	}
	var scratch []byte
	emit := func(ord int64) error {
		scratch = appendRecord(scratch[:0], appendOrd(nil, ord))
		_, err := out.Write(scratch)
		return err
	}
	for {
		aOK := ca != nil && ca.ok
		bOK := cb != nil && cb.ok
		switch {
		case aOK && bOK && ca.ord == cb.ord:
			if err := emit(ca.ord); err != nil {
				out.close()
				return nil, err
			}
			ca.advance()
			cb.advance()
		case aOK && (!bOK || ca.ord < cb.ord):
			if err := emit(ca.ord); err != nil {
				out.close()
				return nil, err
			}
			ca.advance()
		case bOK:
			if err := emit(cb.ord); err != nil {
				out.close()
				return nil, err
			}
			cb.advance()
		default:
			for _, c := range []*ordinalCursor{ca, cb} {
				if c != nil && c.err != nil {
					out.close()
					return nil, c.err
				}
			}
			return out, out.flush()
		}
	}
}

// The streaming engine's side of the hooks above.

func (e *streamEngine) decisionFor(ord int64) decision {
	if e.decisions == nil {
		return decision{}
	}
	return e.decisions.at(ord)
}

func (e *streamEngine) demoted(ord int64) bool {
	if e.demotions == nil {
		return false
	}
	return e.demotions.at(ord)
}

func (e *streamEngine) writeCandidate(ord int64, path string) error {
	if e.candidates == nil {
		return nil
	}
	e.scratch = appendOrd(e.scratch[:0], ord)
	e.scratch = append(e.scratch, path...)
	e.frame = appendRecord(e.frame[:0], e.scratch)
	_, err := e.candidates.Write(e.frame)
	return err
}

func appendOrd(dst []byte, ord int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(ord))
	return append(dst, b[:]...)
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func hasTrailingSlash(s string) bool { return len(s) > 0 && s[len(s)-1] == '/' }
