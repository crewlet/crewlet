package setupapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// ascIdentityCompany is a company with App Store Connect connected and its growth
// agent on at it.
const ascIdentityCompany = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {"app_store_connect": {"enabled": true, "connections": {"main": {
    "issuer_id": "57246542-96fe-1a63-e053-0824d011072a",
    "keys": {"marketing": {"key_id": "2X9R4HXF34", "private_key": "${ASC_MARKETING_KEY}"}}}}}},
  "roles": [
    {"name": "Growth", "handle": "growth", "llm": "zulu",
     "integrations": {"app_store_connect": {"enabled": true, "level": "read", "apps": ["111"]}}}
  ]
}`

const identityPath = "/setup/integrations/app_store_connect/access/seats/growth/app_store_connect/identity"

// A SEAT'S IDENTITY IS WRITTEN BEFORE ITS SYNC IS ASKED, and answered as a
// verdict and the tab: keeping one writes `identity: own` and hands the key to
// the engine, never echoing it; giving it up writes `company`. A lane whose
// vendor offers no identity is not found, and a body that is no hand-in is
// refused before the engine is asked.
//
// Mutation: ask the engine before the revision lands, and its check reads the
// seat as still on the company's; echo the body, and the key leaves in the answer.
func TestAnAgentsIdentityIsWrittenThenChecked(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.access.identityOffered = map[seataccess.Lane]bool{seataccess.LaneAppStoreConnect: true}
	s.seedDocument(t, ascIdentityCompany)
	res := s.do(t, http.MethodPost, identityPath,
		`{"way": "handed_in", "value": "-----BEGIN PRIVATE KEY-----fake", "file_name": "AuthKey_7KQ4M2ZX9P.p8"}`, nil)
	body := decode(t, res)
	verdict, _ := body["verdict"].(map[string]any)
	if res.Code != http.StatusOK || verdict["outcome"] != "works" || body["access"] == nil {
		t.Fatalf("keep = %d %s", res.Code, res.Body)
	}
	if strings.Contains(res.Body.String(), "BEGIN PRIVATE KEY") {
		t.Fatalf("the answer echoes the key: %s", res.Body)
	}
	if got := s.activeCompany(t).Roles[0].Integrations.AppStoreConnect.Identity; got != "own" {
		t.Fatalf("the document's identity is %q after a keep, want own", got)
	}
	if len(s.access.identities) != 1 || !s.access.identities[0].Keep ||
		s.access.identities[0].FileName != "AuthKey_7KQ4M2ZX9P.p8" {
		t.Fatalf("the engine was asked %+v", s.access.identities)
	}

	res = s.do(t, http.MethodDelete, identityPath, "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("drop = %d %s", res.Code, res.Body)
	}
	if got := s.activeCompany(t).Roles[0].Integrations.AppStoreConnect.Identity; got != "company" {
		t.Fatalf("the document's identity is %q after a drop, want company", got)
	}

	for name, c := range map[string]struct{ body, field string }{
		"no way":            {`{"value": "x"}`, "way"},
		"an empty hand-in":  {`{"way": "handed_in"}`, "value"},
		"a reference":       {`{"way": "handed_in", "value": "${KEY}"}`, "value"},
		"a value to invite": {`{"way": "invited", "value": "x"}`, "value"},
	} {
		res := s.do(t, http.MethodPost, identityPath, c.body, nil)
		if body := decode(t, res); res.Code != http.StatusBadRequest || body["field"] != c.field {
			t.Errorf("%s = %d %v", name, res.Code, body)
		}
	}
	if len(s.access.identities) != 2 {
		t.Errorf("a refused body reached the engine: %+v", s.access.identities)
	}
	// A SAVE CARRIES THE IDENTITY ONLY UNCHANGED: who signs moves on its own
	// route alone, which asks for a credential write and a fresh step-up.
	res = s.do(t, http.MethodPut, "/setup/integrations/app_store_connect/access",
		`{"seats": {"growth": {"app_store_connect": {"enabled": true, "level": "read", "scope": ["111"], "identity": "own"}}}}`, nil)
	if body := decode(t, res); res.Code != http.StatusBadRequest || body["field"] != "identity" {
		t.Errorf("a save moving the identity = %d %v", res.Code, body)
	}
	if got := s.activeCompany(t).Roles[0].Integrations.AppStoreConnect.Identity; got != "company" {
		t.Errorf("a save moved the identity to %q", got)
	}
	s.access.identityOffered = nil
	if res := s.do(t, http.MethodDelete, identityPath, "", nil); res.Code != http.StatusNotFound {
		t.Errorf("a lane offering no identity = %d %s, want 404", res.Code, res.Body)
	}
}

// THE ENGINE'S REFUSALS MAP TO THE ROUTE'S, beside the field.
func TestAnAgentsIdentityAnswersTheEnginesRefusals(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.access.identityOffered = map[seataccess.Lane]bool{seataccess.LaneAppStoreConnect: true}
	s.seedDocument(t, ascIdentityCompany)
	for _, c := range []struct {
		err  error
		code int
	}{
		{&setupapi.AccessRefusal{Status: http.StatusBadRequest, Reason: "not a key", Remedy: "choose the file"},
			http.StatusBadRequest},
		{setupapi.ErrNotAwaitingHandIn, http.StatusConflict},
		{setupapi.ErrAccessSyncInFlight, http.StatusConflict},
		{setupapi.ErrNoIdentity, http.StatusNotFound},
	} {
		s.access.identityErr = c.err
		res := s.do(t, http.MethodPost, identityPath, `{"way": "handed_in", "value": "k"}`, nil)
		if res.Code != c.code {
			t.Errorf("%v = %d %s, want %d", c.err, res.Code, res.Body, c.code)
		}
	}
}
