package integration

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
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

	// TeardownHeldBy is what the last failed teardown pass was held by,
	// empty where none failed: the third-party app's half, or the company's
	// own settings refusing the revision that removes the integration
	// ([ErrBlockNotRemoved]). Stored beside LastError so a reader says which
	// without parsing the fault's text.
	TeardownHeldBy TeardownHold `json:"teardown_held_by,omitempty"`

	// LastAttemptAt and SettledAt are different questions and both are
	// asked. A row that keeps running and never settling reads nothing
	// like one nobody has looked at, and a single timestamp cannot tell
	// them apart.
	LastAttemptAt time.Time `json:"last_attempt_at,omitzero"`
	SettledAt     time.Time `json:"settled_at,omitzero"`

	// NextAttemptAt is when this integration becomes due again.
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`

	// Endpoint is the public base URL this surface was last set up
	// against, which its registration at the third-party app points at.
	//
	// WRITTEN SO IT CAN BE COMPARED WITH THE ADDRESS IN FORCE. A public
	// base moves (a tunnel restarts, a proxy goes in front). Where a pass
	// registers the hook it re-registers and this field follows. Where
	// NOTHING does, the registration points at a dead address, the surface
	// still reports ready, and the first symptom is an agent that stopped
	// replying. A stale one is an ingress fault a person fixes at the
	// third-party app, and the only evidence: Slack's request URL cannot be
	// read back without a configuration token the operator may lack.
	Endpoint string `json:"endpoint,omitempty"`

	// Registration is the NAME this surface's registration is held under
	// at the third-party app, where the address is not enough to find it.
	//
	// Datadog is the case and today the only one: a webhook definition is
	// addressed by name, that name is a live config field, and Datadog
	// serves no listing at all — so a definition the engine stops managing
	// can never be found again by anything. Recorded so a rename is VISIBLE:
	// the previous definition goes on delivering, correctly, for every
	// monitor still naming it, and this is the only place an operator can
	// learn there are now two. See [StampRegistration].
	Registration string `json:"registration,omitempty"`

	// Disconnecting is set the moment somebody asks for removal, BEFORE
	// any teardown pass has set a phase; otherwise the screen shows a
	// connected integration until the first pass starts and a reader
	// presses Disconnect again. It also makes the teardown survive its own
	// failures: the intent lives on the fleet row, not the request, so a
	// third-party app refusing the delete leaves the surface disconnecting
	// and retrying rather than quietly connected again.
	Disconnecting bool `json:"disconnecting,omitempty"`

	// RemoveSeats carries the operator's answer to "also remove the
	// accounts Crewlet created". The engine's own webhooks come out
	// either way — it registered them and nothing else uses them — but
	// an account may be a person's colleague in that third-party app, so deleting
	// one is never inferred.
	RemoveSeats bool `json:"remove_seats,omitempty"`

	// RequestedBy is who asked for the integration to be taken away; every
	// teardown write (the revision dropping the block, cleared seat
	// credentials, deleted sealed values) is recorded under them.
	//
	// ON THE ROW, for Disconnecting's reason: the loop finishes the
	// teardown on whichever node holds its duty, long after the request
	// answered. Without it the dropped block's revision would name the
	// loop while a FORCED disconnect (inside the request) names the person.
	//
	// Written with the intent by its one writer ([AskTeardown]), from an
	// attribution that always names somebody.
	RequestedBy Requester `json:"requested_by,omitzero"`
}

// Requester is a party as a row records it: [iam.Actor], which carries no wire
// names of its own, in the three fields every trail records a write under.
type Requester struct {
	// Name is the author, Kind which of the four actor kinds it is, and
	// OperatorID the credential it acted through.
	Name       string        `json:"name,omitempty"`
	Kind       iam.ActorKind `json:"kind,omitempty"`
	OperatorID string        `json:"operator_id,omitempty"`
}

// RequesterOf is a party as a row records it.
func RequesterOf(by iam.Actor) Requester {
	return Requester{Name: by.Name, Kind: by.Kind, OperatorID: by.OperatorID}
}

// Actor is the party this row names, and false for a row that names nobody.
func (r Requester) Actor() (iam.Actor, bool) {
	if r.Name == "" {
		return iam.Actor{}, false
	}
	return iam.Actor{Name: r.Name, Kind: r.Kind, OperatorID: r.OperatorID}, true
}

// Reported is the report a reader should be shown.
//
// The stored one, except between Disconnect being pressed and the first
// teardown pass: the intent is set, no pass has written a phase, and the
// stored report is still the last RECONCILE's (usually ready), so a screen
// reading it shows a connected integration already asked to go.
//
// Derived rather than written at request time because the intent is the
// operator's and the phase is the loop's; writing a phase for the operator
// would claim a pass had run when none had.
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
// A ROW IS NOT A REPORT. Two writers put rows here: a pass, which records
// a phase, and the setup write that stamps [State.Endpoint] for a surface no
// pass converges (internal/api/setupapi), leaving a row with one address and
// nothing else. A reader treating that row as "the loop has reported" renders
// an EMPTY phase: a dashboard card with no state, on exactly the surfaces a
// person has to act on.
//
// The phase is the test, not a flag, because a phase is what a report IS, and
// a value a newer node wrote is still a phase to a reader that cannot name it.
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

// LogWhole puts on this node's log, WHOLE, every piece of text the status row
// a pass is about to write will carry shortened — a finding's detail past
// [MaxDetailLength], a fault past [MaxLastErrorLength].
//
// The row's bounds are its KV value's and they stand. But the row was the only
// place that text was written, so what a bound left out was lost: a validation
// list's later fields, the scope a refusal named after its first sentence. It
// is said once, here, where the pass ran, by both writers of a row — the loop
// and the dashboard's pass — before the fold, so whichever ran it leaves the
// whole text behind.
func LogWhole(ctx context.Context, kind Kind, findings []Finding, err error) {
	for _, f := range findings {
		if len(f.Detail) > MaxDetailLength {
			log.InfoContext(ctx, "integration_finding_shortened", "integration", kind,
				"finding", string(f.Kind), "subject", f.Subject, "bytes", len(f.Detail),
				"detail", f.Detail)
		}
	}
	if err != nil && len(err.Error()) > MaxLastErrorLength {
		log.InfoContext(ctx, "integration_fault_shortened", "integration", kind,
			"bytes", len(err.Error()), "error", err.Error())
	}
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

	// LoadIntegration reads ONE surface's state, reporting whether there is
	// one to read.
	//
	// Separate from the plural read because only one use may be stale.
	// [Worker.Tick] reads every row once to decide what is DUE, which
	// tolerates being seconds old. Folding a pass's outcome into a row read
	// before the pass began does not: an operator's disconnect landing in
	// that window is silently overwritten. So the fold's row is re-read
	// here, under the surface's own guard, with every other writer excluded.
	//
	// found is false for a surface nobody has recorded anything about (every
	// surface before its first pass): an ordinary answer, not an error.
	LoadIntegration(ctx context.Context, kind Kind) (state State, found bool, err error)

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
// THE SAME RULE WHATEVER RAN THE PASS: the loop's tick and an operator's
// button are one event, and two callers writing a row two ways is how it
// comes to disagree with its surface. So the outcomes live here.
//
// forget reports [ErrNotConfigured]: the block left the company document, so
// the row is removed; a status for a surface nobody configured would describe
// a gone integration in the fleet view.
//
// The caller sets NextAttemptAt, through [Schedule.Next]: the cadence is the
// loop's business and a pass run by hand does not change it.
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
		// NOT A WAIT. The case below treats a fault as one because most
		// are a third-party app briefly unreachable; a refused credential
		// is refused identically on every pass, so "the engine is working
		// on it" would tell an operator to wait for what never happens
		// while the fix is theirs.
		//
		// Synthesised as a finding so phase and actor come from the same
		// [FindingKind.Verdict] table every other kind reads.
		//
		// NO Detail: the kind's own sentence is the headline and the
		// third-party app's words go in LastError below; the raw error in
		// both printed the same long chain twice.
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
	// A CHANGE OF WAIT RESTARTS THE COUNT. [Schedule.Next] reads Attempts
	// against whichever wait the row is NOW on, so ten ticks spent waiting
	// on the engine would start the wait for a PERSON at the ceiling. That
	// is the one cadence where the ceiling is wrong: the brisk admin
	// interval exists so an operator who installs an app "sees provisioning
	// continue without pressing anything", and inherited attempts skipped
	// it, leaving a screen that would not move for ten minutes.
	if CadenceOf(state.Report) != was {
		state.Attempts = min(state.Attempts, 1)
	}
	state.Outcome = state.Report.Outcome()
	return state, false
}

// AskTeardown records that an operator asked for this surface to be taken
// away, before any pass has run.
//
// A NAMED FOLD beside [Observe] and [ObserveTeardown]: a row means the same
// whether a tick or a button wrote it.
//
// IT CLEARS THE LAST RECONCILE'S FINDINGS: they describe a world being taken
// apart (a stale "re-enable it at Datadog" under Disconnecting says the
// operator has work to do). [ObserveTeardown] erases them on later passes, and
// nothing unsets Disconnecting, so they never matter again. NextAttemptAt is
// zeroed (DUE NOW) so the disconnect skips a reconciled row's backoff. BY is
// replaced by a later ask: the last to press Disconnect is who it is for.
func AskTeardown(state State, kind Kind, removeSeats bool, by iam.Actor) State {
	state.Kind = kind
	state.Disconnecting = true
	state.RemoveSeats = removeSeats
	state.RequestedBy = RequesterOf(by)
	state.NextAttemptAt = time.Time{}
	state.Attempts = 0

	state.Findings = nil
	state.LastError = ""
	state.TeardownHeldBy = ""
	state.Report = Report{
		Phase: PhaseDisconnecting, Actor: ActorEngine,
		Detail: "the engine is removing what this integration holds",
	}
	state.Outcome = state.Report.Outcome()
	return state
}

// ObserveTeardown records what a TEARDOWN pass concluded, and reports whether
// the surface is finished with.
//
// Separate from [Observe] because the same inputs reach opposite conclusions:
// a failed normal pass is a wait, a failed teardown must hold the surface in
// [PhaseDisconnecting] (the block is still in the document, so a normal pass
// would report it healthy), and a successful one ends the row.
//
// forget is true only on success. The caller removes the block in the same
// step, and the order matters: the block is the credential the teardown
// authenticates with, so removing it first would strand whatever the
// third-party app still holds.
func ObserveTeardown(state State, kind Kind, err error, now time.Time) (next State, forget bool) {
	state.Kind = kind
	state.LastAttemptAt = now
	state.Disconnecting = true
	state.Findings = nil

	if err == nil {
		state.LastError = ""
		state.TeardownHeldBy = ""
		state.SettledAt = now
		state.Attempts = 0
		return state, true
	}

	// WHAT HELD IT IS SAID, in the report's sentence and as a value: a
	// revision the company's settings refused is nothing the third-party app
	// did, and a screen that blamed the vendor for it sent people to look
	// where nothing was wrong.
	state.TeardownHeldBy = TeardownHeldByVendor
	detail := "the last teardown pass could not finish"
	if errors.Is(err, ErrBlockNotRemoved) {
		state.TeardownHeldBy = TeardownHeldBySettings
		detail = "what this integration holds was removed, and the company's settings " +
			"refused the change that removes the integration itself"
	}
	state.Report = Report{Phase: PhaseDisconnecting, Actor: ActorEngine, Detail: detail}
	state.Outcome = state.Report.Outcome()
	state.Attempts++
	state.LastError = truncateError(err.Error())
	return state, false
}

// TeardownHold is what a failed teardown pass was held by.
type TeardownHold string

// What holds a teardown.
const (
	// TeardownHeldByVendor is the third-party app's half: a call it refused
	// or did not answer.
	TeardownHeldByVendor TeardownHold = "vendor"
	// TeardownHeldBySettings is the company's settings refusing the revision
	// that removes the integration ([ErrBlockNotRemoved]).
	TeardownHeldBySettings TeardownHold = "settings"
)

// Valid reports whether h is a member of the closed set.
func (h TeardownHold) Valid() bool {
	return h == TeardownHeldByVendor || h == TeardownHeldBySettings
}
