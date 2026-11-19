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
// A seat's inbox is on an interest-retention stream: an event published to a
// subject no consumer covers is discarded and the publisher told it succeeded.
// A mailbox is two consumers, the inbox's and the priority lane's
// ([queue.HasLane]), and until both exist a wake is lost silently.
//
// The boot makes every mailbox before it starts a producer and an apply makes a
// revision's before it routes to them, but a feed replaying at boot, a create
// the broker refused or a mailbox not yet reached would still race. So the
// guarantee is held where a wake is sent too ([Node.Wakes]).

// Wakes is the node's queue for publishing a seat's wakes: every verb is the
// queue's own, and Publish to a seat's inbox reaches a complete mailbox or is
// refused with [queue.ErrUnavailable] rather than handed to a broker that would
// discard it. Producers retry, not drop: the inbound router and a park NAK; the
// scheduler and agent-to-agent asks call [Node.ConfirmMailbox] BEFORE spending a
// claim or opening a channel, so a refusal leaves nothing half done.
//
// THE CONFIRMED SET IS THIS NODE'S MEMORY ([mailboxes]): a confirmed seat costs
// a lookup, no broker call; it is pruned to the revision served on every pass.
// A topic that is no seat's inbox, and a seat outside the company (its mailbox
// is the retirement sweep's to own), are published as the queue would.
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
// the node cannot say it is, as [queue.ErrUnavailable]. A seat outside the
// company the node serves answers nil: not its mailbox to make.
//
// EXPORTED FOR THE PRODUCERS THAT SPEND SOMETHING BEFORE THEY PUBLISH: the
// scheduler claims a fire and the agent-to-agent service opens a channel, and a
// refusal after either would strand them.
//
// THE WHOLE PASS, not a create for this seat alone: the one place that
// registers before creating, reads the broker's listing rather than this node's
// memory, and counts a mailbox missing its lane as missing
// ([queue.Subscription.Partial]). Serialised: a wake waits for a pass in flight.
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
