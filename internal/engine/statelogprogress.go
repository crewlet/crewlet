package engine

import (
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// progress is the half of a domain's readiness a single reading cannot know:
// whether a number has MOVED, how long it has not, and whether the node has
// drained since it last stopped keeping up.
//
// # Why this exists at all
//
// [statelog.Health] is a SNAPSHOT — every field describes this instant — and
// three of the conditions built on it are properties of a SERIES. `Stalled` is
// "the applied prefix has not moved for [statelog.StallGrace] with records
// waiting"; the shed in [statelog.Health.Healthy] is "a record this build
// cannot decode has been held past [statelog.DeferralGrace]"; and `CaughtUp`
// is "drained to nothing pending at least once, and not stalled since". None of
// them can be derived from one reading, so each is held here and a reading
// reports what this holds.
//
// # The series is observed on the position heartbeat
//
// "Has not moved" is only meaningful against a previous look, so the
// observation rides [PositionHeartbeat], which already ticks per node per
// interval and already reads exactly these numbers to write the register row —
// so it costs nothing and cannot drift from what the fleet is told.
//
// A HEALTH READ RECORDS ONE THING: the caught-up latch, from what that read
// saw. A drain is a fact of the instant it is seen — a node that emptied its
// backlog between two beats has drained — so a read that finds nothing past the
// checkpoint sets the latch, and a read that finds the prefix stalled clears
// it, exactly as a beat does. The stall clock and the deferral clock are the
// heartbeat's alone, and a health read only reads them — which is what keeps
// [stateLog.health] callable from every screen, the admission gate and the
// report without any of them advancing a clock the others depend on.
type progress struct {
	mu sync.Mutex

	// appliedThrough is the last value seen, and movedAt when it last
	// changed.
	//
	// A NODE THAT HAS NEVER APPLIED ANYTHING is stamped at its first
	// observation rather than at the zero instant, so a fresh boot is not
	// born stalled — the zero value would make every node stalled by
	// StallGrace after the epoch, which is to say always.
	appliedThrough uint64
	movedAt        time.Time

	// deferredSince is when the record this node cannot decode first
	// appeared, and held whether it still holds one.
	//
	// CLEARED THE MOMENT IT DOES NOT, so an upgraded node is healthy at
	// its next observation rather than after a second grace. The grace is
	// about how long a fleet tolerates a node that cannot read its
	// peers' records, not about how long it distrusts one that can.
	deferredSince time.Time
	held          bool

	// caught is the latch [statelog.Health.CaughtUp] reports, and caughtAt
	// the last look that found nothing past the checkpoint.
	//
	// A LATCH RATHER THAN THE INSTANT'S LAG, because every node is a record
	// or two behind for a moment after every write. The questions asked of
	// it — may this node claim seats, is its copy worth donating, is its
	// replication row ready — are about whether it keeps up, and a lag of
	// zero read at one instant answers whether a write happened to land in
	// the millisecond before the look: admission withheld on a busy node, a
	// snapshot skipped as never drained, a row that flaps.
	caught   bool
	caughtAt time.Time
}

// observe records one heartbeat's look at a domain's applied prefix.
//
// lag is how many records the broker's own last sequence is past the
// checkpoint, and NIL when the broker did not answer. It is what separates a
// stalled node from an idle one: a caught-up node's applied prefix does not
// move because there is nothing to move it, and calling that stalled would
// refuse every read on a healthy company between two writes.
//
// AN UNANSWERED READ IS NEITHER. It owes no measured work, so the stall clock
// restarts on it as on an idle node — tearing a node's seats down over a
// number nobody could read is what `unknown` exists throughout this engine to
// prevent — and it is no drain either, so it leaves the caught-up latch alone.
func (p *progress) observe(now time.Time, appliedThrough uint64, lag *uint64, deferred bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.movedAt.IsZero() || appliedThrough != p.appliedThrough {
		p.appliedThrough, p.movedAt = appliedThrough, now
	}
	// AND WHEN IT OWES NOTHING THE CLOCK RESTARTS, because the stall
	// question is "is this node failing to make progress it owes", and a
	// node that owes none is not failing to make it. Without this an idle
	// company's first write after a quiet hour would land on a node that
	// had already declared itself stalled.
	if lag == nil || *lag == 0 {
		p.movedAt = now
	}

	switch {
	case deferred && !p.held:
		p.deferredSince, p.held = now, true
	case !deferred:
		p.deferredSince, p.held = time.Time{}, false
	}
	p.latchLocked(now, lag != nil && *lag == 0)
}

// caughtUp records what one health read saw of the latch and answers it:
// drained is whether that read found nothing past the checkpoint.
func (p *progress) caughtUp(now time.Time, drained bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.latchLocked(now, drained)
	return p.caught
}

// latchLocked sets the caught-up latch on a look that found nothing to apply,
// and clears it on one that finds the applied prefix stalled — but only for a
// stall that began AFTER the latch was last set.
//
// # Why the stall's start is compared
//
// The stall clock is the heartbeat's, so for up to one beat after a stalled
// node drains, the clock still reads stalled while the node owes nothing. A
// read that drained in that beat has seen the node caught up after the stall
// ended; a later read that cleared the latch on the stale clock would undo it,
// and the node would be reported behind until it next happened to drain.
func (p *progress) latchLocked(now time.Time, drained bool) {
	switch {
	case drained:
		p.caught, p.caughtAt = true, now
	case p.stalledLocked(now) && p.movedAt.Add(statelog.StallGrace).After(p.caughtAt):
		p.caught = false
	}
}

// restart clears the latch, for applier loops started again over a replicated
// estate an adoption replaced: the latch describes the rows the loops run
// over, and the rows a node adopted are rows it has not yet drained.
func (p *progress) restart() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.caught, p.caughtAt = false, time.Time{}
}

// stalled reports an applied prefix frozen past [statelog.StallGrace].
//
// FALSE BEFORE THE FIRST OBSERVATION, which is the honest answer for a node
// whose heartbeat has not run yet: nothing has been measured, and a refusal
// derived from no measurement is a refusal derived from the zero value.
func (p *progress) stalled(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stalledLocked(now)
}

func (p *progress) stalledLocked(now time.Time) bool {
	return !p.movedAt.IsZero() && now.Sub(p.movedAt) > statelog.StallGrace
}

// deferredSinceValue is what [statelog.Health.Healthy] takes as its second
// argument: when the oldest undecodable record arrived, and whether one is
// still held.
func (p *progress) deferredSinceValue() statelog.DeferredSince {
	p.mu.Lock()
	defer p.mu.Unlock()
	return statelog.DeferredSince{Since: p.deferredSince, Held: p.held}
}
