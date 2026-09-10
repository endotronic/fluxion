package diff

import (
	"container/heap"
	"sort"
)

// External sort for the intermediate record streams of Phase 3
// (knowledge/diff-memory.md). Records are sorted on a fixed-width key that is
// the first keyLen bytes of the payload, so comparison is a byte compare and
// needs no decoding.
//
// Keys are constructed to make byte order the order actually wanted: a hash
// record's key is the 21-byte hash followed by the node's 8-byte big-endian
// ordinal, which groups by content and, within a group, restores the pre-order
// traversal that decides which candidate a move is attributed to. Ordinals are
// unique, so no two records ever compare equal and stability is not a question.

// sortMemLimit is how much of a sort is held in memory before a run is written
// out. One sorter is alive at a time, so this is the phase's real memory knob.
var sortMemLimit = 64 << 20

type extSorter struct {
	dir    string
	keyLen int
	meter  *spillMeter

	arena []byte // packed framed records
	offs  []int  // frame offsets into arena
	runs  []*spillFile
	err   error
}

func newExtSorter(dir string, keyLen int, meter *spillMeter) *extSorter {
	return &extSorter{dir: dir, keyLen: keyLen, meter: meter}
}

func (s *extSorter) add(payload []byte) error {
	if s.err != nil {
		return s.err
	}
	need := recHeaderLen + len(payload)
	if len(s.arena)+need > sortMemLimit && len(s.offs) > 0 {
		if err := s.flushRun(); err != nil {
			return err
		}
	}
	s.grow(need)
	s.offs = append(s.offs, len(s.arena))
	s.arena = appendRecord(s.arena, payload)
	return nil
}

// grow makes room for need more bytes without letting append's doubling take
// the arena past the limit. Left to append, a buffer at 40 MiB that needs one
// more record reallocates to 80 MiB - so the knob that says "64 MiB of sort
// buffer" would quietly cost 128 MiB of resident memory, which is exactly the
// kind of factor this phase exists to remove.
func (s *extSorter) grow(need int) {
	if cap(s.arena)-len(s.arena) >= need {
		return
	}
	size := cap(s.arena) * 2
	if size < 64<<10 {
		size = 64 << 10
	}
	if size > sortMemLimit {
		size = sortMemLimit
	}
	if size < len(s.arena)+need {
		size = len(s.arena) + need // one oversized record still has to fit
	}
	grown := make([]byte, len(s.arena), size)
	copy(grown, s.arena)
	s.arena = grown
}

func (s *extSorter) key(off int) []byte {
	return s.arena[off+recHeaderLen : off+recHeaderLen+s.keyLen]
}

func (s *extSorter) sortPending() {
	sort.Slice(s.offs, func(i, j int) bool {
		return string(s.key(s.offs[i])) < string(s.key(s.offs[j]))
	})
}

func (s *extSorter) flushRun() error {
	if len(s.offs) == 0 {
		return nil
	}
	s.sortPending()
	run := newSpill(s.dir, s.meter)
	for _, off := range s.offs {
		n := recHeaderLen + int(leUint32(s.arena[off:]))
		if _, err := run.Write(s.arena[off : off+n]); err != nil {
			run.close()
			return err
		}
	}
	if err := run.flush(); err != nil {
		run.close()
		return err
	}
	s.runs = append(s.runs, run)
	s.arena, s.offs = s.arena[:0], s.offs[:0]
	return nil
}

// finish returns the sorted records as a single log. The caller owns it and
// must close it.
func (s *extSorter) finish() (*spillFile, error) {
	if s.err != nil {
		return nil, s.err
	}
	if len(s.runs) == 0 {
		// Everything fit in memory: sort in place and write one log. This is
		// the path every test-sized diff takes, and it never touches the disk.
		s.sortPending()
		out := newSpill(s.dir, s.meter)
		for _, off := range s.offs {
			n := recHeaderLen + int(leUint32(s.arena[off:]))
			if _, err := out.Write(s.arena[off : off+n]); err != nil {
				out.close()
				return nil, err
			}
		}
		s.arena, s.offs = nil, nil
		return out, out.flush()
	}

	if err := s.flushRun(); err != nil {
		return nil, err
	}
	defer func() {
		for _, r := range s.runs {
			r.close()
		}
		s.runs = nil
	}()
	return s.mergeRuns()
}

func (s *extSorter) mergeRuns() (*spillFile, error) {
	h := &runHeap{keyLen: s.keyLen}
	for _, run := range s.runs {
		c := &runCursor{r: newRecReader(run, 0, run.size)}
		if err := c.advance(); err != nil {
			return nil, err
		}
		if !c.done {
			h.cursors = append(h.cursors, c)
		}
	}
	heap.Init(h)

	out := newSpill(s.dir, s.meter)
	var frame []byte
	for h.Len() > 0 {
		c := h.cursors[0]
		frame = appendRecord(frame[:0], c.cur)
		if _, err := out.Write(frame); err != nil {
			out.close()
			return nil, err
		}
		if err := c.advance(); err != nil {
			out.close()
			return nil, err
		}
		if c.done {
			heap.Pop(h)
		} else {
			heap.Fix(h, 0)
		}
	}
	return out, out.flush()
}

type runCursor struct {
	r    *recReader
	cur  []byte
	done bool
}

func (c *runCursor) advance() error {
	rec, err := c.r.next()
	if err != nil {
		return err
	}
	if rec == nil {
		c.done, c.cur = true, nil
		return nil
	}
	// The reader's slice is only valid until its next call, and the heap holds
	// one record per run across calls, so this copy is load-bearing.
	c.cur = append(c.cur[:0], rec...)
	return nil
}

type runHeap struct {
	cursors []*runCursor
	keyLen  int
}

func (h *runHeap) Len() int { return len(h.cursors) }
func (h *runHeap) Less(i, j int) bool {
	return string(h.cursors[i].cur[:h.keyLen]) < string(h.cursors[j].cur[:h.keyLen])
}
func (h *runHeap) Swap(i, j int) { h.cursors[i], h.cursors[j] = h.cursors[j], h.cursors[i] }
func (h *runHeap) Push(x any)    { h.cursors = append(h.cursors, x.(*runCursor)) }
func (h *runHeap) Pop() any {
	old := h.cursors
	n := len(old)
	last := old[n-1]
	h.cursors = old[:n-1]
	return last
}

func leUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
