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
// until ctx's deadline and not a moment longer: every wait made on it that ends
// in an honest answer — a read's barrier and its wait for a position (refused
// `no_quorum` and `behind`), a write's wait for its own record (`pending`) and
// for a position it must decide from (`behind`), and whatever its caller waits
// through [WaitWithin] — ends margin before that deadline rather than at it, so
// the node SENDS that answer while its asker is still listening.
//
// # Why a wait that runs to the deadline answers nobody
//
// Because the asker cannot tell its answer from a node that is gone. A wait
// bounded only by its own budget and by the request's deadline, still running
// when the deadline passes, produces its honest answer — "behind", "pending" —
// after the asker has stopped listening; and the asker, having heard nothing
// for a whole attempt of its own, judges the node wedged, asks it last for a
// while and reports it to the fleet's placement. The node was merely behind,
// and said so in time for nobody. Nor can an asker size its attempt as the sum
// of the waits instead: a linearizable read waits for the asker's floor, for a
// barrier — which may first wait out one already in flight — and for the
// barrier's position, so the sum is two [ReadBudget]s and two
// [DefaultBarrierBudget]s, and an attempt that long is one no test can scale
// down. Fitted into the attempt instead, the waits make ANY attempt longer than
// the margin one a healthy node answers inside; a shorter attempt costs only
// that a node behind by more than it allows says so sooner.
//
// THE MARGIN IS THE CALLER'S, never this package's: the answer's trip back and
// the skew between the asker's clock, which set the deadline, and this node's,
// which reads it — properties of the transport the request arrived over. A
// context with no deadline, or a margin that is not positive, comes back
// unchanged: nobody beyond a wire is waiting on it.
func WithReplyMargin(ctx context.Context, margin time.Duration) context.Context {
	deadline, ok := ctx.Deadline()
	if !ok || margin <= 0 {
		return ctx
	}
	return context.WithValue(ctx, replyKey{}, replyBy{at: deadline.Add(-margin), deadline: deadline})
}

// answerBy is the instant a wait made on ctx ends by, and false where ctx is no
// served request ([WithReplyMargin]) — or no longer one BOUNDED by the request
// it carries the mark of. A context detached from the request
// ([context.WithoutCancel]) keeps its values and none of its deadline, and one
// given a deadline of its own past the request's has outlived it on purpose: a
// rollback or a teardown, whose waits keep their own bounds rather than a
// request's that has gone.
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
// attempt left of it — which, said as a bare duration, would read as a budget
// somebody chose.
func (b WaitBound) String() string {
	if b.Asker {
		return fmt.Sprintf("%s, all its asker's attempt left", b.Within.Round(time.Millisecond))
	}
	return b.Within.String()
}

// WaitWithin is the context a wait that ends in an honest answer is made on:
// bounded by budget and, on a served request ([WithReplyMargin]), by the
// instant the request must be answered by, whichever comes first — and that
// bound, for the answer that names it.
//
// THE WAIT ENDS AT AN INSTANT, never after a length: the asker's answer-by
// instant is the context's deadline itself, and the budget's end is laid off
// from the one reading of the clock the two were compared at. Turned into what
// is left and armed as a timeout, the bound is re-anchored to whenever the
// timeout is armed, so a goroutine stalled between reading the clock and
// arming it — a stop of the world, a starved runner — ran the wait on by the
// stall: past the instant its asker must be answered by and, for a stall the
// length of the margin, to the asker's own deadline, which is the answer said
// for nobody. The budget goes from the same reading because the choice between
// the two was made at it: a budget ahead of the instant then, armed later,
// could end after the instant it was chosen ahead of.
//
// An instant already past is a wait of NOTHING rather than a refusal of its
// own: a position already reached is still found at once, because every
// waiter here looks before it waits ([Runner.WaitReached]), and any other
// wait ends at its first step with the answer its budget running out gives.
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
