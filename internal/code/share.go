package code

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE NODE'S SEAT GIT-WORK SHARE: how much of a git node's READ LANE the
// seats' git work may hold at once — half of it.
//
// The read lane (internal/code/gitproc, ClassRead: a core a slot) is where
// every short git process of a node waits, FIFO, for at most gitproc.SlotWait
// before it is refused busy: a person's push's scans and its force checks, a
// sync round's peel of the refs, a fetch's check of the objects it wants — and
// the git a seat's read of a repository by object id runs
// (internal/code/query), and the build of an engine write a seat or a person
// asks through a tool (internal/code/host). A seat's read is git work of up to
// a whole read deadline, and so is a write's build (WriteBuildDeadline); sized
// past the lane, a burst of them would hold every slot, and the person whose
// push queued behind them would be answered busy after ten seconds of
// nothing. So the node's seat git work holds at most [ShareOf] the lane's
// slots — ONE share a node, which every kind of seat git work takes its unit
// of ([SeatWorks]), so no two kinds can between them take the whole lane — and
// the other half stays the persons' and the node's own.
//
// A UNIT IS HELD AROUND THE WORK'S LONG GIT AND NOTHING ELSE: a read waiting
// on its sync, on a re-sync at the log's head or on a barrier holds no
// read-lane slot and so holds no unit — held across a cold download, one read
// would refuse every other seat's read on a node whose share is one — and a
// read's short check of the objects it names waits in the read lane's own
// queue like any short read. A write is held to the same rule: its check of
// where its branch is and of the objects it names by id is a short read like
// any, and its unit is held around its build alone — which reads what every
// path it names and every directory above one is, a read whose cost grows
// with the square of a path's depth and so is no short read — given back
// before its landing's upload, its proposal and its wait for its row, which
// hold no read-lane slot. What the share bounds is the long processes, which
// are what a person's push waits behind.
//
// PAST [GitShareWait] the work is answered [ErrGitUnready]{[UnreadyShare]} —
// never queued for longer, and never refused as the asker's fault — so the
// walk that asked it moves to another git node, which answers as well as a
// saturated node would have.

// SeatWork is which seat git work takes a unit of the share: the `work`
// attribute of crewlet.code.seatwork.waits. A closed set ([SeatWorks]).
type SeatWork string

const (
	// SeatWorkRead is a server-side read of a repository by object id: the
	// git a read's kind runs (internal/code/query), and a merge preview's
	// merge (internal/code/home), a read of two commits by their ids too.
	SeatWorkRead SeatWork = "read"
	// SeatWorkWrite is an engine write's BUILD (internal/code/host): its git
	// from its read of what every path the change names and every directory
	// above one is, and of what it touches — the parent commit, the
	// directories it rewrites, the files an edit rewrites, a pick's two
	// commits — to its pack indexed. Never the check before the build, of
	// where the write's branch is and of what the objects it names by id
	// are, a short read in the read lane's own queue as a read's check of
	// what it names is; never its landing's upload, proposal or wait for its
	// row, which hold no read-lane slot; and a branch created or deleted
	// builds nothing and takes no unit.
	SeatWorkWrite SeatWork = "write"
)

// SeatWorks is the closed set.
func SeatWorks() []SeatWork { return []SeatWork{SeatWorkRead, SeatWorkWrite} }

// Valid reports a kind of seat git work this build knows.
func (w SeatWork) Valid() bool { return slices.Contains(SeatWorks(), w) }

// ShareOf is how many units a node whose read lane is slots wide holds: HALF
// of it, at least one — the other half is what a person's push's scans, a sync
// round's peel and a fetch's check of its wants wait in.
//
// HALF STANDS, MEASURED AGAINST (internal/code/host's BenchmarkEngineWrite,
// beside=push, its repository kept compacted between rounds so the person's
// push alone, beside the writes and after them meet one pack state): on two
// cores the share is its floor of one unit, and sixty-four engine writes at
// once into the person's repository took the push's p99 from 0.24–0.38 s
// alone to 0.92–1.64 s, past the twice the benchmark holds, and back to
// 0.25–0.51 s once they stopped (four runs over the three object stores).
// What the writes added is not a read-lane slot — the person was refused
// nothing: the copy's syncs a push waits for ran on the two cores the
// writes' builds and landings used (their p99 0.25–0.39 s beside the writes,
// 0.05–0.13 s alone), and the person's proposal waited in the repository's
// one commit queue behind theirs (its p99 0.09–1.25 s beside them, 34–82 ms
// alone). A narrower share has no unit to give at one and would shorten
// neither, and a wider node's half leaves more slots, not fewer: the figure
// decides the node's cores, never this fraction.
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
// recording nothing; the gauge starts at zero, so a git node whose seats run
// no git yet shows a share held by nobody rather than no share at all.
func NewGitShare(node string, readSlots int, m *metrics.Recorder) *GitShare {
	units := int64(ShareOf(readSlots))
	s := &GitShare{node: node, readSlots: readSlots, units: units, sem: semaphore.NewWeighted(units),
		metrics: m, wait: GitShareWait}
	s.moved(0)
	return s
}

// errShareWait is the cause of a take's own wait ending.
var errShareWait = errors.New("code: the seat git-work share's wait ended")

// Take holds one unit for work, waiting at most [GitShareWait] for one to come
// free: past it, [ErrGitUnready]{[UnreadyShare]} naming the node. ctx ending
// first is ctx's own error, and is not counted, since the take did not wait
// its while: its caller went away, and nobody waits for that answer — or the
// work's answer fell due first ([AnswerDue]), which the work answers itself,
// naming what spent its asker's attempt.
// release gives the unit back, and is idempotent — every holder defers it,
// and the one that releases early calls it again harmlessly.
//
// EVERY TAKE IS TIMED, granted or unready (crewlet.code.seatwork.waits, a
// histogram): the wait is a whole term of each attempt seat git work is
// given — an engine write's (estate.GitWriteAttempt) and a read's
// (estate.Timing's GitReadAttempt) — and a count of the waits that ran out
// says only how often the node gave that term up whole, never how near the
// granted ones came to it.
func (s *GitShare) Take(ctx context.Context, work SeatWork) (release func(), err error) {
	if !work.Valid() {
		return nil, fmt.Errorf("%w: seat git work %q is not one of %v", ErrInvalid, work, SeatWorks())
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
				"within %s; another git node answers meanwhile", s.node, s.units, s.readSlots, s.wait)}
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
