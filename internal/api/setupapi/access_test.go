package setupapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// fakeAccess is the engine's per-seat access as a case scripts it: an answer
// built from the company the route read (each agent seat's lane as the document
// holds it), the plan a case names, and every sync asked for.
type fakeAccess struct {
	mu sync.Mutex
	// options are what each lane offers.
	options []seataccess.Options
	// observed is what the vendor shows, by handle and lane.
	observed map[string]map[seataccess.Lane]seataccess.Observed
	// stepUp is what every plan answers.
	stepUp bool
	// outcomes are the last sync's outcome per seat, on every lane.
	outcomes map[string]seataccess.Outcome
	// syncErr is what Sync answers.
	syncErr error
	// ghost is a seat the answer lists that the document does not hold, as
	// one removed between the tab's read and its save.
	ghost string
	// unreadAfterSync fails every answer once a sync was asked, as a store
	// that stops answering after a save landed.
	unreadAfterSync bool
	syncs           []setupapi.AccessSync
	plans           []seataccess.Draft
}

func (f *fakeAccess) Answer(_ context.Context, q setupapi.AccessQuery) (seataccess.Answer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreadAfterSync && len(f.syncs) > 0 {
		return seataccess.Answer{}, errors.New("the ledger did not answer")
	}
	answer := seataccess.Answer{Kind: q.Kind, Revision: q.Revision, Options: f.options}
	if f.ghost != "" {
		answer.Seats = append(answer.Seats, seataccess.SeatAnswer{Handle: f.ghost,
			Name: f.ghost, Lanes: map[seataccess.Lane]seataccess.LaneAnswer{}})
	}
	for r := range q.Company.EachRole() {
		if r.Seat().IsHuman() {
			continue
		}
		seat := seataccess.SeatAnswer{Handle: r.Seat().Handle(), Name: r.Name,
			Lanes: map[seataccess.Lane]seataccess.LaneAnswer{}}
		for _, lane := range seataccess.LanesOf(q.Kind) {
			la := seataccess.LaneAnswer{Desired: desiredOf(r, lane),
				Observed: f.observed[seat.Handle][lane], Plan: []seataccess.Step{}}
			if outcome, ok := f.outcomes[seat.Handle]; ok {
				la.Sync = &seataccess.SyncResult{Outcome: outcome}
			}
			seat.Lanes[lane] = la
		}
		answer.Seats = append(answer.Seats, seat)
	}
	return answer, nil
}

func (f *fakeAccess) Plan(_ context.Context, q setupapi.AccessQuery,
	d seataccess.Draft) (seataccess.PlanAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans = append(f.plans, d)
	plan := seataccess.PlanAnswer{Kind: q.Kind, Revision: q.Revision, StepUp: f.stepUp}
	offers := map[seataccess.Lane]seataccess.Options{}
	for _, o := range f.options {
		offers[o.Lane] = o
	}
	held := map[string]*config.Role{}
	for r := range q.Company.EachRole() {
		held[r.Seat().Handle()] = r
	}
	for handle, lanes := range d.Seats {
		for lane, after := range lanes {
			// THE ENGINE'S OWN RULE, from the vendor's own facts: what the
			// route asks a raise for is what the plan says is one.
			raise := f.stepUp || seataccess.NeedsStepUp(offers[lane],
				desiredOf(held[handle], lane), after)
			plan.StepUp = plan.StepUp || raise
			plan.Plans = append(plan.Plans, seataccess.SeatPlan{Handle: handle, Lane: lane,
				StepUp: raise, Steps: []seataccess.Step{{Op: seataccess.OpLevel,
					Verb: seataccess.VerbRerole, Text: "Move it at the vendor",
					By: seataccess.ByCrewlet}}})
		}
	}
	return plan, nil
}

func (f *fakeAccess) Sync(_ context.Context, req setupapi.AccessSync) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, req)
	return f.syncErr
}

