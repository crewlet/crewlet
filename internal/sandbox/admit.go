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
// admission. A resume STARTS A TURN but is no mailbox delivery (a completion arrives on
// the control topic, an answer's retry on a timer), so it must pass admission
// or a draining node starts turns past its cap. Asked BEFORE THE CLAIM: a
// refused completion stays unclaimed for the next holder, where refusing after
// would collect its box on every redelivery. HELD until the resume settles.
// A refusal is a [*TurnRefused] when it names what would lift it
// ([Coordinator.waitOwed]), so callers key on the condition. It is told the
// [ResumeRoute] because the first model call is on a different chain on each
// (reviewer's, relaunch's), and the conversation the turn works in, "" for a
// completion, whose run already holds the conversation's one turn (ADR-0169).
// Admitted, it returns the turn's context (a drain may end it) and the release.
type TurnGate func(ctx context.Context, handle, conversation string, route ResumeRoute) (context.Context, func(), error)

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
// UNEXPORTED, AND THE ONE WAY TO A RESUME. [Coordinator.resumeAndSettle], the
// only frame that calls the [Resumer], takes one of these rather than a bare
// context, so a resume cannot be written outside this package without one.
// INSIDE it the compiler proves nothing; the guard
// (internal/node.TestEveryTurnStartIsAdmitted) holds it: only
// [Coordinator.admitTurn] may build a value of this type, only it and the four
// frames taking one (resumeAndSettle, completeAdmitted, resumeOwedAdmitted,
// carryOnAdmitted) may name it, and each of those is called only from the frame
// that asked the gate.
type admitted struct {
	ctx     context.Context
	release func()
}

// admitTurn asks the gate to admit a turn of conversation on handle's seat, by
// route.
//
// A coordinator built without a gate admits every turn under the caller's
// context: one with no node beneath it, which a test's is, has no admission to
// pass. The engine always passes its node's ([CoordinatorOptions.Gate]).
func (c *Coordinator) admitTurn(ctx context.Context, handle, conversation string, route ResumeRoute) (admitted, error) {
	if c.gate == nil {
		return admitted{ctx: ctx, release: func() {}}, nil
	}
	turnCtx, release, err := c.gate(ctx, handle, conversation, route)
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

	// until is the instant the refusal named for its lifting on the clock
	// ([Refusal.Until]), held or not, and zero where it named none: what the
	// kept completion's own re-fire is armed for ([Coordinator.keepLocked]).
	until time.Time

	// heldOnRow, where set, is the instant the hold on the run's row lifts
	// ([PendingRun.HeldUntil]): a resume its seat's budget held
	// ([Coordinator.keepHeldOnRow]). No claim of the job wins before it, so
	// the keep's own re-fire waits for that instant and never comes sooner,
	// and its condition's signal re-fires it only once it has lifted the
	// hold ([Coordinator.liftAndRefire]). This node refuses nothing for it:
	// the row does.
	heldOnRow time.Time
	// hold is that hold's number ([PendingRun.Holds]), the one its lift names.
	hold uint64

	// backstop is that re-fire, armed while the completion is kept and
	// cancelled when it goes ([Coordinator.dropKeptLocked]). Under mu.
	backstop *keptRefire
}

// keptRefire is a kept completion's own re-fire ([Coordinator.keepLocked]),
// one per keep: the timer's callback re-fires only the keep it was armed for.
type keptRefire struct {
	// stop cancels the armed re-fire, reporting whether it had not started,
	// [CoordinatorOptions.After]'s contract.
	stop func() bool
}

