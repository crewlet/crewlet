package integration

import (
	"slices"
	"strings"
)

// Phase is where an integration has got to.
//
// One vocabulary for every surface. Vendors differ in what they call things,
// but an integration always passes through the same stations: a credential,
// sometimes an app or an approval, then identities, then the vendor applying
// what it accepted.
//
// The phase is what an operator reads and what the cadence is derived from,
// so a new corner case is added here once rather than in seven classifiers
// and a dashboard.
type Phase string

const (
	// PhaseUnconfigured means this deployment cannot talk to the surface
	// at all: the block is enabled and the credential its ${VAR} names
	// resolved to nothing. Distinct from an ABSENT block, which is not a
	// phase because it is not an integration.
	PhaseUnconfigured Phase = "unconfigured"

	// PhaseAwaitingAdmin means a person must install or approve something
	// at the vendor before the engine can act: a Slack app a workspace
	// administrator has to approve, a GitHub App installation, the
	// Atlassian Forge app. The engine cannot do it and retrying does not
	// help, but it resumes on its own the moment they finish.
	PhaseAwaitingAdmin Phase = "awaiting_admin"

	// PhaseProvisioning means the engine is creating identities. Its own
	// work, and the next pass carries it forward.
	PhaseProvisioning Phase = "provisioning"

	// PhaseActivating means the vendor accepted something and has not
	// applied it yet: a membership, a role, an install. Nobody has to act.
	PhaseActivating Phase = "activating"

	// PhaseDegraded means agents are working but something needs a person:
	// access the operator's own scheme grants beyond what the engine asks
	// for, a tier the company document names that the vendor does not
	// have, a webhook that cannot be registered.
	PhaseDegraded Phase = "degraded"

	// PhaseDisconnecting means the integration is being taken away and
	// what the engine registered at the vendor is being removed. The
	// engine's own work, and the last thing it does for this surface.
	//
	// The block stays in the company document for the whole of it. A
	// disconnect that removed the block first would leave the loop with
	// no credential to authenticate the teardown with, and the webhooks
	// it was meant to withdraw registered forever.
	PhaseDisconnecting Phase = "disconnecting"

	// PhaseReady means observed matches desired.
	PhaseReady Phase = "ready"
)

// Label is the phase in the words a person reads, rather than the words the
// wire carries.
//
// TWO VOCABULARIES ON PURPOSE, and this is the seam between them. [Phase] is
// a stored value: it is written into the fleet's coordination store, read
// back by a peer that may be a different build, and named in the docs and the
// API. What an operator scanning a list of integrations wants is not that.
//
// THE WORDS ARE THE CONTROL PLANE'S, so one company reads the same status
// whichever console it is looking at. backlet's ReconcilePhase and this one
// are the same vocabulary with two names, and its console renders them as
// below; anything else here would mean an integration that says "activating"
// in one product and something else in the other.
//
// Two of backlet's phases have no counterpart here, and neither is an
// omission. `disconnected` is a tenant who has not connected an integration
// yet, which in this engine is a company document with no block at all, so
// there is no row and no phase to report. `disconnecting` is a teardown pass;
// this engine's disconnect is ONE CONFIG WRITE that removes the block, after
// which the loop reports the surface not configured and forgets it, so there
// is no interval during which a teardown could be shown.
func (p Phase) Label() string {
	switch p {
	case PhaseReady:
		return "Connected"
	case PhaseDegraded:
		// Agents are working and somebody has to act, which is the one
		// case where the count of what is broken is not the point.
		return "Action required"
	case PhaseAwaitingAdmin:
		return "Action needed"
	case PhaseProvisioning:
		return "Setting up agents"
	case PhaseActivating:
		return "Waiting for the provider"
	case PhaseDisconnecting:
		return "Disconnecting"
	case PhaseUnconfigured:
		// FAILED, not "not connected". This phase is only ever reached
		// with a block present: an absent one is [ErrNotConfigured] and
		// the row is forgotten rather than reported. So what it names is
		// an integration somebody configured whose credential is missing
		// or refused, and calling that "not connected" would read as
		// nobody having tried.
		return "Failed"
	default:
		// A phase a newer node wrote. Rendered as its own wire value with
		// the underscores opened up, which is honest about not knowing
		// it, where any of the words above would be a claim this build
		// cannot support.
		return strings.ReplaceAll(string(p), "_", " ")
	}
}

// Phases is every phase, ordered from furthest-from-working to working.
var Phases = []Phase{
	// DISCONNECTING IS FIRST, which is not a claim that it is the worst
	// thing that can happen to an integration: this slice is what the
	// dashboard reports the least ready surface from, and a teardown has
	// to win. Showing a tool as connected while one of its surfaces is
	// being removed invites a reader to act on something that is going
	// away, which is the same precedence the console gives it.
	PhaseDisconnecting,
	PhaseUnconfigured, PhaseAwaitingAdmin, PhaseProvisioning,
	PhaseActivating, PhaseDegraded, PhaseReady,
}

