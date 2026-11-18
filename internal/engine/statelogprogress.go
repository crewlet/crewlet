package engine

import (
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// progress is the half of a domain's readiness a single reading cannot know:
// whether a number has moved, and for how long it has not. `Stalled` and the
// deferral age [statelog.Health.Healthy] judges are properties of a series, so
// they are observed here, on [statelog.PositionHeartbeat], which already reads
// these numbers for the register row and cannot drift from what the fleet is told.
//
// Movement is the committed CHECKPOINT's, never APPLIED_THROUGH: that stops at
// the first retained record (a newer peer's, or one under a key this node lacks),
// so measured there a routine deferral read as a whole-node stall, which Healthy
// forbids inside [statelog.DeferralGrace]; what a retained record withholds is
// scoped ([statelog] `DeferredIn`). A health read derives nothing from this, so
// [stateLog.health] stays callable from anywhere without advancing a clock.
type progress struct {
	mu sync.Mutex

	// committed is the last checkpoint seen, and movedAt when it last
	// changed. A node that has never applied anything is stamped at its first
	// observation, not the zero instant, which would make every node stalled.
	committed uint64
	movedAt   time.Time

	// behind is whether the last look found the log's head past the
	// checkpoint: it makes [progress.frozenFor] a stall rather than a quiet
	// company, and it is kept across a look that could not read the log
	// ([progress.owed]).
	behind bool

	// deferredSince is when the undecodable record this node holds first
	// appeared, and held whether it still holds one. Cleared the moment it does
	// not, so an upgraded node is healthy at its next look, not after a second
	// grace.
	deferredSince time.Time
	held          bool
}

// observe records one look at a domain: the position the register row
// publishes for it, and whether the log's head is past its checkpoint.
// Movement is pos.Seq, never pos.AppliedThrough (see [progress]). `behind`
// separates a stalled node from an idle one, whose checkpoint does not move
// because nothing moves it.
func (p *progress) observe(now time.Time, pos coord.DomainPosition, behind bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.movedAt.IsZero() || pos.Seq != p.committed {
		p.committed, p.movedAt = pos.Seq, now
	}
	deferred := pos.Deferred > 0
	p.behind = behind
	// Not behind restarts the clock: a node owing no progress is not failing
	// to make it, and otherwise an idle company's first write after a quiet
	// hour would land on a node already declared stalled.
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
// work. False before the first observation: a refusal derived from no
// measurement is a refusal derived from the zero value.
func (p *progress) stalled(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.movedAt.IsZero() && now.Sub(p.movedAt) > statelog.StallGrace
}

// frozenFor is how long this domain's checkpoint has not moved while it owed
// work, as of now; zero when the last look found nothing to apply, and before
// the first look.
//
// It is the request path's half of a stall: that path compares [runningLog.Lag]
// against [statelog.StallGrace], and a backlog over the drain rate alone stays
// small while a wedged applier runs no batch to re-measure the rate. How long
// the checkpoint has been frozen is the one figure a wedge cannot hold down (nor
// a retained record, see [progress]), so the lag is the larger of the two.
// Gated on behind, as the stall is, so a quiet company's lag does not grow
// between two writes.
func (p *progress) frozenFor(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.movedAt.IsZero() || !p.behind {
		return 0
	}
	return max(now.Sub(p.movedAt), 0)
}

// owed is whether the last look found work this domain owes, which a look
// that could not read the log carries forward: unreadable is not caught up.
// Read as "nothing to apply", a failed look would restart the stall clock, so
// an applier and its broker connection failing together, the ordinary shape
// of a wedge, would never read as stalled.
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