// keepLocked keeps kept for its condition, replacing anything kept for its run,
// and arms its own re-fire ([Coordinator.keptRefireIn]): at the refusal's
// instant, no later than [answerWaitBackstop] unless this node holds the
// completion off until that instant; at the backstop where it named none or one
// past; and never sooner than a hold on the run's row lifts. A keep needs a
// timer because a condition can lift with no signal, and a silent host store
// stops the waiter. The re-fire is no work in flight, and [Coordinator.Stop]
// cancels it. The caller holds c.mu and has checked the coordinator is open.
// See the package doc, "Kept completions".
func (c *Coordinator) keepLocked(kept refusedCompletion) {
	turnID := kept.ev.TurnID
	c.dropKeptLocked(turnID)
	wait := c.keptRefireIn(kept)
	armed := &keptRefire{}
	kept.backstop = armed
	c.refused[turnID] = kept
	armed.stop = c.after(wait, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closed || c.refused[turnID].backstop != armed {
			return
		}
		if c.heldHereLocked(kept) {
			c.recheckHeldLocked(kept, armed)
			return
		}
		delete(c.refused, turnID)
		c.refireLocked(kept)
	})
}

// heldHereLocked reports whether this node still holds kept off itself: held
// after its claim ([Coordinator.keepRefused]) and short of the hold's instant.
// The caller holds c.mu.
func (c *Coordinator) heldHereLocked(kept refusedCompletion) bool {
	return !kept.heldUntil.IsZero() && c.now().Before(kept.heldUntil)
}

// recheckHeldLocked runs a held completion's backstop ([Coordinator.recheckHeld])
// on a goroutine of the coordinator's own lifetime, which [Coordinator.Stop]
// waits for. The caller holds c.mu and has checked the coordinator is not
// closed.
func (c *Coordinator) recheckHeldLocked(kept refusedCompletion, armed *keptRefire) {
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		c.recheckHeld(kept, armed)
	}()
}

