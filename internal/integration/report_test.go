package integration

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE READER'S VOCABULARY IS SHORTER THAN THE WIRE'S, and this is the seam.
//
// Six phases exist because the engine has to say precisely which of six
// situations a surface is in, and that value is stored, shared with peers and
// documented. An operator scanning a list wants a narrower answer, so the
// phases collapse. Pinned because the collapse is a decision, not a
// formatting rule: a later edit that made `activating` read "connected", or
// that split "setting up" back into two, would change what the screen claims.
func TestEveryPhaseHasAConciseLabel(t *testing.T) {
	// The control plane's own words (backlet's ReconcilePhase, rendered by
	// the console), so one company reads the same status in either product.
	want := map[Phase]string{
		PhaseDisconnecting: "Disconnecting",
		PhaseUnconfigured:  "Failed",
		PhaseAwaitingAdmin: "Action needed",
		PhaseProvisioning:  "Setting up agents",
		PhaseActivating:    "Waiting for the provider",
		PhaseDegraded:      "Action required",
		PhaseReady:         "Connected",
	}
	for _, phase := range Phases {
		got := phase.Label()
		if got != want[phase] {
			t.Errorf("%s labels as %q, want %q", phase, got, want[phase])
		}
	}
	// Every phase is covered, so adding one without deciding its label
	// fails here rather than reaching a screen as a raw wire value.
	if len(want) != len(Phases) {
		t.Errorf("%d phases and %d labels; a phase without a label reaches the reader as its wire value",
			len(Phases), len(want))
	}
	// READY IS THE ONLY ONE THAT CLAIMS THE INTEGRATION WORKS. Anything
	// else labelled "connected" would tell an operator a surface was fine
	// while the engine was still bringing it up or a person was still owed
	// something.
	for _, phase := range Phases {
		if phase != PhaseReady && phase.Label() == "Connected" {
			t.Errorf("%s reads as connected, but only ready means observed matches desired", phase)
		}
	}
}

// A phase this build does not know is rendered as itself, never guessed at.
func TestAnUnknownPhaseIsNotLabelled(t *testing.T) {
	got := Phase("something_a_newer_build_wrote").Label()
	if got != "something a newer build wrote" {
		t.Errorf("unknown phase labelled %q; it must render as its own value", got)
	}
}

// THE BARE WORD "admin" IS THE COMPANY'S.
//
// Two people can owe an integration a step, and they are not the same person:
// somebody AT THE THIRD-PARTY APP (a Slack workspace admin, a GitHub
// organization owner) and the company's own admin, whose fix is in this
// deployment's configuration. "admin" is a role a person holds in this company,
// so the bare value names that person and the vendor's is spelled out — a
// value that called the vendor's person "admin" would send the company's admin
// to a console they may hold no account on.
//
// Pinned on the wire values AND on every known kind's verdict, because the
// rename that introduced the split swapped what "admin" meant: a kind left on
// the old reading would now point at the other person without a compile error.
func TestTheBareAdminIsTheCompanys(t *testing.T) {
	t.Parallel()
	if ActorAdmin != "admin" || ActorVendorAdmin != "vendor_admin" {
		t.Fatalf("admin is %q and the vendor's is %q; the bare word belongs to "+
			"the company", ActorAdmin, ActorVendorAdmin)
	}
	// Whose step each kind is. The company's admin edits this deployment's
	// configuration or its secret store; the vendor's acts at the app.
	want := map[FindingKind]Actor{
		FindingCredentialMissing:    ActorAdmin,
		FindingCredentialRejected:   ActorAdmin,
		FindingCredentialExpiring:   ActorAdmin,
		FindingUnknownTier:          ActorAdmin,
		FindingCoveragePartial:      ActorAdmin,
		FindingApprovalRequired:     ActorVendorAdmin,
		FindingIngressBlocked:       ActorVendorAdmin,
		FindingIdentityFailed:       ActorVendorAdmin,
		FindingGrantShort:           ActorVendorAdmin,
		FindingGrantExcess:          ActorVendorAdmin,
		FindingRegistrationOrphaned: ActorVendorAdmin,
		FindingIngressPending:       ActorEngine,
		FindingIdentityMissing:      ActorEngine,
		FindingGrantPending:         ActorProvider,
	}
	for _, kind := range knownKinds {
		wantActor, ok := want[kind]
		if !ok {
			t.Errorf("%s has no expected actor here; decide whose step it is", kind)
			continue
		}
		if _, got := kind.Verdict(); got != wantActor {
			t.Errorf("%s is owed by %q, want %q", kind, got, wantActor)
		}
	}
	// A kind this build cannot read goes to the person who can read the
	// peer's logs, which is the company's admin, never the vendor's.
	if _, got := FindingKind("a kind a newer build found").Verdict(); got != ActorAdmin {
		t.Errorf("an unknown kind is owed by %q, want the company's admin", got)
	}
	// And the phase that awaits an admin awaits the VENDOR's: its name is the
	// control plane's and stays, so the actor beside it is what says whose.
	if phase, actor := FindingApprovalRequired.Verdict(); phase != PhaseAwaitingAdmin ||
		actor != ActorVendorAdmin {
		t.Errorf("approval_required is %s/%s, want awaiting_admin owed by the vendor's admin",
			phase, actor)
	}
}

// The dashboard declares the actors it can be sent, and that declaration is
// held to the engine's: a screen that knew "operator" after the company's
// admin took the bare word would label the wrong person, and nothing else
// would fail.
func TestTheDashboardKnowsExactlyTheActors(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Union(clientsource.Tree(t), "ReconcileActor")
	if err != nil {
		t.Fatalf("%v — this gate cannot run without the client's declaration", err)
	}
	want := make([]string, 0, len(Actors))
	for _, a := range Actors {
		want = append(want, string(a))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's ReconcileActor is %q; the engine sends %q", got, want)
	}
}
