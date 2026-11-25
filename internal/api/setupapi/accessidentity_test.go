package setupapi_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// metaIdentityCompany is a company with Meta Ads connected and its growth agent
// on at it: a lane whose agent may sign as its own or the company's.
const metaIdentityCompany = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {"meta_ads": {"enabled": true, "app_id": "987654321012345",
    "app_secret": "${META_ADS_APP_SECRET}", "read_token": "${META_ADS_READ_TOKEN}"}},
  "roles": [
    {"name": "Growth", "handle": "growth", "llm": "zulu",
     "integrations": {"meta_ads": {"enabled": true, "level": "read", "ad_accounts": ["act_1112223333"]}}}
  ]
}`

const identityPath = "/setup/integrations/meta_ads/access/seats/growth/meta_ads/identity"

// ascIdentityCompany is a company with App Store Connect connected by its
// issuer alone and its growth agent on at it.
const ascIdentityCompany = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {"app_store_connect": {"enabled": true, "connections": {"main": {
    "issuer_id": "57246542-96fe-1a63-e053-0824d011072a"}}}},
  "roles": [
    {"name": "Growth", "handle": "growth", "llm": "zulu",
     "integrations": {"app_store_connect": {"enabled": true, "level": "read", "apps": ["111"]}}}
  ]
}`

