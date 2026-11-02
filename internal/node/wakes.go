package node

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// A WAKE REACHES A MAILBOX OR IS REFUSED, never dropped on the way.
//
// A seat's inbox lives on an interest-retention stream: an event published to a
// subject no consumer covers is discarded by the broker the moment it is
// stored, and the publisher is told it succeeded. A seat's mailbox is two such
// consumers, its inbox's and its priority lane's ([queue.HasLane]), and until
// both exist a wake published to it is lost with nothing anywhere to say so.
//
// The mailbox pass ([Node.EnsureMailboxes]) is what makes them exist, and on
// its own it could only be a race against the producers: a node boots its
// scheduler, its change feeds and its inbound consumer before the pass has
// run, and a feed replaying a person's message at boot publishes a priority
// wake onto a lane that is not there yet. Ordering the boot so the pass runs
// first would close that one window and leave the rest: a seat whose create
// the broker refused on the pass, and a mailbox the pass has not reached yet.
//
// So the boot makes every mailbox before it starts a producer, and an apply
// makes a revision's before it can route to them; and the guarantee is held
// where a wake is sent as well, as the backstop for a create the broker
// refused. Every producer of a seat's wake (the inbound router, the scheduler,
// the agent-to-agent asks, a park) publishes through [Node.Wakes], and a
// publish to a seat this node has not confirmed a complete mailbox for runs
// the pass first. A mailbox it still cannot confirm refuses the publish with
// [queue.ErrUnavailable] instead of handing the wake to a broker that would
// discard it, and each producer retries rather than drops: the inbound router
// NAKs the delivery, so the broker redelivers it; a park's NAK returns the
// partition; and the scheduler and the agent-to-agent asks ask
// [Node.ConfirmMailbox] BEFORE they spend a claim or open a channel, so a
// refusal leaves nothing half done for the retry to trip over.
//
// THE CONFIRMED SET IS THIS NODE'S MEMORY ([mailboxes]): once a seat is in it,
// a wake costs a map lookup and no broker call. It is keyed on the seat's id,
// which is what the mailbox is named by, so no revision can give a seat a
// different mailbox; and it answers for the revision being served, because
// every pass prunes it to that revision's seats and an apply runs one, so a
// seat a revision removed and a later one restored is confirmed again.

// Wakes is the node's queue for publishing a seat's wakes: every verb is the
// queue's own, and Publish to a seat's inbox reaches a complete mailbox or is
// refused.
//
// A seat this node has confirmed costs one lookup in its own set and no broker
// call. A topic that is not a seat's inbox, and a seat that is not in the
// company, are published exactly as the queue would: a seat outside the
// company is one whose mailbox the retirement sweep owns, and recreating it
// from a publish would undo that sweep.
func (n *Node) Wakes() queue.EventQueue {
	return wakes{EventQueue: n.cfg.Queue, n: n}
}

// wakes is [Node.Wakes]: the node's queue with the mailbox check on Publish.
type wakes struct {
	queue.EventQueue
	n *Node
}

// Publish confirms the seat's mailbox before the wake is sent to it.
func (w wakes) Publish(ctx context.Context, topic string, ev *events.Event) error {
	if seat, ok := topics.SeatFromInbox(topic); ok {
		if err := w.n.ConfirmMailbox(ctx, seat); err != nil {
			return err
		}
	}
	return w.EventQueue.Publish(ctx, topic, ev)
}

// ConfirmMailbox returns once the seat's mailbox is complete, or reports why
// the node cannot say it is, with an error that is [queue.ErrUnavailable]. A
// seat outside the company the node serves is not this node's to make a
// mailbox for, and answers nil.
//
// EXPORTED FOR THE PRODUCERS THAT SPEND SOMETHING BEFORE THEY PUBLISH: the
// scheduler claims a fire and the agent-to-agent service opens a channel, and
// a refusal after either would leave a claimed fire no tick retries or a
// channel nobody is woken for. Asked first, a refusal leaves nothing behind.
//
// THE WHOLE PASS, not a create for this seat alone: it is the one place that
// registers a mailbox before creating it, reads the broker's own listing
// rather than this node's memory, and treats a mailbox missing its lane as
// missing ([queue.Subscription.Partial]). Serialised with every other pass, so
// a wake waits for one already in flight instead of proposing the same create
// beside it, and a pass with nothing missing makes no broker call.
func (n *Node) ConfirmMailbox(ctx context.Context, id uuid.UUID) error {
	if n.mail.has(id) {
		return nil
	}
	// A SEAT OUTSIDE THE COMPANY IS ANSWERED BEFORE ANY PASS. Its mailbox is
	// the retirement sweep's, not this node's to make, so there is nothing
	// to confirm; and a pass on its behalf would cost a broker listing per
	// wake (a feed's backlog for a removed seat is one wake per record), each
	// serialised under the lock every other wake's confirmation waits on.
	company := n.cfg.Seats()
	seat, ok := seatNamed(addressable(company), id)
	if !ok {
		return nil
	}
	n.ensureMailboxes(ctx, company)
	if n.mail.has(id) {
		return nil
	}
	return fmt.Errorf("node: the mailbox of seat %q is not complete, so its wake was not "+
		"published; publish it again once the broker answers: %w", seat.Handle, queue.ErrUnavailable)
}

// seatNamed is the seat of seats whose id is id, and false for none.
func seatNamed(seats []placement.Seat, id uuid.UUID) (placement.Seat, bool) {
	for _, s := range seats {
		if s.ID == id {
			return s, true
		}
	}
	return placement.Seat{}, false
}
