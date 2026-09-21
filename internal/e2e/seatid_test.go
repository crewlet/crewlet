package e2e

import (
	"testing"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// seatInbox is one seat's mailbox subject on a running node.
//
// THROUGH THE NODE'S OWN COMPANY, because a mailbox is named by the seat's ID
// rather than its handle. A case that built the subject from a handle would
// publish into a topic nothing subscribes and subscribe one nothing publishes
// to, and both halves would look like a quiet engine rather than a mistake.
func seatInbox(t *testing.T, n *node, handle string) string {
	t.Helper()
	c := n.engine.Company()
	if c == nil || c.Org == nil {
		t.Fatalf("the node runs no company, so seat %q has no mailbox", handle)
	}
	seat := c.Org.SeatByHandle(handle)
	if seat == nil {
		t.Fatalf("seat %q is not in the running company at all", handle)
	}
	if id, ok := c.Org.AgentIDFor(seat); ok {
		return topics.AgentInbox(id)
	}
	// A HUMAN SEAT HAS NO AGENT ID and therefore no mailbox, which is the
	// whole of the guard a case watching one is about. So this answers the
	// subject that seat's mailbox WOULD be on if it were an agent: nothing
	// may ever publish there, and a case asserting silence on a subject
	// that cannot be named would be asserting nothing at all.
	id, ok := org.DeriveAgentID(c.Org.Name, seat.Origin())
	if !ok {
		t.Fatalf("seat %q derives no id at all, so this case can watch nothing", handle)
	}
	return topics.AgentInbox(id)
}
