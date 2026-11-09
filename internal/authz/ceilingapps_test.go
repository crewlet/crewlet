package authz_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// appsOf is the apps a's row names: its app, or a ClassAnyApp row's set.
func appsOf(a authz.Action) []iam.Grant {
	if app, _ := authz.AppOf(a); app != "" {
		return []iam.Grant{app}
	}
	apps, _ := authz.AnyOf(a)
	return apps
}

// objectFor is an object of the kind a's row belongs to on which the agent seat
// "cto" — who leads the PLATFORM project, the RUNBOOKS container and the sre
// unit in the fixture, and is named its owner and author besides — holds every
// relation a class reads, so what refuses is never the agent's own relation.
func objectFor(apps []iam.Grant) authz.Object {
	switch {
	case slices.Contains(apps, iam.GrantDoclet) && len(apps) == 1:
		return authz.Object{Kind: authz.KindPage, Owner: "cto", Author: "cto", Container: "RUNBOOKS"}
	case slices.Contains(apps, iam.GrantArtifacts):
		return authz.Object{Kind: authz.KindArtifact, Owner: "cto"}
	case slices.Contains(apps, iam.GrantChatlet):
		return authz.Object{Kind: authz.KindChannel, Owner: "cto", Author: "cto", Container: "sre"}
	}
	return authz.Object{Kind: authz.KindProject, Owner: "cto", Author: "cto", Container: "PLATFORM",
		ContainerKind: authz.KindProject}
}

// A REQUESTER WHO DOES NOT USE AN APP HAS NO AGENT USE IT FOR THEM (ADR-0109).
//
// Every verb whose row names an app, taken by an agent that may take it, in a
// turn a person woke who does not hold the app, is refused [authz.ReasonCeiling]
// naming the app in [authz.Decision.RequesterGrants] — never in Grants, which
// names what the agent lacks, and the agent lacks nothing. Given the app, the
// same requester is no bound at all; and a ClassAnyApp row admits them on any
// one of its set. The walk covers the whole table, so an app's new verb is
// held the day its row is written.
//
// Mutation: compose the ceiling's person with every app (the requester ceiling
// before ADR-0109), or skip the app half — every row goes red.
func TestARequesterWithoutTheAppRefusesItsVerbs(t *testing.T) {
	t.Parallel()
	agent := seat("cto", iam.Apps...)
	walked := map[iam.Grant]int{}
	for _, a := range authz.Actions() {
		apps := appsOf(a)
		if len(apps) == 0 {
			continue
		}
		object := objectFor(apps)
		if own := authz.Decide(t.Context(), agent, a, object, nimbus(), decidedAt); own.Unknown() || !own.Allowed {
			// The agent's own rule refuses it — a person's gesture, a
			// human-only verb — and that refusal is the answer, whoever woke
			// the turn (TestTheCeilingOnlyNarrowsAndCannotTellIsNotNo).
			continue
		}
		for _, app := range apps {
			walked[app]++
		}
		without := authz.DecideWithinApps(t.Context(), agent, requester(personLeading("mallory")), a, object,
			nimbus(), decidedAt)
		if without.Unknown() || without.Allowed || without.Reason != authz.ReasonCeiling ||
			without.Ceiling != authz.CeilingRequester || !slices.Equal(without.RequesterGrants, apps) ||
			without.RequesterReason != authz.ReasonNoGrant ||
			len(without.Grants) != 0 {
			t.Errorf("%s for a requester using no app decided %+v, want the ceiling naming %v as the "+
				"requester's", a, without, apps)
		}
		for _, app := range apps {
			with := authz.DecideWithinApps(t.Context(), agent, requester(personLeading("mallory", app)), a,
				object, nimbus(), decidedAt)
			if with.Unknown() || !with.Allowed {
				t.Errorf("%s for a requester using %s decided %+v, want admitted", a, app, with)
			}
		}
	}
	for _, app := range iam.Apps {
		if walked[app] == 0 {
			t.Errorf("no verb of %s was walked: the case holds nothing of it", app)
		}
	}
}

