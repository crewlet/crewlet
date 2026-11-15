package setupapi_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
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

// founderAsking is the context of a person signed in to choose a request's
// grant: what the answer's route hands the save.
func founderAsking() context.Context {
	return iam.WithPrincipal(context.Background(), iam.Principal{
		ID: uuid.MustParse("018f3a9c-0000-7000-8000-00000000f001"), Kind: iam.KindPerson,
		Login: "founder", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour), Via: "session:browser",
	})
}

// originRequest is the SRE's request for Origin Rules: Edit on example.com for
// four hours, as the engine stored it.
func originRequest(id, handle string) seataccess.GrantRequest {
	return seataccess.GrantRequest{ID: id, Kind: integration.KindCloudflare, Lane: seataccess.LaneCloudflare,
		Handle: handle, Permission: "Origin Write", Scope: []string{"example.com"}, Hours: 4,
		Reason: "add the origin rule for the library route", Item: "ENG-58", Ask: "owner",
		AskedAt: time.Now().UTC()}
}

// CHOOSING A REQUEST'S GRANT SAVES THE GRANT THE STORED REQUEST ASKS FOR
// (ADR-0118): its permission, scope, hours and reason, the request it answers,
// and the person choosing as its author, written as one revision, recorded and
// synced. admit is handed whether the grant raises (a write permission does)
// before anything is written. Choosing it again saves nothing twice. Mutation:
// build the grant from anything but the stored request, or ask admit after the
// write, and this fails.
func TestChoosingARequestsGrantSavesTheGrantItAsked(t *testing.T) {
	t.Parallel()
	s := newCloudflareSurface(t)
	s.access.requests = map[string]seataccess.GrantRequest{"abcde12345": originRequest("abcde12345", "sre")}
	var raised []bool
	admit := func(raise bool) error {
		raised = append(raised, raise)
		if n := len(s.sreGrants(t)); n != 0 {
			t.Errorf("admit was asked after %d grants were written", n)
		}
		return nil
	}
	start := time.Now().UTC()
	if err := s.setup.SaveRequestedGrant(founderAsking(), "sre", "abcde12345", false, admit); err != nil {
		t.Fatalf("save: %v", err)
	}
	grants := s.sreGrants(t)
	if len(grants) != 1 {
		t.Fatalf("grants = %+v, want the one the request asked for", grants)
	}
	g := grants[0]
	lapse := g.ExpiresAt.Sub(start) //fixedwait:stated the expiry the save stamped against the instant before it
	if g.Permission != "Origin Write" || !slices.Equal(g.Scope, []string{"example.com"}) ||
		g.Request != "abcde12345" || g.Reason != "add the origin rule for the library route" ||
		g.GrantedBy == "" || lapse < 4*time.Hour || lapse > 4*time.Hour+time.Minute {
		t.Errorf("grant = %+v, want the request's permission, scope, reason and four hours", g)
	}
	if !slices.Equal(raised, []bool{true}) {
		t.Errorf("admit was handed %v, want one raise for a write permission", raised)
	}
	if len(s.access.grants) != 1 || len(s.access.grants[0].Added) != 1 || len(s.access.syncs) != 1 ||
		!slices.Equal(s.access.syncs[0].Handles, []string{"sre"}) {
		t.Errorf("recorded %+v and synced %+v, want the grant recorded and the SRE synced",
			s.access.grants, s.access.syncs)
	}
	revision := s.active(t).ID
	if err := s.setup.SaveRequestedGrant(founderAsking(), "sre", "abcde12345", false, admit); err != nil ||
		s.active(t).ID != revision || len(raised) != 1 {
		t.Errorf("choosing it again = %v, revision %s from %s, admit asked %d times; want "+
			"nothing saved twice", err, s.active(t).ID, revision, len(raised))
	}
}

// A REQUEST'S GRANT THAT CANNOT BE SAVED AS ASKED WRITES NOTHING, saying why: a
// request another agent asked, one lapsed or settled, a permission the
// company's connection does not hold, and a person admit refuses, whose
// refusal is returned as it is. Mutation: save before admit answers, or skip
// the catalogue's check, and this fails.
func TestARequestsGrantThatCannotBeSavedWritesNothing(t *testing.T) {
	t.Parallel()
	refused := errors.New("refused: step up")
	dns := originRequest("dnsdnsdns1", "sre")
	dns.Permission, dns.Scope = "DNS Write", nil
	for _, c := range []struct {
		name  string
		asker string
		id    string
		admit error
		is    func(error) bool
	}{
		{"another agent's", "cto", "abcde12345", nil, isGrantRefused},
		{"lapsed or settled", "sre", "zzzzz99999", nil,
			func(err error) bool { return errors.Is(err, seataccess.ErrNoRequest) }},
		{"one the connection lacks", "sre", "dnsdnsdns1", nil, isGrantRefused},
		{"refused by admit", "sre", "abcde12345", refused,
			func(err error) bool { return errors.Is(err, refused) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newCloudflareSurface(t)
			s.access.requests = map[string]seataccess.GrantRequest{
				"abcde12345": originRequest("abcde12345", "sre"), "dnsdnsdns1": dns}
			revision := s.active(t).ID
			err := s.setup.SaveRequestedGrant(founderAsking(), c.asker, c.id, false,
				func(bool) error { return c.admit })
			if !c.is(err) {
				t.Errorf("save = %v", err)
			}
			if s.active(t).ID != revision || len(s.sreGrants(t)) != 0 || len(s.access.grants) != 0 {
				t.Errorf("a refused save wrote revision %s over %s with grants %+v", s.active(t).ID,
					revision, s.sreGrants(t))
			}
		})
	}
}

// isGrantRefused reports a save refused for what it asked.
func isGrantRefused(err error) bool {
	var refused *seataccess.GrantRefused
	return errors.As(err, &refused)
}