// recheckHeld is the backstop of a completion this node holds off itself until
// an instant, before that instant: it asks only what an announcement would
// ([Coordinator.OnCompleted]), ending the run if its turn is past its deadline
// ([Coordinator.completeAtDeadline]), and otherwise keeps the hold and arms the
// next backstop, or re-fires where the instant has come. armed is the re-fire
// this backstop was. Its lifetime is the coordinator's. See the package doc,
// "Kept completions".
//
//nolint:contextcheck // the coordinator's own lifetime (c.life), never a caller's; see [Coordinator.refire]
func (c *Coordinator) recheckHeld(kept refusedCompletion, armed *keptRefire) {
	ctx := c.life
	if ctx.Err() != nil {
		return
	}
	ev := kept.ev
	unsettled := false
	if refusal, held := c.stillRefused(ev); held && c.pastDeadline(ctx, ev) {
		fence, leased := c.leaseOf(ev.AgentHandle)
		if !leased {
			c.forgetRefused(ev.TurnID)
			return
		}
		err := c.completeAtDeadline(ctx, ev, fence, refusal.Reason)
		switch {
		case errors.Is(err, ErrSeatRecovering):
			// THE SEAT'S RECOVERY PASS RUNS HERE, and its gate refused the
			// claim: handed to the pass, which re-fires the completion when it
			// is done, and that re-fire, still held here and past the
			// deadline, ends the run (recoverygate.go). Left to the next
			// backstop, the run waited five minutes past its deadline for a
			// pass of a second or so.
			unsettled = true
			c.carryPastRecovery(ctx, ev.AgentHandle, passCarry{refire: &kept})
		case err != nil:
			unsettled = true
			log.InfoContext(ctx, "sandbox_completion_held_unsettled",
				"turn_id", ev.TurnID, "agent", ev.AgentHandle, "condition", string(kept.cond),
				"error", err.Error(),
				"detail", "the held completion's turn is past its deadline and its run could not be ended "+
					"yet; this node keeps it held and asks again at its next backstop")
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	current, ok := c.refused[ev.TurnID]
	switch {
	case ok && current.backstop != armed:
		// KEPT AFRESH since, by whatever replaced it: that keep's own.
	case !ok && !unsettled:
		// GONE: carried, ended, dropped or let go of with the seat.
	case c.heldHereLocked(kept):
		c.keepLocked(kept)
	default:
		// ITS INSTANT CAME while this backstop asked.
		c.dropKeptLocked(ev.TurnID)
		c.refireLocked(kept)
	}
}

// keptRefireIn is how long kept waits for its own re-fire
// ([Coordinator.keepLocked]): until its refusal's instant, no longer than
// [answerWaitBackstop], or that backstop where it named none or one past; for a
// completion held on its run's row, until that hold lifts, uncapped and never
// sooner ([Coordinator.keepHeldOnRow]). A completion this node holds itself is
// re-fired at its instant, never at the backstop, which would resume it into
// the same limit; its timer still fires at the backstop to ask about the
// deadline ([Coordinator.recheckHeld]). See the package doc, "Kept
// completions".
func (c *Coordinator) keptRefireIn(kept refusedCompletion) time.Duration {
	now := c.now()
	if !kept.heldOnRow.IsZero() {
		return max(kept.heldOnRow.Sub(now), 0)
	}
	if held := kept.heldUntil.Sub(now); !kept.heldUntil.IsZero() && held > 0 {
		return min(answerWaitBackstop, held)
	}
	if until := kept.until.Sub(now); !kept.until.IsZero() && until > 0 {
		return min(answerWaitBackstop, until)
	}
	return answerWaitBackstop
}

// dropKeptLocked drops what is kept for turnID's run, if anything, its own
// re-fire cancelled. The caller holds c.mu.
func (c *Coordinator) dropKeptLocked(turnID string) {
	kept, ok := c.refused[turnID]
	if !ok {
		return
	}
	if kept.backstop != nil && kept.backstop.stop != nil {
		kept.backstop.stop()
	}
	delete(c.refused, turnID)
}

// keepRefused keeps a completion refused on refusal's condition, for
// [Coordinator.Readmit] to re-fire the moment that condition clears, and, where
// held, holds its announcements off on this node until refusal.Until. The
// waiter's re-announcement is the backstop; the keep re-fires sooner. Where no
// waiter can announce the job its delivery is handed back and renewed, and an
// acknowledged refusal is written on the job's record first. The hold stays in
// this node's memory, never on the row. A signal or a stop that lands while the
// gate refuses re-fires at once (seen); a refusal with no condition, or for a
// seat this node lets go of, is not kept. See the package doc, "Kept
// completions".
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
	kept := refusedCompletion{cond: refusal.Condition, ev: ev, trigger: trigger, until: refusal.Until}
	if held {
		kept.heldUntil, kept.reason = refusal.Until, refusal.Reason
	}
	if c.signalledSinceLocked(refusal.Condition, ev.AgentHandle, seen) ||
		c.signalledSinceLocked(stopLanded, ev.AgentHandle, seen) {
		c.refireLocked(kept)
		c.mu.Unlock()
		return
	}
	c.keepLocked(kept)
	c.mu.Unlock()
	if _, leased := c.leaseOf(ev.AgentHandle); !leased {
		c.forgetRefused(ev.TurnID)
	}
}

// keepHeldOnRow keeps the completion of run's job whose resume its seat's
// budget refused and whose claim was handed back holding the row until
// hold.Until ([HeldError], [Coordinator.unclaimHeld]): kept unheld here, for a
// re-fire at that instant and never sooner, and for the condition's signal,
// which lifts the row's hold and re-fires ([Coordinator.liftAndRefire]).
// trigger is the event, or nil for one rebuilt from the row; seen is what was
// signalled before the admission; recovered says the caller is a seat's
// recovery. It reports whether it kept the completion for the hold's instant.
// See the package doc, "Kept completions".
func (c *Coordinator) keepHeldOnRow(run PendingRun, trigger *events.Event, hold Refusal, number uint64,
	seen map[signal]uint64, recovered bool,
) bool {
	if hold.Condition == "" {
		return false
	}
	ev, announced := announcementOf(run)
	if trigger == nil {
		trigger = announced
	}
	kept := refusedCompletion{cond: hold.Condition, ev: ev, trigger: trigger, heldOnRow: hold.Until,
		hold: number}
	c.mu.Lock()
	if c.closed || (recovered && c.carriedLocked(ev.TurnID)) {
		c.mu.Unlock()
		return false
	}
	if c.signalledSinceLocked(hold.Condition, ev.AgentHandle, seen) ||
		c.signalledSinceLocked(stopLanded, ev.AgentHandle, seen) {
		c.liftAndRefireLocked(kept)
		c.mu.Unlock()
		return false
	}
	c.keepLocked(kept)
	c.mu.Unlock()
	if _, leased := c.leaseOf(ev.AgentHandle); !leased {
		c.forgetRefused(ev.TurnID)
		return false
	}
	return true
}

// oweRefused is answer, what a delivery of ev's completion leaves with the
// control topic, once a refusal the topic acknowledges is recorded on the job's
// record as this holder's obligation ([PendingStore.OweRefusal],
// [LaunchRecord.RefusalOwed]); every other answer is returned as it is. Every
// acknowledged refusal is recorded, not one marked [ErrUnannounced], a store's
// silence, [SeatLost] or [Draining]. Where the record does not land the refusal
// is not acknowledged: the answer is the store's silence, or this node's ended
// context. See the package doc, "Owed refusals".
func (c *Coordinator) oweRefused(ctx context.Context, ev types.ExecutorFinished, answer error) error {
	var refused *TurnRefused
	if !errors.As(answer, &refused) || refused.Condition == "" || refused.Condition == SeatLost ||
		refused.Condition == Draining || errors.Is(answer, ErrUnannounced) ||
		errors.Is(answer, ErrStoreSilent) {
		return answer
	}
	fence, held := c.leaseOf(ev.AgentHandle)
	if !held {
		return fmt.Errorf("%w: %w", ErrResumeElsewhere,
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
				"delivery is not acknowledged: handed back, and recorded when it comes back")
		return storeSilent(ctx, fmt.Errorf("sandbox: recording the refusal of %s's completion: %w",
			ev.TurnID, err))
	case wrote:
		log.InfoContext(ctx, "sandbox_completion_refusal_owed",
			"turn_id", ev.TurnID, "launch_id", ev.LaunchID, "agent", ev.AgentHandle,
			"condition", string(refused.Condition),
			"detail", "the refused completion is acknowledged and recorded on the run's record as owed its "+
				"re-fire, which this seat's holder after a restart or a seat move reads at its recovery")
	}
	return answer
}

// keepAgain keeps kept once more, its re-fire armed afresh
// ([Coordinator.keepLocked]), after a re-fire that neither settled it nor was
// kept again by a refusal: unless something is kept for its run already, or
// this node no longer holds the seat. Kept unheld, for the backstop alone: what
// re-fired it ended whatever its refusal held it to, and kept as it was a held
// completion would wait for an instant its signal had already ended. See the
// package doc, "Kept completions".
func (c *Coordinator) keepAgain(kept refusedCompletion) {
	kept.heldUntil, kept.reason, kept.until, kept.heldOnRow = time.Time{}, "", time.Time{}, time.Time{}
	kept.hold = 0
	c.keepUnlessKept(kept)
}

// keepUnlessKept keeps kept with its own re-fire armed afresh
// ([Coordinator.keepLocked]), as it stands, unless something is kept for its
// run already or the coordinator is closed, and forgets it again where this
// node no longer holds the seat: [Coordinator.keepAgain]'s keep, and a lift of
// its row's hold that the store did not answer ([Coordinator.liftAndRefire]).
func (c *Coordinator) keepUnlessKept(kept refusedCompletion) {
	c.mu.Lock()
	if _, already := c.refused[kept.ev.TurnID]; already || c.closed {
		c.mu.Unlock()
		return
	}
	c.keepLocked(kept)
	c.mu.Unlock()
	if _, leased := c.leaseOf(kept.ev.AgentHandle); !leased {
		c.forgetRefused(kept.ev.TurnID)
	}
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
		c.dropKeptLocked(ev.TurnID)
		return Refusal{}, false
	case !c.now().Before(kept.heldUntil):
		return Refusal{}, false
	}
	return Refusal{Condition: kept.cond, Reason: kept.reason, Until: kept.heldUntil}, true
}

