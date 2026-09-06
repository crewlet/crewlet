package integration

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/textcut"
)

// State is one integration's standing, as the fleet holds it.
//
// # Why the fleet holds it at all
//
// The loop is a singleton, so one node writes this. But the node that READS
// it is usually a different one: the dashboard and the REST API answer from
// whichever node serves ingress, and an operator running `-roles ingress`
// has split those onto separate hosts on purpose. On the node's own database
// this would be invisible from the surface that exists to show it, which is
// the same failure the control plane's activation pointer was built to
// remove.
type State struct {
	Kind Kind `json:"kind"`

	// Report is the last pass's conclusion, and the only field an operator
	// reads directly.
	Report Report `json:"report"`

	// Findings is EVERYTHING the last pass observed, not only the one
	// [Classify] promoted into Report.
	//
	// Kept because the report answers "what should I do next" and this
	// answers "what is actually wrong", and they are different questions
	// the moment more than one thing is. A company with a broken webhook
	// and four under-granted seats reports the webhook, and an operator
	// who fixes it should not have to wait a full pass to discover there
	// were four more things behind it.
	Findings []Finding `json:"findings,omitempty"`

	// Outcome is how the loop read Report. Stored rather than recomputed so
	// a reader sees the value the schedule was actually derived from, even
	// when a later build would classify the same report differently.
	Outcome Outcome `json:"outcome"`

	// Attempts counts consecutive passes that did not settle, and is what
	// the backoff is computed from. Reset the moment one does.
	Attempts int `json:"attempts"`

	// LastError is the fault from the last pass that returned one, cleared
	// by any pass that does not. A FAULT IS NOT A FINDING: a finding is a
	// statement about the operator's world, and this is the engine or the
	// vendor failing to look at it.
	LastError string `json:"last_error,omitempty"`

	// LastAttemptAt and SettledAt are different questions and both are
	// asked. A row that keeps running and never settling reads nothing
	// like one nobody has looked at, and a single timestamp cannot tell
	// them apart.
	LastAttemptAt time.Time `json:"last_attempt_at,omitzero"`
	SettledAt     time.Time `json:"settled_at,omitzero"`

	// NextAttemptAt is when this integration becomes due again.
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
}

// Due reports whether this integration wants a pass at now.
func (s State) Due(now time.Time) bool { return !now.Before(s.NextAttemptAt) }

// MaxLastErrorLength bounds the fault text one state carries.
//
// A wrapped error chain from a vendor client is a sentence or two; a client
// that pastes a response body into its error is a megabyte, and this record
// is written to a KV whose value size is a hard limit shared with every other
// integration's state. Truncated rather than refused: the first sentence of a
// long error is the useful part, and dropping the whole write would lose the
// phase alongside it.
const MaxLastErrorLength = 2000

// truncateError applies [MaxLastErrorLength].
//
// Through [textcut.Ellipsis] rather than a slice, and through textcut rather
// than a local helper: a plain detail[:n] splits whatever multi-byte
// character straddles the cut, which the KV's JSON encoding then replaces
// with U+FFFD, so a vendor error carrying an accented message reaches the
// operator garbled rather than merely shortened. That rule already has one
// home in this tree and does not need a second, which is the whole reason
// textcut exists.
func truncateError(detail string) string {
	return textcut.Ellipsis(detail, MaxLastErrorLength)
}

// Store is where the loop keeps what it found.
//
// Defined here, by the consumer, and kept to the three calls the loop makes.
// The coordination store is the implementation; nothing in this package knows
// that, which is what lets the worker be tested against a map.
type Store interface {
	// LoadIntegrations reads every integration's state.
	//
	// RAISES rather than answering empty on an unreachable store. "No
	// integration has ever been reconciled" and "the store cannot be read"
	// send the loop down opposite paths: the first is a fleet that should
	// start converging, and the second is one that must not conclude
	// anything about a company it cannot see.
	LoadIntegrations(ctx context.Context) ([]State, error)

	// SaveIntegration records one integration's state.
	SaveIntegration(ctx context.Context, state State) error

	// ForgetIntegration drops an integration's state once its block has
	// left the company document.
	//
	// Removing the block is the operator saying what the engine should
	// stop talking to. It is NOT a request to destroy the accounts,
	// memberships and webhooks a previous pass created: those stay until
	// somebody types the vendor subcommand's decommission flag and reads
	// what it is about to delete. See this package's doc.
	ForgetIntegration(ctx context.Context, kind Kind) error
}

// Observe folds one pass's outcome into a state.
//
// THE SAME RULE WHATEVER RAN THE PASS. The loop's tick and an operator
// pressing a button in the dashboard are the same event as far as this record
// is concerned, and two callers writing it two ways is how a status row
// starts disagreeing with the surface that produced it. So the three
// outcomes, and what each does to the record, live here rather than inside
// the worker's loop.
//
// forget reports [ErrNotConfigured]: the block left the company document, so
// the row is removed rather than recorded. A status for a surface nobody
// configured would sit in a fleet view describing an integration that is
// gone.
//
// The caller sets NextAttemptAt, through [Schedule.Next], because the cadence
// is the loop's business and a pass run by hand does not change it.
func Observe(state State, kind Kind, findings []Finding, err error, now time.Time) (next State, forget bool) {
	state.Kind = kind
	state.LastAttemptAt = now

	switch {
	case errors.Is(err, ErrNotConfigured):
		return state, true
	case err != nil:
		// A FAULT IS A WAIT. Almost every one is a vendor briefly
		// unreachable, and none of the rest is fixed by giving up. The
		// findings are DROPPED rather than kept: a pass that failed did
		// not observe the world, and rendering a previous pass's
		// observations under this pass's timestamp would age a stale
		// answer into a current one.
		state.Report = Report{
			Phase: PhaseActivating, Actor: ActorEngine,
			Detail: "the last pass could not read this integration",
		}
		state.Findings = nil
		state.Attempts++
		state.LastError = truncateError(err.Error())
	default:
		state.Report = Classify(findings)
		state.Findings = findings
		state.LastError = ""
		if state.Report.Phase == PhaseReady {
			state.Attempts = 0
			state.SettledAt = now
		} else {
			state.Attempts++
		}
	}
	state.Outcome = state.Report.Outcome()
	return state, false
}
