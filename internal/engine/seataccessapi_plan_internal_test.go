package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// accessCompany is a company with the SRE on Datadog at the Standard Role and
// the CTO on nothing, under the integrations and migration marker a case
// gives, as kind's access query.
func accessCompany(t *testing.T, kind integration.Kind, integrations,
	migrated string) setupapi.AccessQuery {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(`{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": ` + integrations + `,
  "seat_access_migrated": ` + migrated + `,
  "roles": [
    {"name": "SRE", "handle": "sre", "llm": "zulu",
     "integrations": {"datadog": {"enabled": true, "role": "role-std"}}},
    {"name": "CTO", "handle": "cto", "llm": "zulu"}
  ]
}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	roster, err := cfg.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	return setupapi.AccessQuery{Kind: kind, Company: cfg, Roster: roster, Revision: "rev-1"}
}

// datadogProvisioned is Datadog connected and provisioned by the engine, its
// keys named but unset, so its vendor cannot be read.
const datadogProvisioned = `{"datadog": {"enabled": true, "route_to": "sre",
  "webhook_token": "${DATADOG_WEBHOOK_TOKEN}", "provisioning": {"site": "datadoghq.com",
  "api_key": "${DATADOG_API_KEY}", "app_key": "${DATADOG_APP_KEY}"}}}`

// A PLAN IS DRAWN ONLY WHERE THE SYNC TAKES STEPS (H3).
//
// Datadog provisioned and migrated: the CTO's turn-on is planned. The same
// Datadog before its migration has finished: the sync takes no step yet, so
// neither the tab nor the draft's plan lists any, and the turn-on still asks
// a step-up, since the saved setting is the one the sync applies once the
// migration finishes. GitLab connected without provisioning: the engine
// grants nothing there, so no plan and no step-up at all. Mutation: plan
// regardless of the mode or the marker, and a case here fails.
func TestAPlanIsDrawnOnlyWhereTheSyncTakesSteps(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	api := e.SeatAccessAPI(nil, nil, func() bool { return false })
	turnOn := func(lane seataccess.Lane, level string) seataccess.Draft {
		return seataccess.Draft{Seats: map[string]map[seataccess.Lane]seataccess.Desired{
			"cto": {lane: {Enabled: true, Level: level}}}}
	}
	external := `{"gitlab": {"enabled": true, "url": "https://gitlab.example.invalid",
  "signing_secret": "${GITLAB_SIGNING_SECRET}"}}`
	steps := func(p seataccess.PlanAnswer) int {
		n := 0
		for _, sp := range p.Plans {
			n += len(sp.Steps)
		}
		return n
	}
	for name, tc := range map[string]struct {
		lane                   seataccess.Lane
		level                  string
		integrations, migrated string
		planned, stepUp        bool
	}{
		"provisioned and migrated": {seataccess.LaneDatadog, "role-std", datadogProvisioned,
			`["datadog"]`, true, true},
		"waiting on its migration": {seataccess.LaneDatadog, "role-std", datadogProvisioned,
			`[]`, false, true},
		"managed outside Crewlet": {seataccess.LaneGitLab, "30", external, `["gitlab"]`,
			false, false},
	} {
		q := accessCompany(t, tc.lane.Kind(), tc.integrations, tc.migrated)
		plan, err := api.Plan(t.Context(), q, turnOn(tc.lane, tc.level))
		if err != nil {
			t.Fatalf("%s: plan: %v", name, err)
		}
		if got := steps(plan) > 0; got != tc.planned {
			t.Errorf("%s: the draft's plan lists steps = %v, want %v (%+v)", name, got,
				tc.planned, plan.Plans)
		}
		if plan.StepUp != tc.stepUp {
			t.Errorf("%s: step_up = %v, want %v", name, plan.StepUp, tc.stepUp)
		}
		answer, err := api.Answer(t.Context(), q)
		if err != nil {
			t.Fatalf("%s: answer: %v", name, err)
		}
		for _, seat := range answer.Seats {
			la := seat.Lanes[tc.lane]
			if got := len(la.Plan) > 0; got && !tc.planned {
				t.Errorf("%s: the tab plans %s: %+v", name, seat.Handle, la.Plan)
			}
			// THE STATUS IS THE ENGINE'S, read off the lane as answered.
			if want := seataccess.StatusOf(la, answer.Syncing, seat.Departed); la.Status != want {
				t.Errorf("%s: %s reads %q, want %q", name, seat.Handle, la.Status, want)
			}
		}
	}
}

// AN OFFER THAT COULD NOT BE READ VOUCHES FOR NOTHING, AND IS NEVER KEPT.
//
// Datadog's keys are unset, so its vendor cannot be opened: the lane offers no
// levels (a turn-on on it asks a step-up) and nothing is cached, so the next
// read asks again. GitLab managed outside Crewlet needs no vendor, so its
// offer is whole and is kept, its reach read from the document. Mutation: cache what failed, and the first case
// fails.
func TestAFailedOfferReadIsNeverKept(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	cached := func(kind integration.Kind, revision string) bool {
		e.offers.mu.Lock()
		defer e.offers.mu.Unlock()
		_, ok := e.offers.byKey[string(kind)+"\x00"+revision]
		return ok
	}
	q := accessCompany(t, integration.KindDatadog, datadogProvisioned, `["datadog"]`)
	offers := e.seatOffers(t.Context(), q.Kind, q.Company, q.Roster, "rev-failed", false)
	if len(offers) != 1 || len(offers[0].Levels) != 0 || !seataccess.AsksStepUp(offers[0],
		seataccess.Desired{}, seataccess.Desired{Enabled: true, Level: "role-std"}) {
		t.Errorf("an unreadable Datadog offers %+v, want no levels, so a turn-on asks a "+
			"step-up", offers)
	}
	if cached(integration.KindDatadog, "rev-failed") {
		t.Error("an offer that could not be read was cached")
	}
	q = accessCompany(t, integration.KindGitLab, `{"gitlab": {"enabled": true,
  "url": "https://gitlab.example.invalid", "signing_secret": "${GITLAB_SIGNING_SECRET}"}}`,
		`["gitlab"]`)
	offers = e.seatOffers(t.Context(), q.Kind, q.Company, q.Roster, "rev-external", false)
	if len(offers) != 1 || offers[0].Managed != seataccess.ManagedExternal ||
		!offers[0].Scoped || !offers[0].ScopeLive || !offers[0].ScopeOpen ||
		!cached(integration.KindGitLab, "rev-external") {
		t.Errorf("an external GitLab offers %+v (cached %v), want it external and kept, its "+
			"reach the document's (open, since it lists no project)",
			offers, cached(integration.KindGitLab, "rev-external"))
	}
}

// A VENDOR'S ANSWER KEEPS THE CATALOG'S FACTS, and marks a scoped lane's scope
// as read, so an empty list then means none rather than unread. Mutation: take
// the vendor's answer whole, and a scoped lane reads as unscoped and unread.
func TestALiveOfferKeepsTheCatalogsFactsAndMarksItsScopeRead(t *testing.T) {
	t.Parallel()
	base := seataccess.Options{Lane: seataccess.LaneGitLab, Rotatable: true, Scoped: true, ScopeRequired: true}
	live := liveOffer(base, seataccess.Options{Lane: seataccess.LaneGitLab, Live: true,
		Scope: []string{"acme/api"}, ScopeNames: map[string]string{"acme/api": "API"}})
	if !live.Rotatable || !live.Scoped || !live.ScopeRequired || !live.ScopeLive || !live.Live ||
		len(live.Scope) != 1 || live.ScopeNames["acme/api"] != "API" {
		t.Fatalf("live offer = %+v", live)
	}
	unscoped := liveOffer(seataccess.Options{Lane: seataccess.LaneDatadog}, seataccess.Options{Lane: seataccess.LaneDatadog, Live: true})
	if unscoped.Scoped || unscoped.ScopeLive {
		t.Errorf("an unscoped lane = %+v, want neither scoped nor read", unscoped)
	}
	// RELEASABLE IS THE CATALOG'S, and the vendor's page is the vendor's: an
	// Expo offer read live keeps both, or the Agents tab drops "Keep the robot".
	const page = "https://expo.dev/accounts/acme/settings/access-tokens"
	expo := liveOffer(seataccess.Options{Lane: seataccess.LaneExpo, Rotatable: true, Releasable: true},
		seataccess.Options{Lane: seataccess.LaneExpo, ActionURL: page})
	if !expo.Releasable || !expo.Rotatable || expo.ActionURL != page {
		t.Errorf("an Expo lane's live offer = %+v, want it releasable and rotatable with its page", expo)
	}
}
