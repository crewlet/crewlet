package setupapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// cloudflareCompany is a company on Cloudflare with its SRE on Secure.
const cloudflareCompany = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {"cloudflare": {"enabled": true, "provisioning": {
    "account_id": "0123456789abcdef0123456789abcdef", "api_token": "${CLOUDFLARE_API_TOKEN}"}}},
  "roles": [
    {"name": "SRE", "handle": "sre", "llm": "zulu",
     "integrations": {"cloudflare": {"enabled": true, "level": "secure"}}},
    {"name": "CTO", "handle": "cto", "llm": "zulu"},
    {"name": "Owner", "handle": "owner", "kind": "human"}
  ]
}`

// cloudflareOptions is Cloudflare's levels and a few groups of its live
// catalogue: Origin Rules read and write, and DNS Write, which the provisioner
// does not hold.
var cloudflareOptions = []seataccess.Options{{Lane: seataccess.LaneCloudflare, Live: true,
	Levels: []seataccess.Level{{ID: "read", Name: "Read"}, {ID: "operate", Name: "Operate"},
		{ID: "configure", Name: "Configure"}, {ID: "secure", Name: "Secure", Administers: true}},
	Grants: []seataccess.GrantEntry{
		{ID: "Origin Read", Name: "Origin Rules: Read", Group: "Zone", ScopeKind: "zone"},
		{ID: "Origin Write", Name: "Origin Rules: Edit", Group: "Zone", ScopeKind: "zone", Write: true},
		{ID: "DNS Write", Name: "DNS Write", Group: "Zone", ScopeKind: "zone", Write: true,
			Unavailable: "The provisioner token does not hold DNS Write", Missing: []string{"DNS Write"}},
	},
	GrantScope: []string{"example.com"},
}}

func newCloudflareSurface(t *testing.T) *surface {
	t.Helper()
	s := newSurface(t)
	s.access.options = cloudflareOptions
	s.seedDocument(t, cloudflareCompany)
	return s
}

// sreGrants is the SRE's Cloudflare grants in the fleet's active revision.
func (s *surface) sreGrants(t *testing.T) []seataccess.Grant {
	t.Helper()
	for r := range s.activeCompany(t).EachRole() {
		if r.Seat().Handle() == "sre" {
			return desiredOf(r, seataccess.LaneCloudflare).Grants
		}
	}
	t.Fatal("no sre seat")
	return nil
}

// A GRANT IS STAMPED BY THE ENGINE, NEVER BY THE BODY: a grant added on the tab
// is saved with a fresh id, the person saving it as its author and the time it
// was saved, whatever the body claimed, and recorded for the audit; one sent
// back edited in place is refused, so who granted what stays what the engine
// recorded. Mutation: keep the body's granted_by and the stored author is
// whoever the body named.
func TestAGrantIsStampedByTheEngineNeverByTheBody(t *testing.T) {
	t.Parallel()
	s := newCloudflareSurface(t)
	body := `{"seats": {"sre": {"cloudflare": {"enabled": true, "level": "secure", "grants": [
	  {"id": "", "permission": "Origin Read", "scope": ["example.com"], "granted_by": "mallory",
	   "granted_at": "2020-01-01T00:00:00Z", "reason": "read the origin rules"}]}}}}`
	if res := s.do(t, http.MethodPut, "/setup/integrations/cloudflare/access", body, nil); res.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", res.Code, res.Body)
	}
	grants := s.sreGrants(t)
	if len(grants) != 1 {
		t.Fatalf("grants = %+v, want the one added", grants)
	}
	g := grants[0]
	if !strings.HasPrefix(g.ID, "g") || len(g.ID) != 11 || g.GrantedBy == "" || g.GrantedBy == "mallory" ||
		time.Since(g.GrantedAt) > time.Hour { //fixedwait:measure the grant was stamped by this save
		t.Errorf("grant = %+v, want an engine id, the caller as its author and now as its time", g)
	}
	if len(s.access.grants) != 1 || len(s.access.grants[0].Added) != 1 {
		t.Errorf("recorded %+v, want the one grant added", s.access.grants)
	}

	edited := `{"seats": {"sre": {"cloudflare": {"enabled": true, "level": "secure", "grants": [
	  {"id": "` + g.ID + `", "permission": "Origin Read", "scope": ["example.com"],
	   "granted_by": "` + g.GrantedBy + `", "granted_at": "` + g.GrantedAt.Format(time.RFC3339Nano) + `",
	   "expires_at": "2099-01-01T00:00:00Z", "reason": "read the origin rules"}]}}}}`
	res := s.do(t, http.MethodPut, "/setup/integrations/cloudflare/access", edited, nil)
	if res.Code != http.StatusBadRequest || decode(t, res)["field"] != string(seataccess.FieldGrants) {
		t.Errorf("an edited grant = %d %s, want 400 on its grants", res.Code, res.Body)
	}
}

// A GRANT THE CATALOGUE CANNOT GIVE IS REFUSED beside the grants control: one
// the provisioner does not hold, one the catalogue does not list, and one on a
// zone outside the company's. Mutation: drop grantRefusal from checkDraft and
// each lands in the document.
func TestAGrantTheCatalogueCannotGiveIsRefused(t *testing.T) {
	t.Parallel()
	s := newCloudflareSurface(t)
	for name, grant := range map[string]string{
		"unheld":   `{"permission": "DNS Write", "reason": "edit a record"}`,
		"unlisted": `{"permission": "Account API Tokens Write", "reason": "mint tokens"}`,
		"outside":  `{"permission": "Origin Read", "scope": ["example.org"], "reason": "read"}`,
	} {
		body := `{"seats": {"sre": {"cloudflare": {"enabled": true, "level": "secure", "grants": [` + grant + `]}}}}`
		res := s.do(t, http.MethodPut, "/setup/integrations/cloudflare/access", body, nil)
		if res.Code != http.StatusBadRequest || decode(t, res)["field"] != string(seataccess.FieldGrants) {
			t.Errorf("%s: PUT = %d %s, want 400 on the grants", name, res.Code, res.Body)
		}
	}
	if got := s.sreGrants(t); len(got) != 0 {
		t.Errorf("refused grants landed: %+v", got)
	}
}

// A WRITE GRANT ASKS WHAT SECURE ASKS: an administrator's machine token adds a
// read grant, and is refused a write grant, which only a person present with
// a fresh sign-in hands out. Mutation: drop RaisesGrant from NeedsStepUp and
// the token grants Origin Rules: Edit.
func TestAWriteGrantIsAskedOfAPerson(t *testing.T) {
	t.Parallel()
	s := newCloudflareSurface(t)
	token := iam.Principal{ID: uuid.New(), Login: iam.TokenLogin("ci"), Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: iam.AllGrants, ReauthAt: time.Now().Add(time.Hour),
		Via: iam.MachineTokenPrefix + "ci"}
	read := `{"seats": {"sre": {"cloudflare": {"enabled": true, "level": "secure", "grants": [
	  {"permission": "Origin Read", "reason": "read the origin rules"}]}}}}`
	if res := s.doAs(t, token, http.MethodPut, "/setup/integrations/cloudflare/access", read, nil); res.Code != http.StatusOK {
		t.Fatalf("a token adding a read grant = %d %s, want 200", res.Code, res.Body)
	}
	held := s.sreGrants(t)[0]
	write := `{"seats": {"sre": {"cloudflare": {"enabled": true, "level": "secure", "grants": [
	  {"id": "` + held.ID + `", "permission": "Origin Read", "granted_by": "` + held.GrantedBy + `",
	   "granted_at": "` + held.GrantedAt.Format(time.RFC3339Nano) + `", "reason": "read the origin rules"},
	  {"permission": "Origin Write", "scope": ["example.com"], "reason": "add an origin rule"}]}}}}`
	res := s.doAs(t, token, http.MethodPut, "/setup/integrations/cloudflare/access", write, nil)
	if res.Code != http.StatusForbidden {
		t.Fatalf("a token adding a write grant = %d %s, want 403", res.Code, res.Body)
	}
}

// A REQUEST'S GRANT IS BUILT FROM THE REQUEST: the agent's own reason and the
// hours it asked for, whatever the body says, so the person's choice of
// temporary or permanent is all a body decides; a request another agent made,
// or one already settled, is refused. Mutation: keep the body's reason or
// expiry and a person's draft decides what the agent's request granted.
func TestARequestsGrantIsBuiltFromTheRequest(t *testing.T) {
	t.Parallel()
	s := newCloudflareSurface(t)
	s.access.requests = map[string]seataccess.GrantRequest{"abcdefghij": {
		ID: "abcdefghij", Kind: integration.KindCloudflare, Lane: seataccess.LaneCloudflare, Handle: "sre",
		Permission: "Origin Write", Scope: []string{"example.com"}, Hours: 4,
		Reason: "an origin rule for the new backend", Item: "OPS-1", Ask: "owner", AskedAt: time.Now(),
	}}
	body := `{"seats": {"sre": {"cloudflare": {"enabled": true, "level": "secure", "grants": [
	  {"permission": "Origin Write", "scope": ["example.com"], "expires_at": "2099-01-01T00:00:00Z",
	   "reason": "something else", "request": "abcdefghij"}]}}}}`
	if res := s.do(t, http.MethodPut, "/setup/integrations/cloudflare/access", body, nil); res.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", res.Code, res.Body)
	}
	g := s.sreGrants(t)[0]
	lapse := time.Until(g.ExpiresAt) //fixedwait:measure the request's four hours from this save
	if g.Reason != "an origin rule for the new backend" || lapse > 4*time.Hour || lapse < 3*time.Hour ||
		g.Request != "abcdefghij" {
		t.Errorf("grant = %+v, want the request's reason, four hours and its id", g)
	}

	s.access.requests["bbbbbbbbbb"] = seataccess.GrantRequest{ID: "bbbbbbbbbb", Kind: integration.KindCloudflare,
		Lane: seataccess.LaneCloudflare, Handle: "cto", Permission: "Origin Read", Hours: 4, Reason: "x"}
	for name, request := range map[string]string{"another agent's": "bbbbbbbbbb", "settled": "cccccccccc"} {
		body := `{"seats": {"sre": {"cloudflare": {"enabled": true, "level": "secure", "grants": [
		  {"id": "` + g.ID + `", "permission": "Origin Write", "scope": ["example.com"],
		   "expires_at": "` + g.ExpiresAt.Format(time.RFC3339Nano) + `", "granted_by": "` + g.GrantedBy + `",
		   "granted_at": "` + g.GrantedAt.Format(time.RFC3339Nano) + `", "reason": "` + g.Reason + `",
		   "request": "abcdefghij"},
		  {"permission": "Origin Read", "reason": "x", "request": "` + request + `"}]}}}}`
		res := s.do(t, http.MethodPut, "/setup/integrations/cloudflare/access", body, nil)
		if res.Code != http.StatusBadRequest || res.Body.Len() == 0 ||
			decode(t, res)["error"] != string(httpjson.CodeAccessNotOffered) {
			t.Errorf("%s request: PUT = %d %s, want it refused", name, res.Code, res.Body)
		}
	}
}