// forgetRefused drops a kept completion of turnID's run: once a claim of its
// completion has won, since from there the claim settles it, whichever
// announcement carried it, and where this node no longer holds the seat, whose
// next holder owes the completion. A delivery admitted drops what is kept for
// its run before its claim is asked ([Coordinator.forgetAdmitted]).
//
// AND A LIFT OF THE JOB'S HOLD LEFT FOR A RE-FIRE'S END GOES WITH IT
// ([Coordinator.liftedDuring]): a claim that won came after the lift, so a
// hold its resume meets is a new one, which no lift has cleared, and a seat
// let go of is its next holder's.
func (c *Coordinator) forgetRefused(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropKeptLocked(turnID)
	delete(c.liftedDuring, turnID)
}

// forgetAdmitted drops what is kept for ev's run once a delivery of ev's
// completion has been admitted, for the claim the delivery goes on to make
// ([Coordinator.forgetRefused]). Save a keep for the hold on the row of ev's
// own job, short of the hold's instant ([Coordinator.keepHeldOnRow]): the store
// refuses that claim, and the keep is this node's one carrier of the job while
// the box host's store is silent. A claim that wins drops it
// ([Coordinator.completeAdmitted]).
func (c *Coordinator) forgetAdmitted(ev types.ExecutorFinished) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if kept, ok := c.refused[ev.TurnID]; ok && kept.ev.LaunchID == ev.LaunchID &&
		c.now().Before(kept.heldOnRow) {
		return
	}
	c.dropKeptLocked(ev.TurnID)
}

