package types

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/events"
)

// Live config management: the control plane's two halves — a revision becoming
// the one to serve, and each node reporting what it did about that.

func init() {
	events.Register[ConfigRevisionActivated]()
	events.Register[ConfigRevisionApplied]()
	events.Register[ConfigRevisionScrubbed]()
}

// ConfigRevisionActivated is published when a new company_config revision is
// activated (a config write, a per-entity edit, a revert, or a CLI import).
//
// Every node fetches the payload from the DB and applies it. Deliberately thin:
// the event is a nudge, and the authoritative path is the epoch pointer, so
// losing one costs a poll interval rather than a revision.
//
// The tempting shape redeclares the envelope's own `source` field. It is NOT a
// payload field here: the envelope owns that key and drops a colliding one, so
// declaring it would produce a field that silently never arrives. Read the
// envelope's Source instead.
//
// CreatedBy is the revision's AUTHOR, and CreatedByKind and OperatorID are the
// two facts every other trail records beside one: what sort of author, and
// the credential the write was made through — empty on a write no credential
// made. Additive, so a peer that knows neither round-trips both.
type ConfigRevisionActivated struct {
	RevisionID      string `json:"revision_id"`
	RevisionSummary string `json:"revision_summary"`
	CreatedBy       string `json:"created_by"`
	CreatedByKind   string `json:"created_by_kind,omitempty"`
	OperatorID      string `json:"operator_id,omitempty"`
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
// Status has no useful zero value: the wire default is ApplyOK while the Go
// zero value is "", so a publisher must set it explicitly. An unset status
// reads as not-ok.
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

// ConfigRevisionScrubbed records that a SUPERSEDED revision's personal fields
// were erased.
//
// # Why erasing something is worth an event
//
// Every other write to company_config appends, which is what makes the
// history a record. A scrub rewrites a row in place — the only write that
// does — so a diff across it shows a tombstone where an address used to be,
// and a reader with no event to find would have to decide between "somebody
// ran the scrub" and "this row is damaged". This is what makes that
// answerable a year later.
//
// It names the revision and COUNTS the fields; it never names them, and it
// certainly never carries what they held. An event that said which addresses
// it removed would put them straight back into the audit log the scrub was
// run to keep them out of — in a table this node also keeps, also on every
// peer, and also in every backup.
type ConfigRevisionScrubbed struct {
	RevisionID string `json:"revision_id"`

	// Fields is how many values were replaced with the tombstone. Zero
	// means the revision was already clean, which is a different fact from
	// not having been scrubbed and is why the event is published either
	// way.
	Fields int `json:"fields"`

	// ScrubbedBy is the operator who ran it. The envelope owns `source`,
	// so this does not restate it — see [ConfigRevisionActivated].
	ScrubbedBy string `json:"scrubbed_by"`
}

// EventType is the "config_revision_scrubbed" wire type.
func (ConfigRevisionScrubbed) EventType() string { return "config_revision_scrubbed" }

// Summary says what happened to the revision, and reads as a no-op where it
// was one: "scrubbed 0 fields" is the answer to "did this need it", and a
// line that hid the difference would make a second run look like a first.
func (e ConfigRevisionScrubbed) Summary() string {
	if e.Fields == 0 {
		return "Config revision " + e.RevisionID + " held no personal data to scrub"
	}
	return fmt.Sprintf("Config revision %s scrubbed: %d personal field(s) erased",
		e.RevisionID, e.Fields)
}