func (f *fakeAccess) InstallLink(_ context.Context, _ setupapi.AccessQuery,
	handle string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outcomes[handle] != seataccess.OutcomeWaiting {
		return "", setupapi.ErrNotAwaitingInstall
	}
	return "https://slack.com/oauth/v2/authorize?state=fake-" + handle, nil
}

func (f *fakeAccess) lastSync(t *testing.T) setupapi.AccessSync {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.syncs) == 0 {
		t.Fatal("no sync was asked for")
	}
	return f.syncs[len(f.syncs)-1]
}

// datadogCompany is a company with Datadog's block and two agents on it in the
// seat blocks' own terms: the SRE lead on, at the Standard Role, and the CTO
// off.
const datadogCompany = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {"datadog": {"enabled": true, "route_to": "sre-lead",
    "webhook_token": "${DATADOG_WEBHOOK_TOKEN}", "provisioning": {"site": "datadoghq.com",
    "api_key": "${DATADOG_API_KEY}", "app_key": "${DATADOG_APP_KEY}"}}},
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu",
     "integrations": {"datadog": {"enabled": true, "role": "role-std"}}},
    {"name": "CTO", "handle": "cto", "llm": "zulu",
     "integrations": {"datadog": {"enabled": false}}},
    {"name": "Owner", "handle": "owner", "kind": "human"}
  ]
}`

// datadogOptions are Datadog's roles as the organization lists them.
var datadogOptions = []seataccess.Options{{Lane: seataccess.LaneDatadog, Live: true,
	Levels: []seataccess.Level{
		{ID: "role-ro", Name: "Datadog Read Only Role"},
		{ID: "role-std", Name: "Datadog Standard Role"},
		{ID: "role-admin", Name: "Datadog Admin Role", Administers: true},
	}}}

func newAccessSurface(t *testing.T) *surface {
	t.Helper()
	s := newSurface(t)
	s.access.options = datadogOptions
	s.seedDocument(t, datadogCompany)
	return s
}

// THE TAB READS THE ENGINE'S ANSWER AT THE FLEET'S REVISION, TAGGED WITH IT.
//
// The answer names the revision it was read at, in its body and as its entity
// tag, which is what a save sends back as If-Match. A kind that manages no
// account per agent is refused by name, and Jira is pointed at Atlassian.
func TestTheAgentsTabReadsAtTheFleetsRevision(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	res := s.do(t, http.MethodGet, "/setup/integrations/datadog/access", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", res.Code, res.Body)
	}
	var answer seataccess.Answer
	if err := json.Unmarshal(res.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	revision := s.active(t).ID
	if answer.Revision != revision || res.Header().Get("ETag") != `"`+revision+`"` {
		t.Errorf("revision %q, ETag %q; want the fleet's %q", answer.Revision,
			res.Header().Get("ETag"), revision)
	}
	if len(answer.Seats) != 2 {
		t.Errorf("seats = %+v, want the two agents and not the person", answer.Seats)
	}
	for path, want := range map[string]httpjson.Code{
		"/setup/integrations/jira/access":   httpjson.CodeNoSeatAccess,
		"/setup/integrations/nope/access":   httpjson.CodeNoSeatAccess,
		"/setup/integrations/linear/access": httpjson.CodeNoSeatAccess,
	} {
		res := s.do(t, http.MethodGet, path, "", nil)
		if res.Code != http.StatusNotFound || decode(t, res)["error"] != string(want) {
			t.Errorf("GET %s = %d %s, want 404 %s", path, res.Code, res.Body, want)
		}
	}
}

// A SAVE IS ONE REVISION OF THE SEATS IT CHANGES, THEN A SYNC.
//
// Turning the CTO on at the Read Only Role writes one revision held to the one
// the tab was read at, in the seat block's own keys, attributed to the caller
// with a history line naming what changed, and starts the sync on that
// revision. A save on a revision the fleet has left is refused and writes
// nothing.
func TestASaveIsOneRevisionThenASync(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	base := s.active(t).ID
	body := `{"seats": {"cto": {"datadog": {"enabled": true, "level": "role-ro"}}}}`
	res := s.do(t, http.MethodPut, "/setup/integrations/datadog/access", body,
		map[string]string{"If-Match": `"` + base + `"`})
	if res.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", res.Code, res.Body)
	}
	after := s.active(t)
	if after.ID == base || after.ParentID != base {
		t.Fatalf("the save did not land one revision on %s: %+v", base, after)
	}
	if !strings.Contains(after.Summary, "turn on CTO on datadog") {
		t.Errorf("history line %q does not say what changed", after.Summary)
	}
	company := s.activeCompany(t)
	for r := range company.EachRole() {
		if r.Seat().Handle() != "cto" {
			continue
		}
		got := desiredOf(r, seataccess.LaneDatadog)
		if !got.Enabled || got.Level != "role-ro" {
			t.Errorf("the CTO's Datadog block = %+v, want on at role-ro", got)
		}
	}
	sync := s.access.lastSync(t)
	if sync.Kind != integration.KindDatadog || sync.Revision != after.ID || len(sync.Asks) != 0 {
		t.Errorf("sync = %+v, want datadog on the saved revision, asking nothing", sync)
	}

	// A HEADER IS A PRECONDITION: the revision its If-Match named is gone,
	// so the save is refused 412 and writes nothing.
	stale := s.do(t, http.MethodPut, "/setup/integrations/datadog/access",
		`{"seats": {"cto": {"datadog": {"enabled": true, "level": "role-std"}}}}`,
		map[string]string{"If-Match": `"` + base + `"`})
	if stale.Code != http.StatusPreconditionFailed ||
		decode(t, stale)["error"] != string(httpjson.CodeRevisionAdvanced) {
		t.Errorf("a save under a stale If-Match = %d %s, want 412", stale.Code, stale.Body)
	}
	// A BODY'S REVISION IS THE DOMAIN'S BASE: the same stale revision named
	// in the body is the domain's 409.
	body409 := s.do(t, http.MethodPut, "/setup/integrations/datadog/access",
		`{"revision": "`+base+`", "seats": {"cto": {"datadog": {"enabled": true, "level": "role-std"}}}}`, nil)
	if body409.Code != http.StatusConflict ||
		decode(t, body409)["error"] != string(httpjson.CodeRevisionAdvanced) {
		t.Errorf("a save naming a stale revision in its body = %d %s, want 409",
			body409.Code, body409.Body)
	}
	if s.active(t).ID != after.ID {
		t.Error("a stale save wrote a revision")
	}
}

// EVERY ACCESS WRITE ENDS IN THE CHECK.
//
// A save, an action at the vendor (here a retry) and a keep each start a sync,
// and each sync carries the check route's own pass as its follow-up: read-only,
// with no sink, and recorded through the loop's fold, so the integration's
// status row holds what the pass found. That row is the integration's issues,
// notes and card; a sync records none of it, and before this a person pressed
// Check now to see what their own save had done.
//
// Mutation: drop `Then` from any of the three writes, or record nothing in
// [setupapi.Service] recheck, and a case here fails.
func TestEveryAccessWriteEndsInTheCheck(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	s.access.observed = map[string]map[seataccess.Lane]seataccess.Observed{
		"sre-lead": {seataccess.LaneDatadog: {Exists: true, Enabled: true, Level: "role-ro"}},
	}
	note := integration.Finding{Kind: integration.FindingRegistrationOrphaned,
		Subject: "datadog service accounts", Title: "Service accounts with no seat",
		Detail: "Crewlet made 1 enabled service account matching no seat"}
	pass := &recordingPass{kind: integration.KindDatadog, findings: []integration.Finding{note}}
	status, _ := s.withPass(t, pass)
	var recorded atomic.Int64
	status.onSave = func(integration.State) { recorded.Add(1) }

	followed := func(what string) {
		t.Helper()
		pass.mu.Lock()
		before := len(pass.calls)
		pass.mu.Unlock()
		saves := recorded.Load()
		sync := s.access.lastSync(t)
		if sync.Then == nil {
			t.Fatalf("%s started a sync with no follow-up", what)
		}
		// THE ENGINE RUNS IT once the sync has finished.
		sync.Then(t.Context())
		pass.mu.Lock()
		calls := slices.Clone(pass.calls)
		pass.mu.Unlock()
		if len(calls) != before+1 {
			t.Fatalf("%s: the follow-up ran %d passes, want the check's one", what, len(calls)-before)
		}
		if in := calls[len(calls)-1]; in.Sink != nil || in.WebhookBase != fixtureWebhookBase {
			t.Errorf("%s: the follow-up ran %+v, want the check's read-only pass with the address", what, in)
		}
		row, ok := status.get(integration.KindDatadog)
		if recorded.Load() != saves+1 || !ok || len(row.Findings) != 1 || !row.Findings[0].Same(note) {
			t.Errorf("%s: the status row holds %+v after %d writes, want what the check found, "+
				"written once", what, row.Findings, recorded.Load()-saves)
		}
	}

	base := s.active(t).ID
	res := s.do(t, http.MethodPut, "/setup/integrations/datadog/access",
		`{"seats": {"cto": {"datadog": {"enabled": true, "level": "role-ro"}}}}`,
		map[string]string{"If-Match": `"` + base + `"`})
	if res.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", res.Code, res.Body)
	}
	followed("a save")

	res = s.do(t, http.MethodPost, "/setup/integrations/datadog/access/seats/sre-lead/datadog/retry", `{}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("retry = %d: %s", res.Code, res.Body)
	}
	followed("a retry")

	res = s.do(t, http.MethodPost, "/setup/integrations/datadog/access/seats/sre-lead/datadog/keep", `{}`,
		map[string]string{"If-Match": `"` + s.active(t).ID + `"`})
	if res.Code != http.StatusOK {
		t.Fatalf("keep = %d: %s", res.Code, res.Body)
	}
	if sync := s.access.lastSync(t); !sync.AfterSave || !slices.Equal(sync.Handles, []string{"sre-lead"}) {
		t.Errorf("the keep's sync = %+v, want the kept seat's, after its revision applies", sync)
	}
	followed("a keep")
}

