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
	// [Classify] promoted into Report — and the promoted one is FIRST, so a
	// reader wanting "what else is wrong" takes the tail rather than
	// re-deriving which finding the report is about. See [Promote].
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
	// third-party app failing to look at it.
	LastError string `json:"last_error,omitempty"`

	// LastAttemptAt and SettledAt are different questions and both are
	// asked. A row that keeps running and never settling reads nothing
	// like one nobody has looked at, and a single timestamp cannot tell
	// them apart.
	LastAttemptAt time.Time `json:"last_attempt_at,omitzero"`
	SettledAt     time.Time `json:"settled_at,omitzero"`

	// NextAttemptAt is when this integration becomes due again.
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`

	// Endpoint is the public base URL this surface was last set up
	// against, which is what its registration at the third-party app
	// points at.
	//
	// WRITTEN SO THE ADDRESS CAN BE COMPARED WITH THE ONE IN FORCE. A
	// company's public base moves: a tunnel is restarted, a deployment is
	// renamed, a proxy is put in front. Where a pass registers the hook,
	// the next tick re-registers it at the new address on its own and this
	// field simply follows. Where NOTHING does, the registration goes on
	// pointing at an address that no longer answers, the surface reports
	// ready because nothing it can see is wrong, and the first symptom is
	// an agent that has stopped replying.
	//
	// So the two are kept apart and compared. A stale one is an ingress
	// fault a person has to fix at the third-party app, and it is the only
	// evidence there is: Slack's request URL, for one, cannot be read back
	// without an app-configuration token the operator may not have.
	Endpoint string `json:"endpoint,omitempty"`

	// Disconnecting is set the moment somebody asks for the integration
	// to be taken away, which is BEFORE any teardown pass has run and
	// set a phase. Without it the screen shows a connected integration
	// for as long as it takes the first pass to start, and a reader
	// presses Disconnect again.
	//
	// It is also what makes the teardown survive its own failures: the
	// intent lives on the fleet row rather than in the request that
	// asked, so a third-party app refusing the delete leaves the surface
	// disconnecting and retrying rather than quietly connected again.
	Disconnecting bool `json:"disconnecting,omitempty"`

	// RemoveSeats carries the operator's answer to "also remove the
	// accounts Crewlet created". The engine's own webhooks come out
	// either way — it registered them and nothing else uses them — but
	// an account may be a person's colleague in that third-party app, so deleting
	// one is never inferred.
	RemoveSeats bool `json:"remove_seats,omitempty"`
}

// Reported is the report a reader should be shown.
//
// The stored one, except in the window this method exists for: between
// somebody pressing Disconnect and the first teardown pass running, the
// intent is set and no pass has written a phase yet. The stored report is
// then whatever the last RECONCILE concluded — usually ready — so a screen
// reading it directly shows a connected integration somebody has already
// asked to remove, and they press the button again.
//
// Derived rather than written at request time because the two facts have
// different owners: the intent is the operator's and the phase is the loop's,
// and writing a phase on the operator's behalf would make a status row claim
// a pass had run when none had.
func (s State) Reported() Report {
	if s.Disconnecting && s.Report.Phase != PhaseDisconnecting {
		return Report{
			Phase: PhaseDisconnecting, Actor: ActorEngine,
			Detail: "this integration is being removed",
		}
	}
	return s.Report
}

// Observed reports whether a pass has ever said anything about this surface.
//
// A ROW IS NOT A REPORT. Two writers put rows in this store: a pass, which
// records a phase, and the setup write that stamps [State.Endpoint] for a
// surface no pass converges (internal/api/setupapi). The second leaves a row
// carrying one address and nothing else, and a reader that treats the row's
// existence as "the loop has reported on this" renders an EMPTY phase as the
// integration's status — which on the dashboard is a card with no state on it
// at all, on precisely the surfaces whose state a person has to act on.
//
// The phase is the test rather than a flag of its own, because a phase is
// what a report IS: every writer that has observed anything sets one, and a
// value a newer node wrote is still a phase to a reader that cannot name it.
func (s State) Observed() bool { return s.Reported().Phase != "" }

// TearingDown reports whether this surface is being taken away.
//
// Reads the INTENT rather than the phase, because the two are not the same
// for the first pass: the flag is set when somebody presses Disconnect and
// the phase only follows once a pass has run. A caller that watched the phase
// would treat the gap as a still-connected integration.
func (s State) TearingDown() bool {
	return s.Disconnecting || s.Report.Phase == PhaseDisconnecting
}

// Due reports whether this integration wants a pass at now.
func (s State) Due(now time.Time) bool { return !now.Before(s.NextAttemptAt) }

// MaxLastErrorLength bounds the fault text one state carries.
//
// A wrapped error chain from a third-party app client is a sentence or two; a client
// that pastes a response body into its error is a megabyte, and this record
// is written to a KV whose value size is a hard limit shared with every other
// integration's state. Truncated rather than refused: the first sentence of a
// long error is the useful part, and dropping the whole write would lose the
// phase alongside it.
const MaxLastErrorLength = 2000

// MaxDetailLength bounds one finding's own sentence, and the report's.
//
// SMALLER THAN [MaxLastErrorLength] because there can be many: a row carries
// one fault text and a finding PER SEAT, so a company with fifty agents on a
// vendor that pastes response bodies into its messages writes fifty of these
// into a single KV value. Capping only the fault left the larger half
// unbounded, and an oversized row is not truncated by the store — it is
// REFUSED, so the surface's whole status silently stops being recorded.
//
// A sentence naming what is outstanding fits easily; this is a ceiling on a
// third-party app's prose, not a budget for the engine's own.
const MaxDetailLength = 500

// bound caps every piece of third-party text a status row carries.
//
// AT THE BOUNDARY rather than in each vendor, because the limit belongs to
// what this row is written into and there are seven vendors who would each
// have to remember it — and the two most recent did not.
func bound(report Report, findings []Finding) (Report, []Finding) {
	report.Detail = textcut.Ellipsis(report.Detail, MaxDetailLength)
	for i := range findings {
		findings[i].Detail = textcut.Ellipsis(findings[i].Detail, MaxDetailLength)
	}
	return report, findings
}

// truncateError applies [MaxLastErrorLength].
//
// Through [textcut.Ellipsis] rather than a slice, and through textcut rather
// than a local helper: a plain detail[:n] splits whatever multi-byte
// character straddles the cut, which the KV's JSON encoding then replaces
// with U+FFFD, so a third-party app error carrying an accented message reaches the
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
	// somebody types the integration subcommand's decommission flag and reads
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
	// THE WAIT THIS ROW WAS ON before this pass changed it. Attempts pace a
	// backoff, and a backoff only means anything within one wait — see
	// [CadenceOf].
	was := CadenceOf(state.Report)

	state.Kind = kind
	state.LastAttemptAt = now

	switch {
	case errors.Is(err, ErrNotConfigured):
		return state, true
	case errors.Is(err, ErrCredentialRejected):
		// NOT A WAIT. The case below treats a fault as one because almost
		// every fault is a third-party app briefly unreachable, and that reasoning
		// is exactly backwards here: a refused credential is refused
		// identically on every subsequent pass, so reporting "the engine
		// is working on it" tells an operator to wait for something that
		// will never happen while the one action that would fix it is
		// theirs.
		//
		// Synthesised as a finding rather than assembled inline, so the
		// phase and actor come from the same [FindingKind.Verdict] table
		// every other kind reads and cannot drift from it.
		//
		// NO Detail, deliberately: the kind's own sentence is the
		// headline, and the third-party app's words go in LastError below. Putting
		// the raw error in both printed the same wrapped chain twice, one
		// line under the other, and the chain is long enough that the two
		// copies filled the row.
		finding := Finding{Kind: FindingCredentialRejected}
		state.Report = Classify([]Finding{finding})
		state.Findings = []Finding{finding}
		state.Attempts++
		state.LastError = truncateError(err.Error())
	case err != nil:
		// A FAULT IS A WAIT. Almost every one is a third-party app briefly
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
		state.Report, state.Findings = bound(Classify(findings), Promote(findings))
		state.LastError = ""
		if state.Report.Phase == PhaseReady {
			state.Attempts = 0
			state.SettledAt = now
		} else {
			state.Attempts++
		}
	}
	// A CHANGE OF WAIT RESTARTS THE COUNT. Attempts are consecutive passes
	// that did not settle, and [Schedule.Next] reads them against whichever
	// wait the row is NOW on — so a surface that spent ten ticks waiting on
	// the engine carried a count of ten into the wait for a PERSON and
	// started it at the ceiling.
	//
	// That is the one cadence where the ceiling is wrong. The brisk admin
	// interval exists so an operator who installs an app "sees provisioning
	// continue without pressing anything", and inherited attempts skipped it
	// entirely: the fast retries never happened, and the operator watched a
	// screen that would not move for ten minutes.
	if CadenceOf(state.Report) != was {
		state.Attempts = min(state.Attempts, 1)
	}
	state.Outcome = state.Report.Outcome()
	return state, false
}

// ObserveTeardown records what a TEARDOWN pass concluded, and reports whether
// the surface is finished with.
//
// Separate from [Observe] because the two read the same inputs to opposite
// conclusions. A normal pass that fails is a wait: the integration is still
// meant to exist and the next pass carries it forward. A teardown that
// SUCCEEDS is the end of the row — nothing is left to reconcile — and one
// that fails must hold the surface in [PhaseDisconnecting] rather than let it
// drift back to looking connected, because the block is still in the company
// document and a normal pass would report it healthy.
//
// forget is true only on success. The caller removes the block in the same
// step, and the order matters: the block is the credential the teardown
// authenticates with, so removing it first would strand whatever the third-party app
// still holds.
func ObserveTeardown(state State, kind Kind, err error, now time.Time) (next State, forget bool) {
	state.Kind = kind
	state.LastAttemptAt = now
	state.Disconnecting = true
	state.Findings = nil

	if err == nil {
		state.LastError = ""
		state.SettledAt = now
		state.Attempts = 0
		return state, true
	}

	state.Report = Report{
		Phase: PhaseDisconnecting, Actor: ActorEngine,
		Detail: "the last teardown pass could not finish",
	}
	state.Outcome = state.Report.Outcome()
	state.Attempts++
	state.LastError = truncateError(err.Error())
	return state, false
}
