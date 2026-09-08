package integration

import (
	"strings"
	"testing"
)

// knownKinds is every finding kind this build defines, in no particular
// order. Listed rather than derived, so adding a kind without deciding where
// it ranks fails a test rather than landing on the unknown-kind default.
var knownKinds = []FindingKind{
	FindingCredentialMissing,
	FindingCredentialRejected,
	FindingApprovalRequired,
	FindingIngressBlocked,
	FindingIngressPending,
	FindingIdentityMissing,
	FindingIdentityFailed,
	FindingGrantPending,
	FindingUnknownTier,
	FindingGrantShort,
	FindingGrantExcess,
}

// A pass that found nothing is ready. This is the whole success path: a
// third-party app that converged reports an empty slice rather than having to remember
// to say it is fine.
func TestClassifyNoFindingsIsReady(t *testing.T) {
	got := Classify(nil)
	if got.Phase != PhaseReady || got.Actor != ActorNobody {
		t.Fatalf("no findings classified as %+v, want a ready report", got)
	}
	if got.Outcome() != OutcomeSettled {
		t.Fatalf("a ready report is %q, want %q", got.Outcome(), OutcomeSettled)
	}
}

// THE INVARIANT THIS PACKAGE EXISTS FOR.
//
// The excess-access advisory reports PhaseReady, so anything it outranks
// disappears from the report entirely. Three of the five hand-written
// classifiers this replaces returned it early and hid, variously, a
// short grant, a failed agent, and a webhook that reached nobody.
//
// Paired against every other kind, in both slice orders, the advisory must
// never win.
func TestAdvisoryNeverHidesAProblem(t *testing.T) {
	for _, other := range knownKinds {
		if other == FindingGrantExcess {
			continue
		}
		excess := Finding{Kind: FindingGrantExcess, Subject: "ceo"}
		problem := Finding{Kind: other, Subject: "ceo"}

		for _, order := range []struct {
			name string
			in   []Finding
		}{
			{"advisory first", []Finding{excess, problem}},
			{"advisory last", []Finding{problem, excess}},
		} {
			got := Classify(order.in)
			if got.Phase == PhaseReady {
				t.Errorf("%s with %s: classified ready, so the %s finding "+
					"is invisible to an operator", order.name, other, other)
			}
			wantPhase, wantActor := other.Verdict()
			if got.Phase != wantPhase || got.Actor != wantActor {
				t.Errorf("%s with %s: got %s/%s, want %s/%s",
					order.name, other, got.Phase, got.Actor, wantPhase, wantActor)
			}
		}
	}
}

// A kind a newer peer wrote outranks the advisory and nothing else. Ranking
// it worst would let an older node call a healthy company broken; ranking it
// below the advisory would let one spare permission hide a finding this
// binary cannot read.
func TestUnknownKindOutranksOnlyTheAdvisory(t *testing.T) {
	unknown := Finding{Kind: FindingKind("something_a_newer_build_found")}

	got := Classify([]Finding{{Kind: FindingGrantExcess}, unknown})
	if got.Phase == PhaseReady {
		t.Fatalf("an unknown kind beside the advisory classified ready (%+v)", got)
	}

	for _, other := range knownKinds {
		if other == FindingGrantExcess {
			continue
		}
		got := Classify([]Finding{unknown, {Kind: other}})
		wantPhase, _ := other.Verdict()
		if got.Phase != wantPhase {
			t.Errorf("unknown kind beat %s: got %s, want %s", other, got.Phase, wantPhase)
		}
	}
}

// The ranking is a STRICT total order over the known kinds. Two kinds sharing
// a rank would make Classify's answer depend on slice order, which is how a
// report starts changing between two passes that found the same things.
func TestSeverityIsAStrictOrder(t *testing.T) {
	seen := map[int]FindingKind{}
	for _, kind := range knownKinds {
		rank := kind.severity()
		if other, dup := seen[rank]; dup {
			t.Errorf("%s and %s share severity %d, so which one is reported "+
				"depends on the order a third-party app happened to emit them", kind, other, rank)
		}
		seen[rank] = kind
	}
	// The advisory is last, which is the property every other test here
	// depends on.
	for _, kind := range knownKinds {
		if kind == FindingGrantExcess {
			continue
		}
		if kind.severity() >= FindingGrantExcess.severity() {
			t.Errorf("%s ranks at or below the advisory (%d >= %d)",
				kind, kind.severity(), FindingGrantExcess.severity())
		}
	}
}

