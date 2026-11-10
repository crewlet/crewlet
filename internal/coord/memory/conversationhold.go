package memory

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the seats' held conversations ------------------------------------- //

// ConversationHolds is every held conversation of seat, each with its revision.
func (f *Fleet) ConversationHolds(_ context.Context, seat uuid.UUID) ([]coord.ConversationHold, error) {
	if seat == uuid.Nil {
		return nil, errors.New("coord/memory: conversation holds are read per seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.SortedFunc(maps.Values(f.conversationHolds[seat]), func(a, b coord.ConversationHold) int {
		return cmp.Compare(a.Conversation, b.Conversation)
	}), nil
}

// ListConversationHolds is every held conversation of every seat, by seat then conversation.
func (f *Fleet) ListConversationHolds(_ context.Context) ([]coord.ConversationHold, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []coord.ConversationHold
	for _, held := range f.conversationHolds {
		out = append(out, slices.Collect(maps.Values(held))...)
	}
	slices.SortFunc(out, func(a, b coord.ConversationHold) int {
		return cmp.Or(cmp.Compare(a.Seat.String(), b.Seat.String()), cmp.Compare(a.Conversation, b.Conversation))
	})
	return out, nil
}

// CreateConversationHold writes h, false where its conversation is held already.
func (f *Fleet) CreateConversationHold(_ context.Context, h coord.ConversationHold) (bool, error) {
	if err := h.Validate(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, held := f.conversationHolds[h.Seat][h.Conversation]; held {
		return false, nil
	}
	if f.conversationHolds[h.Seat] == nil {
		f.conversationHolds[h.Seat] = map[string]coord.ConversationHold{}
	}
	f.conversationHoldRevision++
	h.Revision = f.conversationHoldRevision
	f.conversationHolds[h.Seat][h.Conversation] = h
	return true, nil
}

// DeleteConversationHold lifts the hold read at revision, false where it moved.
func (f *Fleet) DeleteConversationHold(_ context.Context, seat uuid.UUID, conversation string,
	revision uint64) (bool, error) {

	if seat == uuid.Nil || conversation == "" || revision == 0 {
		return false, errors.New("coord/memory: a conversation hold is lifted at the revision it was read at")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, held := f.conversationHolds[seat][conversation]; !held || h.Revision != revision {
		return false, nil
	}
	delete(f.conversationHolds[seat], conversation)
	return true, nil
}
