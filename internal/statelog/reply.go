package statelog

import (
	"context"
	"fmt"
	"time"
)

// replyKey carries the instant a served request must be answered by
// ([WithReplyMargin]).
type replyKey struct{}

// replyBy is a served request's answer-by instant beside the deadline it was
// taken from, which is what tells a wait of that request from a teardown that
// outlives it ([answerBy]).
type replyBy struct{ at, deadline time.Time }

// WithReplyMargin is ctx marked as a request somebody ACROSS A WIRE waits for
// until ctx's deadline: every wait on it that ends in an honest answer (a read's
// barrier and position, refused `no_quorum` and `behind`; a write's own record,
// `pending`, and position, `behind`; any via [WaitWithin]) ends margin before
// that deadline, so the node SENDS the answer while its asker still listens. Run
// to the deadline, the answer reaches nobody and the asker judges a node merely
// behind to be wedged; sizing the attempt as the sum of the waits (two
// [ReadBudget]s and two [ElectionBarrierBudget]s) is too long for a test to scale.
//
// The margin is the CALLER'S: the answer's trip back plus the skew between the
// asker's clock and this node's. Without a deadline, or with a margin that is not
// positive, ctx comes back unchanged.
func WithReplyMargin(ctx context.Context, margin time.Duration) context.Context {
	deadline, ok := ctx.Deadline()
	if !ok || margin <= 0 {
		return ctx
	}
	return context.WithValue(ctx, replyKey{}, replyBy{at: deadline.Add(-margin), deadline: deadline})
}

// answerBy is the instant a wait on ctx ends by, and false where ctx is no served
// request ([WithReplyMargin]) or no longer bounded by it: one detached
// ([context.WithoutCancel]) or given a later deadline of its own is a rollback or
// teardown that outlives the request on purpose and keeps its own bounds.
func answerBy(ctx context.Context) (time.Time, bool) {
	r, ok := ctx.Value(replyKey{}).(replyBy)
	if !ok {
		return time.Time{}, false
	}
	if deadline, bounded := ctx.Deadline(); !bounded || deadline.After(r.deadline) {
		return time.Time{}, false
	}
	return r.at, true
}

// WaitBound is how long one wait may last ([WaitWithin]), and what bounded it.
type WaitBound struct {
	// Within is the wait's whole length, never negative.
	Within time.Duration

	// Asker is set where the served request's answer-by instant bounded the
	// wait rather than the wait's own budget.
	Asker bool
}

// String is the bound as an answer names it: the budget, or what the asker's
// attempt left of it, said so lest it read as a budget somebody chose.
func (b WaitBound) String() string {
	if b.Asker {
		return fmt.Sprintf("%s, all its asker's attempt left", b.Within.Round(time.Millisecond))
	}
	return b.Within.String()
}

// WaitWithin is the context a wait that ends in an honest answer is made on:
// bounded by budget and, on a served request ([WithReplyMargin]), by its answer-by
// instant, whichever comes first; and that bound, for the answer that names it.
//
// Both bounds are INSTANTS laid off from one clock reading, never lengths armed
// as a timeout: a goroutine stalled between reading and arming (a stop of the
// world, a starved runner) would run the wait on by the stall, past its asker's
// instant or past the one its budget was chosen ahead of. An instant already past
// is a wait of nothing, not a refusal: every waiter looks before it waits
// ([Runner.WaitReached]).
func WaitWithin(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc, WaitBound) {
	return waitWithin(ctx, budget, time.Now())
}

// waitWithin is [WaitWithin] with the clock read at now.
func waitWithin(ctx context.Context, budget time.Duration, now time.Time) (context.Context, context.CancelFunc, WaitBound) {
	end, bound := now.Add(budget), WaitBound{Within: budget}
	if at, ok := answerBy(ctx); ok && at.Before(end) {
		end, bound = at, WaitBound{Within: max(at.Sub(now), 0), Asker: true}
	}
	waitCtx, cancel := context.WithDeadline(ctx, end)
	return waitCtx, cancel, bound
}

// untilAnswered is ctx bounded by the instant its request must be answered by,
// for a wait with no budget of its own on the caller's side — a read's barrier,
// whose append its read index bounds — and served false where ctx is no served
// request, when it is ctx itself.
func untilAnswered(ctx context.Context) (bounded context.Context, cancel context.CancelFunc, served bool) {
	at, ok := answerBy(ctx)
	if !ok {
		return ctx, func() {}, false
	}
	bounded, cancel = context.WithDeadline(ctx, at)
	return bounded, cancel, true
}