// Every known kind is ranked and judged by a case of its own rather than
// falling through to the unknown-kind default, which would report a finding
// this build defines as one it cannot interpret.
//
// Checked on the RANK rather than on the phase and actor, because a known
// kind is allowed to share a verdict with the default (an unknown tier is
// genuinely degraded and genuinely the operator's, which is what the default
// says too). The rank is what proves the switch has a case for it: the
// default's rank sits between the short grant and the advisory, and no known
// kind may land there.
func TestEveryKnownKindIsRankedAndJudged(t *testing.T) {
	unknownRank := FindingKind("a kind a newer build found").severity()
	for _, kind := range knownKinds {
		if kind.severity() == unknownRank {
			t.Errorf("%s falls through to the unknown-kind rank %d, so nothing "+
				"here decides how severe it is", kind, unknownRank)
		}
		phase, actor := kind.Verdict()
		if !phase.Valid() {
			t.Errorf("%s maps to phase %q, which is not a phase", kind, phase)
		}
		if !actor.Valid() {
			t.Errorf("%s maps to actor %q, which is not an actor", kind, actor)
		}
	}
}

// The count names how many MORE of the same thing there are, not how many
// findings the pass produced. A total that swept in an unrelated advisory
// would read as another seat needing the same grant.
func TestClassifyCountsOnlyTheWinningKind(t *testing.T) {
	got := Classify([]Finding{
		{Kind: FindingGrantShort, Subject: "ceo", Detail: "ceo needs maintainer"},
		{Kind: FindingGrantShort, Subject: "cto"},
		{Kind: FindingGrantExcess, Subject: "vp"},
	})
	if !strings.Contains(got.Detail, "and 1 more") {
		t.Fatalf("detail %q does not name the one other short grant", got.Detail)
	}
	if strings.Contains(got.Detail, "2 more") {
		t.Fatalf("detail %q counted the advisory as another short grant", got.Detail)
	}
}

// A single finding says what it found and nothing about a count. "an agent
// needs maintainer" and "an agent needs maintainer (and 0 more)" describe the
// same world, and only one of them reads like a sentence.
func TestClassifySingleFindingHasNoCount(t *testing.T) {
	got := Classify([]Finding{{Kind: FindingGrantShort, Detail: "ceo needs maintainer"}})
	if got.Detail != "ceo needs maintainer" {
		t.Fatalf("detail is %q, want the finding's own sentence unchanged", got.Detail)
	}
}

// A finding that carried no detail still produces a sentence. A report whose
// detail is empty sends an operator to the logs for the one thing this
// package exists to save them from.
func TestClassifyFallsBackToASentence(t *testing.T) {
	for _, kind := range knownKinds {
		got := Classify([]Finding{{Kind: kind, Subject: "api-gateway"}})
		if strings.TrimSpace(got.Detail) == "" {
			t.Errorf("%s with no detail produced an empty report detail", kind)
		}
		if !strings.Contains(got.Detail, "api-gateway") {
			t.Errorf("%s: detail %q does not name its subject", kind, got.Detail)
		}
	}
}

// An action URL is for a person. Handing an operator a settings link for
// "the third-party app is applying agent access" invites them to interfere with a
// grant that is landing on its own.
func TestClassifyDropsTheLinkWhenNobodyHasToAct(t *testing.T) {
	for _, kind := range knownKinds {
		got := Classify([]Finding{{Kind: kind, ActionURL: "https://example.com/settings"}})
		_, actor := kind.Verdict()
		switch {
		case actor.WaitsOnAPerson() && got.ActionURL == "":
			t.Errorf("%s is owed by %s but the report carries no link", kind, actor)
		case !actor.WaitsOnAPerson() && got.ActionURL != "":
			t.Errorf("%s is owed by %s but the report carries a link to %q",
				kind, actor, got.ActionURL)
		}
	}
}

// Ties keep the third-party app's own order, so a reconciler that walks its seats in a
// stable order reports a stable seat rather than a different one each pass.
func TestClassifyKeepsEmissionOrderOnATie(t *testing.T) {
	got := Classify([]Finding{
		{Kind: FindingGrantShort, Detail: "first"},
		{Kind: FindingGrantShort, Detail: "second"},
	})
	if !strings.HasPrefix(got.Detail, "first") {
		t.Fatalf("detail %q does not lead with the first finding emitted", got.Detail)
	}
}

