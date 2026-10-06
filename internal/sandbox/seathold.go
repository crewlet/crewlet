package sandbox

import "context"

// THE SEAT'S INBOX IS HELD WHILE A RUN HOLDS THE SEAT, OR AN ANSWER IS OWED.
//
// # What used to spin
//
// A seat a detached run held took no new turn, and the inbox screening PARKED
// every delivery that reached it: requeued it onto the seat's own inbox and
// acked the original. Nothing stopped the consumer, so the copy was the next
// thing it fetched — immediately, since a pull consumer fetches again the
// moment its handler returns — and it was parked again. For the length of the
// run (hours, for a real coding job) every message waiting on a busy seat went
// round that loop at whatever rate the broker would serve: a fetch, a
// screening, a store read for the answer offer, a replicated publish (and the
// event-store write the publish listener makes for it) and an ack, per message
// per lap. Nothing was lost — the same-id dedupe and the completion ledger
// held — but a seat parked on a long run was the busiest thing on its node,
// doing nothing.
//
// # What happens instead
//
// The coordinator holds the seat's inbox — a pause hold under ONE reason,
// [SeatHold] — for exactly as long as the seat has a run in [Holding] or owes
// one of its runs a recorded answer's resume, and lifts it the moment it has
// neither. Held, the consumer fetches nothing: the mail waits on the broker
// with its order and its delivery count intact, costs nothing while it waits,
// and is delivered in order when the hold lifts. Both conditions are this
// node's own counts ([seatRuns], the owed set), moved by the coordinator's own
// transitions, so the hold follows them at the transition itself rather than
// at whichever delivery happens to arrive next: a park on a question, a run
// that settles, a resume that frees the seat — and a seat recovered mid-run,
// whose hold is taken in its preparation, before its mailbox is attached.
//
// A delivery that reaches a held seat anyway — one that raced the hold, or a
// hold the queue refused — is DEFERRED by the screening, which asks for the
// hold again first ([Coordinator.HoldSeat]). A deferral keeps the delivery at
// the head of the inbox at the cost of one of its deliveries, once per hold
// rather than once per lap.
//
// # What it costs
//
// A HELD SEAT RECEIVES NOTHING, a person's answer to ANOTHER of its runs
// included. The answer was offered on a held seat before, beside the park, so
// a seat driving one job could resume a second that a person had just
// answered. Keeping that would have meant consuming the held seat's mail to
// look for the answer, and a consumed message that is not the answer has
// nowhere to go but back onto the inbox — the loop this replaces. So the
// answer waits on the inbox with everything else and is the first thing the
// seat is offered when its job settles or parks: a seat runs one coding job at
// a time, and a person who answers a second question while the first job is
// still going is answered when it is done. Nothing is lost: the reply keeps its
// place, the question keeps its anchor, and a box that outlives
// `pause_ttl_seconds` meanwhile re-seeds from its branch when the answer
// arrives.
//
// WHO HAS TO AGREE ON IT: this node alone. The hold lives in this process's
// queue client, keyed on the subscription, and is derived from this node's
// counts of the seat's runs. A detach drops it with the attachment, and the
// seat's next holder takes its own from its recovery pass.

// SeatHold takes and lifts the hold on a seat's inbox — see the top of this
// file for when.
//
// DECLARED HERE AND IMPLEMENTED BY THE ENGINE, which owns the seat's inbox and
// the name the hold is taken under. The coordinator calls Hold when a seat
// starts to need it and Release when it stops, once each: holds are keyed by
// reason, so a second Hold would be the same hold and a Release the last of
// it.
type SeatHold interface {
	Hold(ctx context.Context, handle string) error
	Release(ctx context.Context, handle string) error
}

// wantsHoldLocked reports whether a seat's inbox must be held: a run holds the
// seat, or the seat owes one of its runs an answer's resume. The caller holds
// c.mu.
func (c *Coordinator) wantsHoldLocked(handle string) bool {
	return c.runs[handle].holding > 0 || len(c.owed[handle]) > 0
}

// HoldSeat brings a seat's inbox hold into line with what the seat needs, for
// a delivery that reached a seat this node counts as held: one that raced the
// hold, or one the queue let through because the hold was refused. Asking
// again is the retry a refused hold gets, at the spacing a deferral imposes.
func (c *Coordinator) HoldSeat(ctx context.Context, handle string) {
	c.reconcileHold(ctx, handle)
}

// reconcileHold brings a seat's inbox hold into line with what it needs.
//
// ONE RECONCILER PER SEAT AT A TIME, and it loops until what it applied is
// still what is wanted. The queue call cannot be made under the lock — the
// in-memory twin's release drains the inbox synchronously into handlers that
// come straight back here — and two callers each applying their own decision
// outside it could land in either order, leaving a seat held with nothing to
// hold it for or needing a hold it does not have. So a caller that finds a
// reconcile in progress marks the seat dirty and leaves, and the one in
// progress goes round again.
func (c *Coordinator) reconcileHold(ctx context.Context, handle string) {
	if c.hold == nil || handle == "" {
		return
	}
	// A hold is the seat's, not the request's: one taken or lifted on a
	// context a drain is cancelling would do nothing at all.
	ctx = context.WithoutCancel(ctx)
	for {
		c.mu.Lock()
		if c.holdBusy[handle] {
			c.holdDirty[handle] = true
			c.mu.Unlock()
			return
		}
		want := c.wantsHoldLocked(handle)
		if want == c.held[handle] {
			c.mu.Unlock()
			return
		}
		c.holdBusy[handle] = true
		gen := c.holdGen[handle]
		c.mu.Unlock()

		var err error
		if want {
			err = c.hold.Hold(ctx, handle)
		} else {
			err = c.hold.Release(ctx, handle)
		}

		c.mu.Lock()
		delete(c.holdBusy, handle)
		dirty := c.holdDirty[handle]
		delete(c.holdDirty, handle)
		// A SEAT FORGOTTEN WHILE THE CALL WAS OUT ([Coordinator.forgetHold])
		// was detached under it, so what the call did describes an
		// attachment that is gone: nothing is recorded, and the loop
		// decides again from "not held".
		forgotten := c.holdGen[handle] != gen
		dirty = dirty || forgotten
		if err == nil && !forgotten {
			if want {
				c.held[handle] = true
			} else {
				delete(c.held, handle)
			}
		}
		c.mu.Unlock()
		if err != nil {
			// Logged and left: a hold that could not be taken lets the
			// seat's mail reach the screening, which defers it and asks
			// for the hold again (see [Coordinator.HoldSeat]); one that
			// could not be lifted is lifted by the seat's next detach,
			// which releases every hold its inbox carries. Retrying a
			// queue call that just refused, from a loop that may be
			// holding a delivery, is worse than either.
			log.WarnContext(ctx, "sandbox_seat_hold_failed", "agent", handle,
				"hold", want, "error", err.Error())
			if !dirty {
				return
			}
		}
	}
}

// forgetHold drops what this node believes about a seat's hold, for a seat it
// has released: the release DETACHED the inbox, and a detach drops every hold
// the attachment carried, so there is nothing to lift — and a Release from here
// would act on the subscription this node takes its own hold on again the
// moment it re-acquires the seat.
//
// A GENERATION, because a reconcile may be out at the queue as this runs: left
// to finish, it would record a hold on an attachment that no longer exists,
// and a seat re-acquired here would then be believed held and never be.
func (c *Coordinator) forgetHold(handle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.held, handle)
	c.holdGen[handle]++
}
