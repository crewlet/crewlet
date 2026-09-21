package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// seatInbox and seatInboxGroup are one seat's mailbox as the engine names it.
//
// THROUGH THE ENGINE'S OWN COMPANY, because a mailbox is named by the seat's
// ID: a case that built the subject from a handle would be naming a topic
// nothing publishes to and nothing subscribes, so every assertion over it
// would pass by being empty.
func seatInbox(e *Engine, handle string) string {
	id, err := e.seatID(handle)
	if err != nil {
		return ""
	}
	return topics.AgentInbox(id)
}

func seatInboxGroup(e *Engine, handle string) string {
	id, err := e.seatID(handle)
	if err != nil {
		return ""
	}
	return topics.AgentInboxGroup(id)
}

// mustSeatInbox is [seatInbox] for a case that cannot proceed without one.
func mustSeatInbox(t *testing.T, e *Engine, handle string) (string, string) {
	t.Helper()
	id, err := e.seatID(handle)
	if err != nil {
		t.Fatalf("seat %q has no mailbox on this engine: %v", handle, err)
	}
	return topics.AgentInbox(id), topics.AgentInboxGroup(id)
}
