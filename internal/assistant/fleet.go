package assistant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/live"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/redact"
)

// THE SERVICE ACROSS A FLEET: the running node's answers to its peers
// (live.Server), the adapter that hands the service its fleet half
// ([FleetRemote]), and a person's turn rate counted across every node.

// turnRateWindow is the window [TurnsPerMinute] is counted over across the
// fleet: a minute, which the fleet's counter keeps (coord.MaxRateWindow).
const turnRateWindow = time.Minute

// turnRateBucket is one person's turn-rate counter on the fleet's store.
func turnRateBucket(person uuid.UUID) string { return turnRateScope + ":" + person.String() }

// admitAcrossTheFleet counts one turn start of person on the fleet's counter,
// refusing it past [TurnsPerMinute] in the current minute.
//
// A COUNTER THAT CANNOT BE READ ADMITS, logged: this node's own rate
// ([TurnsPerMinute], [TurnBurst]) and running allowance have already taken
// the turn, so an outage of the coordination store narrows the assistant to
// what one node holds a person to, never to no limit at all, and a person's
// question is never refused for a store the engine could not reach.
func (s *Service) admitAcrossTheFleet(ctx context.Context, person uuid.UUID) error {
	if s.rate == nil {
		return nil
	}
	bucket := turnRateBucket(person)
	now := s.now()
	ok, err := s.rate.Allow(ctx, bucket, TurnsPerMinute, turnRateWindow, now)
	switch {
	case err != nil:
		s.log.WarnContext(ctx, "assistant_turn_rate_unread", "bucket", bucket,
			"error", redact.Secrets(err.Error()),
			"detail", "the fleet's turn count could not be read; this node's own turn "+
				"rate still holds the person")
		return nil
	case !ok:
		return &Busy{RetryAfter: now.Truncate(turnRateWindow).Add(turnRateWindow).Sub(now),
			Reason: turnRateReason}
	}
	return nil
}

var _ live.Server = (*Service)(nil)

// ReplayHere answers a peer asking for the frames of owner's turn after seq
// after (live.Server): this node's frame log, a snapshot first where it no
// longer holds them, exactly as this node's own client is answered.
func (s *Service) ReplayHere(ctx context.Context, owner, turnID uuid.UUID,
	after uint64) ([]wire.Frame, bool, error) {

	l, ok := s.hub.Get(turnID)
	switch {
	case !ok:
		return nil, false, live.ErrNotRunning
	case l.Owner() != owner:
		return nil, false, live.ErrNotYours
	}
	frames, gap, ended := l.Since(after)
	if !gap {
		return frames, ended, nil
	}
	row, err := s.transcripts.Turn(ctx, owner, turnID)
	if err != nil {
		return nil, false, s.readFailure(err)
	}
	frames, err = s.filled(ctx, owner, row, after, frames, gap)
	return frames, ended, err
}

// StopHere stops owner's turn running on this node for a peer that asked
// (live.Server).
func (s *Service) StopHere(ctx context.Context, owner, turnID uuid.UUID) (wire.TurnState, error) {
	s.mu.Lock()
	t, ok := s.turns[turnID]
	s.mu.Unlock()
	switch {
	case !ok:
		return "", live.ErrNotRunning
	case t.person.ID != owner:
		return "", live.ErrNotYours
	}
	return s.stopRunner(ctx, t), nil
}

// FleetRemote is f as the service's [Remote]: the fleet half's own answers,
// with a turn that is somebody else's told as [ErrNotFound], never as
// forbidden, so it says nothing about whether the turn exists (ADR-0052).
func FleetRemote(f *live.Fleet) Remote { return fleetRemote{f: f} }

type fleetRemote struct{ f *live.Fleet }

func (r fleetRemote) Publish(fr wire.Frame) { r.f.Publish(fr) }

func (r fleetRemote) Follow(ctx context.Context, owner, turnID uuid.UUID, node string,
	after uint64, w FrameWriter) (live.Reach, error) {

	reach, err := r.f.Follow(ctx, owner, turnID, node, after, w)
	return reach, notFound(err, turnID)
}

func (r fleetRemote) Stop(ctx context.Context, owner, turnID uuid.UUID,
	node string) (wire.TurnState, live.Reach, error) {

	state, reach, err := r.f.Stop(ctx, owner, turnID, node)
	return state, reach, notFound(err, turnID)
}

// notFound is err with the running node's "not yours" as [ErrNotFound].
func notFound(err error, turnID uuid.UUID) error {
	if errors.Is(err, live.ErrNotYours) {
		return fmt.Errorf("%w: turn %s", ErrNotFound, turnID)
	}
	return err
}
