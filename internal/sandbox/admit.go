package sandbox

import (
	"context"
	"errors"
)

// TurnGate admits the turn a resume re-enters, or refuses it: the node's own
// admission, which every turn on the node passes whatever started it.
//
// # Why a resume asks
//
// A resume STARTS A TURN. It re-enters a suspended conversation and runs every
// phase left in it: model calls, tool calls, posts to chat, writes to the
// tracker. The node admits each turn a seat's mailbox delivers (the seat owned
// fresh, a concurrency slot, a drain's closed gate), and a resume is not a
// mailbox delivery: a completion arrives on the seat's control topic, and a
// recorded answer's retry on this coordinator's own timer. Neither asked, so a
// draining node went on starting full turns through both, past the cap the
// node is sized by, while every mailbox was quiesced.
//
// # When it is asked
//
// BEFORE THE CLAIM, on both routes. A refused completion leaves the run exactly
// as it found it, unclaimed, and goes back to the broker for the seat's next
// holder; refused after the claim, the run would be claimed and its box
// collected, then given back, on every redelivery of a completion the node had
// already decided not to run. And the admission is HELD until the resume and
// its settle return, so the slot it takes covers the whole of the turn.
//
// # What a refusal says
//
// An error, and a [*TurnRefused] when the refusal names what would lift it: an
// answer's retry waits on that ([Coordinator.waitOwed]), and a completion is
// handed back carrying it, so the caller that settles the delivery keys what it
// does on the condition (a node that is leaving, or a condition that clears by
// waiting) rather than on the error behind it.
//
// Admitted, it returns the context the turn runs under, which the admission may
// end (a drain that can no longer wait for it), and the release, which the
// caller calls once the resume is over.
type TurnGate func(ctx context.Context, handle string) (context.Context, func(), error)

// TurnRefused is a [TurnGate]'s refusal: what an answer's retry waits on, and
// the gate's own cause, which [errors.Is] reaches through it.
//
// ITS CONDITION IS THE DATA A CALLER DECIDES ON. A gate that refuses for a new
// reason (a provider's limit, a spent budget) names a new [Condition], and every
// caller that keys on the condition handles it by its kind with no change of
// its own: an answer waits on it, and a completion is handed back by it.
type TurnRefused struct {
	Refusal
	Cause error
}

func (r *TurnRefused) Error() string { return r.Reason }

func (r *TurnRefused) Unwrap() error { return r.Cause }

// admitted is a resume's hold on its turn's admission.
//
// UNEXPORTED, AND THE ONE WAY TO A RESUME. [Coordinator.resumeAndSettle] is
// the only frame that calls the [Resumer], and it takes one of these rather
// than a bare context, so a route that would resume a turn without admitting
// it does not compile. Only [Coordinator.admitTurn] makes one (a guard test
// holds every other frame to that).
type admitted struct {
	ctx     context.Context
	release func()
}

// admitTurn asks the gate to admit a turn on handle's seat.
//
// A coordinator built without a gate admits every turn under the caller's
// context: one with no node beneath it, which a test's is, has no admission to
// pass. The engine always passes its node's ([CoordinatorOptions.Gate]).
func (c *Coordinator) admitTurn(ctx context.Context, handle string) (admitted, error) {
	if c.gate == nil {
		return admitted{ctx: ctx, release: func() {}}, nil
	}
	turnCtx, release, err := c.gate(ctx, handle)
	if err != nil {
		return admitted{}, err
	}
	return admitted{ctx: turnCtx, release: release}, nil
}

// refusalOf is what an answer refused by the gate waits on: the condition the
// gate named, or, for a refusal that names none, nothing but the clock and the
// backstop ([answerWaitBackstop]).
func refusalOf(err error) Refusal {
	var refused *TurnRefused
	if errors.As(err, &refused) {
		return refused.Refusal
	}
	return Refusal{Reason: err.Error()}
}
