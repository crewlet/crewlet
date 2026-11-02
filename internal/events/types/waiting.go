package types

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// What waits in the seats' inboxes, as the nodes holding them read it.

func init() {
	events.Register[SeatInboxWaiting]()
}

// SeatInboxWaitingHeartbeat is how often a node publishes its report when no
// count moved. It is ONE number for both ends: the reporting loop publishes on
// it, and the live projection forgets a reporter it has not heard from for
// three of it, which is how the counts of a node that went away clear.
const SeatInboxWaitingHeartbeat = 30 * time.Second

// SeatWaiting is one seat's mail that no turn is running on yet, per mailbox
// of its inbox: the priority lane (a person waiting) and the rest.
type SeatWaiting struct {
	// AgentID is the seat's derived agent id, the key of its live row.
	AgentID    string `json:"agent_id"`
	Priority   int    `json:"priority"`
	Background int    `json:"background"`
}

// SeatInboxWaiting is one node's report of the seats it holds and what waits in
// each one's inbox.
//
// THE HOLDING NODE COUNTS, because only it can leave out the mail its own turn
// is running on, which is not waiting; any other reader would count that
// mail too. Published on a short tick when a count moved and on a longer one
// when none did, so a dashboard that started since hears every seat.
//
// A STREAM-ONLY SNAPSHOT, like [BudgetMeters]: each report supersedes the
// reporter's last, and a durable row per report would fill the log with
// readings the next one replaces.
type SeatInboxWaiting struct {
	// ReporterID identifies the reporting node's INCARNATION. A report is
	// complete for its reporter: a seat it listed before and lists no
	// longer is one it no longer holds.
	ReporterID string `json:"reporter_id"`
	// Seq is monotonic within ReporterID, the reorder guard between one
	// reporter's reports.
	Seq   int           `json:"seq"`
	Seats []SeatWaiting `json:"seats"`
}

// EventType is the "seat_inbox_waiting" wire type.
func (SeatInboxWaiting) EventType() string { return "seat_inbox_waiting" }

// Summary counts the seats reported rather than leading with an actor: the
// report is a node's, not any one seat's.
func (e SeatInboxWaiting) Summary() string {
	return fmt.Sprintf("Inbox counts reported for %d agents", len(e.Seats))
}
