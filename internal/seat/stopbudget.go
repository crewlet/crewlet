package seat

import (
	"context"
	"sync"
	"time"
)

// StopBudget is the one allowance a stopping node's coordination round trips
// share: the stop's announcement, the presence and seat leases it gives back,
// the seats' last lifecycle events, the admission it withdraws, the duties it
// releases and the holds its loops give back as they are stopped.
//
// # Why one allowance rather than one per step
//
// Every one of those steps races a lease — or, for the announcement and the
// lifecycle events, a live screen — and every one falls back to the same
// thing when it cannot reach the store: the lease lapses on its TTL, the
// screen ages out. Each used to run under a bound of its own, so a member that
// had lost quorum spent the SUM of them: the e2e fleet measured a lone member's
// stop at 25 s, five seconds per step, every one of them failing. A process
// under an orchestrator's thirty-second kill grace that spends that long on
// deadlines that cannot succeed is killed before its custody flush and its
// store close.
//
// So the steps draw on one allowance, [StopAllowance] of the lease TTL: on a
// healthy fleet each takes milliseconds and the allowance never binds; on an
// unreachable one the stop's coordination costs one allowance in total, after
// which every remaining step fails at once and takes its documented fallback.
// A store that blinks during a healthy stop costs the steps the blink, not the
// stop its allowance — the alternative, abandoning every step after the first
// failure, turns one transient error into a TTL of dark seats for every peer.
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

// StopAllowance is the share of a lease TTL a stop's coordination steps get
// between them: one heartbeat interval, the TTL over [HeartbeatRatio] — 15 s
// at the shipped 45 s TTL. The largest allowance still strictly inside the
// leases it is racing, which is the bound each seat's release already had on
// its own; it is now the bound for all of them together.
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
