package engine

import (
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// progress is the half of a domain's readiness a single reading cannot know:
// whether a number has MOVED, and how long it has not.
//
// # Why this exists at all
//
// [statelog.Health] is a SNAPSHOT — every field describes this instant — and
// two of the conditions built on it are properties of a SERIES. `Stalled` is
// "the checkpoint has not moved for [statelog.StallGrace]", and the shed
// in [statelog.Health.Healthy] is "a record this build cannot decode has been
// held past [statelog.DeferralGrace]". Neither can be derived from one
// reading, so both were left unset: `Health.Stalled` was never assigned by
// anything, which made the `stalled` arm of [statelog.Health.Refusal]
// unreachable and left a frozen applier serving reads as though it were
// current, and `DeferredSince` had no producer at all, so the shed the
// `deferred_old` alarm promises an operator — "its seats move at 30m0s" —
// never happened.
//
// # It is observed on the position heartbeat, not on the read
//
// The observation has to happen on a loop with its own cadence, because "has
// not moved" is only meaningful against a previous look. It rides
// [PositionHeartbeat], which already ticks per node per interval and already
// reads exactly these numbers to write the register row — so the observation
// costs nothing and cannot drift from what the fleet is told.
//
// # What "moved" is measured on: the applier's COMMITTED checkpoint
//
// The register row carries two positions, and only one of them is the
// applier's progress. The CHECKPOINT moves past every record the applier
// settles, a record it RETAINS included — one a newer peer wrote mid-upgrade,
// or one signed under a keyring key this node was not restarted with — while
// APPLIED_THROUGH stops at the first retained record for as long as it is
// held. Measured on applied-through, a stall was a routine deferral: on a node
// behind the log at each look — catching up, or on a busy company with a
// record in flight at every beat — the prefix read as frozen past
// [statelog.StallGrace], so the request path answered 503 for every session,
// machine token and seat binding on the node, and `Health.Stalled` refused its
// reads and shed its seats — which [statelog.Health.Healthy] says a deferral
// must never do inside [statelog.DeferralGrace], and which a whole-node answer
// never needed to do at all: what a retained record withholds is scoped, and
// the readers that depend on it ask their own scope ([statelog] `DeferredIn`),
// while the deferral's age is this tracker's other half.
//
// A HEALTH READ IS THEREFORE PURE. It reads this and derives nothing itself,
// which is what keeps [stateLog.health] callable from five places — three
// screens, the admission gate and the report — without any of them advancing
// a clock the others depend on.
type progress struct {
	mu sync.Mutex

	// committed is the last checkpoint seen, and movedAt when it last
	// changed.
	//
	// A NODE THAT HAS NEVER APPLIED ANYTHING is stamped at its first
	// observation rather than at the zero instant, so a fresh boot is not
	// born stalled — the zero value would make every node stalled by
	// StallGrace after the epoch, which is to say always.
	committed uint64
	movedAt   time.Time

	// behind is whether the last look found work this domain owes — the
	// log's head past its checkpoint. It is what makes [progress.frozenFor]
	// a stall rather than a quiet company, and it is KEPT across a look
	// that could not read the log: see [progress.owed].
	behind bool

	// deferredSince is when the record this node cannot decode first
	// appeared, and held whether it still holds one.
	//
	// CLEARED THE MOMENT IT DOES NOT, so an upgraded node is healthy at
	// its next observation rather than after a second grace. The grace is
	// about how long a fleet tolerates a node that cannot read its
	// peers' records, not about how long it distrusts one that can.
	deferredSince time.Time
	held          bool
}

// observe records one look at a domain: the position the register row
// publishes for it, and whether the log's head is past its checkpoint.
//
// MOVEMENT IS THE CHECKPOINT'S, pos.Seq, and never pos.AppliedThrough — see
// this type's doc for what a retained record did to the other one. What pos
// says about a deferral is the deferral clock's input and nothing else's.
//
// `behind` is whether there is anything left to apply, and it is what
// separates a stalled node from an idle one: a caught-up node's checkpoint
// does not move because there is nothing to move it, and calling that stalled
// would refuse every read on a healthy company between two writes.
func (p *progress) observe(now time.Time, pos coord.DomainPosition, behind bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.movedAt.IsZero() || pos.Seq != p.committed {
		p.committed, p.movedAt = pos.Seq, now
	}
	deferred := pos.Deferred > 0
	p.behind = behind
	// AND WHEN IT IS NOT BEHIND THE CLOCK RESTARTS, because the stall
	// question is "is this node failing to make progress it owes", and a
	// node that owes none is not failing to make it. Without this an idle
	// company's first write after a quiet hour would land on a node that
	// had already declared itself stalled.
	if !behind {
		p.movedAt = now
	}

	switch {
	case deferred && !p.held:
		p.deferredSince, p.held = now, true
	case !deferred:
		p.deferredSince, p.held = time.Time{}, false
	}
}

// stalled reports a checkpoint frozen past [statelog.StallGrace] while it owed
// work.
//
// FALSE BEFORE THE FIRST OBSERVATION, which is the honest answer for a node
// whose heartbeat has not run yet: nothing has been measured, and a refusal
// derived from no measurement is a refusal derived from the zero value.
func (p *progress) stalled(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.movedAt.IsZero() && now.Sub(p.movedAt) > statelog.StallGrace
}

// frozenFor is how long this domain's checkpoint has not moved while it owed
// work, as of now — zero when the last look found nothing to apply, and zero
// before the first look.
//
// # It is the request path's half of a stall
//
// [progress.stalled] feeds [statelog.Health.Stalled], which a health read and
// the serviceability gate take. The REQUEST path — every session this node
// validates, every seat a bound credential acts as, every login's record —
// reads [runningDomain.Lag] instead and compares it against the same
// [statelog.StallGrace], and that figure was a backlog divided by the drain
// rate alone. The drain rate is measured over APPLY time and nothing moves it
// while no batch runs, so an applier that wedged with a small backlog kept the
// rate it last had and reported milliseconds of lag for as long as it stayed
// wedged: the session table's `stalled` row never fired, a read of a session
// this node had not seen was served indefinitely, and a revocation stuck in the
// backlog was never honoured here. How long the checkpoint has been frozen is
// the one figure a wedged applier cannot hold down, so the lag is the larger
// of the two — and it is the CHECKPOINT, which a retained record does not
// hold down either, so a node holding a newer peer's record is not a wedged
// one.
//
// GATED ON behind, as the stall is: a caught-up node's checkpoint does not
// move because nothing moves it, and a lag that grew between two writes would
// refuse every read on a quiet company.
func (p *progress) frozenFor(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.movedAt.IsZero() || !p.behind {
		return 0
	}
	return max(now.Sub(p.movedAt), 0)
}

// owed is whether the last look found work this domain owes, which is what a
// look that could not read the log carries forward.
//
// # Unreadable is not caught up
//
// A heartbeat whose stream statistics failed does not know whether the log
// moved. Reading that as "nothing to apply" restarted the stall clock on every
// failed look, so an applier and a broker connection that failed together —
// which is the ordinary shape of a wedge — never read as stalled at all. The
// lag figure beside it already keeps its last value on the same failure, for
// the same reason; this keeps the half the stall is judged on.
func (p *progress) owed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.behind
}

// deferredSinceValue is what [statelog.Health.Healthy] takes as its second
// argument: when the oldest undecodable record arrived, and whether one is
// still held.
func (p *progress) deferredSinceValue() statelog.DeferredSince {
	p.mu.Lock()
	defer p.mu.Unlock()
	return statelog.DeferredSince{Since: p.deferredSince, Held: p.held}
}
