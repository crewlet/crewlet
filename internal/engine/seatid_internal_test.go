package engine

import "github.com/crewlet/crewlet/internal/queue/topics"

// seatInbox and seatInboxGroup are one seat's mailbox as the engine names it.
//
// THROUGH THE ENGINE'S OWN COMPANY, because a mailbox is named by the seat's
// ID: a subject built from a handle is one nothing uses, so assertions over it
// pass empty.
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
