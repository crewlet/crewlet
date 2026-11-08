package types

import (
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// A seat told what changed in its own access (ADR-0104).

func init() {
	events.Register[AgentCapabilityTold]()
}

// AgentCapabilityTold fires when a turn's first launch carried the seat what
// changed in its access since it was last told, or its first snapshot of it:
// the fleet trail of the told record the access log keeps.
//
// Lines are the change lines the turn was shown, in the engine's words, and
// carry no credential: what a seat holds and at what level, never a value. The
// seat's activity rather than a step of its turn, so it names no turn: the told
// record on the access log names it.
type AgentCapabilityTold struct {
	AgentHandle string `json:"agent_handle"`

	// Digest is the snapshot's digest, Baseline a seat's first told record.
	Digest   string `json:"digest"`
	Baseline bool   `json:"baseline,omitempty"`

	// Changes is how many changes the turn was told, and Lines the first of
	// them as the turn read them.
	Changes int      `json:"changes"`
	Lines   []string `json:"lines,omitempty"`

	// RevisionFrom and RevisionTo are the company revisions the changes span.
	RevisionFrom string `json:"revision_from,omitempty"`
	RevisionTo   string `json:"revision_to,omitempty"`

	ToldAt time.Time `json:"told_at"`
}

// EventType is the "agent_capability_told" wire type.
func (AgentCapabilityTold) EventType() string { return "agent_capability_told" }

// SummaryFor names the seat and how much it was told.
func (e AgentCapabilityTold) SummaryFor(actor string) string {
	who := e.AgentHandle
	if actor != "" {
		who = actor
	}
	if e.Changes == 0 {
		return who + " was told its access for the first time"
	}
	if e.Changes == 1 {
		return who + " was told 1 change to its access"
	}
	return who + " was told " + strconv.Itoa(e.Changes) + " changes to its access"
}
