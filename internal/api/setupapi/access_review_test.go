package setupapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// accessToken is an administrator's machine token, fresh.
func accessToken() iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: iam.TokenLogin("ci"), Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: iam.AllGrants, ReauthAt: time.Now().Add(time.Hour),
		Via: iam.MachineTokenPrefix + "ci"}
}

// accessPerson is an administrator at a keyboard, fresh.
func accessPerson() iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: iam.AllGrants, ReauthAt: time.Now().Add(time.Hour)}
}

// AN ACCOUNT MANAGED OUTSIDE CREWLET IS NOT REMOVED OR TURNED BACK ON HERE.
//
// The engine opens no session on a lane it does not provision, so an ask would
// change nothing while the card said it had: both are refused, saying where
// the account is changed, and no sync is asked. Mutation: let either reach the
// sync, and a case here fails.
func TestAnAccountManagedOutsideIsNotRemovedOrRestored(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	s.access.options = []seataccess.Options{{Lane: seataccess.LaneDatadog,
		Managed: seataccess.ManagedExternal}}
	for _, action := range []string{"remove", "restore"} {
		res := s.do(t, http.MethodPost, "/setup/integrations/datadog/access/seats/cto/datadog/"+
			action, "", nil)
		if res.Code != http.StatusConflict ||
			decode(t, res)["error"] != string(httpjson.CodeAccessExternal) {
			t.Errorf("%s on an external lane = %d %s, want 409 access_external", action,
				res.Code, res.Body)
		}
	}
	if len(s.access.syncs) != 0 {
		t.Errorf("a refused ask reached the sync: %+v", s.access.syncs)
	}
}

// KEEPING THE VENDOR'S SETTING IS A SAVE, AND A RAISE WHERE IT IS ONE (D10).
//
// Somebody gave the SRE lead the Admin Role at Datadog. Keeping that writes an
// administration grant into the document, so a machine token is refused it
// and a person keeps it; keeping an ordinary role is the token's to do.
// Mutation: keep on the route's own verb alone, and the token raises the
// agent.
func TestKeepingAnAdministrationRoleNeedsAPerson(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	keep := "/setup/integrations/datadog/access/seats/sre-lead/datadog/keep"
	s.access.observed = map[string]map[seataccess.Lane]seataccess.Observed{
		"sre-lead": {seataccess.LaneDatadog: {Exists: true, Enabled: true, Level: "role-admin"}},
	}
	res := s.doAs(t, accessToken(), http.MethodPost, keep, "", nil)
	if res.Code != http.StatusForbidden ||
		decode(t, res)["error"] != string(httpjson.CodeTokenRefused) {
		t.Fatalf("a token keeping the Admin Role = %d %s, want 403 token_refused", res.Code,
			res.Body)
	}
	if res := s.doAs(t, accessPerson(), http.MethodPost, keep, "", nil); res.Code != http.StatusOK {
		t.Fatalf("a person keeping the Admin Role = %d %s", res.Code, res.Body)
	}
	s.access.observed["sre-lead"][seataccess.LaneDatadog] = seataccess.Observed{Exists: true,
		Enabled: true, Level: "role-ro"}
	if res := s.doAs(t, accessToken(), http.MethodPost, keep, "", nil); res.Code != http.StatusOK {
		t.Errorf("a token keeping an ordinary role = %d %s, want 200", res.Code, res.Body)
	}
}

