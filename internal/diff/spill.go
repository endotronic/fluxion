package diff

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"fluxion/internal/util"
)

// Phase 3 of knowledge/diff-memory.md needs somewhere to put the intermediate
// records - one per node, several times over - that the external move/copy
// matching passes produce. They cannot live in RAM (that is the whole point of
// the phase) and they must not cost a temp file for a diff of eleven files
// either, because the equivalence harness runs hundreds of thousands of tiny
// diffs and a syscall per record would dominate it.
//
// spillFile is therefore an append-only byte log that stays in memory until it
// exceeds a limit and only then moves to disk. Every intermediate in this phase
// is one of these, so "small diff" and "fleet diff" are the same code path with
// different backing.

// spillMemLimit is how much of one intermediate stays in memory before it moves
// to a temp file. There are a handful of these alive at once, so this is a
// per-file figure rather than a budget for the phase.
var spillMemLimit = 8 << 20

// DefaultMinFreeTempBytes is how much room the streaming engine insists on
// leaving on the temp filesystem. On the fleet in knowledge/fleet.md every pool
// sits at 94-96%, where the last gigabyte is not a rounding error - and the
// thing being protected is the filesystem the user still has to work on after
// the diff is abandoned.
const DefaultMinFreeTempBytes = 512 << 20

// errTempSpaceExhausted reports that the temp filesystem ran too low to keep
// going. It is deliberately a hard stop: a partial diff that looks complete is
// the failure knowledge/goals.md ranks worst, and there is no way to finish
// honestly once the intermediates cannot be written.
var errTempSpaceExhausted = errors.New("diff: not enough free space for the diff's temporary files")

// spillMeter tracks how much temp *disk* a diff is using across all its
// intermediates at once - bytes still in memory do not count, because the
// question it answers is "will this fill the filesystem".
//
// It also enforces the answer. An up-front estimate has to be pessimistic
// enough to be useless as a hard gate (see EstimateTempBytes, which runs three
// to five times the observed figure), so the real check is here: statfs the temp
// directory as the intermediates grow, and stop while there is still room to
// stop in. That refuses when the run genuinely would fill the filesystem rather
// than when a guess says it might.
type spillMeter struct {
	live int64
	peak int64

	// Guard state. checkEvery is how many bytes may be written between statfs
	// calls: the check costs a syscall, and one per Write on a fleet-size diff
	// would be millions of them.
	dir        string
	minFree    int64
	checkEvery int64
	sinceCheck int64
	err        error
}

func (m *spillMeter) add(n int64) {
	if m == nil {
		return
	}
	m.live += n
	if m.live > m.peak {
		m.peak = m.live
	}
	m.sinceCheck += n
}

func (m *spillMeter) sub(n int64) {
	if m == nil {
		return
	}
	m.live -= n
}

// tempFreeBytes reports free space on the filesystem holding dir. A variable so
// the guard can be driven from a test without filling a real filesystem.
var tempFreeBytes = func(dir string) (int64, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	n, err := util.GetFSAvail(dir)
	return int64(n), err
}

// check statfs's the temp filesystem if enough has been written since the last
// look, and returns an error once it is too full to continue.
func (m *spillMeter) check() error {
	if m == nil || m.minFree <= 0 {
		return nil
	}
	if m.err != nil {
		return m.err
	}
	if m.sinceCheck < m.checkEvery {
		return nil
	}
	m.sinceCheck = 0

	free, err := tempFreeBytes(m.dir)
	if err != nil {
		// Not being able to ask is not a reason to stop: the run is still
		// correct, and the guard is a courtesy to the filesystem.
		return nil
	}
	if free < m.minFree {
		where := m.dir
		if where == "" {
			where = os.TempDir() + " (the system default)"
		}
		m.err = fmt.Errorf("%w: %s has %s free, below the %s this diff keeps in reserve; "+
			"point --temp-dir at a filesystem with room",
			errTempSpaceExhausted, where, humanBytes(free), humanBytes(m.minFree))
		return m.err
	}
	return nil
}

// humanBytes formats a byte count for an error a person has to act on.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// spillFile is an append-only log of bytes, readable at any offset and
// truncatable back to any offset. Writes go to memory until spillMemLimit is
// exceeded, at which point the whole thing moves to a temp file and stays
// there.
type spillFile struct {
	dir   string
	limit int
	meter *spillMeter

	mem  []byte
	f    *os.File
	w    *bufio.Writer
	size int64

	onDiskBytes int64 // what this file is charging the meter for
}

func newSpill(dir string, meter *spillMeter) *spillFile {
	return &spillFile{dir: dir, limit: spillMemLimit, meter: meter}
}

func (s *spillFile) Write(p []byte) (int, error) {
	if s.f == nil {
		if len(s.mem)+len(p) <= s.limit {
			s.mem = append(s.mem, p...)
			s.size += int64(len(p))
			return len(p), nil
		}
		if err := s.moveToDisk(); err != nil {
			return 0, err
		}
	}
	n, err := s.w.Write(p)
	s.size += int64(n)
	s.charge()
	if err == nil {
		err = s.meter.check()
	}
	return n, err
}

