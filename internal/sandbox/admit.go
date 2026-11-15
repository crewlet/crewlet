package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
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
	// ResumeCompletion is a launch's completion: its job collected,
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
	ev      types.ExecutorFinished
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
// The waiter that drives the run's box (the fleet's, or the box host's own,
// boxhost.go) re-announces a finished run for as long as its row is
// running, on its schedule (reannounce.go), and that is what makes
// acknowledging a refused completion safe: nothing is lost with the delivery.
// But the next announcement is up to [reannounceCeilingPolls] poll intervals
// away, and the end of a condition is an event the
// engine already observes (a seat's turn ending, a renew proving ownership
// again, a provider's limit lifting). So the clearing re-fires the completion
// itself, and the poll is the backstop for a clearing nothing signals. Both
// may arrive: the claim is at-most-once, so the run resumes once.
//
// # Unless no waiter can announce it
//
// THE WAITER IS THE BACKSTOP WHILE THE STORE ANSWERS THE BOX'S HOST, AND THE
// BROKER IS WHILE IT DOES NOT. While this node keeps a clock on the store of
// the host of the job's box ([Coordinator.hostClocked]), or the run's row says
// what such a clock would ([Coordinator.unannouncedJob]), that host's waiter
// reads the run's row through a store that does not answer it and announces
// nothing, and what is kept here is this node's memory alone: a restart, a
// letting go of the seat, or a clearing nothing signals would leave the job
// with nothing to announce it. So the refusal is still kept for the signalled
// re-fire, and its delivery is handed back as well, never acknowledged, and
// renewed before its last ([ErrUnannounced], renew.go). And where neither says
// so yet, the refusal its delivery's acknowledgement leaves this node owing is
// written on the job's record first ([Coordinator.oweRefused]), so a restart
// or the seat's next holder owes it as this node did ([Coordinator.rearmOwed]). A duplicate is
// harmless, since the claim is made once, and the re-fire announces nothing
// afresh while that delivery is in flight ([Coordinator.refire]).
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
// THE HOLD IS IN THIS NODE'S MEMORY, NEVER ON THE RUN'S ROW. (What the row
// keeps is that a refusal was acknowledged and is owed its re-fire
// ([Coordinator.oweRefused]): an obligation the seat's next holder honours by
// asking its own gate, never a hold anybody reads.) The refusal is this node's
// models', whose limits are its own: the sit-out is not shared with the fleet,
// and a restart, or the seat moving to a peer, asks the models again on the
// first delivery (ADR-0064). The row is read by every node, and a hold written
// there silenced every waiter until its instant, whoever held the seat next: a
// restarted node, or the seat's next holder, whose models may know of no limit
// at all, left the job unannounced for up to the hour a vendor's window is
// benched for. Held here, it goes with the node: its restart forgets it, and
// so does its letting go of the seat ([Coordinator.ReleaseSeat]), so the job's
// next announcement asks whoever holds the seat then. The waiter announces the
// job on its schedule meanwhile, as it does for any refused completion; each
// announcement costs this node a lookup, not a collection and a review, and
// the job's box, which the collection paused, is left asleep, since the job was
// read back already ([Waiter.reach]). A seat's budget, which every node reads
// alike, holds on the row instead ([HeldError]).
//
// A SIGNAL THAT LANDS WHILE THE GATE IS REFUSING IS NOT LOST. Readmit re-fires
// only what is kept when it runs, so a condition that cleared between the
// gate's answer and the keep would leave the completion kept for a signal that
// has already been and gone, until the waiter's next announcement of it. So the
// caller reads what was signalled BEFORE it asks the gate (seen), and the keep,
// under the lock Readmit takes, re-fires at once instead of keeping when the
// condition has been signalled for the seat since: the same rule
// [Coordinator.waitOwed] applies to an answer.
//
// NOR IS A PERSON'S STOP. A stop ends a collected job's wait by re-firing what is
// kept for it ([Coordinator.endCollectedWait]), and it leaves a run whose
// resume is out to that resume's turn. A resume refused after its claim
// returns minutes on, its review's call refused as the stop landed: kept then,
// its hold outlived the stop, and the stopped seat's work stayed open until the
// limit's instant, up to the hour an entry is benched for. So a stop is
// signalled for its seat too ([stopLanded]), and a keep that finds the seat
// stopped since it was asked re-fires at once, into a gate that admits a
// stopped seat's completion and a turn its fence closes as stopped.
//
// A refusal that names no condition is not kept: nothing would signal its end.
// Nor is one for a seat this node lets go of ([Coordinator.ReleaseSeat]): the
// waiter that drives its box announces it to the seat's next holder, or, while
// no waiter can, the delivery handed back for it does; and an acknowledged one
// is on the job's record, which that holder's recovery re-arms
// ([Coordinator.rearmOwed]). A drain's is kept and never re-fired, since
// nothing signals a drain's end, until that release drops it.
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
func (c *Coordinator) keepRefused(ev types.ExecutorFinished, trigger *events.Event, refusal Refusal,
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
	if c.signalledSinceLocked(refusal.Condition, ev.AgentHandle, seen) ||
		c.signalledSinceLocked(stopLanded, ev.AgentHandle, seen) {
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

// oweRefused is answer, what a delivery of ev's completion leaves with the
// control topic, once a refusal the topic acknowledges is recorded on the job's
// record as this holder's obligation ([PendingStore.OweRefusal],
// [LaunchRecord.RefusalOwed]); every other answer is returned as it is.
//
// # Why an acknowledged refusal is written down
//
// Acknowledging a refusal is safe while the waiter that drives the job's box
// announces the job again, and the completion is kept here for the refusal's
// signal besides ([Coordinator.keepRefused]). But that waiter reads the run's
// row through its node's coordination store, and a silence of the store's runs
// alone has no bound: its node's lease and presence are another stream, so no
// watchdog ends it and no peer finds it gone. A kept completion is this node's
// memory, which its restart, or its letting go of the seat, forgets; one
// acknowledged before any collection of the job had found that silence left
// nothing anywhere that owed the job a re-fire, for as long as the silence
// lasted. So the obligation is durable state, on the row every holder of the
// seat reads at its recovery ([Coordinator.RecoverSeat]), written by ONE write
// before the delivery is let go, and only where the record owes none yet, so a
// re-announcement refused again writes nothing: each acknowledged refusal
// costs one read of the row, and the first of a job's a write besides.
//
// # Which refusals
//
// EVERY ONE THE TOPIC ACKNOWLEDGES, wherever the job's box is hosted: a refusal
// that names a condition (internal/engine's controlOutcome), the gate's own,
// a completion this node holds off itself ([Coordinator.stillRefused]) and a
// resume reverted on a wait after its claim alike. Not one handed back marked
// [ErrUnannounced], which is renewed and so carried by its delivery; not a
// store's silence; and not [SeatLost], which is deferred to the seat's holder.
// A drain's refusal is recorded as well: the engine defers it rather than
// acknowledge it, a condition of the engine's this package does not name, and
// the record costs its write and nothing more there, since the seat's next
// holder claims the completion once whichever of the deferred delivery and the
// record's re-fire reaches it first, and one the store does not take is still
// deferred (below).
//
// # Where the record does not land
//
// The refusal is NOT acknowledged. The answer is then the store's silence
// ([ErrStoreSilent], the node's failure, as every step the store does not
// answer is) with the refusal behind it, as a hand-back marked [ErrUnannounced]
// carries one: the caller reads the silence before the refusal could
// acknowledge it, so the delivery is handed back and comes back to be
// recorded, while a condition it hands on rather than waits on (a drain) is
// still handed on. Answered as the silence alone, a drain's delivery whose
// record the store did not take was handed back to the node that is leaving,
// delivery after delivery, rather than on to the seat's next holder. Where this
// node's own context ended, that is the cause ([storeSilent]) and the refusal
// is not carried: nothing ahead of it would then keep the caller from
// acknowledging it. The completion is still kept here for its signal either
// way. A row a newer lease owns refuses the write ([ErrSeatNotHeld]), which
// the caller names [SeatLost] where this node no longer holds the seat
// ([Coordinator.lost]), so the delivery goes on to the holder, and hands back,
// a drain's too, while this node's own lease still says it holds the seat,
// for the heartbeat that settles which is right ([SeatLost]); and a seat this
// node let go of before the write is deferred the same way, never acknowledged
// by a node that owes nothing any more.
func (c *Coordinator) oweRefused(ctx context.Context, ev types.ExecutorFinished, answer error) error {
	var refused *TurnRefused
	if !errors.As(answer, &refused) || refused.Condition == "" || refused.Condition == SeatLost ||
		errors.Is(answer, ErrUnannounced) || errors.Is(answer, ErrStoreSilent) {
		return answer
	}
	fence, held := c.leaseOf(ev.AgentHandle)
	if !held {
		return fmt.Errorf("%w: %w", ErrResumeUnavailable,
			notHeld(ctx, ev.AgentHandle, ev.TurnID, "recording a refused completion"))
	}
	since := c.now().UTC()
	wrote, err := c.pending.OweRefusal(ctx, ev.TurnID, ev.LaunchID,
		OwedRefusal{Condition: refused.Condition, Since: since}, fence)
	switch {
	case errors.Is(err, ErrSeatNotHeld):
		return fmt.Errorf("sandbox: the refusal of %s's completion (%s) is not recorded: %w", ev.TurnID,
			answer.Error(), err)
	case err != nil:
		log.WarnContext(ctx, "sandbox_completion_refusal_unrecorded",
			"turn_id", ev.TurnID, "launch_id", ev.LaunchID, "agent", ev.AgentHandle,
			"condition", string(refused.Condition), "error", err.Error(),
			"detail", "the refused completion could not be recorded as owed on the run's record, so its "+
				"delivery is not acknowledged: handed back, or on to the seat's next holder where its "+
				"condition is handed on, and recorded when it comes back")
		unrecorded := storeSilent(ctx, fmt.Errorf("sandbox: recording the refusal of %s's completion: %w",
			ev.TurnID, err))
		if !errors.Is(unrecorded, ErrStoreSilent) {
			// THIS NODE'S OWN CONTEXT ENDED, which is the cause: nothing ahead
			// of the refusal would keep the caller from acknowledging it.
			return unrecorded
		}
		// THE REFUSAL BEHIND THE STORE'S SILENCE, which hands the delivery
		// back, and a condition the caller hands on is still handed on.
		return fmt.Errorf("%w; %w", unrecorded, answer)
	case wrote:
		log.InfoContext(ctx, "sandbox_completion_refusal_owed",
			"turn_id", ev.TurnID, "launch_id", ev.LaunchID, "agent", ev.AgentHandle,
			"condition", string(refused.Condition),
			"detail", "the refused completion is acknowledged and recorded on the run's record as owed its "+
				"re-fire, which this seat's holder after a restart or a seat move reads at its recovery")
	}
	return answer
}

// stopLanded is what a person's stop of a seat is signalled as
// ([Coordinator.StopSeat]): no condition a refusal waits on, but one a
// completion refused while the stop landed is re-fired for rather than kept
// ([Coordinator.keepRefused]).
const stopLanded Condition = "stop"

// stillRefused reports whether this node holds ev's job off itself, and the
// refusal it holds it with: a completion whose resume was refused after its
// claim, kept with the instant that refusal lifts ([Coordinator.keepRefused]),
// announced again before that instant and before any signal re-fired it.
//
// ANOTHER JOB OF THE SAME TURN IS NOT HELD: a hold is the one job's whose
// resume met the refusal, and a turn that launched again since has a new job,
// whose own completion is the next question. The stale keep is dropped.
func (c *Coordinator) stillRefused(ev types.ExecutorFinished) (Refusal, bool) {
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
//
// BUT NO WAITER ANNOUNCES A JOB WHOSE BOX'S HOST FINDS ITS STORE SILENT, and a
// re-fire has no delivery in hand for the broker to bring back. Where its own
// attempt started this node's clock on that host's store
// ([Coordinator.hostClocked]), its claim won and its read of the box found
// that store silent first ([startingClock]), the delivery that carried the
// completion was acknowledged when it was kept, no clock running then, so the
// re-fire's completion is announced afresh at once, whatever the hand-back
// (renew.go),
// and a renewal the broker does not take is said here, as what it leaves: no
// delivery to make it again on, so this node makes it again on its own timer
// until one lands or this node settles the run, whoever holds the seat by then
// ([refirePublish]).
// WHERE THE CLOCK RAN BEFORE IT, OR THE RUN'S ROW SAID WHAT IT WOULD, NOTHING
// IS ANNOUNCED: the delivery refused then was handed back, never acknowledged,
// and is in flight still, renewed on its own budget, so an announcement made
// here beside it would leave two in flight, and one more with every refusal
// and re-fire after it. NOR WHERE ANOTHER ATTEMPT STARTED THE CLOCK while this
// one ran, its claim losing to that attempt's, or refused while that
// attempt's release was out, or winning after it to find the clock running:
// that attempt was a delivery's, handed back and in flight, which carries the
// completion. Renewed wherever the clock ran once the attempt was done, such a
// re-fire put a second announcement beside it. NOR WHERE ITS OWN ATTEMPT
// SETTLED THE RUN after starting the clock, its hand-back refused and the run
// ended, which drops the clock ([Coordinator.runSettled]): the clock it
// started must still run once the attempt is done. Renewed on the start
// alone, such a re-fire announced the completion of a run already ended, and
// a refusal of that announcement was made again on this node's timer with no
// settle left to drop it.
//
// ITS LIFETIME IS THE COORDINATOR'S (c.life, ended by [Coordinator.Stop]),
// never a caller's: a re-fire runs on a goroutine of its own after the
// delivery, the signal or the seat's release that started it has returned,
// so no caller's context reaches it.
//
//nolint:contextcheck // the coordinator's own lifetime (c.life), never a caller's; see above
func (c *Coordinator) refire(kept refusedCompletion) {
	ctx := c.life
	if ctx.Err() != nil {
		return
	}
	before := c.unannouncedJob(ctx, kept.ev, nil)
	started := new(atomic.Bool)
	err := c.OnCompleted(startingClock(ctx, started), kept.ev, kept.trigger)
	switch {
	case !before && started.Load() && c.hostClocked(kept.ev.TurnID, kept.ev.LaunchID):
		// RENEWED BELOW: nothing else carries it.
	case err == nil:
		return
	case before, c.hostClocked(kept.ev.TurnID, kept.ev.LaunchID):
		log.InfoContext(ctx, "sandbox_completion_refire_unsettled",
			"turn_id", kept.ev.TurnID, "agent", kept.ev.AgentHandle, "condition", string(kept.cond),
			"error", unannounced(kept.ev, err).Error(),
			"detail", "the completion re-fired when its condition cleared was not settled; its box's host "+
				"cannot read the coordination store, so no waiter announces it, and a delivery of it handed "+
				"back carries it on")
		return
	default:
		log.InfoContext(ctx, "sandbox_completion_refire_unsettled",
			"turn_id", kept.ev.TurnID, "agent", kept.ev.AgentHandle, "condition", string(kept.cond),
			"error", err.Error(),
			"detail", "the completion re-fired when its condition cleared was not settled; "+
				"the waiter announces it again on its schedule")
		return
	}
	err = unannounced(kept.ev, err)
	refused := c.renew(ctx, kept.ev, kept.trigger)
	if refused == nil {
		// ONE ANNOUNCEMENT IN FLIGHT: an earlier re-fire's this node still
		// owed is this one's now.
		c.mu.Lock()
		c.dropRepublishLocked(kept.ev.TurnID)
		c.mu.Unlock()
		return
	}
	delay, armed := c.armRepublish(kept, 1, nil)
	if !armed {
		return
	}
	log.WarnContext(ctx, "sandbox_completion_refire_unsettled",
		"turn_id", kept.ev.TurnID, "agent", kept.ev.AgentHandle, "condition", string(kept.cond),
		"attempts", 1, "retry_in_s", delay.Seconds(), "error", err.Error()+"; "+refused.Error(),
		"detail", "the completion re-fired when its condition cleared was handed back, and its "+
			"box's host cannot read the coordination store, so no waiter announces it; it could not "+
			"be announced afresh either, and this node announces it again on its timer until one "+
			"lands or it settles the run, to whoever holds the seat")
}