// A DRAFT NAMES ONLY WHAT THE TAB OFFERS.
//
// A seat that is a person or not there, a lane the integration does not grant,
// a role the vendor does not list, and a field the surface does not take (the
// incident's shape: a body naming a variable) are each refused, and nothing is
// written.
func TestADraftNamesOnlyWhatTheTabOffers(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	base := s.active(t).ID
	for body, want := range map[string]httpjson.Code{
		`{"seats": {"owner": {"datadog": {"enabled": true}}}}`:                         httpjson.CodeNoSuchSeat,
		`{"seats": {"nobody": {"datadog": {"enabled": true}}}}`:                        httpjson.CodeNoSuchSeat,
		`{"seats": {"cto": {"gitlab": {"enabled": true}}}}`:                            httpjson.CodeNoSeatAccess,
		`{"seats": {"cto": {"datadog": {"enabled": true, "level": "role-root"}}}}`:     httpjson.CodeAccessNotOffered,
		`{"seats": {"cto": {"datadog": {"enabled": true, "variable": "DD_KEY"}}}}`:     httpjson.CodeInvalidBody,
		`{"seats": {"cto": {"datadog": {"enabled": true}}}, "sink": "${DD_KEY}"}`:      httpjson.CodeInvalidBody,
		`{"seats": {"sre-lead": {"datadog": {"enabled": true, "level": "role-std"}}}}`: httpjson.CodeInvalidBody,
	} {
		res := s.do(t, http.MethodPut, "/setup/integrations/datadog/access", body, nil)
		if res.Code < 400 || decode(t, res)["error"] != string(want) {
			t.Errorf("PUT %s = %d %s, want %s", body, res.Code, res.Body, want)
		}
	}
	if now := s.active(t).ID; now != base {
		t.Errorf("a refused draft moved the fleet to %s", now)
	}
}

