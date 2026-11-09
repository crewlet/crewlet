package coord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ConversationHolds are the conversations of an agent seat whose turns the
// engine stopped starting, because their context did not fit twice in a row
// (ADR-0117). One record per (seat, conversation), in the positions register,
// which no clock ages: a person's new message or a change of the seat's models
// ends it, never a timer.
type ConversationHolds interface {
	// ConversationHolds is every held conversation of seat, each with its revision.
	ConversationHolds(ctx context.Context, seat uuid.UUID) ([]ConversationHold, error)
	// ListConversationHolds is every held conversation of every seat.
	ListConversationHolds(ctx context.Context) ([]ConversationHold, error)
	// CreateConversationHold writes h, false where its conversation is held already.
	CreateConversationHold(ctx context.Context, h ConversationHold) (bool, error)
	// DeleteConversationHold lifts the hold read at revision, false where it moved.
	DeleteConversationHold(ctx context.Context, seat uuid.UUID, conversation string, revision uint64) (bool, error)
}

// ConversationHold is one held conversation of one seat.
type ConversationHold struct {
	Seat         uuid.UUID `json:"seat"`
	Conversation string    `json:"conversation"`
	// Kind names the failure; Basis fingerprints the settings it depends on.
	Kind  string    `json:"kind"`
	Basis string    `json:"basis"`
	At    time.Time `json:"at"`
	// Revision is the store's revision of the record as it was read; not on the wire.
	Revision uint64 `json:"-"`
}

// Validate reports why a hold cannot be written.
func (h ConversationHold) Validate() error {
	switch {
	case h.Seat == uuid.Nil || h.Conversation == "":
		return errors.New("coord: a conversation hold names its seat and its conversation")
	case h.Kind == "" || h.Basis == "":
		return errors.New("coord: a conversation hold names the failure and the basis it failed on")
	case h.At.IsZero():
		return errors.New("coord: a conversation hold needs the instant it was taken")
	}
	return nil
}

// ConversationHoldClass is the key class a hold is filed under.
const ConversationHoldClass = "conversation_hold"

// ConversationHoldKey is a hold's key: the conversation is hashed, since a
// conversation key carries characters a key segment cannot.
func ConversationHoldKey(seat uuid.UUID, conversation string) string {
	sum := sha256.Sum256([]byte(conversation))
	return DocumentKey(ConversationHoldClass, seat.String(), hex.EncodeToString(sum[:16]))
}

// ConversationHoldFilter selects every hold of seat.
func ConversationHoldFilter(seat uuid.UUID) string {
	return DocumentFilter(ConversationHoldClass, seat.String())
}

// ConversationHoldsFilter selects every hold of every seat.
func ConversationHoldsFilter() string { return DocumentFilter(ConversationHoldClass) }