const ascIdentityPath = "/setup/integrations/app_store_connect/access/seats/growth/app_store_connect/identity"

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
	s.access.identityOffered = map[seataccess.Lane]bool{seataccess.LaneMetaAds: true}
	s.seedDocument(t, metaIdentityCompany)
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
	if got := s.activeCompany(t).Roles[0].Integrations.MetaAds.Identity; got != "own" {
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
	if got := s.activeCompany(t).Roles[0].Integrations.MetaAds.Identity; got != "company" {
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
	res = s.do(t, http.MethodPut, "/setup/integrations/meta_ads/access",
		`{"seats": {"growth": {"meta_ads": {"enabled": true, "level": "read", "scope": ["act_1112223333"], "identity": "own"}}}}`, nil)
	if body := decode(t, res); res.Code != http.StatusBadRequest || body["field"] != "identity" {
		t.Errorf("a save moving the identity = %d %v", res.Code, body)
	}
	if got := s.activeCompany(t).Roles[0].Integrations.MetaAds.Identity; got != "company" {
		t.Errorf("a save moved the identity to %q", got)
	}
	s.access.identityOffered = nil
	if res := s.do(t, http.MethodDelete, identityPath, "", nil); res.Code != http.StatusNotFound {
		t.Errorf("a lane offering no identity = %d %s, want 404", res.Code, res.Body)
	}
}

// unavailable is a vendor's error that says whether the vendor failed to serve.
type unavailable bool

func (u unavailable) Error() string           { return "the vendor answered" }
func (u unavailable) VendorUnavailable() bool { return bool(u) }

// refused is a vendor's error carrying its definite answer, as a 4xx is.
type refused struct{ said, remedy string }

func (r refused) Error() string { return "the vendor answered 403" }
func (r refused) VendorRefusal() *seataccess.Refusal {
	return &seataccess.Refusal{Said: r.said, Remedy: r.remedy}
}

// THE ENGINE'S REFUSALS MAP TO THE ROUTE'S, beside the field, and only the
// vendor's own fault says the vendor did not answer: no answer, a timeout or a
// 5xx, told by the error's type. A vendor's definite answer is its refusal in
// its own words. Anything of Crewlet's own says so instead.
//
// Mutation: answer every error as the vendor not answering, and a key the
// engine could not check for a reason of its own sends a person to wait on
// the vendor.
func TestAnAgentsIdentityAnswersTheEnginesRefusals(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.access.identityOffered = map[seataccess.Lane]bool{seataccess.LaneMetaAds: true}
	s.seedDocument(t, metaIdentityCompany)
	const vendor, crewlet = "the vendor did not answer", "Crewlet could not finish the check"
	for _, c := range []struct {
		err  error
		code int
		said string
	}{
		{&setupapi.AccessRefusal{Status: http.StatusBadRequest, Reason: "not a key", Remedy: "choose the file"},
			http.StatusBadRequest, "not a key"},
		{setupapi.ErrNotAwaitingHandIn, http.StatusConflict, ""},
		{setupapi.ErrAccessSyncInFlight, http.StatusConflict, ""},
		{setupapi.ErrNoIdentity, http.StatusNotFound, ""},
		{fmt.Errorf("seataccess: identity for growth: %w", context.DeadlineExceeded), http.StatusBadGateway, vendor},
		{&net.DNSError{Err: "no such host", IsTimeout: true}, http.StatusBadGateway, vendor},
		{fmt.Errorf("check: %w", unavailable(true)), http.StatusBadGateway, vendor},
		{fmt.Errorf("check: %w", unavailable(false)), http.StatusInternalServerError, crewlet},
		{fmt.Errorf("seataccess: identity for growth: %w", refused{"Accept the agreement first.", "Sign in as the Account Holder."}),
			http.StatusConflict, "Accept the agreement first."},
		{errors.New("seal META_ADS_KEY_GROWTH: the store is down"), http.StatusInternalServerError, crewlet},
		{fmt.Errorf("%w: %w", setupapi.ErrAccessUnavailable, context.DeadlineExceeded), http.StatusServiceUnavailable,
			""},
	} {
		s.access.identityErr = c.err
		res := s.do(t, http.MethodPost, identityPath, `{"way": "handed_in", "value": "k"}`, nil)
		if res.Code != c.code || !strings.Contains(strings.ToLower(res.Body.String()), strings.ToLower(c.said)) {
			t.Errorf("%v = %d %s, want %d saying %q", c.err, res.Code, res.Body, c.code, c.said)
		}
	}
}

// AN APP STORE CONNECT KEY CARRIES ITS DETAIL, AND HAS NO COMPANY'S TO GO BACK
// TO: the vendor number handed in beside the file reaches the engine with it,
// and a drop is refused before the engine is asked.
//
// Mutation: drop the detail from the request, and a Sales key is never kept.
func TestAnAppStoreConnectKeyCarriesItsDetail(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.access.identityOffered = map[seataccess.Lane]bool{seataccess.LaneAppStoreConnect: true}
	s.seedDocument(t, ascIdentityCompany)
	res := s.do(t, http.MethodPost, ascIdentityPath, `{"way": "handed_in", "value": "-----BEGIN PRIVATE KEY-----fake",
		"file_name": "AuthKey_7KQ4M2ZX9P.p8", "detail": " 85012345 "}`, nil)
	if res.Code != http.StatusOK || len(s.access.identities) != 1 || s.access.identities[0].Detail != "85012345" {
		t.Fatalf("keep = %d %s, asked %+v", res.Code, res.Body, s.access.identities)
	}
	if res := s.do(t, http.MethodDelete, ascIdentityPath, "", nil); res.Code != http.StatusConflict {
		t.Fatalf("drop = %d %s, want 409", res.Code, res.Body)
	}
	if len(s.access.identities) != 1 {
		t.Errorf("the engine was asked to drop: %+v", s.access.identities)
	}
}

// playIdentityCompany is a company with Google Play connected through Google
// Cloud's identity and its mobile agent on at it.
const playIdentityCompany = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {
    "google_cloud": {"enabled": true, "provisioning": {
      "identity_project": "acme-crewlet-id",
      "provider": "projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine",
      "federation_key": "${GOOGLE_CLOUD_FEDERATION_KEY}"}},
    "google_play": {"enabled": true}},
  "roles": [
    {"name": "Mobile", "handle": "mobile", "llm": "zulu",
     "integrations": {"google_play": {"enabled": true, "level": "read", "apps": ["com.acme.app"]}}}
  ]
}`

// AN IDENTITY OF ITS OWN BY CONSTRUCTION HAS NO COMPANY'S TO GO BACK TO: a
// drop on a Google Play agent is refused before the engine is asked, naming
// turning the agent off as the way to give it up.
//
// Mutation: let the drop through, and the route writes an identity setting
// the Google Play block does not take.
func TestAnOwnByConstructionIdentityCannotGoBackToTheCompanys(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.access.identityOffered = map[seataccess.Lane]bool{seataccess.LaneGooglePlay: true}
	s.seedDocument(t, playIdentityCompany)
	res := s.do(t, http.MethodDelete, "/setup/integrations/google_play/access/seats/mobile/google_play/identity",
		"", nil)
	if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "urn the agent off") {
		t.Fatalf("drop = %d %s", res.Code, res.Body)
	}
	if len(s.access.identities) != 0 {
		t.Errorf("the engine was asked %+v", s.access.identities)
	}
}

// A REFUSED CHECK NAMES THE STEP IT STOPPED AT, so the sheet draws that step
// failed and the ones after it not run: the first failed read, or the first
// step where the vendor refused before any read passed. Each step that ran
// carries what it found and the vendor's answers.
//
// Mutation: leave failed_at out, and the sheet draws the last step failed
// whichever read failed, and every earlier one done.
func TestARefusedCheckNamesTheStepItStoppedAt(t *testing.T) {
	t.Parallel()
	guide := &seataccess.IdentityGuide{Checks: []seataccess.GuideCheck{
		{ID: "sign", Label: "Sign in"}, {ID: "apps", Label: "Find the apps"}, {ID: "role", Label: "Read the role"}}}
	refusedAtRole := seataccess.IdentityCheck{Status: seataccess.IdentityWrongRole, Said: "Fits no role.",
		ID: "QD2KH2G8B3", Needs: "App Manager", Team: "main",
		Reads: []seataccess.IdentityRead{{Label: "Sign in", OK: true}, {Label: "Find the apps", OK: true},
			{Label: "Read the role", Said: "Fits no role.", Detail: "financeReports_getCollection 200"}}}
	v := setupapi.IdentityVerdict(refusedAtRole, guide)
	if v.FailedAt != "role" || len(v.Steps) != 1 || v.Steps[0].ID != "role" ||
		v.Steps[0].Detail != "financeReports_getCollection 200" {
		t.Fatalf("a role read that failed: %+v", v)
	}
	// EACH READ IS SAID ON ITS STEP, so the facts are the identity alone, its
	// IDs marked for the mono face.
	want := []setup.Fact{{Label: "Identity", Value: "QD2KH2G8B3", Identifier: true},
		{Label: "Role needed", Value: "App Manager"}, {Label: "Team", Value: "main", Identifier: true}}
	if !reflect.DeepEqual(v.Facts, want) {
		t.Fatalf("facts %+v, want %+v", v.Facts, want)
	}
	if v := setupapi.IdentityVerdict(seataccess.IdentityCheck{Status: seataccess.IdentityRefused,
		Said: "Accept the agreement."}, guide); v.FailedAt != "sign" {
		t.Fatalf("a refusal before any read: failed at %q, want the first step", v.FailedAt)
	}
	if v := setupapi.IdentityVerdict(seataccess.IdentityCheck{Status: seataccess.IdentityReady,
		Reads: []seataccess.IdentityRead{{Label: "Sign in", OK: true}}}, guide); v.FailedAt != "" {
		t.Fatalf("a check that works names a failed step %q", v.FailedAt)
	}
}
