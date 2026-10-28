package sandbox

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// TurnGate admits the turn a resume re-enters, or refuses it: the node's own
// admission, which every turn on the node passes whatever started it.
//
// # Why a resume asks
//
// A resume STARTS A TURN. It re-enters the turn its executor's run parked and
// runs every phase left in it: model calls, tool calls, posts to chat, writes
// to the tracker. The node admits each turn a seat's mailbox delivers (the seat owned
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
// # Which route it is told
//
// The route the resume takes ([ResumeRoute]), because the turn's first model
// call is on a different chain on each: a completion's is its reviewer's, and
// an answer's is the relaunch that carries the run on. A gate that refuses a
// resume whose models are all at their limit has to ask the chain that call
// would be made on, and a gate asked one chain for both refused a finished
// run's review for its executor's limit, or admitted an answer into it.
//
// Admitted, it returns the context the turn runs under, which the admission may
// end (a drain that can no longer wait for it), and the release, which the
// caller calls once the resume is over.
type TurnGate func(ctx context.Context, handle string, route ResumeRoute) (context.Context, func(), error)

// ResumeRoute is which of the two roads a resume takes into its turn, as its
// [TurnGate] is told it.
type ResumeRoute string

const (
	// ResumeCompletion is a coding run's completion: its job collected,
	// and its turn re-entered for the engine's delivery check and the
	// reviewer, whose send-back launches the run again.
	ResumeCompletion ResumeRoute = "completion"

	// ResumeAnswer is a person's answer to a run parked on its question,
	// recorded on the run: its turn launches the run again to carry it on
	// with the answer.
	ResumeAnswer ResumeRoute = "answer"
)

// Valid reports whether r is one of the two routes.
func (r ResumeRoute) Valid() bool {
	return r == ResumeCompletion || r == ResumeAnswer
}

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
// than a bare context, so a resume cannot be written outside this package
// without one. INSIDE it the compiler proves nothing: any frame here could
// declare one and fill it in. What holds that is the guard
// (internal/node.TestEveryTurnStartIsAdmitted): only [Coordinator.admitTurn]
// may mention this type in a value (a literal, a variable, new, a conversion,
// an alias, a field), only it and the three frames that take one
// (resumeAndSettle, completeAdmitted, resumeOwedAdmitted) may name it at all,
// and each of those three is called only from the frame that asked the gate
// for it, or from one of the other two.
type admitted struct {
	ctx     context.Context
	release func()
}

