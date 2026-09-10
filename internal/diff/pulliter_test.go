package diff

import (
	"errors"
	"testing"

	"fluxion/internal/models"
)

func TestPullIter_YieldsEverythingInOrder(t *testing.T) {
	src := mapToIter(map[string]models.FileRecord{
		"/a": {SHA1: "1"},
		"/b": {SHA1: "2"},
		"/c": {SHA1: "3"},
	})

	p := newPullIter(src)
	defer p.stop()

	var got []string
	for p.advance() {
		got = append(got, p.cur.path)
	}
	if p.err() != nil {
		t.Fatalf("unexpected error: %v", p.err())
	}

	want := []string{"/a", "/b", "/c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestPullIter_StopBeforeExhaustionDoesNotHang(t *testing.T) {
	// A source with far more records than we intend to read. If stopping early
	// left the producer parked mid-yield this would hang or leak; iter.Pull
	// unwinds it instead. The test failing by timeout is the real signal.
	big := make(map[string]models.FileRecord, 10000)
	for i := 0; i < 10000; i++ {
		big[string(rune('a'+i%26))+string(rune('a'+(i/26)%26))+string(rune('a'+(i/676)%26))] = models.FileRecord{SHA1: "x"}
	}

	p := newPullIter(mapToIter(big))
	if !p.advance() {
		t.Fatal("expected at least one record")
	}
	p.stop()

	// Pulling after stop is defined to report exhaustion, not panic.
	if p.advance() {
		t.Error("advance() after stop() should report exhaustion")
	}
}

func TestPullIter_PropagatesSourceError(t *testing.T) {
	boom := errors.New("boom")
	src := FileIterator(func(yield func(string, models.FileRecord) error) error {
		if err := yield("/a", models.FileRecord{SHA1: "1"}); err != nil {
			return err
		}
		return boom
	})

	p := newPullIter(src)
	defer p.stop()

	if !p.advance() {
		t.Fatal("expected the first record before the error")
	}
	if p.advance() {
		t.Fatal("expected exhaustion after the source failed")
	}
	if !errors.Is(p.err(), boom) {
		t.Errorf("err() = %v, want %v", p.err(), boom)
	}
}

func TestPullIter_EmptySource(t *testing.T) {
	p := newPullIter(mapToIter(map[string]models.FileRecord{}))
	defer p.stop()

	if p.advance() {
		t.Error("expected no records")
	}
	if p.err() != nil {
		t.Errorf("unexpected error: %v", p.err())
	}
}
