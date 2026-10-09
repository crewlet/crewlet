package seat

import (
	"context"
	"sync"
	"time"
)

// StopBudget is the one allowance a stopping node's lease give-backs share:
// the presence and seat leases it hands back, the duties it releases and the
// holds its loops give back as they are stopped.
//
// # Why one allowance rather than one per step
//
// Every one of those steps races a lease, and every one falls back to the
// same thing when it cannot reach the store: the lease lapses on its TTL.
// Each used to run under a bound of its own, so a member that had lost quorum
// spent the SUM of them: the e2e fleet measured a lone member's stop at 25 s,
// five seconds per step, every one of them failing. A process under an
// orchestrator's thirty-second kill grace that spends that long on deadlines
// that cannot succeed is killed before its custody flush and its store close.
//
// So the steps draw on one allowance, [StopAllowance] of the lease TTL: on a
// healthy fleet each takes milliseconds and the allowance never binds; on an
// unreachable one the stop's coordination costs one allowance in total, after
// which every remaining step fails at once and takes its documented fallback.
// A store that blinks during a healthy stop costs the steps the blink, not the
// stop its allowance — the alternative, abandoning every step after the first
// failure, turns one transient error into a TTL of dark seats for every peer.
//
// # Only what lapses
//
// A round trip whose fallback is NOT a lease lapsing is never a step of it,
// because the allowance's whole argument is that running out of it costs no
// more than not trying. Two of a stop's round trips are not that:
//
//   - Its LIFECYCLE EVENTS — the node's `org_stopped`, each seat's last
//     `agent_terminated` — travel on the event stream, which is replicated
//     apart from the coordination buckets and can lose its quorum alone. A
//     publish the stream never acknowledges waits out whatever deadline it is
//     handed, so as steps of this allowance a stream without quorum spent the
//     time every lease behind them needed, and a store that was answering was
//     left holding the presence, the seats, the admission and every duty. The
//     engine bounds each on its own and publishes it beside the give-backs
//     rather than in front of them.
//   - The engine's ADMISSION withdrawal falls back to nothing at all: an
//     admission has no TTL, so one that is not withdrawn stays until the node
//     restarts under the same id or an operator excludes it. The engine
//     reserves it a share of the allowance no step before it can spend, and
//     hands this budget the rest.
//
// # The clock runs only while a step does
//
// Between the stop's first steps and its last sits the drain's wait for the
// turns still running, which has no bound of the engine's own (the drain's
// doc says why). The allowance is charged only while at least one step is in
// flight, so a long drain spends none of it, and steps running concurrently —
// a node's seats are released together — are charged once for the time they
// overlap rather than once each.
//
// # Carried on the context
//
// The steps live in three packages — the engine, the node that composes the
// drain, and this one — and the drain passes between them as a context. A
// value is what survives the [context.WithoutCancel] every teardown takes, so
// the one allowance reaches every step without a parameter at each layer. The
// one step no stop's context reaches is a hold given back on the context it
// was TAKEN on, made before the stop existed; the engine binds that one at the
// lease store the hold is taken through, which asks whether a stop has begun.
type StopBudget struct {
	total time.Duration

	mu     sync.Mutex
	spent  time.Duration
	active int
	since  time.Time
}

// StopAllowance is the share of a lease TTL a stop's coordination round trips
// get between them: one heartbeat interval, the TTL over [HeartbeatRatio] —
// 15 s at the shipped 45 s TTL. The largest allowance still strictly inside
// the leases it is racing, which is the bound each seat's release already had
// on its own; it is now the bound for all of them together, an engine's
// admission withdrawal included, whose share the engine takes out of it.
func StopAllowance(ttl time.Duration) time.Duration { return ttl / HeartbeatRatio }

// NewStopBudget is an allowance of total for one stop.
func NewStopBudget(total time.Duration) *StopBudget { return &StopBudget{total: total} }

// stopBudgetKey carries a [StopBudget] on a context.
type stopBudgetKey struct{}

// WithStopBudget is ctx carrying b, for every step of the stop it bounds.
func WithStopBudget(ctx context.Context, b *StopBudget) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, stopBudgetKey{}, b)
}

// WithinStop is ctx carrying the allowance of the stop it is part of: the one
// ctx already carries, where a caller further up began the stop, or a fresh
// [StopAllowance] of ttl, where the stop begins here.
//
// For the layers that can each be where a stop begins — a node's drain and its
// seat host's stop are reached from an engine's stop, which carries one, and
// from callers that carry none — so that every give-back they make is a
// [StopStep] whoever called them. Without it, a step outside any stop keeps its
// caller's bound, which for a give-back is no bound at all: the teardown that
// asks takes [context.WithoutCancel], because a caller's deadline has usually
// passed by then and a release that inherited it would do nothing.
func WithinStop(ctx context.Context, ttl time.Duration) context.Context {
	if b, ok := ctx.Value(stopBudgetKey{}).(*StopBudget); ok && b != nil {
		return ctx
	}
	return WithStopBudget(ctx, NewStopBudget(StopAllowance(ttl)))
}

// StopStep bounds one coordination round trip of a stop by what is left of
// the budget ctx carries, and returns the context to make it on and the call
// that ends the step, which the caller defers. A step begun with nothing left
// is handed a context that has already ended, so it fails at once.
//
// WITHOUT A BUDGET, ctx is returned as it is: a step outside a stop keeps
// whatever bound its caller gave it.
func StopStep(ctx context.Context) (context.Context, context.CancelFunc) {
	b, ok := ctx.Value(stopBudgetKey{}).(*StopBudget)
	if !ok || b == nil {
		return ctx, func() {}
	}
	deadline := b.begin(time.Now())
	stepCtx, cancel := context.WithDeadline(ctx, deadline)
	var once sync.Once
	return stepCtx, func() {
		once.Do(func() {
			cancel()
			b.end(time.Now())
		})
	}
}

// begin starts a step at now and answers when the allowance runs out, if the
// steps in flight then are all still running.
func (b *StopBudget) begin(now time.Time) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.charge(now)
	b.active++
	return now.Add(b.total - b.spent)
}

// end finishes a step at now.
func (b *StopBudget) end(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.charge(now)
	b.active--
}

// charge adds the time since the last change to what is spent, if any step
// was running through it. The caller holds mu.
func (b *StopBudget) charge(now time.Time) {
	if b.active > 0 {
		b.spent += now.Sub(b.since)
	}
	b.since = now
}