// keepHeldClaim keeps ev's completion where a claim of it under fence the store
// refused finds nothing kept here for ev's job and the row still running the
// job under no newer lease: for the row's hold where it holds the job's resume
// ([PendingRun.Held], [Coordinator.keepHeldOnRow]), against seen; and for the
// backstop where it no longer does, or the store did not answer
// ([Coordinator.keepAgain]). trigger is the completion's event. A refused claim
// reads the row because the keep made where the hold was written may have been
// replaced. See the package doc, "Kept completions".
func (c *Coordinator) keepHeldClaim(ctx context.Context, ev types.ExecutorFinished, trigger *events.Event,
	fence Fence, seen map[signal]uint64,
) {
	c.mu.Lock()
	kept, ok := c.refused[ev.TurnID]
	c.mu.Unlock()
	if ok && kept.ev.LaunchID == ev.LaunchID {
		return
	}
	run, found, err := c.pending.Get(ctx, ev.TurnID)
	switch {
	case err != nil:
		log.InfoContext(ctx, "sandbox_completion_hold_unread",
			"turn_id", ev.TurnID, "launch_id", ev.LaunchID, "agent", ev.AgentHandle, "error", err.Error(),
			"detail", "a claim of the completion settled nothing and the run's row could not be read for a "+
				"hold on it; this node keeps the completion for its backstop")
		c.keepAgain(refusedCompletion{ev: ev, trigger: trigger})
	case !found || run.LaunchID != ev.LaunchID || run.Status != StatusRunning || superseded(run, fence):
		// NOT THIS DELIVERY'S ANY MORE: another job, this one claimed or
		// settled, or a newer lease's.
	case run.Held(c.now()):
		c.keepHeldOnRow(run, trigger, Refusal{Condition: run.HeldFor, Until: run.HeldUntil}, run.Holds, seen,
			false)
	default:
		c.keepAgain(refusedCompletion{ev: ev, trigger: trigger})
	}
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
		c.dropKeptLocked(turnID)
		if !kept.heldOnRow.IsZero() {
			c.liftAndRefireLocked(kept)
			continue
		}
		c.refireLocked(kept)
	}
}

