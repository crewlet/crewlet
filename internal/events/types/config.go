package types

import (
	"strings"

	"github.com/crewlet/crewlet/internal/events"
)

// Live config management: the control plane's two halves — a revision becoming
// the one to serve, and each node reporting what it did about that.

func init() {
	events.Register[ConfigRevisionActivated]()
	events.Register[ConfigRevisionApplied]()
}

// ConfigRevisionActivated is published when a new company_config revision is
// activated (a config write, a per-entity edit, a revert, or a CLI import).
//
// Every node fetches the payload from the DB and applies it. Deliberately thin:
// the event is a nudge, and the authoritative path is the epoch pointer, so
// losing one costs a poll interval rather than a revision.
//
// The Python class redeclares the envelope's own `source` field. It is NOT a
// payload field here: the envelope owns that key and drops a colliding one, so
// declaring it would produce a field that silently never arrives. Read the
// envelope's Source instead.
type ConfigRevisionActivated struct {
	RevisionID      string `json:"revision_id"`
	RevisionSummary string `json:"revision_summary"`
	CreatedBy       string `json:"created_by"`
}

// EventType is the "config_revision_activated" wire type.
func (ConfigRevisionActivated) EventType() string { return "config_revision_activated" }

// Summary prefers the author's own revision summary and falls back to the id:
// an operator reading the feed wants to know WHAT changed, and the id alone
// never says.
func (e ConfigRevisionActivated) Summary() string {
	if e.RevisionSummary != "" {
		return "Config revision activated: " + e.RevisionSummary
	}
	return "Config revision " + e.RevisionID + " activated"
}

// ApplyStatus is one node's outcome for one revision.
//
// Three-valued, and the third value is why it cannot be a bool: degraded means
// the apply failed AFTER a restart-required subsystem had already been mutated,
// so the rollback could not restore it. A degraded node is not converged and
// must never be counted as one.
type ApplyStatus string

// The three outcomes an apply can report. There is no fourth for "still
// going": a node publishes this once it has finished, and silence is what a
// convergence check reads as not-yet-applied.
const (
	ApplyOK       ApplyStatus = "ok"
	ApplyError    ApplyStatus = "error"
	ApplyDegraded ApplyStatus = "degraded"
)

// ConfigRevisionApplied reports one node's outcome after applying a revision.
//
// The DB row stays active whatever the outcome — divergence is surfaced through
// this event, not by deactivating what the rest of the fleet is happily serving.
//
// Status has no useful zero value: ApplyOK is Python's default while Go's zero
// value is "", so a publisher must set it explicitly. An unset status reads as
// not-ok here, exactly as it would in Python.
type ConfigRevisionApplied struct {
	RevisionID        string      `json:"revision_id"`
	Status            ApplyStatus `json:"status"`
	AppliedSubsystems []string    `json:"applied_subsystems,omitempty"`
	Error             string      `json:"error"`
}

// EventType is the "config_revision_applied" wire type.
func (ConfigRevisionApplied) EventType() string { return "config_revision_applied" }

// Summary reads as a failure for every status other than ApplyOK, degraded
// included: a node that could not finish an apply has not converged, and a line
// that hedged would let the fleet look healthier than it is. The envelope's
// source names which node — this line does not repeat it.
func (e ConfigRevisionApplied) Summary() string {
	if e.Status == ApplyOK {
		return "Config revision " + e.RevisionID + " applied"
	}
	reason := e.Error
	if reason == "" {
		reason = "unknown error"
	}
	return strings.Join([]string{"Config revision", e.RevisionID, "failed:", reason}, " ")
}
