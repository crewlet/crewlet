package maintenance_test

import (
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// seat is the seat a case calls handle.
//
// A mailbox — its registry record, its inbox, its consumer group, its control
// subscription and the lease a retirement claims to exclude a running node —
// is named by the seat's ID. The handle is the label a log line reads. Derived
// rather than random so a case can register a seat, retire it, and register it
// again under one identity.
func seat(handle string) placement.Seat {
	return placement.Seat{ID: seatID(handle), Handle: handle}
}

func seatID(handle string) uuid.UUID {
	return uuid.NewSHA1(uuid.MustParse("6f1c2a4e-9d3b-4a71-8f5e-2b0c7d81a940"),
		[]byte(handle))
}

// seats is [seat] over a roster.
func seats(handles ...string) []placement.Seat {
	out := make([]placement.Seat, 0, len(handles))
	for _, handle := range handles {
		out = append(out, seat(handle))
	}
	return out
}

// The four wire names one seat's mailbox comprises.
func inbox(handle string) string        { return topics.AgentInbox(seatID(handle)) }
func inboxGroup(handle string) string   { return topics.AgentInboxGroup(seatID(handle)) }
func control(handle string) string      { return topics.AgentControl(seatID(handle)) }
func controlGroup(handle string) string { return topics.AgentControlGroup(seatID(handle)) }
