package diff

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// The intermediates of Phase 3 have to behave identically whether they fit in
// memory or spill to a temp file, because the equivalence harness only ever
// exercises the in-memory path and the fleet only ever exercises the other one.
// Both limits are therefore swept here over the same data.
func TestExtSorter_SortsAndSpills(t *testing.T) {
	const keyLen = 8
	const n = 5000

	for _, limits := range []struct {
		name              string
		sortMem, spillMem int
	}{
		{"in memory", 64 << 20, 8 << 20},
		{"spilled", 4 << 10, 1 << 10},
	} {
		t.Run(limits.name, func(t *testing.T) {
			defer swapLimits(limits.sortMem, limits.spillMem)()

			var meter spillMeter
			s := newExtSorter(t.TempDir(), keyLen, &meter)
			r := rand.New(rand.NewSource(7))
			want := make([]uint64, 0, n)
			for i := 0; i < n; i++ {
				k := r.Uint64() >> 1
				want = append(want, k)
				var rec [keyLen]byte
				binary.BigEndian.PutUint64(rec[:], k)
				// A variable-length tail, since source records carry a path.
				payload := append(rec[:], fmt.Sprintf("payload-%d", k)...)
				if err := s.add(payload); err != nil {
					t.Fatalf("add: %v", err)
				}
			}
			sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })

			out, err := s.finish()
			if err != nil {
				t.Fatalf("finish: %v", err)
			}
			defer out.close()

			if limits.name == "spilled" && !out.onDisk() {
				t.Error("expected the sorted output to have spilled to disk")
			}

			rr := newRecReader(out, 0, out.size)
			for i := 0; i < n; i++ {
				rec, err := rr.next()
				if err != nil {
					t.Fatalf("record %d: %v", i, err)
				}
				if rec == nil {
					t.Fatalf("stream ended after %d records, want %d", i, n)
				}
				got := binary.BigEndian.Uint64(rec[:keyLen])
				if got != want[i] {
					t.Fatalf("record %d: key %d, want %d", i, got, want[i])
				}
				if string(rec[keyLen:]) != fmt.Sprintf("payload-%d", got) {
					t.Fatalf("record %d: payload %q does not match its key", i, rec[keyLen:])
				}
			}
			if rec, err := rr.next(); err != nil || rec != nil {
				t.Fatalf("expected exactly %d records; next() = %q, %v", n, rec, err)
			}
		})
	}
}

// truncate is what lets the candidate walk cancel a subtree it has already
// written records for, so it has to work identically on both backings.
func TestSpillFile_Truncate(t *testing.T) {
	for _, limit := range []int{8 << 20, 16} {
		defer swapLimits(sortMemLimit, limit)()

		s := newSpill(t.TempDir(), nil)
		var frame []byte
		for i := 0; i < 100; i++ {
			frame = appendRecord(frame[:0], []byte(fmt.Sprintf("record-%03d", i)))
			if _, err := s.Write(frame); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		mark := s.size
		for i := 0; i < 50; i++ {
			frame = appendRecord(frame[:0], []byte("doomed"))
			if _, err := s.Write(frame); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		if err := s.truncate(mark); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		frame = appendRecord(frame[:0], []byte("replacement"))
		if _, err := s.Write(frame); err != nil {
			t.Fatalf("write after truncate: %v", err)
		}
		if err := s.flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}

		rr := newRecReader(s, 0, s.size)
		var got []string
		for {
			rec, err := rr.next()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if rec == nil {
				break
			}
			got = append(got, string(rec))
		}
		if len(got) != 101 {
			t.Fatalf("limit %d: got %d records, want 101", limit, len(got))
		}
		if got[100] != "replacement" {
			t.Errorf("limit %d: last record %q, want %q", limit, got[100], "replacement")
		}
		for i := 0; i < 100; i++ {
			if want := fmt.Sprintf("record-%03d", i); got[i] != want {
				t.Fatalf("limit %d: record %d is %q, want %q", limit, i, got[i], want)
			}
		}
		s.close()
	}
}

func swapLimits(sortMem, spillMem int) func() {
	oldSort, oldSpill := sortMemLimit, spillMemLimit
	sortMemLimit, spillMemLimit = sortMem, spillMem
	return func() { sortMemLimit, spillMemLimit = oldSort, oldSpill }
}