// liftAndRefireLocked lifts the hold on the row of a completion kept for it
// ([refusedCompletion.heldOnRow]) and then re-fires it, on a goroutine of the
// coordinator's own lifetime, which [Coordinator.Stop] waits for. The caller
// holds c.mu and has checked the coordinator is not closed.
func (c *Coordinator) liftAndRefireLocked(kept refusedCompletion) {
	c.inflight.Add(1)
	c.refiringLocked(kept.ev.TurnID)
	go func() {
		defer c.inflight.Done()
		defer c.refired(kept.ev.TurnID)
		c.liftAndRefire(kept)
	}()
}

// liftAndRefire is a signalled re-fire of a completion whose resume is held on
// its run's row: the hold lifted first ([PendingStore.LiftHold]), since the
// claim refuses a held row, and the completion re-fired once that lands
// ([Coordinator.refire]). A lift the store does not answer leaves the
// completion kept for the hold's instant ([Coordinator.keepUnlessKept]). Its
// lifetime is the coordinator's, for the reason [Coordinator.refire] gives.
//
//nolint:contextcheck // the coordinator's own lifetime (c.life), never a caller's; see [Coordinator.refire]
func (c *Coordinator) liftAndRefire(kept refusedCompletion) {
	ctx := c.life
	if ctx.Err() != nil {
		return
	}
	if _, leased := c.leaseOf(kept.ev.AgentHandle); !leased {
		return
	}
	lifted, err := c.pending.LiftHold(ctx, kept.ev.TurnID, kept.cond, kept.hold)
	if err != nil {
		log.WarnContext(ctx, "sandbox_hold_lift_failed", "turn_id", kept.ev.TurnID,
			"agent", kept.ev.AgentHandle, "condition", string(kept.cond), "error", err.Error(),
			"detail", "the completion stays kept on this node for the hold's instant, when it is re-fired")
		c.keepUnlessKept(kept)
		return
	}
	if lifted {
		log.InfoContext(ctx, "sandbox_resume_hold_lifted", "turn_id", kept.ev.TurnID,
			"agent", kept.ev.AgentHandle, "condition", string(kept.cond))
	}
	kept.heldOnRow = time.Time{}
	c.refire(kept)
}

// refireLifted re-fires the completion of run's job once a signal's lift of the
// hold on its row for cond has landed ([Coordinator.liftHoldsLocked]), on a
// goroutine of the coordinator's lifetime: the hold spent the delivery, so the
// lift must re-fire. Not a job this node already carries
// ([Coordinator.carriedLocked]), save a keep for the very hold this lift
// cleared, which is re-fired in its place ([Coordinator.refireHeldKeepLocked]),
// so one re-fire is on its way at a time. See the package doc, "Kept
// completions".
func (c *Coordinator) refireLifted(run PendingRun, cond Condition) {
	ev, trigger := announcementOf(run)
	lifted := rowHold{cond: cond, number: run.Holds}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	switch _, kept := c.refused[run.TurnID]; {
	case c.refiring[run.TurnID] > 0:
		if c.liftedDuring == nil {
			c.liftedDuring = map[string]rowHold{}
		}
		c.liftedDuring[run.TurnID] = lifted
	case kept:
		c.refireHeldKeepLocked(run.TurnID, lifted)
	default:
		c.refireLocked(refusedCompletion{cond: cond, ev: ev, trigger: trigger})
	}
}

// rowHold names one hold on a run's row: its condition and its number
// ([PendingRun.Holds]).
type rowHold struct {
	cond   Condition
	number uint64
}

