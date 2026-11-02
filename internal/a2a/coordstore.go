package a2a

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// CoordStore is the channel store, on the FLEET's coordination store.
//
// The one implementation, and the node's own database is deliberately not an
// option. A channel is an authorization record read by the node that owns the
// ANSWERING seat — which is precisely the node that did not write it — so on a
// per-node store a cross-node ask woke its target and then dropped the reply
// with "no such channel". Two seats on one node worked, which is why it looked
// fine in every single-node deployment.
//
// A single-node company is not a special case here: it runs the in-memory
// coordination twin, which is a real implementation of the same contract
// rather than a stub. See internal/coord/fleet.go.
type CoordStore struct{ channels coord.Channels }

// NewCoordStore wraps the fleet's channel records.
func NewCoordStore(channels coord.Channels) *CoordStore {
	return &CoordStore{channels: channels}
}

var _ Store = (*CoordStore)(nil)

// Open records a new channel.
func (s *CoordStore) Open(ctx context.Context, ch Channel) error {
	if ch.ID == "" {
		return fmt.Errorf("a2a: cannot open a channel with no id")
	}
	if err := s.channels.OpenChannel(ctx, toCoord(ch)); err != nil {
		return fmt.Errorf("a2a: open channel %s: %w", ch.ID, err)
	}
	return nil
}

// Get returns one channel by key.
func (s *CoordStore) Get(ctx context.Context, id string) (Channel, error) {
	ch, found, err := s.channels.Channel(ctx, id)
	if err != nil {
		return Channel{}, fmt.Errorf("a2a: read channel %s: %w", id, err)
	}
	if !found {
		return Channel{}, ErrNoChannel
	}
	return fromCoord(ch), nil
}

// Close marks the channel closed and returns its stored state.
func (s *CoordStore) Close(ctx context.Context, id string, at time.Time) (Channel, error) {
	ch, found, err := s.channels.CloseChannel(ctx, id, at)
	if err != nil {
		return Channel{}, fmt.Errorf("a2a: close channel %s: %w", id, err)
	}
	if !found {
		return Channel{}, ErrNoChannel
	}
	return fromCoord(ch), nil
}

// CountMessage increments the counter and bumps the activity stamp.
func (s *CoordStore) CountMessage(ctx context.Context, id string, at time.Time) (int, error) {
	ch, found, err := s.channels.CountChannelMessage(ctx, id, at)
	if err != nil {
		return 0, fmt.Errorf("a2a: count message on %s: %w", id, err)
	}
	if !found {
		return 0, ErrNoChannel
	}
	return ch.Messages, nil
}

// Idle lists every open channel idle since before cutoff.
//
// Listed, and closed one at a time by the sweep that decides on each
// ([Sweeper.SweepIdle]): the caller needs to know WHICH channels it closed —
// each is an ask that was never answered, and publishing that is how an
// operator finds a seat that has stopped replying — and which ones it must
// not, because their answer is still owed.
func (s *CoordStore) Idle(ctx context.Context, cutoff time.Time) ([]Channel, error) {
	open, err := s.channels.OpenChannels(ctx)
	if err != nil {
		return nil, fmt.Errorf("a2a: list open channels: %w", err)
	}
	var idle []Channel
	for _, ch := range open {
		if ch.LastAt.Before(cutoff) {
			idle = append(idle, fromCoord(ch))
		}
	}
	return idle, nil
}

// Purge deletes channels closed before cutoff.
func (s *CoordStore) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	n, err := s.channels.PurgeChannels(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("a2a: purge channels: %w", err)
	}
	return n, nil
}

// toCoord and fromCoord are the whole translation.
//
// Two types rather than one because the direction of the dependency has to
// stay honest: coord holds what a fleet agrees on and knows nothing about
// asks, while [Channel]'s participant logic — OtherParty, and the
// authorization decision built on it — is this package's own. A shared struct
// would put agent-to-agent semantics inside the coordination contract.
func toCoord(ch Channel) coord.Channel {
	return coord.Channel{
		ID: ch.ID, Requester: ch.Requester, Target: ch.Target,
		Messages: ch.Messages, OpenedAt: ch.OpenedAt, LastAt: ch.LastAt,
		ClosedAt: ch.ClosedAt,
	}
}

func fromCoord(ch coord.Channel) Channel {
	return Channel{
		ID: ch.ID, Requester: ch.Requester, Target: ch.Target,
		Messages: ch.Messages, OpenedAt: ch.OpenedAt, LastAt: ch.LastAt,
		ClosedAt: ch.ClosedAt,
	}
}