// THE PERSON AN OPEN ASK NAMES MAY HAVE AN AGENT ANSWER IT, as the row admits
// them asking themselves ([authz.ClassAppOrAsked]): somebody asked a question
// on a work item reads it and answers it without the Worklet app, so an agent
// they wake may too — and nobody else without the app may. Where nobody has
// read the item yet, whether an ask names the requester is unknown: read it,
// resolve its asks, decide again.
//
// Mutation: hold the asked arm to the app alone — the asked person is refused.
func TestTheAskedArmAdmitsTheRequesterTheAskNames(t *testing.T) {
	t.Parallel()
	agent := seat("cto", iam.Apps...)
	asked := requester(personLeading("mallory"))
	for _, a := range []authz.Action{authz.ActionWorkRead, authz.ActionWorkComment} {
		item := authz.Object{Kind: authz.KindTask, Asked: "mallory"}
		if d := authz.DecideWithinApps(t.Context(), agent, asked, a, item, nimbus(), decidedAt); d.Unknown() ||
			!d.Allowed {
			t.Errorf("%s on an item asking the requester decided %+v, want admitted", a, d)
		}
		item.Asked = "somebody-else"
		if d := authz.DecideWithinApps(t.Context(), agent, asked, a, item, nimbus(), decidedAt); d.Unknown() ||
			d.Allowed || d.Reason != authz.ReasonCeiling {
			t.Errorf("%s on an item asking somebody else decided %+v, want the ceiling", a, d)
		}
		unread := authz.Object{Kind: authz.KindTask, Unresolved: true}
		if d := authz.DecideWithinApps(t.Context(), agent, asked, a, unread, nimbus(), decidedAt); !errors.Is(d.Err,
			authz.ErrUnresolved) {
			t.Errorf("%s on an item nobody has read decided %+v, want it unresolved", a, d)
		}
	}
}

// A STRANGER USES NO APP: an agent woken by somebody the company does not know
// takes no app's verb for them, however its own relations stand, and the
// refusal says it was a stranger's — while an edit to its own work that no app
// gates stays steered ([authz.DecideWithin]).
//
// Mutation: let a stranger through the app half — the board reads for them.
func TestAStrangerHoldsNoApp(t *testing.T) {
	t.Parallel()
	agent := seat("cto", iam.Apps...)
	stranger := authz.Ceiling{Kind: authz.CeilingStranger}
	board := objectFor([]iam.Grant{iam.GrantWorklet})
	d := authz.DecideWithinApps(t.Context(), agent, stranger, authz.ActionWorkCreate, board, nimbus(), decidedAt)
	if d.Unknown() || d.Allowed || d.Reason != authz.ReasonCeiling || d.Ceiling != authz.CeilingStranger {
		t.Errorf("a Worklet verb for a stranger decided %+v, want the ceiling, a stranger's", d)
	}
	d = authz.DecideWithin(t.Context(), seat("ops-bot", iam.Apps...), stranger, authz.ActionArtifactDataWrite,
		agentOwned("ops-bot"), nimbus(), decidedAt)
	if d.Unknown() || !d.Allowed {
		t.Errorf("filling in its own data for a stranger decided %+v, want it steered", d)
	}
}

// A REQUESTER WHO MAY NOT ACT USES NO APP, whatever their grants say: a
// suspended person's message, read after they were suspended, has an agent do
// nothing in an app for them.
//
// Mutation: read the requester's grants without their stage — the suspended
// person's Worklet verb is admitted.
func TestARequesterWhoMayNotActHoldsNoApp(t *testing.T) {
	t.Parallel()
	suspended := personLeading("mallory", iam.Apps...)
	suspended.Stage = iam.StageSuspended
	d := authz.DecideWithinApps(t.Context(), seat("cto", iam.Apps...), requester(suspended),
		authz.ActionWorkCreate, objectFor([]iam.Grant{iam.GrantWorklet}), nimbus(), decidedAt)
	if d.Unknown() || d.Allowed || d.Reason != authz.ReasonCeiling {
		t.Errorf("a Worklet verb for a suspended requester decided %+v, want the ceiling", d)
	}
	// THEIR STAGE IS WHAT REFUSES THEM, and the refusal says so rather than
	// naming an app they hold already: no administrator's grant would
	// change it.
	if d.RequesterReason != authz.ReasonStage || len(d.RequesterGrants) != 0 {
		t.Errorf("the suspended requester's refusal says %q naming %v, want their stage and no app",
			d.RequesterReason, d.RequesterGrants)
	}
}

// A ROW NAMING NO APP IS NOT THE APP HALF'S TO BOUND: reading an artifact is
// decided by its shares, which [authz.DecideWithin] holds the requester to, and
// asking a colleague names no app at all — so the app half answers the
// agent's own decision for both, whoever woke the turn.
//
// Mutation: refuse every row for a requester holding no app — the read is
// refused before its shares are asked.
func TestARowNamingNoAppIsNotBoundedByTheApps(t *testing.T) {
	t.Parallel()
	bot := seat("ops-bot", iam.Apps...)
	nobody := requester(personLeading("mallory"))
	for _, a := range []authz.Action{authz.ActionArtifactRead, authz.ActionColleagueAsk} {
		if apps := appsOf(a); len(apps) != 0 {
			t.Fatalf("%s's row names %v: the case needs a row naming none", a, apps)
		}
	}
	shared := agentOwned("ops-bot")
	if d := authz.DecideWithinApps(t.Context(), bot, nobody, authz.ActionArtifactRead, shared, nimbus(),
		decidedAt); d.Unknown() || !d.Allowed {
		t.Errorf("reading its own artifact, as far as the apps go, decided %+v, want admitted", d)
	}
}