// A RAISE TO AN ADMINISTRATION ROLE NEEDS A PERSON PRESENT (D10).
//
// The plan says the draft hands out an administration grant. A machine token
// holding every permission is refused it; the same save by a person with a
// fresh proof lands.
func TestARaiseToAnAdministrationRoleNeedsAPerson(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	s.access.stepUp = true
	body := `{"seats": {"cto": {"datadog": {"enabled": true, "level": "role-admin"}}}}`
	token := iam.Principal{ID: uuid.New(), Login: iam.TokenLogin("ci"), Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: iam.AllGrants, ReauthAt: time.Now().Add(time.Hour),
		Via: iam.MachineTokenPrefix + "ci"}
	res := s.doAs(t, token, http.MethodPut, "/setup/integrations/datadog/access", body, nil)
	if res.Code != http.StatusForbidden {
		t.Fatalf("a machine token raising an agent to Admin = %d %s, want 403",
			res.Code, res.Body)
	}
	person := iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: iam.AllGrants, ReauthAt: time.Now().Add(time.Hour)}
	res = s.doAs(t, person, http.MethodPut, "/setup/integrations/datadog/access", body, nil)
	if res.Code != http.StatusOK {
		t.Errorf("a person with a fresh proof = %d %s, want 200", res.Code, res.Body)
	}
}