// charge brings the meter in line with what this file now occupies on disk.
func (s *spillFile) charge() {
	if s.f == nil {
		return
	}
	s.meter.add(s.size - s.onDiskBytes)
	s.onDiskBytes = s.size
}

func (s *spillFile) moveToDisk() error {
	f, err := os.CreateTemp(s.dir, "fluxion-diff-*.tmp")
	if err != nil {
		return fmt.Errorf("diff: creating temp file: %w", err)
	}
	// Unlinked immediately on Unix: the space is reclaimed when the process
	// exits however it exits, so an interrupted fleet-size diff cannot leave
	// tens of gigabytes behind in a pool that is already at 95%.
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return fmt.Errorf("diff: unlinking temp file: %w", err)
	}
	w := bufio.NewWriterSize(f, 1<<16)
	if _, err := w.Write(s.mem); err != nil {
		f.Close()
		return err
	}
	s.mem, s.f, s.w = nil, f, w
	s.charge()
	return nil
}

// flush makes everything written so far visible to ReadAt.
func (s *spillFile) flush() error {
	if s.w != nil {
		return s.w.Flush()
	}
	return nil
}

func (s *spillFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= s.size {
		return 0, io.EOF
	}
	if s.f == nil {
		n := copy(p, s.mem[off:])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}
	return s.f.ReadAt(p, off)
}

// truncate discards everything written past off, so the next write lands there.
//
// This is what lets the candidate walk in external.go cancel a subtree it has
// already emitted records for: a directory only learns that it is itself a
// suppressed move source after its children have closed, and at that point the
// single record for the directory replaces every record its subtree wrote.
func (s *spillFile) truncate(off int64) error {
	if off > s.size {
		return errors.New("diff: truncate past end of spill file")
	}
	if s.f == nil {
		s.mem = s.mem[:off]
		s.size = off
		return nil
	}
	if err := s.w.Flush(); err != nil {
		return err
	}
	if err := s.f.Truncate(off); err != nil {
		return err
	}
	if _, err := s.f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	s.w.Reset(s.f)
	s.size = off
	s.meter.sub(s.onDiskBytes - s.size)
	s.onDiskBytes = s.size
	return nil
}

func (s *spillFile) close() error {
	s.mem = nil
	if s.f == nil {
		return nil
	}
	s.meter.sub(s.onDiskBytes)
	s.onDiskBytes = 0
	f := s.f
	s.f, s.w = nil, nil
	return f.Close() // already unlinked, so the space is reclaimed here
}

// onDisk reports whether this log outgrew memory. Only tests care.
func (s *spillFile) onDisk() bool { return s.f != nil }

// Records are framed as a 4-byte little-endian length followed by the payload.
// Fixed-width payloads would not need framing, but the source-side records
// carry a path, and paths are the one thing in this package that is not a
// bounded width.

const recHeaderLen = 4

func appendRecord(dst []byte, payload []byte) []byte {
	var hdr [recHeaderLen]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	dst = append(dst, hdr[:]...)
	return append(dst, payload...)
}

// recReader reads framed records from a byte range of a spillFile, buffering so
// that a file-backed log costs one read per bufferful rather than one per
// record.
type recReader struct {
	s   *spillFile
	pos int64 // offset of the next record
	end int64

	buf      []byte
	bufStart int64
	bufLen   int
}

func newRecReader(s *spillFile, start, end int64) *recReader {
	return &recReader{s: s, pos: start, end: end, buf: make([]byte, 32<<10), bufStart: start}
}

// ensure returns the n bytes at the current position, reading more if needed.
// The returned slice is only valid until the next call.
func (r *recReader) ensure(n int) ([]byte, error) {
	if r.pos+int64(n) > r.end {
		return nil, io.ErrUnexpectedEOF
	}
	off := int(r.pos - r.bufStart)
	if off+n <= r.bufLen {
		return r.buf[off : off+n], nil
	}

	// Compact what is left, then top up.
	copy(r.buf, r.buf[off:r.bufLen])
	r.bufLen -= off
	r.bufStart = r.pos
	if n > len(r.buf) {
		grown := make([]byte, n+len(r.buf))
		copy(grown, r.buf[:r.bufLen])
		r.buf = grown
	}
	for r.bufLen < n {
		want := r.buf[r.bufLen:]
		if avail := r.end - (r.bufStart + int64(r.bufLen)); avail < int64(len(want)) {
			want = want[:avail]
		}
		m, err := r.s.ReadAt(want, r.bufStart+int64(r.bufLen))
		r.bufLen += m
		if m == 0 {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
	return r.buf[:n], nil
}

// next returns the next record's payload, or nil at the end of the range. The
// slice is only valid until the following call.
func (r *recReader) next() ([]byte, error) {
	if r.pos >= r.end {
		return nil, nil
	}
	hdr, err := r.ensure(recHeaderLen)
	if err != nil {
		return nil, err
	}
	n := int(binary.LittleEndian.Uint32(hdr))
	full, err := r.ensure(recHeaderLen + n)
	if err != nil {
		return nil, err
	}
	r.pos += int64(recHeaderLen + n)
	return full[recHeaderLen:], nil
}

// offset is where the record next would return begins.
func (r *recReader) offset() int64 { return r.pos }
