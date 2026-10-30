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
			if got := len(seat.Lanes[tc.lane].Plan) > 0; got && !tc.planned {
				t.Errorf("%s: the tab plans %s: %+v", name, seat.Handle,
					seat.Lanes[tc.lane].Plan)
			}
		}
	}
}

// AN OFFER THAT COULD NOT BE READ IS MARKED SO, AND NEVER KEPT.
//
// Datadog's keys are unset, so its vendor cannot be opened: the lane is
// unread (a turn-on on it asks a step-up) and nothing is cached, so the next
// read asks again. GitLab managed outside Crewlet needs no vendor, so its
// offer is whole and is kept. Mutation: cache what failed, and the first case
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
	if len(offers) != 1 || !offers[0].Unread {
		t.Errorf("an unreadable Datadog offers %+v, want its lane unread", offers)
	}
	if cached(integration.KindDatadog, "rev-failed") {
		t.Error("an offer that could not be read was cached")
	}
	q = accessCompany(t, integration.KindGitLab, `{"gitlab": {"enabled": true,
  "url": "https://gitlab.example.invalid", "signing_secret": "${GITLAB_SIGNING_SECRET}"}}`,
		`["gitlab"]`)
	offers = e.seatOffers(t.Context(), q.Kind, q.Company, q.Roster, "rev-external", false)
	if len(offers) != 1 || offers[0].Managed != seataccess.ManagedExternal ||
		!cached(integration.KindGitLab, "rev-external") {
		t.Errorf("an external GitLab offers %+v (cached %v), want it external and kept",
			offers, cached(integration.KindGitLab, "rev-external"))
	}
}
