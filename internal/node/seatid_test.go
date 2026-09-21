package node_test

import (
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// seat is the seat a case calls handle: the id every durable name for it is
// built from — its lease resource, its mailbox subject, its consumer group —
// and the handle every log line and every hook reads.
//
// DERIVED rather than random, so a case can name the same seat twice and get
// the same id, and so a case that publishes to a seat's inbox and a case that
// subscribes one build the same subject. The engine's own derivation is a
// UUIDv5 over (company name, origin handle); this is that shape with a
// namespace of its own, because what a case here needs is a stable id rather
// than the value a particular company would mint.
func seat(handle string) placement.Seat {
	return placement.Seat{ID: seatID(handle), Handle: handle}
}

func seatID(handle string) uuid.UUID {
	return uuid.NewSHA1(uuid.MustParse("6f1c2a4e-9d3b-4a71-8f5e-2b0c7d81a940"),
		[]byte(handle))
}

// seats is [seat] over a company's worth of them.
func seats(handles ...string) []placement.Seat {
	out := make([]placement.Seat, 0, len(handles))
	for _, handle := range handles {
		out = append(out, seat(handle))
	}
	return out
}

// inbox and inboxGroup are that seat's mailbox, as the engine names it.
func inbox(handle string) string      { return topics.AgentInbox(seatID(handle)) }
func inboxGroup(handle string) string { return topics.AgentInboxGroup(seatID(handle)) }
