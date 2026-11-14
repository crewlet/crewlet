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
// a fresh session, whose provider refused its request twice in a row, the
// second time on a fresh session, or whose runs' harness the kernel killed for
// memory twice in a row, is held: its seat starts no turn on it until a person
// writes on it or the settings it failed on change, and its other conversations
// go on (ADR-0117).

// The failures that hold a conversation rather than its seat.
const (
	// failureContextExhausted is a conversation whose brief does not fit its models.
	failureContextExhausted failureKind = "context_exhausted"
	// failureOutOfMemory is a conversation whose work does not fit its box's memory.
	failureOutOfMemory failureKind = "out_of_memory"
	// failureRequestRefused is a conversation whose provider refuses the request
	// its runs make as built, a fresh session's included.
	failureRequestRefused failureKind = "request_refused"
)

// conversationFailures are the collected endings that hold a conversation, by
// the kind their record names.
var conversationFailures = map[string]failureKind{
	types.ExecutorErrorContextExhausted: failureContextExhausted,
	types.ExecutorErrorOutOfMemory:      failureOutOfMemory,
	types.ExecutorErrorRequestRefused:   failureRequestRefused,
}

// conversationRun is one seat's conversation.
type conversationRun struct {
	seat         uuid.UUID
	conversation string
}

// heldConversations is this node's view of the conversations it holds runs of.
type heldConversations struct {
	mu sync.Mutex
	// last is how a conversation's last run failed, for the kinds that hold one.
	last map[conversationRun]failureKind
	// held is each seat's holds as last read from the fleet, nil where unread.
	held map[uuid.UUID]map[string]coord.ConversationHold
}

// failedAgain records how run's latest run failed ("" for none that holds),
// and reports whether the one before it failed the same way.
func (h *heldConversations) failedAgain(run conversationRun, kind failureKind) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last == nil {
		h.last = map[conversationRun]failureKind{}
	}
	before := h.last[run]
	if kind != "" {
		h.last[run] = kind
	} else {
		delete(h.last, run)
	}
	return kind != "" && before == kind
}

// forget drops what this node read of seat's holds, so the next check reads
// them again: the seat moved, or a hold was taken or lifted.
func (h *heldConversations) forget(seat uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.held, seat)
}

// noteConversationRun is a publish listener: a run that ran out of context, or
// that its provider refused, after one that did, on a fresh session, holds its
// conversation.
func (e *Engine) noteConversationRun(ctx context.Context, _ string, ev *events.Event) {
	rec, ok := events.DataAs[*types.ExecutorCollected](ev)
	if !ok || rec == nil || rec.ConversationKey == "" {
		return
	}
	seat, err := uuid.Parse(rec.Agent)
	if err != nil {
		return
	}
	var kind failureKind
	if rec.Failed {
		kind = conversationFailures[rec.ErrorKind]
	}
	if kind == "" && !rec.Failed && rec.Notes != "" {
		return // stopped, lost or asking: it says nothing about the conversation
	}
	if !e.conversations.failedAgain(conversationRun{seat, rec.ConversationKey}, kind) {
		return
	}
	if (kind == failureContextExhausted || kind == failureRequestRefused) && rec.HarnessSessionContinued {
		return // the session, not the brief or the tools: a fresh one may be taken
	}
	handle, known := e.seatHandle(rec.Agent)
	if !known {
		return
	}
	go e.holdConversation(context.WithoutCancel(ctx), seat, handle, rec.ConversationKey, kind)
}

// holdConversation writes the hold the fleet reads; the seat's holder reads it
// on its next delivery of the conversation.
func (e *Engine) holdConversation(ctx context.Context, seat uuid.UUID, handle, conversation string,
	kind failureKind) {

	basis, known := failureBasis(e.Company(), handle, kind)
	if !known || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	created, err := e.backends.Fleet.CreateConversationHold(ctx, coord.ConversationHold{Seat: seat,
		Conversation: conversation, Kind: string(kind), Basis: basis, At: time.Now().UTC()})
	if err != nil {
		log.WarnContext(ctx, "conversation_not_held", "seat", handle, "error", err.Error(),
			"detail", "the conversation's next wake starts a turn as before")
		return
	}
	e.conversations.forget(seat)
	if created {
		log.InfoContext(ctx, "conversation_held", "seat", handle, "conversation", conversation,
			"kind", string(kind), "detail", "it failed the same way twice; a person's message on it, "+
				"or a change of the settings it failed on, releases it")
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
				e.liftConversationHold(ctx, h, handle, "the settings it failed on changed")
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

// clearRemovedConversationHolds lifts the holds of every seat next no longer
// has, as a removed seat's pause goes with it; only the fleet's company clears.
func (e *Engine) clearRemovedConversationHolds(ctx context.Context, next *Company) {
	if next == nil || next.Org == nil || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	if err := e.fleetCompany(ctx, next); err != nil {
		return
	}
	holds, err := e.backends.Fleet.ListConversationHolds(ctx)
	if err != nil {
		log.WarnContext(ctx, "conversation_holds_not_swept", "error", err.Error(),
			"detail", "a removed seat's held conversations are cleared by the next apply")
		return
	}
	for _, h := range holds {
		if next.Org.AgentSeatByID(h.Seat) == nil {
			e.liftConversationHold(ctx, h, h.Seat.String(), "the seat left the company")
		}
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
