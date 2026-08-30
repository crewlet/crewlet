// Package a2a is agent-to-agent asking: one ask, one answer, then closed.
//
// THE CHANNEL IS AN AUTHORIZATION RECORD, NOT A TRANSPORT. Nothing queues
// here. The brief travels ON the wake event and the answer travels on the
// reply, both over the durable seat inbox, so a colleague owned by another
// node is a perfectly ordinary target.
//
// The in-process bus this replaces was a second delivery path beside the seat
// inbox, and a path that only works in one process cannot be a fast path for a
// fleet: the target woke on whichever node owned ITS seat and found the queue
// empty. There was also no reply path at all — the prompt named a tool that
// was never registered.
package a2a

import (
	"context"
	"errors"
	"time"
)

// Channel is one open ask.
type Channel struct {
	ID        string
	Requester string
	Target    string

	// Messages counts what crossed the channel. One ask and one answer is
	// the whole protocol, so a count above two is the anomaly it looks
	// like rather than ordinary traffic.
	Messages int

	OpenedAt time.Time
	LastAt   time.Time

	// ClosedAt is zero while open. A zero time rather than a status field,
	// so "when did it close" and "is it closed" cannot disagree.
	ClosedAt time.Time
}

// Open reports whether the channel still accepts messages.
func (c Channel) Open() bool { return c.ClosedAt.IsZero() }

// Participants is both parties, requester first.
func (c Channel) Participants() []string { return []string{c.Requester, c.Target} }

// OtherParty returns the participant that is not handle, and false when handle
// is not a participant at all.
//
// The two answers are distinct and the caller acts on the difference: a
// non-participant sending on a channel is an authorization failure, not an
// empty recipient.
func (c Channel) OtherParty(handle string) (string, bool) {
	switch handle {
	case c.Requester:
		return c.Target, true
	case c.Target:
		return c.Requester, true
	default:
		return "", false
	}
}

// Duration is how long the channel was, or has been, open.
func (c Channel) Duration(now time.Time) time.Duration {
	end := c.ClosedAt
	if end.IsZero() {
		end = now
	}
	return end.Sub(c.OpenedAt)
}

// Errors the service returns. Each of these is a silent drop if it is not
// reported, and a reply that goes nowhere is the failure the requester
// experiences as "they never answered".
var (
	// ErrNoChannel — the channel id is unknown.
	ErrNoChannel = errors.New("a2a: no such channel")
	// ErrClosed — the channel is closed.
	ErrClosed = errors.New("a2a: channel is closed")
	// ErrNotParticipant — the sender is not on this channel.
	ErrNotParticipant = errors.New("a2a: sender is not a participant")
	// ErrSelfChannel — a seat asked itself.
	ErrSelfChannel = errors.New("a2a: a channel has two parties")
	// ErrNotAnAgent — the target is a human seat or an unknown handle, so
	// it has no inbox and the wake would land on a topic nobody reads.
	ErrNotAnAgent = errors.New("a2a: target is not an agent seat")
)

// Store holds channels. A Postgres-shaped implementation and an in-memory twin
// answer to the same conformance suite.
type Store interface {
	// Open records a new channel.
	Open(ctx context.Context, ch Channel) error

	// Get returns the channel, or ErrNoChannel.
	Get(ctx context.Context, id string) (Channel, error)

	// Close marks the channel closed and returns its final state. Closing
	// an already-closed channel returns it unchanged rather than erroring:
	// both parties may close, and the second one is not a fault.
	Close(ctx context.Context, id string, at time.Time) (Channel, error)

	// CountMessage increments the message counter and bumps LastAt.
	CountMessage(ctx context.Context, id string, at time.Time) (int, error)

	// CloseIdle closes every open channel whose last activity predates
	// cutoff, returning what it closed.
	//
	// This is the sweep for the ask nobody answered: the requester's turn
	// ended when it asked, so without it an unanswered channel stays open
	// for the life of the deployment.
	CloseIdle(ctx context.Context, cutoff time.Time, at time.Time) ([]Channel, error)

	// Purge deletes channels closed before cutoff, returning the count.
	Purge(ctx context.Context, cutoff time.Time) (int64, error)
}
