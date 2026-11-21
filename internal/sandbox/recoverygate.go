package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// A seat's recovery pass is exclusive with every claim of the seat's runs on
// this node: the pass reaps or revives a claim it finds, which is right only
// where no attempt of this node's holds it, and the node's admission does not
// keep every claim out. So every claim passes one gate
// ([Coordinator.claimGateLocked]), refused with [ErrSeatRecovering] while a
// pass runs and counted as a live attempt otherwise
// ([Coordinator.liveHereLocked]); the mark comes before the control topic is
// attached. A refused claim is never lost: a delivery is NAK'd, a re-fire, a
// backstop or an owed answer is carried past the pass
// ([Coordinator.carryPastRecovery]). See the package doc, "The seat's recovery
// gate".

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
// once, on every way out. See the package doc, "The seat's recovery gate".
// The caller holds c.mu.
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
