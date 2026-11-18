package jetstream

import (
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// seatID is the id of the seat a case calls name, the same uuid on every run:
// per-seat subjects are built from a seat's ID, not its handle. (The engine's
// own derivation, by company name, is not this package's business.)
func seatID(name string) uuid.UUID {
	return uuid.NewSHA1(uuid.MustParse("6f1c2a4e-9d3b-4a71-8f5e-2b0c7d81a940"),
		[]byte(name))
}

// seatInbox and seatGroup are that seat's mailbox.
func seatInbox(name string) string { return topics.AgentInbox(seatID(name)) }
func seatGroup(name string) string { return topics.AgentInboxGroup(seatID(name)) }
