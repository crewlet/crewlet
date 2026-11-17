package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// A SEAT'S RECOVERY PASS IS EXCLUSIVE WITH EVERY CLAIM OF THE SEAT'S RUNS ON
// THIS NODE.
//
// The pass ([Coordinator.RecoverSeat]) fences each run of the seat to this
// node's lease, reads it again and takes it over as it found it: a claim it
// finds, a resumed tail, is one whose node stopped, and it is reaped as
// abandoned ([Coordinator.reapTail]), or its answer revived. That reading is
// right only where no process of this node's holds the claim. The acquisition
// made it so by the node's own admission: a seat still establishing admits no
// turn ([seat.Host.MayStart]), so no completion or answer of the seat's was
// claimed while its recovery ran. But not every claim asks that admission, and
// not every pass runs inside the acquisition:
//
//   - a completion past its turn's deadline is claimed without it
//     ([Coordinator.completeAtDeadline]), and is claimed precisely because the
//     admission refused it;
//   - an apply that brings the code-work runtime up on a seat already held
//     runs the seat's recovery with the seat established, so a delivery of a
//     completion, a re-fire of one on a signal, or an owed answer's attempt is
//     admitted and claims while the pass runs.
//
// Each such claim the pass then fenced, read again as resumed, and reaped as
// abandoned while this node's own attempt was collecting or resuming it: an
// ending decided under a live attempt, which that attempt, finishing, kept
// retrying, and a resumed turn ended under the review that was still running.
//
// # The rule, at one place
//
// So every claim of a seat's run on this node passes ONE gate before the store
// is asked ([Coordinator.claimGateLocked]): a completion's claim
// ([Coordinator.claimCompletion]), for its resume or its ending at the
// deadline, whichever announcement carried it (a delivery, a signal's re-fire,
// a re-fire armed by a recovery's re-arm or keep, a held completion's
// backstop), and an owed answer's claim ([Coordinator.resumeOwedAdmitted]).
// While a recovery pass of the seat runs on this node the gate refuses the
// claim ([ErrSeatRecovering]): the acquisition's MayStart rule, carried to every
// claim. And the gate counts every claim it lets through as this node's live
// attempt on the run until the attempt returns, so the pass, which marks the
// seat in the same lock, never reaps a claim this node's own attempt holds
// ([Coordinator.liveHereLocked]): a claim let through before the mark is left
// to the attempt that holds it, and no claim is let through after it.
//
// THE MARK COMES BEFORE THE CONTROL TOPIC, so the pass has no window before it.
// The seat's control topic is attached inside the pass, once the seat is marked
// ([Coordinator.RecoverSeat]'s attach): attached before the pass, a delivery
// could be admitted and claim between the two, live when the pass read the row.
//
// # A claim the gate refuses is never lost
//
// Each road states who carries it:
//
//   - A DELIVERY is handed back to the queue as this node's failure
//     (internal/engine's controlOutcome NAKs [ErrSeatRecovering] as the node's),
//     and the broker brings it back on its backoff, past the pass.
//   - A RE-FIRE (a signal's, a lift's, a recovery's re-arm or keep, a kept
//     completion's own timer) is handed to the pass ([Coordinator.carryPastRecovery])
//     and re-fired at its end, before the pass re-arms and keeps from the rows
//     it read, which start no second re-fire of a job one is on its way for
//     ([Coordinator.carriedLocked]).
//   - A HELD COMPLETION'S BACKSTOP that found its turn past the deadline is
//     re-fired at the pass's end the same way, and the re-fire, still held here
//     and past the deadline, ends the run ([Coordinator.recheckHeld]).
//   - AN OWED ANSWER'S ATTEMPT is handed to the pass, and its series is armed
//     at once at the pass's end, the seat's mail held behind it meanwhile
//     ([Coordinator.resumeOwedAdmitted]).
//
// Handed over in the lock the pass ends in, so a refusal that lands as the pass
// ends is carried at once rather than appended to a pass that is over.

// ErrSeatRecovering reports a claim of a seat's run refused because a recovery
// pass of the seat is under way on this node ([Coordinator.RecoverSeat]). It is
// this node's condition, which clears when the pass ends, and never the
// message's: a delivery refused on it is handed back as the node's failure.
var ErrSeatRecovering = errors.New("sandbox: the seat's recovery pass is under way on this node, " +
	"which claims none of its runs until the pass is done")

// seatRecovery is a recovery pass of one seat under way on this node: how many
// are (one per holding; counted rather than flagged, so a second pass, which
// nothing makes, cannot end the first one's mark), and the claims the gate
// refused meanwhile that the pass carries at its end ([passCarry]). Under mu.
type seatRecovery struct {
	passes  int
	carried []passCarry
}

