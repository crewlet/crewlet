package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the seats' held conversations ------------------------------------- //

// ConversationHolds is every held conversation of seat, each with its revision.
func (f *FleetStore) ConversationHolds(ctx context.Context, seat uuid.UUID) ([]coord.ConversationHold, error) {
	if seat == uuid.Nil {
		return nil, errors.New("coord/kv: conversation holds are read per seat")
	}
	var out []coord.ConversationHold
	err := f.eachUnder(ctx, f.positions, coord.ConversationHoldFilter(seat), "the held conversations",
		func(kve jetstream.KeyValueEntry) error {
			var h coord.ConversationHold
			if err := json.Unmarshal(kve.Value(), &h); err != nil {
				return fmt.Errorf("coord/kv: decode the conversation hold %s: %w", kve.Key(), err)
			}
			h.Revision = kve.Revision()
			out = append(out, h)
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateConversationHold writes h, false where its conversation is held already.
func (f *FleetStore) CreateConversationHold(ctx context.Context, h coord.ConversationHold) (bool, error) {
	if err := h.Validate(); err != nil {
		return false, err
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return false, fmt.Errorf("coord/kv: encode a conversation hold: %w", err)
	}
	_, err = f.create(ctx, f.positions, coord.ConversationHoldKey(h.Seat, h.Conversation), raw)
	switch {
	case errors.Is(err, jetstream.ErrKeyExists):
		return false, nil
	case err != nil:
		return false, unavailable("hold a conversation of "+h.Seat.String(), err)
	}
	return true, nil
}

// DeleteConversationHold lifts the hold read at revision, false where it moved.
func (f *FleetStore) DeleteConversationHold(ctx context.Context, seat uuid.UUID, conversation string,
	revision uint64) (bool, error) {

	if seat == uuid.Nil || conversation == "" || revision == 0 {
		return false, errors.New("coord/kv: a conversation hold is lifted at the revision it was read at")
	}
	err := f.positions.Purge(ctx, coord.ConversationHoldKey(seat, conversation), jetstream.LastRevision(revision))
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	}
	return false, unavailable("lift a conversation hold of "+seat.String(), err)
}
