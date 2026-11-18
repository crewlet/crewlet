package gitlet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/words"
)

// THE NODE'S SEAT GIT-WORK SHARE ([GitShare]): seat git work holds at most
// [ShareOf] a Gitlet node's read lane (gitproc ClassRead), where a person's
// push's scans, a sync round's peel and a fetch's want check wait FIFO at most
// gitproc.SlotWait before they are refused busy. A seat's read may run a whole
// read deadline and a write's build [WriteBuildDeadline], so a burst sized to
// the lane would refuse that push. ONE share a node, every [SeatWorks] kind
// taking from it, so no two kinds together take the lane. A unit is held around
// the work's LONG git alone: never across a wait for a sync, a re-sync or a
// barrier (across a cold download one read would starve a one-unit node), nor
// a short check of named objects, which queues in the lane like any short read.
// Past [GitShareWait] the work is [ErrGitUnready]{[UnreadyShare]}, never queued
// longer nor the asker's fault, so its walk moves to another Gitlet node.

// SeatWork is which seat git work takes a unit of the share: the `work`
// attribute of crewlet.code.seatwork.waits. A closed set ([SeatWorks]).
type SeatWork string

const (
	// SeatWorkRead is a principal's server-side read of a repository by object
	// id (internal/gitlet/query), a merge preview's merge (internal/gitlet/home)
	// included — never the engine's own read ([SeatWorkEngine]).
	SeatWorkRead SeatWork = "read"
	// SeatWorkWrite is an engine write's BUILD (internal/gitlet/host): from its
	// read of every path it names and every directory above one (a cost that
	// grows with the square of a path's depth, so no short read) to its pack
	// indexed. Not the check before it, of its branch and the objects it names
	// (a short read in the lane's own queue), nor its landing's upload, proposal
	// or wait for its row, which hold no read-lane slot; a branch created or
	// deleted builds nothing and takes no unit.
	SeatWorkWrite SeatWork = "write"
	// SeatWorkEngine is a read THE ENGINE makes on its own account
	// ([GitQuery.EnginesOwn]): a home's revision facts or checks file
	// (internal/gitlet/home), or a read made for a check. It takes its unit of
	// the same share but is COUNTED APART: counted as seats', a home's
	// background reads would hide from an operator what holds the share
	// (internal/gitlet/home bounds its own to leave the seats a unit).
	SeatWorkEngine SeatWork = "engine"
)

// SeatWorks is the closed set.
func SeatWorks() []SeatWork { return []SeatWork{SeatWorkRead, SeatWorkWrite, SeatWorkEngine} }

// Valid reports a kind of seat git work this build knows.
func (w SeatWork) Valid() bool { return slices.Contains(SeatWorks(), w) }

// ShareOf is how many units a node whose read lane is slots wide holds: HALF,
// at least one; the other half is what a person's push's scans, a sync round's
// peel and a fetch's want check wait in.
//
// Half stands against internal/gitlet/host's BenchmarkEngineWrite (beside=push):
// at two cores' floor of one unit, 64 concurrent engine writes took the push's
// p99 from 0.24–0.38 s to 0.92–1.64 s, but not through the share. The push was
// refused nothing; it waited on the cores the writes used and in the
// repository's one commit queue. A narrower share has no unit to give at one
// and would shorten neither, and a wider node's half leaves more slots: the
// figure decides the node's cores, never this fraction.
func ShareOf(slots int) int { return max(1, slots/2) }

// The outcomes of a take: crewlet.code.seatwork.waits' `outcome`.
const (
	shareGranted = "granted"
	shareUnready = "unready"
)

// GitShare is the node's bound on SEAT GIT WORK running at once: a weighted
// semaphore of [ShareOf] its read lane's slots, every unit taken through
// [GitShare.Take]. One a node, built beside its git pool and handed to every
// kind of seat git work on it.
type GitShare struct {
	node      string
	readSlots int
	units     int64
	sem       *semaphore.Weighted
	metrics   *metrics.Recorder

	// wait is GitShareWait; a field so this package's tests can scale it
	// (export_test.go).
	wait time.Duration

	// mu orders every change to held with the gauge that reports it, so a
	// later Set never carries an older count.
	mu   sync.Mutex
	held int
}

// NewGitShare is node's share of a read lane readSlots wide: [ShareOf]
// readSlots units. It records crewlet.code.seatwork.held and .waits to m, nil
// recording nothing; the gauge starts at zero, so a Gitlet node whose seats run
// no git yet shows a share held by nobody rather than no share at all.
func NewGitShare(node string, readSlots int, m *metrics.Recorder) *GitShare {
	units := int64(ShareOf(readSlots))
	s := &GitShare{node: node, readSlots: readSlots, units: units, sem: semaphore.NewWeighted(units),
		metrics: m, wait: GitShareWait}
	s.moved(0)
	return s
}

// errShareWait is the cause of a take's own wait ending.
var errShareWait = errors.New("gitlet: the seat git-work share's wait ended")

// Take holds one unit for work, waiting at most [GitShareWait]: past it,
// [ErrGitUnready]{[UnreadyShare]} naming the node. ctx ending first is ctx's
// own error, uncounted since the take did not wait its while: its caller left,
// or the work's answer fell due ([AnswerDue]), which the work answers itself.
// release is idempotent, so a holder that releases early may still defer it.
//
// EVERY TAKE IS TIMED, granted or unready (crewlet.code.seatwork.waits): the
// wait is a term of each attempt seat git work is given (estate.GitWriteAttempt,
// estate.Timing's GitReadAttempt), and a count of the waits that ran out would
// not show how near the granted ones came to it.
func (s *GitShare) Take(ctx context.Context, work SeatWork) (release func(), err error) {
	if !work.Valid() {
		return nil, fmt.Errorf("%w: seat git work %q is not one of %s", ErrInvalid, work, words.Or(SeatWorks()))
	}
	began := time.Now()
	wait, cancel := context.WithTimeoutCause(ctx, s.wait, errShareWait)
	defer cancel()
	if err := s.sem.Acquire(wait, 1); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.waited(work, shareUnready, time.Since(began))
		return nil, &ErrGitUnready{Reason: UnreadyShare, Detail: fmt.Sprintf(
			"%s's seat git work holds its whole share — %d of its read lane's %d slots — and none came free "+
				"within %s; another Gitlet node answers meanwhile", s.node, s.units, s.readSlots, s.wait)}
	}
	s.waited(work, shareGranted, time.Since(began))
	s.moved(+1)
	return sync.OnceFunc(func() {
		s.sem.Release(1)
		s.moved(-1)
	}), nil
}

// Held is how many units are held now.
func (s *GitShare) Held() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

// Units is how many units the share holds in all: [ShareOf] its read lane.
func (s *GitShare) Units() int { return int(s.units) }

// moved changes held by delta and reports it.
func (s *GitShare) moved(delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held += delta
	if s.metrics != nil {
		s.metrics.Set(metrics.CodeSeatworkHeld, float64(s.held), nil)
	}
}

// waited records how long one take waited, by its outcome.
func (s *GitShare) waited(work SeatWork, outcome string, took time.Duration) {
	if s.metrics != nil {
		s.metrics.Observe(metrics.CodeSeatworkWaits, took, metrics.Attrs{"outcome": outcome, "work": string(work)})
	}
}