// admitTurn asks the gate to admit a turn on handle's seat, by route.
//
// A coordinator built without a gate admits every turn under the caller's
// context: one with no node beneath it, which a test's is, has no admission to
// pass. The engine always passes its node's ([CoordinatorOptions.Gate]).
func (c *Coordinator) admitTurn(ctx context.Context, handle string, route ResumeRoute) (admitted, error) {
	if c.gate == nil {
		return admitted{ctx: ctx, release: func() {}}, nil
	}
	turnCtx, release, err := c.gate(ctx, handle, route)
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

// waitingRefusal is the refusal err carries, if it names a condition to wait
// on: a gate's, a resume's that was reverted on one of the gate's own
// conditions met a round later ([Coordinator.resumeAndSettle]), or a resume its
// seat's budget held ([HeldError]). A completion's held resume never reaches a
// caller of this (the hold is on its row), so the last is an answer's, which
// waits on the window uncharged as it waits on the gate's refusals: charged
// as a failure, it spent the attempts its answer is bounded by on a wait.
func waitingRefusal(err error) (Refusal, bool) {
	var refused *TurnRefused
	if errors.As(err, &refused) && refused.Condition != "" {
		return refused.Refusal, true
	}
	var held *HeldError
	if errors.As(err, &held) && held.Refusal.Condition != "" {
		return held.Refusal, true
	}
	return Refusal{}, false
}

// refusedCompletion is a completion the node's gate refused on a condition,
// kept until the condition clears.
type refusedCompletion struct {
	cond    Condition
	ev      types.SandboxRunCompleted
	trigger *events.Event

	// heldUntil, where set, is the instant before which this node refuses the
	// job's announcements itself, without asking its gate or claiming the
	// run, and reason is what it refuses them with: a resume refused AFTER
	// its claim, on a condition the gate could not see ([Coordinator.keepRefused]).
	heldUntil time.Time
	reason    string
}

// keepRefused keeps a completion refused on refusal's condition, for
// [Coordinator.Readmit] to re-fire the moment that condition clears, and, where
// held, holds its announcements off on this node until refusal.Until.
//
// # Why it is kept, when the waiter announces it again anyway
//
// The waiter that drives the run's box — the fleet's, or the box host's own
// (boxhost.go) — re-announces a finished run on every poll for as long as its
// row is running, and that is what makes acknowledging a refused completion
// safe: nothing is lost with the delivery. But the next poll is up to one
// [DefaultPollInterval] away, and the end of a condition is an event the
// engine already observes (a seat's turn ending, a renew proving ownership
// again, a provider's limit lifting). So the clearing re-fires the completion
// itself, and the poll is the backstop for a clearing nothing signals. Both
// may arrive: the claim is at-most-once, so the run resumes once.
//
// A RESUME REFUSED AFTER ITS CLAIM is kept the same way: one whose models were
// all at their limit before its turn wrote anything is reverted, so the run is
// back to running as the gate would have left it, and its refusal names the
// condition ([Coordinator.OnCompleted]).
//
// # Why one refused after its claim is also held here
//
// The gate admitted it, and would admit its next announcement too: a
// completion's gate asks no limit its models know, since its job is collected
// first and its resume refused once it is (the engine's), and a review on a
// ready member of the seat's reviewer chain that sent the run back to runners
// every one of which sits out met the limit a round in, at its relaunch. Asked
// again, the gate says the same, and every announcement of the job collected
// its box and ran the review again into the same refusal, for as long as the
// limit lasted. So such a completion is kept held (held, and
// the refusal's Until is the instant): its announcements are refused HERE,
// before the gate and the claim ([Coordinator.stillRefused]), until that
// instant, and the signal that ends the wait re-fires it as it does any kept
// completion. A refusal that names no instant holds nothing: the gate is asked
// again on every announcement, as for a completion it refused itself.
//
// IN THIS NODE'S MEMORY, NEVER ON THE RUN'S ROW. The refusal is this node's
// models', whose limits are its own: the sit-out is not shared with the fleet,
// and a restart, or the seat moving to a peer, asks the models again on the
// first delivery (ADR-0064). The row is read by every node, and a hold written
// there silenced every waiter until its instant, whoever held the seat next: a
// restarted node, or the seat's next holder, whose models may know of no limit
// at all, left the job unannounced for up to the hour a vendor's window is
// benched for. Held here, it goes with the node: its restart forgets it, and
// so does its letting go of the seat ([Coordinator.ReleaseSeat]), so the job's
// next announcement asks whoever holds the seat then. The waiter announces the
// job on every poll meanwhile, as it does for any refused completion; each
// announcement costs this node a lookup, not a collection and a review, and
// the job's box, which the collection paused, is left asleep, since the job was
// read back already ([Waiter.reach]). A seat's budget, which every node reads
// alike, holds on the row instead ([HeldError]).
//
// A SIGNAL THAT LANDS WHILE THE GATE IS REFUSING IS NOT LOST. Readmit re-fires
// only what is kept when it runs, so a condition that cleared between the
// gate's answer and the keep would leave the completion kept for a signal that
// has already been and gone, until the next poll. So the caller reads what was
// signalled BEFORE it asks the gate (seen), and the keep, under the lock
// Readmit takes, re-fires at once instead of keeping when the condition has been
// signalled for the seat since: the same rule [Coordinator.waitOwed] applies
// to an answer.
//
// A refusal that names no condition is not kept: nothing would signal its end.
// Nor is one for a seat this node lets go of ([Coordinator.ReleaseSeat]): the
// waiter that drives its box announces it to the seat's next holder. A drain's
// is kept and never re-fired, since nothing signals a drain's end, until that
// release drops it.
//
// NOR ONE THAT LANDS AFTER THAT RELEASE. The release waits for no resume, and a
// resume refused after its claim returns once its review has run, minutes on:
// kept then, its hold outlived the seat here, and the seat, back on this node
// inside the limit's window, had the job's announcements refused until the
// instant, by a hold the release was to have forgotten. The seat's lease is
// asked once the keep is made, outside the lock as every lease read here is:
// the seat host lets go of a seat before the release drops what is kept for
// it, so a keep that finds the seat held is one the release drops, and one
// that finds it gone is dropped now.
func (c *Coordinator) keepRefused(ev types.SandboxRunCompleted, trigger *events.Event, refusal Refusal,
	held bool, seen map[signal]uint64,
) {
	if refusal.Condition == "" {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	kept := refusedCompletion{cond: refusal.Condition, ev: ev, trigger: trigger}
	if held {
		kept.heldUntil, kept.reason = refusal.Until, refusal.Reason
	}
	if c.signalledSinceLocked(refusal.Condition, ev.AgentHandle, seen) {
		c.refireLocked(kept)
		c.mu.Unlock()
		return
	}
	c.refused[ev.TurnID] = kept
	c.mu.Unlock()
	if _, leased := c.leaseOf(ev.AgentHandle); !leased {
		c.forgetRefused(ev.TurnID)
	}
}

// stillRefused reports whether this node holds ev's job off itself, and the
// refusal it holds it with: a completion whose resume was refused after its
// claim, kept with the instant that refusal lifts ([Coordinator.keepRefused]),
// announced again before that instant and before any signal re-fired it.
//
// ANOTHER JOB OF THE SAME TURN IS NOT HELD: a hold is the one job's whose
// resume met the refusal, and a turn that launched again since has a new job,
// whose own completion is the next question. The stale keep is dropped.
func (c *Coordinator) stillRefused(ev types.SandboxRunCompleted) (Refusal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept, ok := c.refused[ev.TurnID]
	switch {
	case !ok || kept.heldUntil.IsZero():
		return Refusal{}, false
	case kept.ev.LaunchID != ev.LaunchID:
		delete(c.refused, ev.TurnID)
		return Refusal{}, false
	case !c.now().Before(kept.heldUntil):
		return Refusal{}, false
	}
	return Refusal{Condition: kept.cond, Reason: kept.reason, Until: kept.heldUntil}, true
}

// forgetRefused drops a kept completion of turnID's run, once a completion of
// it has been admitted: from there the claim settles it, whichever announcement
// carried it.
func (c *Coordinator) forgetRefused(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.refused, turnID)
}

// refireRefusedLocked re-fires every kept completion refused on cond, for the
// named seats or every seat, each on a goroutine of the coordinator's own
// lifetime. The caller holds c.mu.
func (c *Coordinator) refireRefusedLocked(cond Condition, handles []string) {
	if c.closed {
		return
	}
	for turnID, kept := range c.refused {
		if kept.cond != cond || (len(handles) > 0 && !slices.Contains(handles, kept.ev.AgentHandle)) {
			continue
		}
		delete(c.refused, turnID)
		c.refireLocked(kept)
	}
}

// refireLocked re-fires one kept completion on a goroutine of the
// coordinator's own lifetime, which [Coordinator.Stop] waits for. The caller
// holds c.mu and has checked the coordinator is not closed.
func (c *Coordinator) refireLocked(kept refusedCompletion) {
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		c.refire(kept)
	}()
}

// refire offers a kept completion to [Coordinator.OnCompleted] again, under
// the coordinator's own lifetime, as the waiter's next announcement would. A
// refusal keeps it again; any other failure is left to that announcement.
func (c *Coordinator) refire(kept refusedCompletion) {
	ctx := c.life
	if ctx.Err() != nil {
		return
	}
	if err := c.OnCompleted(ctx, kept.ev, kept.trigger); err != nil {
		log.InfoContext(ctx, "sandbox_completion_refire_unsettled",
			"turn_id", kept.ev.TurnID, "agent", kept.ev.AgentHandle, "condition", string(kept.cond),
			"error", err.Error(),
			"detail", "the completion re-fired when its condition cleared was not settled; "+
				"the waiter's next poll announces it again")
	}
}
