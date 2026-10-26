package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/live"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/redact"
)

// THE SERVICE ACROSS A FLEET: the running node's answers to its peers
// (live.Server), the adapter that hands the service its fleet half
// ([FleetRemote]), and a person's turn rate counted across every node.

// turnRateWindow is the window [TurnsPerMinute] and [StreamOpensPerMinute]
// are counted over across the fleet: a minute, which the fleet's counter
// keeps (coord.MaxRateWindow).
const turnRateWindow = time.Minute

// rateBucket is one person's counter of scope on the fleet's store.
func rateBucket(scope string, person uuid.UUID) string { return scope + ":" + person.String() }

// admitAcrossTheFleet counts one start of scope (a turn, a stream) by person
// on the fleet's counter, refusing it past limit in the current minute with
// reason.
//
// A COUNTER THAT CANNOT BE READ ADMITS, logged: this node's own rate and
// allowance have already taken it, so an outage of the coordination store
// narrows the assistant to what one node holds a person to, never to no
// limit at all, and a person is never refused for a store the engine could
// not reach.
func (s *Service) admitAcrossTheFleet(ctx context.Context, scope string, person uuid.UUID,
	limit int, reason string) error {

	if s.rate == nil {
		return nil
	}
	bucket := rateBucket(scope, person)
	now := s.now()
	ok, err := s.rate.Allow(ctx, bucket, limit, turnRateWindow, now)
	switch {
	case err != nil:
		s.log.WarnContext(ctx, "assistant_turn_rate_unread", "bucket", bucket,
			"error", redact.Secrets(err.Error()),
			"detail", "the fleet's count could not be read; this node's own rate "+
				"still holds the person")
		return nil
	case !ok:
		return &Busy{RetryAfter: now.Truncate(turnRateWindow).Add(turnRateWindow).Sub(now),
			Reason: reason}
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
// forbidden, so it says nothing about whether the turn exists (ADR-0053).
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

// LastHere is the newest seq this node numbered for owner's turn
// (live.Server), for a peer opening the turn's next segment.
func (s *Service) LastHere(_ context.Context, owner, turnID uuid.UUID) (uint64, error) {
	who, last, ok := s.hub.Known(turnID)
	switch {
	case !ok:
		return 0, live.ErrNotRunning
	case who != owner:
		return 0, live.ErrNotYours
	}
	return last, nil
}

// continuing is the seq a segment of owner's turn opened on this node numbers
// after: the highest any node of the fleet numbered for it, and never below
// parked, the last seq the turn's parked record says its parking segment told.
// A NUMBER THAT ONLY GROWS across nodes, because a client resuming on any of
// them draws a frame only above the last it saw; a jump is harmless (a gap is
// answered with a snapshot) where a repeat is a frame the client drops.
//
// A fleet that cannot be asked leaves the parked record's floor and this
// node's own memory ([live.Hub.Open]), which a segment told on a node the
// probe could not reach can only be below if that node is gone with it.
func (s *Service) continuing(ctx context.Context, owner, turnID uuid.UUID, parked uint64) uint64 {
	if s.remote == nil {
		return parked
	}
	_, last, err := s.remote.LastSeq(ctx, owner, turnID)
	if err != nil {
		s.log.WarnContext(ctx, "assistant_numbering_unprobed", "turn", turnID,
			"error", redact.Secrets(err.Error()),
			"detail", "the segment is numbered after the parked record and this node's memory")
		return parked
	}
	return max(parked, last)
}

// parkedLoop is owner's turn's parked loop, decoded.
func (s *Service) parkedLoop(ctx context.Context, owner, turnID uuid.UUID) (parked, error) {
	raw, err := s.transcripts.Parked(ctx, owner, turnID)
	if err != nil {
		return parked{}, s.readFailure(err)
	}
	var keep parked
	if err = json.Unmarshal(raw, &keep); err != nil {
		return parked{}, fmt.Errorf("assistant: turn %s parked a loop this build "+
			"cannot decode: %w", turnID, err)
	}
	if keep.V != parkedVersion {
		return parked{}, fmt.Errorf("assistant: turn %s parked a loop at version %d, "+
			"and this build reads version %d", turnID, keep.V, parkedVersion)
	}
	return keep, nil
}

func (r fleetRemote) LastSeq(ctx context.Context, owner, turnID uuid.UUID) (string, uint64, error) {
	holder, last, err := r.f.LastSeq(ctx, owner, turnID)
	return holder, last, notFound(err, turnID)
}

// streamSource is where a stream of owner's turn is answered from, after seq
// after: this node's log, the node holding the frames the client has not
// drawn, or neither (the transcript answers).
//
// A RUNNING TURN is answered where its record says it runs. A turn whose
// segment here has ended, or that is not running, may since have been told
// on a node the record does not name (an approval resumed it, or emitted a
// page action, elsewhere), so the fleet is asked which node holds its newest
// frames, and a stream behind them is answered there.
func (s *Service) streamSource(ctx context.Context, owner uuid.UUID, row assistantdomain.Turn,
	after uint64) (*live.Log, string) {

	l, held := s.hub.Get(row.ID)
	switch {
	case held && (!l.Ended() || s.remote == nil):
		return l, ""
	case s.remote == nil:
		return nil, ""
	case !held && !row.State.Ended():
		if row.Node == s.node {
			return nil, ""
		}
		return nil, row.Node
	}
	holder, last, err := s.remote.LastSeq(ctx, owner, row.ID)
	switch {
	case err != nil:
		s.log.WarnContext(ctx, "assistant_numbering_unprobed", "turn", row.ID,
			"error", redact.Secrets(err.Error()),
			"detail", "the stream is answered from what this node holds")
	case held && l.Last() >= last:
		return l, ""
	case holder != "" && holder != s.node && last > after:
		return nil, holder
	}
	switch {
	case held:
		return l, ""
	case !row.State.Ended() && row.Node != s.node:
		return nil, row.Node
	}
	return nil, ""
}
