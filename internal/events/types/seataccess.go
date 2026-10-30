package types

import (
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// An agent's access at a vendor, put back by the reconcile after somebody
// changed it there (D2 of per-seat integration access).

func init() {
	events.Register[SeatAccessDriftRestored]()
}

// SeatAccessDriftRestored fires when an access sync restored a seat's level or
// scope at a vendor to what the company document asks, after somebody changed
// it at the vendor.
//
// THE RECORD THE AGENTS TAB DRAWS BETWEEN PASSES: the setting was put back
// automatically, and a person may want the vendor's setting instead ("Keep the
// vendor's setting" writes it into the document). Before and After are in the
// vendor's own terms, exactly as the document stores them, and carry no
// credential.
type SeatAccessDriftRestored struct {
	// Integration is the account's integration (`atlassian` for Jira and
	// Confluence) and Lane the block the setting lives in.
	Integration string `json:"integration"`
	Lane        string `json:"lane"`

	// AgentHandle is the seat whose account it is.
	AgentHandle string `json:"agent_handle"`

	// Before is what the vendor held; After is what the document asks and
	// the sync restored. Level, permissions and scope, as on the access
	// route.
	Before AccessSetting `json:"before"`
	After  AccessSetting `json:"after"`

	// RestoredAt is when the sync restored it.
	RestoredAt time.Time `json:"restored_at"`
}

// AccessSetting is one seat's access on one lane, in the vendor's terms.
type AccessSetting struct {
	Level       string            `json:"level,omitempty"`
	Permissions map[string]string `json:"permissions,omitempty"`
	Scope       []string          `json:"scope,omitempty"`
}

// EventType is the "seat_access_drift_restored" wire type.
func (SeatAccessDriftRestored) EventType() string { return "seat_access_drift_restored" }

// SummaryFor names the seat, the lane and the level that was put back.
func (e SeatAccessDriftRestored) SummaryFor(actor string) string {
	who := e.AgentHandle
	if actor != "" {
		who = actor
	}
	return who + "'s " + e.Lane + " access was restored after a change at the vendor"
}
