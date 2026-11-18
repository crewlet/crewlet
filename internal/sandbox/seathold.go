package sandbox

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/queue"
)

// The seat's inbox is held while a run holds the seat, or an answer is owed: a
// pause hold under one reason ([SeatHold]) for as long as the seat has a run in
// [Holding] or owes a resume or an ending's finish, lifted the moment it has
// neither, at the transition itself. Held, the mail waits on the broker in
// order at no cost; a delivery that reaches a held seat anyway is deferred. A
// seat busy coding takes no other work, a person's answer to another run
// included; an answer by turn travels on the control topic and its resume waits
// ([SeatCoding]). This node's alone; each hold names its inbox attachment
// ([queue.Attachment]). See the package doc, "The seat's inbox hold".

// SeatHold takes and lifts the hold on a seat's inbox (see the package doc,
// "The seat's inbox hold", for when). Declared here and implemented by the
// engine, which owns the inbox and the hold's name. Hold is called when a seat
// starts to need it and Release when it stops, once each, since holds are keyed
// by reason. Called with the seat's handle and implemented on its id, a pure
// function of the handle (ADR-0013). Each names the attachment the hold is for:
// Hold refuses an ended one with [queue.ErrAttachmentGone], and Release under
// one lifts nothing.
type SeatHold interface {
	Hold(ctx context.Context, handle string, inbox queue.Attachment) error
	Release(ctx context.Context, handle string, inbox queue.Attachment) error
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
	ctx, release := detach(ctx)
	defer release()
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
		// THE ATTACHMENT THIS DECISION IS FOR, read with it: a release that
		// lands before the queue call ends it, and the queue then refuses
		// the hold rather than carry it onto the next attachment.
		inbox := c.inboxes[handle]
		c.mu.Unlock()

		var err error
		if want {
			err = c.hold.Hold(ctx, handle, inbox)
		} else {
			err = c.hold.Release(ctx, handle, inbox)
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
		if errors.Is(err, queue.ErrAttachmentGone) {
			// THE ATTACHMENT HAS ENDED: the seat's release detached it, or
			// this node holds no attachment of the seat yet. Nothing is
			// held, and nothing needs to be: the detach dropped every hold,
			// and the seat's next recovery takes the hold its own attachment
			// needs. Decided again if the seat was forgotten meanwhile.
			log.DebugContext(ctx, "sandbox_seat_hold_attachment_ended", "agent", handle,
				"attachment", uint64(inbox))
			if !dirty {
				return
			}
			continue
		}
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

// forgetHold drops what this node believes about a seat's hold, and the
// attachment it was taken for, for a seat it has released: the release
// DETACHED the inbox, and a detach drops every hold the attachment carried, so
// there is nothing to lift.
//
// A GENERATION, because a reconcile may be out at the queue as this runs: left
// to finish, it would record a hold on an attachment that no longer exists,
// and a seat re-acquired here would then be believed held and never be. What
// that reconcile asked of the queue was for the ended attachment, so the queue
// refused it and it gates nothing.
func (c *Coordinator) forgetHold(handle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.held, handle)
	delete(c.inboxes, handle)
	c.holdGen[handle]++
}

// attachInbox records the attachment of a seat's inbox this node is taking the
// seat under, which every hold of it is taken for from here
// ([Coordinator.RecoverSeat]).
func (c *Coordinator) attachInbox(handle string, inbox queue.Attachment) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inboxes[handle] = inbox
}