// Degraded is blocked when a person owes something and waiting when the
// engine does. A table keyed on the phase alone would have to guess, and
// guessing wrong parks a repair the engine was about to make for an hour.
func TestOutcomeTurnsOnTheActorNotThePhase(t *testing.T) {
	cases := []struct {
		name string
		in   Report
		want Outcome
	}{
		{"ready", Ready(), OutcomeSettled},
		{"degraded on an admin", Report{Phase: PhaseDegraded, Actor: ActorAdmin}, OutcomeBlocked},
		{"degraded on the operator", Report{Phase: PhaseDegraded, Actor: ActorOperator}, OutcomeBlocked},
		{"degraded on the engine", Report{Phase: PhaseDegraded, Actor: ActorEngine}, OutcomeWaiting},
		{"activating on the vendor", Report{Phase: PhaseActivating, Actor: ActorProvider}, OutcomeWaiting},
		{"provisioning", Report{Phase: PhaseProvisioning, Actor: ActorEngine}, OutcomeWaiting},
		{"awaiting an admin", Report{Phase: PhaseAwaitingAdmin, Actor: ActorAdmin}, OutcomeBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Outcome(); got != tc.want {
				t.Fatalf("outcome is %q, want %q", got, tc.want)
			}
		})
	}
}

// The advisory is the one kind whose verdict is ready, and the whole ordering
// rests on that staying true.
func TestTheAdvisoryIsTheOnlyReadyVerdict(t *testing.T) {
	for _, kind := range knownKinds {
		phase, _ := kind.Verdict()
		if phase == PhaseReady && kind != FindingGrantExcess {
			t.Errorf("%s reports ready, so it hides every finding it outranks", kind)
		}
	}
}

// THE ORDER ITSELF, AS DATA. Not "they are all different" and not "the
// advisory is last" — the actual sequence, so a change to it is a change to
// this list.
//
// Distinctness and advisory-last are necessary and nowhere near sufficient.
// Every drift this ranking was written to end sits in the MIDDLE of it, and
// each one keeps both of those properties:
//
//   - GitLab checked excess access immediately before the short-grant check,
//     so a seat holding too much in one dimension and too little in another
//     reported ready with a note.
//   - Datadog checked it before the failed-agent count, so a company with one
//     over-granted agent and one that could not be provisioned at all
//     reported ready.
//   - GitHub checked its installation-level excess before the WEBHOOK check,
//     so an app holding one spare permission reported healthy while its
//     deliveries reached nobody.
//
// And two more disagreed about whether an unknown access tier outranks a
// broken delivery path. Swapping any of those pairs leaves the ranks distinct
// and the advisory last, and passes every other test in this file.
func TestTheSeverityOrderIsPinned(t *testing.T) {
	// Worst first: how much of the integration is not working, from "the
	// pass could not authenticate" down to "it works, but somebody should
	// look". Within one level, engine-owned work before person-owned work,
	// because telling a person to fix something the engine is mid-way
	// through sends them to repair what is not broken.
	want := []FindingKind{
		FindingCredentialMissing,
		FindingCredentialRejected,
		FindingApprovalRequired,
		FindingIngressBlocked,
		FindingIngressPending,
		FindingIdentityMissing,
		FindingIdentityFailed,
		FindingGrantPending,
		FindingUnknownTier,
		FindingGrantShort,
		FindingGrantExcess,
	}
	if len(want) != len(knownKinds) {
		t.Fatalf("this list has %d kinds and the package defines %d: a kind was "+
			"added without deciding where it ranks", len(want), len(knownKinds))
	}
	for i := 1; i < len(want); i++ {
		worse, better := want[i-1], want[i]
		if worse.severity() >= better.severity() {
			t.Errorf("%s no longer outranks %s (%d >= %d)",
				worse, better, worse.severity(), better.severity())
		}
	}
	// AND THE UNKNOWN KIND SITS BETWEEN THE LAST REAL PROBLEM AND THE
	// ADVISORY, which is its own decision: ranked worst, an older node would
	// report a healthy company as broken; ranked below the advisory, one
	// spare permission would hide a finding this binary cannot read.
	unknown := FindingKind("a-kind-from-a-newer-build").severity()
	if !(FindingGrantShort.severity() < unknown && unknown < FindingGrantExcess.severity()) {
		t.Errorf("an unknown kind ranks %d, want between %s (%d) and %s (%d)",
			unknown, FindingGrantShort, FindingGrantShort.severity(),
			FindingGrantExcess, FindingGrantExcess.severity())
	}
}