// Valid reports whether p is a phase this build knows. Unknown values arrive
// the same way an unknown [Kind] does, and for the same reason.
func (p Phase) Valid() bool { return slices.Contains(Phases, p) }

// String makes a Phase printable without a conversion at every log site.
func (p Phase) String() string { return string(p) }

// Actor is who has to do something for the phase to end.
//
// This is the field the cadence turns on, and the distinction it draws is not
// "is something wrong" but "is anybody going to fix it without being asked".
// Nothing the engine or a vendor is doing needs a person told about it. What
// a person owes needs saying plainly, and needs watching at a rate that
// matches how they will actually get to it.
type Actor string

const (
	// ActorNobody means the integration is ready. The zero value, and
	// meaningfully so: a report with nothing outstanding names no actor.
	ActorNobody Actor = ""

	// ActorEngine means the next pass carries it forward.
	ActorEngine Actor = "engine"

	// ActorProvider means the vendor is applying what it already accepted.
	ActorProvider Actor = "provider"

	// ActorAdmin means a person must act AT THE VENDOR: install the app,
	// approve the scopes, widen the grant, remove access somebody else
	// added. Watched briskly, because somebody told to install an app is
	// usually installing it as they read, and the point of the loop is
	// that it resumes without them pressing anything.
	ActorAdmin Actor = "admin"

	// ActorOperator means a person must change THIS DEPLOYMENT'S OWN
	// configuration: a ${VAR} that resolves to nothing, a public base URL
	// nothing set, a tier the company document spells wrong. Re-checked
	// slowly, because nothing at the vendor will ever change it and asking
	// often only spends requests.
	//
	// Separate from ActorAdmin even though both are "a person", because
	// the two differ in the one way the cadence cares about: one is being
	// done right now in another browser tab, and the other is waiting for
	// somebody to edit a file.
	ActorOperator Actor = "operator"
)

// Actors is every actor, engine-owned work first.
var Actors = []Actor{ActorNobody, ActorEngine, ActorProvider, ActorAdmin, ActorOperator}

// Valid reports whether a is an actor this build knows.
func (a Actor) Valid() bool { return slices.Contains(Actors, a) }

// String makes an Actor printable without a conversion at every log site.
func (a Actor) String() string { return string(a) }

// WaitsOnAPerson reports whether the phase ends only when somebody acts.
func (a Actor) WaitsOnAPerson() bool { return a == ActorAdmin || a == ActorOperator }

// Outcome is how the loop treats a report, and it exists so the cadence is
// derived from one small closed set rather than from a phase switch repeated
// at every call site.
type Outcome string

const (
	// OutcomeSettled means observed matches desired. Still re-read on a
	// slow cadence, because access removed by hand at the vendor is only
	// ever found by looking.
	OutcomeSettled Outcome = "settled"
	// OutcomeWaiting means something is in flight. Retried soon, with
	// backoff.
	OutcomeWaiting Outcome = "waiting"
	// OutcomeBlocked means a person must act. Retried on their cadence,
	// because retrying changes nothing and burying the request in a busy
	// loop is how it stops being read.
	OutcomeBlocked Outcome = "blocked"
)

// Report is what a pass concluded.
//
// Detail and ActionURL exist so a blocked integration can be acted on without
// reading logs or guessing. They are filled only for an actor who is a
// person: telling an operator that "the vendor is applying agent access" and
// giving them a link is an invitation to go and interfere with it.
//
// The zero value is NOT a valid report (PhaseReady is a claim, and the empty
// phase would silently pass for one in a switch that defaults). Build one
// with [Classify], or with [Ready] for the trivial case.
type Report struct {
	Phase Phase `json:"phase"`
	Actor Actor `json:"actor,omitempty"`
	// Detail is one sentence naming what is outstanding, addressed to Actor.
	Detail string `json:"detail,omitempty"`
	// ActionURL is where the person named by Actor goes to do it.
	ActionURL string `json:"action_url,omitempty"`
}

// Ready is the report of an integration with nothing outstanding.
func Ready() Report { return Report{Phase: PhaseReady, Actor: ActorNobody} }

// Outcome maps a report to how the loop treats it.
//
// ONLY THE ACTOR MATTERS beyond ready, which is why this is not a phase
// switch: PhaseDegraded is blocked when a person owes something and waiting
// when the engine does, and a table keyed on the phase alone would have to
// guess which.
func (r Report) Outcome() Outcome {
	switch {
	case r.Phase == PhaseReady:
		return OutcomeSettled
	case r.Phase == PhaseDisconnecting:
		// WAITING even when the teardown is stuck on a vendor refusing
		// the delete. Blocked is for something a person can go and do,
		// and there is nothing to do here but let the retries run or
		// force the disconnect, which is a different gesture.
		return OutcomeWaiting
	case r.Actor.WaitsOnAPerson():
		return OutcomeBlocked
	default:
		return OutcomeWaiting
	}
}
