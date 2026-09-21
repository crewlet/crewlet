package engine_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// seatInbox and seatInboxGroup are one seat's mailbox as the running company
// names it.
//
// THROUGH THE COMPANY, because a mailbox is named by the seat's ID: a subject
// a case built from the handle would be one nothing publishes to and nothing
// subscribes, so every assertion over it would pass by being empty.
//
// An unknown seat yields "", which every caller here passes straight to the
// queue — and an empty topic is refused rather than silently accepted, so the
// case fails where it would otherwise pass vacuously.
func seatInbox(e *engine.Engine, handle string) string {
	id, ok := seatID(e, handle)
	if !ok {
		return ""
	}
	return topics.AgentInbox(id)
}

func seatInboxGroup(e *engine.Engine, handle string) string {
	id, ok := seatID(e, handle)
	if !ok {
		return ""
	}
	return topics.AgentInboxGroup(id)
}

func seatID(e *engine.Engine, handle string) (id uuid.UUID, ok bool) {
	c := e.Company()
	if c == nil || c.Org == nil {
		return uuid.UUID{}, false
	}
	got, found := c.Org.AgentIDFor(c.Org.AgentSeatByHandle(handle))
	return got, found
}

// mustSeatInbox is [seatInbox] for a case that cannot proceed without one.
func mustSeatInbox(t *testing.T, e *engine.Engine, handle string) (string, string) {
	t.Helper()
	id, ok := seatID(e, handle)
	if !ok {
		t.Fatalf("seat %q is no agent seat in the running company, so it has no mailbox", handle)
	}
	return topics.AgentInbox(id), topics.AgentInboxGroup(id)
}
