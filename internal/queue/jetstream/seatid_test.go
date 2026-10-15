package jetstream

import (
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// seatID is the id of the seat a case calls name.
//
// Per-seat names are built from a seat's ID rather than its handle, so a case
// that wants "alice's inbox" needs a uuid it can write twice and get the same
// subject from. The engine's own derivation namespaces by the company name
// and is not this package's business; what a case needs is a value that is a
// uuid and is the same on every run.
func seatID(name string) uuid.UUID {
	return uuid.NewSHA1(uuid.MustParse("6f1c2a4e-9d3b-4a71-8f5e-2b0c7d81a940"),
		[]byte(name))
}

// seatInbox and seatGroup are that seat's mailbox.
func seatInbox(name string) string { return topics.AgentInbox(seatID(name)) }
func seatGroup(name string) string { return topics.AgentInboxGroup(seatID(name)) }