// THE SAVE'S CHECK ANSWERS THE PLAN AND WRITES NOTHING (ADR-0055).
func TestTheSavesCheckAnswersThePlan(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	s.access.stepUp = true
	base := s.active(t).ID
	res := s.do(t, http.MethodPut, "/setup/integrations/datadog/access?dry_run=true",
		`{"seats": {"cto": {"datadog": {"enabled": true, "level": "role-admin"}}}}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("check = %d: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["step_up"] != true || body["plans"] == nil || body["warnings"] == nil {
		t.Errorf("the check answered %v, want the plan, its step-up and the warnings", body)
	}
	if now := s.active(t).ID; now != base {
		t.Errorf("a check moved the fleet to %s", now)
	}
	if len(s.access.syncs) != 0 {
		t.Error("a check started a sync")
	}
}

// THE SEAT ACTIONS ARE ASKS, EXCEPT KEEP, WHICH IS A REVISION.
//
// Remove is refused while the seat is on (D1); rotate and remove reach the
// sync as the person's asks; an ask while a sync runs is refused by name; and
// keep writes the vendor's setting into the document (D2), so nothing is
// restored.
func TestTheSeatActions(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	s.access.observed = map[string]map[seataccess.Lane]seataccess.Observed{
		"sre-lead": {seataccess.LaneDatadog: {Exists: true, Enabled: true, Level: "role-ro"}},
		"cto":      {seataccess.LaneDatadog: {Exists: true, Enabled: false}},
	}
	act := func(handle, action string) map[string]any {
		res := s.do(t, http.MethodPost, "/setup/integrations/datadog/access/seats/"+handle+
			"/datadog/"+action, "", nil)
		body := decode(t, res)
		body["status"] = float64(res.Code)
		return body
	}
	if got := act("sre-lead", "remove"); got["error"] != string(httpjson.CodeSeatStillOn) {
		t.Errorf("remove while on = %v", got)
	}
	if got := act("cto", "remove"); got["status"] != float64(http.StatusOK) {
		t.Fatalf("remove while off = %v", got)
	}
	if ask := s.access.lastSync(t).Asks; len(ask) != 1 || !ask[0].Ask.Remove ||
		ask[0].Handle != "cto" {
		t.Errorf("remove reached the sync as %+v", ask)
	}
	if got := act("sre-lead", "rotate"); got["status"] != float64(http.StatusOK) ||
		!s.access.lastSync(t).Asks[0].Ask.Rotate {
		t.Errorf("rotate = %v", got)
	}
	s.access.syncErr = setupapi.ErrAccessSyncInFlight
	if got := act("sre-lead", "rotate"); got["error"] != string(httpjson.CodePassInFlight) {
		t.Errorf("an ask during a sync = %v", got)
	}
	s.access.syncErr = nil
	base := s.active(t).ID
	if got := act("sre-lead", "keep"); got["status"] != float64(http.StatusOK) {
		t.Fatalf("keep = %v", got)
	}
	if s.active(t).ID == base {
		t.Fatal("keep wrote no revision")
	}
	for r := range s.activeCompany(t).EachRole() {
		if r.Seat().Handle() == "sre-lead" {
			if got := desiredOf(r, seataccess.LaneDatadog); got.Level != "role-ro" {
				t.Errorf("keep left the SRE lead at %+v, want the vendor's role-ro", got)
			}
		}
	}
	for _, path := range []string{"seats/cto/datadog/explode", "seats/cto/gitlab/retry"} {
		res := s.do(t, http.MethodPost, "/setup/integrations/datadog/access/"+path, "", nil)
		if res.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", path, res.Code)
		}
	}
}

// activeCompany is the fleet's company, decoded.
func (s *surface) activeCompany(t *testing.T) *config.Company {
	t.Helper()
	company, _, err := s.config.ActiveCompany(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return company
}

// desiredOf is a seat's lane as the document holds it.
func desiredOf(r *config.Role, lane seataccess.Lane) seataccess.Desired {
	d, _ := seataccess.DesiredOf(r, lane)
	return d
}

// A RAISE IS ASKED OF A PERSON, A LOWERING OF NOBODY (D10).
//
// The same administrator's machine token, admitted for the save's own verb,
// lowers the SRE lead from the Standard Role to Read Only and is never asked
// for the raise's verb; raising the CTO to the Admin Role it is refused, and
// the person's assistant carrying the one request they approved raises it.
// Mutation: ask the raise on every save, or drop the raise's person-present
// rule, and a case here fails.
func TestARaiseIsAskedOfAPersonAndALoweringOfNobody(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	fresh := time.Now().Add(time.Hour)
	token := iam.Principal{ID: uuid.New(), Login: iam.TokenLogin("ci"), Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: iam.AllGrants, ReauthAt: fresh,
		Via: iam.MachineTokenPrefix + "ci"}
	lower := `{"seats": {"sre-lead": {"datadog": {"enabled": true, "level": "role-ro"}}}}`
	if res := s.doAs(t, token, http.MethodPut, "/setup/integrations/datadog/access", lower,
		nil); res.Code != http.StatusOK {
		t.Fatalf("a token lowering an agent = %d %s, want 200", res.Code, res.Body)
	}
	raise := `{"seats": {"cto": {"datadog": {"enabled": true, "level": "role-admin"}}}}`
	res := s.doAs(t, token, http.MethodPut, "/setup/integrations/datadog/access", raise, nil)
	if res.Code != http.StatusForbidden || decode(t, res)["reason"] == nil {
		t.Fatalf("a token raising an agent to Admin = %d %s, want a 403 naming the reason",
			res.Code, res.Body)
	}
	approved := iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: iam.AllGrants, ReauthAt: fresh,
		Via: iam.AssistName(uuid.Must(uuid.NewV7())), AssistScope: iam.AssistScopeExec}
	if res := s.doAs(t, approved, http.MethodPut, "/setup/integrations/datadog/access", raise,
		nil); res.Code != http.StatusOK {
		t.Errorf("an approved assistant request raising an agent = %d %s, want 200",
			res.Code, res.Body)
	}
}

// THE OPERATOR'S DOCUMENT IS READ-ONLY HERE.
//
// Where the operator writes the company document (Enforce mode), the tab says
// so, a save and a keep, which write a revision, are refused with where the
// change is made instead, and a rotation, which writes none, still runs.
func TestTheOperatorsDocumentIsReadOnlyHere(t *testing.T) {
	t.Parallel()
	s := newManagedSurface(t, nil, fixtureWebhookBase, "operator")
	s.access.options = datadogOptions
	s.access.observed = map[string]map[seataccess.Lane]seataccess.Observed{
		"sre-lead": {seataccess.LaneDatadog: {Exists: true, Enabled: true, Level: "role-ro"}},
	}
	s.seedDocument(t, datadogCompany)
	res := s.do(t, http.MethodGet, "/setup/integrations/datadog/access", "", nil)
	if res.Code != http.StatusOK || decode(t, res)["managed_by"] != seataccess.ManagedByOperator {
		t.Errorf("the tab = %d %s, want it marked as the operator's", res.Code, res.Body)
	}
	for _, write := range []struct{ method, path, body string }{
		{http.MethodPut, "/setup/integrations/datadog/access",
			`{"seats": {"cto": {"datadog": {"enabled": true, "level": "role-ro"}}}}`},
		{http.MethodPost, "/setup/integrations/datadog/access/seats/sre-lead/datadog/keep", ""},
	} {
		res := s.do(t, write.method, write.path, write.body, nil)
		body := decode(t, res)
		if res.Code != http.StatusForbidden || body["error"] != string(httpjson.CodeConfigManaged) ||
			!strings.Contains(body["detail"].(string), "change the Seat object") {
			t.Errorf("%s %s on the operator's document = %d %v", write.method, write.path,
				res.Code, body)
		}
	}
	res = s.do(t, http.MethodPost, "/setup/integrations/datadog/access/seats/sre-lead/datadog/rotate",
		"", nil)
	if res.Code != http.StatusOK {
		t.Errorf("a rotation on the operator's document = %d %s, want it run", res.Code, res.Body)
	}
}

// A CREDENTIAL HANDED IN FROM OUTSIDE CREWLET IS NOT ROTATED HERE: the engine
// binds it and never writes it, so the refusal says where it is rotated.
func TestAnExternalCredentialIsNotRotated(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	s.access.options = []seataccess.Options{{Lane: seataccess.LaneDatadog,
		Managed: seataccess.ManagedExternal}}
	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/access/seats/sre-lead/datadog/rotate",
		"", nil)
	if res.Code != http.StatusConflict ||
		decode(t, res)["error"] != string(httpjson.CodeCredentialExternal) {
		t.Errorf("rotating a handed-in credential = %d %s", res.Code, res.Body)
	}
	if len(s.access.syncs) != 0 {
		t.Error("a refused rotation reached the sync")
	}
}

// slackCompany is a company with Slack connected and two agents on it: the CTO
// on, whose app waits for its install, and the SRE lead on and installed.
const slackCompany = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {"slack": {}},
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu",
     "integrations": {"slack": {"enabled": true, "bot_token": "${SLACK_BOT_TOKEN_SRE_LEAD}",
       "signing_secret": "${SLACK_SIGNING_SECRET_SRE_LEAD}"}}},
    {"name": "CTO", "handle": "cto", "llm": "zulu",
     "integrations": {"slack": {"enabled": true, "bot_token": "${SLACK_BOT_TOKEN_CTO}",
       "signing_secret": "${SLACK_SIGNING_SECRET_CTO}"}}}
  ]
}`

// A SLACK INSTALL LINK IS MINTED ONLY WHILE THE APP WAITS FOR ITS INSTALL, is
// never cached, and is never part of the tab.
func TestASlackInstallLinkIsMintedOnlyWhileWaiting(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.access.outcomes = map[string]seataccess.Outcome{"cto": seataccess.OutcomeWaiting,
		"sre-lead": seataccess.OutcomeOK}
	s.seedDocument(t, slackCompany)
	res := s.do(t, http.MethodPost, "/setup/integrations/slack/access/seats/cto/slack/install",
		"", nil)
	if res.Code != http.StatusOK || !strings.HasPrefix(decode(t, res)["url"].(string),
		"https://slack.com/") || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("an install link for a waiting app = %d %s %v", res.Code, res.Body, res.Header())
	}
	res = s.do(t, http.MethodPost, "/setup/integrations/slack/access/seats/sre-lead/slack/install",
		"", nil)
	if res.Code != http.StatusConflict {
		t.Errorf("an install link for an installed app = %d %s, want 409", res.Code, res.Body)
	}
	tab := s.do(t, http.MethodGet, "/setup/integrations/slack/access", "", nil)
	if strings.Contains(tab.Body.String(), "oauth") {
		t.Errorf("the tab carries an install link: %s", tab.Body)
	}
}