// passCarry is one claim the recovery gate refused that the pass carries once
// it is done: a completion to re-fire, or an owed answer whose series to arm.
type passCarry struct {
	refire *refusedCompletion
	answer *PendingRun
}

// liveAttempt counts the claims of one run the recovery gate let through and
// whose attempts have not returned, and names the seat they are of. Under mu.
type liveAttempt struct {
	handle string
	n      int
}

// beginRecovery marks handle's seat as recovering on this node: from here every
// claim of its runs is refused ([Coordinator.claimGateLocked]) until
// [Coordinator.endRecovery].
func (c *Coordinator) beginRecovery(handle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.recovering[handle]
	if r == nil {
		r = &seatRecovery{}
		c.recovering[handle] = r
	}
	r.passes++
}

// endRecovery ends a recovery pass of handle's seat begun by
// [Coordinator.beginRecovery], and, the last pass done, carries every claim the
// gate refused while it ran: each completion re-fired, each owed answer's series
// armed at once ([Coordinator.carryLocked]).
func (c *Coordinator) endRecovery(ctx context.Context, handle string) {
	c.mu.Lock()
	r := c.recovering[handle]
	if r == nil {
		c.mu.Unlock()
		return
	}
	if r.passes--; r.passes > 0 {
		c.mu.Unlock()
		return
	}
	delete(c.recovering, handle)
	held := false
	for _, carry := range r.carried {
		held = c.carryLocked(carry) || held
	}
	c.mu.Unlock()
	if held {
		c.reconcileHold(ctx, handle)
	}
	if len(r.carried) > 0 {
		log.InfoContext(ctx, "sandbox_recovery_claims_carried",
			"seat", handle, "carried", len(r.carried),
			"detail", "claims of the seat's runs refused while its recovery pass ran are carried now that it "+
				"is done: each completion re-fired, each owed answer's next attempt armed")
	}
}

// claimGateLocked is the one gate every claim of a seat's run on this node
// passes before the store is asked: refused with [ErrSeatRecovering] while a
// recovery pass of handle's seat runs here, and otherwise counted as this
// node's live attempt on turnID until the attempt calls the done it returns,
// once, on every way out. See the top of this file. The caller holds c.mu.
func (c *Coordinator) claimGateLocked(handle, turnID string) (done func(), err error) {
	if c.recovering[handle] != nil {
		return nil, fmt.Errorf("sandbox: claiming run %s of seat %q: %w", turnID, handle, ErrSeatRecovering)
	}
	a := c.live[turnID]
	if a == nil {
		a = &liveAttempt{handle: handle}
		c.live[turnID] = a
	}
	a.n++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if a.n--; a.n <= 0 && c.live[turnID] == a {
				delete(c.live, turnID)
			}
		})
	}, nil
}

// liveHereLocked reports whether an attempt of this node's that the recovery
// gate let through still holds turnID's run ([Coordinator.claimGateLocked]). The
// caller holds c.mu.
func (c *Coordinator) liveHereLocked(turnID string) bool {
	return c.live[turnID] != nil
}

// liveHere is [Coordinator.liveHereLocked] for a caller not holding c.mu.
func (c *Coordinator) liveHere(turnID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.liveHereLocked(turnID)
}

// carryPastRecovery hands a claim the recovery gate refused to the pass of
// handle's seat that refused it, to carry at its end ([Coordinator.endRecovery]);
// or carries it at once where that pass ended since the refusal. In the one
// lock the pass ends in, so a refusal is carried exactly once, whichever comes
// first.
func (c *Coordinator) carryPastRecovery(ctx context.Context, handle string, carry passCarry) {
	c.mu.Lock()
	if r := c.recovering[handle]; r != nil {
		r.carried = append(r.carried, carry)
		c.mu.Unlock()
		return
	}
	held := c.carryLocked(carry)
	c.mu.Unlock()
	if held {
		c.reconcileHold(ctx, handle)
	}
}

// carryLocked carries one refused claim ([passCarry]): a completion re-fired
// ([Coordinator.refireLocked]), or an owed answer's series recorded, owed and
// armed at once ([Coordinator.keepSeriesLocked]), as a recovery's own queue of
// an answered run arms it ([Coordinator.queueOwed]). It reports whether the
// seat's inbox hold has to follow, which the caller brings into line once it
// lets go of c.mu. The caller holds c.mu.
func (c *Coordinator) carryLocked(carry passCarry) (held bool) {
	if c.closed {
		return false
	}
	switch {
	case carry.refire != nil:
		c.refireLocked(*carry.refire)
	case carry.answer != nil:
		run := *carry.answer
		if _, ok := c.retries[run.TurnID]; !ok {
			c.retries[run.TurnID] = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID,
				first: c.now(), window: answerWindow(run)}
		}
		c.keepSeriesLocked(run, 0)
		return true
	}
	return false
}
