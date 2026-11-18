package types

import (
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// A seat's extra grants beside its level at an integration: asked for by the
// agent, granted, declined or removed by a person, lapsed or refused by the
// engine. The audit trail of who gave an agent what, and for how long.

func init() {
	events.Register[SeatGrantChanged]()
}

// The changes a [SeatGrantChanged] records.
const (
	GrantRequested = "requested"
	GrantGranted   = "granted"
	GrantDeclined  = "declined"
	GrantRemoved   = "removed"
	GrantExpired   = "expired"
	GrantRefused   = "refused"
)

// SeatGrantChanged fires when one of a seat's grants is asked for, granted,
// declined, removed, lapses or is refused by the provisioner's catalogue.
//
// NEVER A CREDENTIAL AND NEVER THE AGENT'S WORDS: the grant is named by its
// permission, scope and target, and why it was asked for stays on the grant.
type SeatGrantChanged struct {
	// Integration is the account's integration and Lane the block the grant
	// lives in.
	Integration string `json:"integration"`
	Lane        string `json:"lane"`

	// AgentHandle is the seat holding, or asking for, the grant.
	AgentHandle string `json:"agent_handle"`

	// Change is one of the constants above.
	Change string `json:"change"`

	// Grant is the grant as it stands.
	Grant GrantSetting `json:"grant"`

	// By is the person who granted, declined or removed it; empty for what
	// the agent or the engine did.
	By string `json:"by,omitempty"`

	// Request is the agent's request the change is about, where there is one.
	Request string `json:"request,omitempty"`

	// Detail is the engine's own words for a refusal: what the provisioner
	// has to hold first.
	Detail string `json:"detail,omitempty"`

	// At is when it changed.
	At time.Time `json:"at"`
}

// GrantSetting is one grant in the vendor's terms, with what the one change it
// was asked for changes, where it was.
type GrantSetting struct {
	ID         string    `json:"id,omitempty"`
	Permission string    `json:"permission"`
	Scope      []string  `json:"scope,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
	Target     string    `json:"target,omitempty"`
}

// EventType is the "seat_grant_changed" wire type.
func (SeatGrantChanged) EventType() string { return "seat_grant_changed" }

// SummaryFor names the seat, the change and the permission.
func (e SeatGrantChanged) SummaryFor(actor string) string {
	who := e.AgentHandle
	if actor != "" {
		who = actor
	}
	return who + "'s " + e.Lane + " grant of " + e.Grant.Permission + " was " + e.Change
}
