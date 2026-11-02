package node

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
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
// So the guarantee is made where a wake is sent. Every producer of a seat's
// wake (the inbound router, the scheduler, the agent-to-agent asks) publishes
// through [Node.Wakes], and a publish to a seat this node has not confirmed a
// complete mailbox for runs the pass first. A mailbox it still cannot confirm
// refuses the publish with [queue.ErrUnavailable], which each producer already
// answers by trying again (the inbound router NAKs the delivery, so the broker
// redelivers it), instead of handing the wake to a broker that would discard
// it.

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
		if err := w.n.awaitMailbox(ctx, seat); err != nil {
			return err
		}
	}
	return w.EventQueue.Publish(ctx, topic, ev)
}

// awaitMailbox returns once the seat's mailbox is complete, or reports why the
// node cannot say it is.
//
// THE WHOLE PASS, not a create for this seat alone: it is the one place that
// registers a mailbox before creating it, reads the broker's own listing
// rather than this node's memory, and treats a mailbox missing its lane as
// missing ([queue.Subscription.Partial]). Serialised with every other pass, so
// a wake waits for one already in flight instead of proposing the same create
// beside it, and a pass with nothing missing makes no broker call.
func (n *Node) awaitMailbox(ctx context.Context, id uuid.UUID) error {
	if n.mail.has(id) {
		return nil
	}
	company := n.cfg.Seats()
	n.ensureMailboxes(ctx, company)
	if n.mail.has(id) {
		return nil
	}
	for _, s := range addressable(company) {
		if s.ID == id {
			return fmt.Errorf("node: the mailbox of seat %q is not complete, so its wake "+
				"was not published; publish it again once the broker answers: %w",
				s.Handle, queue.ErrUnavailable)
		}
	}
	return nil
}