// A SAVE THAT LANDED IS ANSWERED AS ONE, even when the tab cannot be read again.
//
// The revision is the company's once it is written, so a ledger that stops
// answering after it is no failure of the save: the answer is the tab as read
// before, at the revision written, with the saved setting in place and a sync
// under way. Mutation: answer the re-read's failure, and the person is told
// to save a change that already landed.
func TestALandedSaveIsAnsweredWhenTheTabCannotBeRead(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	s.access.unreadAfterSync = true
	res := s.do(t, http.MethodPut, "/setup/integrations/datadog/access",
		`{"seats": {"cto": {"datadog": {"enabled": true, "level": "role-ro"}}}}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("a landed save whose re-read failed = %d %s, want 200", res.Code, res.Body)
	}
	var answer seataccess.Answer
	if err := json.Unmarshal(res.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Revision != s.active(t).ID || !answer.Syncing {
		t.Errorf("answered revision %q syncing %v, want the written revision %q, syncing",
			answer.Revision, answer.Syncing, s.active(t).ID)
	}
	for _, seat := range answer.Seats {
		if d := seat.Lanes[seataccess.LaneDatadog].Desired; seat.Handle == "cto" &&
			(!d.Enabled || d.Level != "role-ro") {
			t.Errorf("the CTO is answered as %+v, want the saved setting", d)
		}
	}
}

// A SEAT GONE FROM THE DOCUMENT BEFORE THE WRITE IS NOT FOUND, not a fault.
func TestASeatGoneBeforeTheWriteIsNotFound(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	s.access.ghost = "departed-agent"
	res := s.do(t, http.MethodPut, "/setup/integrations/datadog/access",
		`{"seats": {"departed-agent": {"datadog": {"enabled": true}}}}`, nil)
	if res.Code != http.StatusNotFound || decode(t, res)["error"] != string(httpjson.CodeNoSuchSeat) {
		t.Errorf("a save for a seat gone from the document = %d %s, want 404 no_such_seat",
			res.Code, res.Body)
	}
}

// A CHECK OF A SAVE ON THE OPERATOR'S DOCUMENT IS REFUSED AS THE SAVE IS: a
// check that answered a plan for a save that would be refused would be wrong.
func TestACheckOnTheOperatorsDocumentIsRefused(t *testing.T) {
	t.Parallel()
	s := newManagedSurface(t, nil, fixtureWebhookBase, "operator")
	s.access.options = datadogOptions
	s.seedDocument(t, datadogCompany)
	res := s.do(t, http.MethodPut, "/setup/integrations/datadog/access?dry_run=true",
		`{"seats": {"cto": {"datadog": {"enabled": true, "level": "role-ro"}}}}`, nil)
	if res.Code != http.StatusForbidden || decode(t, res)["error"] != string(httpjson.CodeConfigManaged) {
		t.Errorf("a check on the operator's document = %d %s, want 403 config_managed",
			res.Code, res.Body)
	}
}

// gitlabCompany is a company with GitLab provisioned in the acme group, no
// project list, and the backend engineer on it.
const gitlabCompany = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {"gitlab": {"enabled": true, "url": "https://gitlab.example.invalid",
    "signing_secret": "${GITLAB_SIGNING_SECRET}",
    "provisioning": {"group": "acme", "admin_token": "${GITLAB_ADMIN_TOKEN}"}}},
  "roles": [
    {"name": "Backend", "handle": "backend", "llm": "zulu",
     "integrations": {"gitlab": {"enabled": true, "access_level": 30}}}
  ]
}`

// A GITLAB PROJECT OUTSIDE THE COMPANY'S GROUP IS NOT OFFERED.
//
// The connection lists no projects, so any project the vendor accepts was
// taken; but the engine grants an agent its projects as a member of the
// company's group, so a project elsewhere is refused by name. Mutation: take
// an open scope as any path at all, and the outside project is planned.
func TestAGitLabProjectOutsideTheGroupIsNotOffered(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.access.options = []seataccess.Options{{Lane: seataccess.LaneGitLab, ScopeOpen: true,
		Levels: []seataccess.Level{{ID: "30", Name: "Developer"}}}}
	s.seedDocument(t, gitlabCompany)
	plan := func(project string) *http.Response {
		res := s.do(t, http.MethodPost, "/setup/integrations/gitlab/access/plan",
			`{"seats": {"backend": {"gitlab": {"enabled": true, "level": "30", "scope": ["`+
				project+`"]}}}}`, nil)
		return res.Result()
	}
	if res := plan("elsewhere/api"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("a project outside the group = %d, want 400", res.StatusCode)
	}
	if res := plan("acme/api"); res.StatusCode != http.StatusOK {
		t.Errorf("a project in the group = %d, want 200", res.StatusCode)
	}
}

// A KEEP UNDER A STALE IF-MATCH IS A FAILED PRECONDITION, as a save's is: the
// revision the header named is gone, so the vendor's setting is not written
// over whatever replaced it. Mutation: drop Stated from keep's write, and the
// header's lost race answers 409 like an unstated one.
func TestAKeepUnderAStaleIfMatchIsAFailedPrecondition(t *testing.T) {
	t.Parallel()
	s := newAccessSurface(t)
	keep := "/setup/integrations/datadog/access/seats/sre-lead/datadog/keep"
	s.access.observed = map[string]map[seataccess.Lane]seataccess.Observed{
		"sre-lead": {seataccess.LaneDatadog: {Exists: true, Enabled: true, Level: "role-ro"}},
	}
	base := s.active(t).ID
	if res := s.doAs(t, accessPerson(), http.MethodPost, keep, "",
		map[string]string{"If-Match": `"` + base + `"`}); res.Code != http.StatusOK {
		t.Fatalf("a keep on the current revision = %d %s", res.Code, res.Body)
	}
	stale := s.doAs(t, accessPerson(), http.MethodPost, keep, "",
		map[string]string{"If-Match": `"` + base + `"`})
	if stale.Code != http.StatusPreconditionFailed ||
		decode(t, stale)["error"] != string(httpjson.CodeRevisionAdvanced) {
		t.Errorf("a keep under a stale If-Match = %d %s, want 412 revision_advanced",
			stale.Code, stale.Body)
	}
}
