package diff

import (
	"errors"
	"iter"

	"fluxion/internal/models"
)

// errPullStopped unwinds a push-based FileIterator when the puller stops before
// the source is exhausted. It never escapes newPullIter.
var errPullStopped = errors.New("diff: pull iterator stopped")

// pullRecord is one yielded (path, record) pair.
type pullRecord struct {
	path string
	rec  models.FileRecord
}

// pullIter adapts a push-based FileIterator to the pull semantics a merge join
// needs: look at the head of each side, compare, then advance only the side you
// consumed.
//
// FileIterator is push-based (`func(yield func(...) error) error`) because that
// is what keeps the diff algorithm free of the database - see
// knowledge/architecture.md, which names it one of the project's two testability
// seams. That shape is fine for "do something with every record" and useless for
// "show me your next record so I can decide" , which is what merge-joining two
// streams requires.
//
// The bridge is stdlib iter.Pull, so the source runs as a runtime coroutine
// rather than a goroutine: control transfers synchronously between next() and
// the iterator body, with no channel, no buffering, no scheduler involvement and
// no way to leak a blocked producer. Hand-rolling this with a goroutine and a
// channel - the pre-Go-1.23 idiom - would have added deadlock and leak failure
// modes to the package knowledge/diff-algo.md calls the highest-risk in the
// project, for no gain.
//
// stop() must be called when done, whether or not the source was exhausted;
// callers should defer it. err() is only meaningful once advance() has returned
// false.
type pullIter struct {
	next func() (pullRecord, bool)
	stop func()

	cur   pullRecord
	valid bool

	srcErr error
}

func newPullIter(it FileIterator) *pullIter {
	p := &pullIter{}

	seq := func(yield func(pullRecord) bool) {
		err := it(func(path string, rec models.FileRecord) error {
			if !yield(pullRecord{path: path, rec: rec}) {
				return errPullStopped
			}
			return nil
		})
		// Assigned inside the coroutine, read by the caller only after advance()
		// reports exhaustion. iter.Pull transfers control synchronously, so the
		// assignment has already happened by the time next() returns false.
		if err != nil && !errors.Is(err, errPullStopped) {
			p.srcErr = err
		}
	}

	p.next, p.stop = iter.Pull(seq)
	return p
}

// advance loads the next record, reporting whether one arrived. When it reports
// false the source is exhausted or failed - check err().
func (p *pullIter) advance() bool {
	p.cur, p.valid = p.next()
	return p.valid
}

// err reports a failure from the underlying FileIterator. Only meaningful after
// advance() has returned false.
func (p *pullIter) err() error { return p.srcErr }
