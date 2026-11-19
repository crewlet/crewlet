package seat

import (
	"context"
	"sync"
	"time"
)

// StopBudget is the one allowance a stopping node's lease give-backs share:
// its presence, seat and taker leases, duties, holds, and the removals of its
// MCP listings and status record. Each falls back to the lease lapsing on its
// TTL, so per-step bounds made a member with a lost store spend their SUM and
// outlive the orchestrator's grace. All steps draw on one [StopAllowance]; once
// spent, every remaining step fails at once and takes its fallback. A blink
// costs only the blink: abandoning all steps after one failure would leave
// peers a TTL of dark seats. Only what lapses is a step: lifecycle events (the
// stream can lose quorum alone), the admission withdrawal (no TTL) and the last
// settle attempts (steerSettleStopBudget) keep bounds of their own. The clock
// runs only while a step is in flight and rides the context, so it survives
// [context.WithoutCancel]. docs/guides/deployment.md derives the pod grace.
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

// stopBudgetOf is the allowance ctx carries, or nil where it carries none.
func stopBudgetOf(ctx context.Context) *StopBudget {
	b, _ := ctx.Value(stopBudgetKey{}).(*StopBudget)
	return b
}

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
	if stopBudgetOf(ctx) != nil {
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
	b := stopBudgetOf(ctx)
	if b == nil {
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
