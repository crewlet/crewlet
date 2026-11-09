package engine

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// A conversation whose context did not fit twice in a row, the second time on
// a fresh session, is held: its seat starts no turn on it until a person writes
// on it or the seat's models change, and its other conversations go on (ADR-0117).

// failureContextExhausted is a conversation whose brief does not fit its models.
const failureContextExhausted failureKind = "context_exhausted"

// conversationRun is one seat's conversation.
type conversationRun struct {
	seat         uuid.UUID
	conversation string
}

// heldConversations is this node's view of the conversations it holds runs of.
type heldConversations struct {
	mu sync.Mutex
	// exhausted marks a conversation whose last run ran out of context.
	exhausted map[conversationRun]bool
	// held is each seat's holds as last read from the fleet, nil where unread.
	held map[uuid.UUID]map[string]coord.ConversationHold
}

// ranOut records whether run's latest run ran out of context, and reports
// whether the one before it did too.
func (h *heldConversations) ranOut(run conversationRun, out bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.exhausted == nil {
		h.exhausted = map[conversationRun]bool{}
	}
	before := h.exhausted[run]
	if out {
		h.exhausted[run] = true
	} else {
		delete(h.exhausted, run)
	}
	return before && out
}

// forget drops what this node read of seat's holds, so the next check reads
// them again: the seat moved, or a hold was taken or lifted.
func (h *heldConversations) forget(seat uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.held, seat)
}

// noteConversationRun is a publish listener: a run that ran out of context
// after one that did, on a fresh session, holds its conversation.
func (e *Engine) noteConversationRun(ctx context.Context, _ string, ev *events.Event) {
	rec, ok := events.DataAs[*types.ExecutorCollected](ev)
	if !ok || rec == nil || rec.ConversationKey == "" {
		return
	}
	seat, err := uuid.Parse(rec.Agent)
	if err != nil {
		return
	}
	out := rec.Failed && rec.ErrorKind == types.ExecutorErrorContextExhausted
	if !out && !rec.Failed && rec.Notes != "" {
		return // stopped, lost or asking: it says nothing about the context
	}
	if !e.conversations.ranOut(conversationRun{seat, rec.ConversationKey}, out) || rec.HarnessSessionContinued {
		return
	}
	handle, known := e.seatHandle(rec.Agent)
	if !known {
		return
	}
	go e.holdConversation(context.WithoutCancel(ctx), seat, handle, rec.ConversationKey)
}

// holdConversation writes the hold the fleet reads; the seat's holder reads it
// on its next delivery of the conversation.
func (e *Engine) holdConversation(ctx context.Context, seat uuid.UUID, handle, conversation string) {
	basis, known := failureBasis(e.Company(), handle, failureContextExhausted)
	if !known || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	created, err := e.backends.Fleet.CreateConversationHold(ctx, coord.ConversationHold{Seat: seat,
		Conversation: conversation, Kind: string(failureContextExhausted), Basis: basis, At: time.Now().UTC()})
	if err != nil {
		log.WarnContext(ctx, "conversation_not_held", "seat", handle, "error", err.Error(),
			"detail", "the conversation's next wake starts a turn as before")
		return
	}
	e.conversations.forget(seat)
	if created {
		log.InfoContext(ctx, "conversation_held", "seat", handle, "conversation", conversation,
			"detail", "its context did not fit twice; a person's message on it or new models release it")
	}
}

// conversationHeld is the dispatcher's [Dispatcher.Held]: whether handle's
// conversation is held. A delivery a person wrote lifts the hold and runs.
func (e *Engine) conversationHeld(ctx context.Context, handle, conversation string, evs []*events.Event) bool {
	seat, err := e.seatID(handle)
	if conversation == "" || err != nil || e.backends == nil || e.backends.Fleet == nil {
		return false
	}
	hold, held := e.heldConversation(ctx, seat, handle, conversation)
	if !held {
		return false
	}
	if !personWrote(e.liveParties(), evs) {
		return true
	}
	e.liftConversationHold(ctx, hold, handle, "a person wrote on it")
	return false
}

// heldConversation is seat's hold on conversation, reading the seat's holds
// from the fleet once per seat on this node. A hold whose models changed since
// is lifted rather than kept.
func (e *Engine) heldConversation(ctx context.Context, seat uuid.UUID, handle,
	conversation string) (coord.ConversationHold, bool) {

	e.conversations.mu.Lock()
	holds, read := e.conversations.held[seat]
	e.conversations.mu.Unlock()
	if !read {
		list, err := e.backends.Fleet.ConversationHolds(ctx, seat)
		if err != nil {
			log.WarnContext(ctx, "conversation_holds_unread", "seat", handle, "error", err.Error(),
				"detail", "this delivery runs; the holds are read again on the next")
			return coord.ConversationHold{}, false
		}
		holds = map[string]coord.ConversationHold{}
		for _, h := range list {
			basis, known := failureBasis(e.Company(), handle, failureKind(h.Kind))
			if known && basis != h.Basis {
				e.liftConversationHold(ctx, h, handle, "the seat's models changed")
				continue
			}
			holds[h.Conversation] = h
		}
		e.conversations.mu.Lock()
		if e.conversations.held == nil {
			e.conversations.held = map[uuid.UUID]map[string]coord.ConversationHold{}
		}
		e.conversations.held[seat] = holds
		e.conversations.mu.Unlock()
	}
	h, held := holds[conversation]
	return h, held
}

// liftConversationHold ends hold, as the engine, for why.
func (e *Engine) liftConversationHold(ctx context.Context, hold coord.ConversationHold, handle, why string) {
	lifted, err := e.backends.Fleet.DeleteConversationHold(ctx, hold.Seat, hold.Conversation, hold.Revision)
	e.conversations.forget(hold.Seat)
	switch {
	case err != nil:
		log.WarnContext(ctx, "conversation_hold_not_lifted", "seat", handle, "error", err.Error(),
			"detail", "the hold is read again on the conversation's next delivery")
	case lifted:
		log.InfoContext(ctx, "conversation_hold_lifted", "seat", handle, "conversation", hold.Conversation,
			"reason", why)
	}
}

// rereadConversationHolds drops this node's reading of every seat's holds on an
// apply, so each is judged again against the models it brings.
func (e *Engine) rereadConversationHolds() {
	e.conversations.mu.Lock()
	defer e.conversations.mu.Unlock()
	e.conversations.held = nil
}

// personWrote reports whether a person wrote anything evs deliver.
func personWrote(parties notify.Parties, evs []*events.Event) bool {
	for _, ev := range evs {
		if n, ok := events.DataAs[*types.ExternalNotification](ev); ok && n != nil && notify.PersonWrote(parties, *n) {
			return true
		}
	}
	return false
}
