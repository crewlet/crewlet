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

	// PhaseReady means observed matches desired.
	PhaseReady Phase = "ready"
)

// Label is the phase in the words a person reads, rather than the words the
// wire carries.
//
// TWO VOCABULARIES ON PURPOSE, and this is the seam between them. [Phase] is
// a stored value: it is written into the fleet's coordination store, read
// back by a peer that may be a different build, and named in the docs and the
// API. It says precisely which of six situations a surface is in, and it has
// to keep saying that. What an operator scanning a list of integrations wants
// is narrower — is this working, is somebody needed, or is it still coming
// up — and six words for that is four too many.
//
// So the phases collapse. Provisioning and activating are one answer, "still
// coming up, nobody has to act", because the difference between them is which
// side is doing the work and neither side is the reader. Unconfigured reads
// as not connected, because that is what a surface with no usable credential
// IS, whether the credential is absent or the vendor refused it.
//
// Deliberately here rather than in the dashboard. A label derived on the
// client is a second place that has to know what every phase value means, and
// a client cannot know what a phase a NEWER node wrote is meant to say, which
// is exactly the case where guessing is worst.
func (p Phase) Label() string {
	switch p {
	case PhaseReady:
		return "connected"
	case PhaseDegraded:
		return "needs attention"
	case PhaseAwaitingAdmin:
		return "action needed"
	case PhaseProvisioning, PhaseActivating:
		return "setting up"
	case PhaseUnconfigured:
		return "not connected"
	default:
		// A phase a newer node wrote. Rendered as its own wire value with
		// the underscores opened up, which is honest about not knowing it,
		// where any of the words above would be a claim this build cannot
		// support.
		return strings.ReplaceAll(string(p), "_", " ")
	}
}

// Phases is every phase, ordered from furthest-from-working to working.
var Phases = []Phase{
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
	case r.Actor.WaitsOnAPerson():
		return OutcomeBlocked
	default:
		return OutcomeWaiting
	}
}