// refireHeldKeepLocked re-fires, in its keep's place, a completion this node
// keeps for the hold on its run's row that a lift has cleared
// ([refusedCompletion.heldOnRow]): no longer waiting for the hold's instant,
// which the lift ended. A keep for anything else, a newer hold included, is
// left to carry the job ([Coordinator.refireLifted]). The caller holds c.mu
// and has checked the coordinator is not closed.
func (c *Coordinator) refireHeldKeepLocked(turnID string, lifted rowHold) {
	kept, ok := c.refused[turnID]
	if !ok || kept.heldOnRow.IsZero() || kept.cond != lifted.cond || kept.hold != lifted.number {
		return
	}
	c.dropKeptLocked(turnID)
	kept.heldOnRow = time.Time{}
	c.refireLocked(kept)
}

// refireLocked re-fires one kept completion on a goroutine of the
// coordinator's own lifetime, which [Coordinator.Stop] waits for. The caller
// holds c.mu and has checked the coordinator is not closed.
func (c *Coordinator) refireLocked(kept refusedCompletion) {
	c.inflight.Add(1)
	c.refiringLocked(kept.ev.TurnID)
	go func() {
		defer c.inflight.Done()
		defer c.refired(kept.ev.TurnID)
		c.refire(kept)
	}()
}

// refiringLocked counts a re-fire of turnID's job on its way
// ([Coordinator.refiring]). The caller holds c.mu.
func (c *Coordinator) refiringLocked(turnID string) {
	if c.refiring == nil {
		c.refiring = map[string]int{}
	}
	c.refiring[turnID]++
}

// refired counts a re-fire of turnID's job off once it has returned
// ([Coordinator.refiringLocked]), and, the last of the job's re-fires done,
// takes up a signal's lift of the job's hold that landed while they were on
// their way ([Coordinator.liftedDuring]): the job re-fired at once where they
// left it kept for that hold, a keep made against a read of the row from
// before the lift, which waited for the hold's instant once the hold was gone
// ([Coordinator.refireHeldKeepLocked]). A claim of the job that won meanwhile
// has dropped the lift ([Coordinator.forgetRefused]).
func (c *Coordinator) refired(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refiring[turnID]--; c.refiring[turnID] > 0 {
		return
	}
	delete(c.refiring, turnID)
	hold, lifted := c.liftedDuring[turnID]
	delete(c.liftedDuring, turnID)
	if lifted && !c.closed {
		c.refireHeldKeepLocked(turnID, hold)
	}
}

// carriedLocked reports whether this node already carries turnID's job: it
// keeps a completion of it ([Coordinator.refused]) or a re-fire of it is on its
// way ([Coordinator.refiring]). A signal's lift or a recovery's keep or re-arm
// starts no re-fire where it does, since a second re-fire claims, reads and
// keeps for a job the first carries; save that a lift re-fires a keep for the
// hold it cleared ([Coordinator.refireHeldKeepLocked]). The caller holds c.mu.
// See the package doc, "Kept completions".
func (c *Coordinator) carriedLocked(turnID string) bool {
	_, kept := c.refused[turnID]
	return kept || c.refiring[turnID] > 0
}

// refire offers a kept completion to [Coordinator.OnCompleted] again, under the
// coordinator's own lifetime. A refusal keeps it again, and so does any other
// failure ([Coordinator.keepAgain]). Where its own attempt started this node's
// clock on the box host's store ([startingClock]), its completion is announced
// afresh at once (renew.go, [refirePublish]); not where the clock ran before,
// another attempt started it, or its attempt settled the run. Its lifetime is
// c.life, ended by [Coordinator.Stop], since it outlives its caller. See the
// package doc, "Kept completions".
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
	case errors.Is(err, ErrSeatRecovering):
		// REFUSED BY THE SEAT'S RECOVERY GATE, whose pass runs here: handed
		// to the pass, which re-fires it when it is done (recoverygate.go).
		// Kept for the backstop instead, the job waited five minutes for a
		// pass of a second or so.
		c.carryPastRecovery(ctx, kept.ev.AgentHandle, passCarry{refire: &kept})
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
				"the waiter announces it again on its schedule, and this node re-fires it at its backstop")
		c.keepAgain(kept)
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
