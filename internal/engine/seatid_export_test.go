package engine_test

import (
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// seatInbox and seatInboxGroup are one seat's mailbox as the running company
// names it.
//
// THROUGH THE COMPANY, because a mailbox is named by the seat's ID: a subject
// built from the handle is one nothing uses, so assertions over it pass empty.
// An unknown seat yields "", which the queue refuses as a topic, so the case
// fails rather than passing vacuously.
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
