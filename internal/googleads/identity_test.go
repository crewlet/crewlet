package googleads_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleads"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

const seatEmail = "crewlet-ads-lead-0a1b2c@acme-crewlet-id.iam.gserviceaccount.com"

// issued is the engine's tokens; Google Ads takes no admin sign-in.
type issued struct{}

func (issued) SeatToken(context.Context, integration.Kind, string, []string) (string, string, error) {
	return "ya29.seat", seatEmail, nil
}

func (issued) AdminToken(context.Context, integration.Kind) (string, error) {
	return "", seataccess.ErrNotIssued
}

// ads is Google Ads as the identity sees it: the accounts it reaches through
// the manager it was added to, its access level there, and whether that
// account lets it list its users.
type ads struct {
	mu       sync.Mutex
	role     string
	listable bool
	managers []string
}

func (f *ads) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var q struct{ Query string }
	_ = json.NewDecoder(r.Body).Decode(&q)
	f.managers = append(f.managers, r.Header.Get("Login-Customer-Id"))
	customer := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v26/customers/"), "/googleAds:search")
	refused := func() {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"refused"}}`))
	}
	switch {
	case f.role == "" || r.Header.Get("Login-Customer-Id") != "9998887776":
		refused()
	case strings.Contains(q.Query, "FROM customer_user_access"):
		if !f.listable || customer != "9998887776" || !strings.Contains(q.Query, "'"+seatEmail+"'") {
			refused()
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"customerUserAccess":{"accessRole":"` + f.role + `"}}]}`))
	case strings.Contains(q.Query, "FROM customer") && customer == "1234567890":
		_, _ = w.Write([]byte(`{"results":[{"customer":{"descriptiveName":"Acme US"}}]}`))
	default:
		refused()
	}
}

func (f *ads) sawManagers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.managers)
}

// adsCheck checks an operate seat on account 1234567890, reached through
// manager 9998887776, against f.
func adsCheck(t *testing.T, f *ads) (seataccess.IdentityCheck, googlegrowth.SeatAccess, *config.Company,
	seataccess.IdentityInput) {
	t.Helper()
	company, err := config.ParseCompanyDocument([]byte(`
name: Acme
integrations:
  google_cloud:
    enabled: true
    provisioning:
      identity_project: acme-crewlet-id
      provider: projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine
      federation_key: "${GOOGLE_CLOUD_FEDERATION_KEY}"
  google_ads:
    enabled: true
    customers: [{id: "1234567890", manager: "9998887776"}]
roles:
  - name: Ads lead
    handle: ads-lead
    integrations:
      google_ads: {enabled: true, level: operate, customers: ["1234567890"]}
`))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := seataccess.Lookup(googleads.Kind)
	access := *v.(*googlegrowth.SeatAccess)
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	access.HTTP = &http.Client{Transport: httpxtest.Rewrite(t, srv)}
	s, err := access.Open(t.Context(), seataccess.OpenInput{Config: company, Tokens: issued{}})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := seataccess.DesiredOf(seataccess.SeatsOf(company)[0].Role, seataccess.LaneGoogleAds)
	in := seataccess.IdentityInput{Seat: seataccess.SeatRef{Handle: "ads-lead", Name: "Ads lead"},
		Lane: seataccess.LaneGoogleAds, Want: want}
	check, err := s.(seataccess.IdentityMaker).CheckIdentity(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	return check, access, company, in
}

// AN ADS ACCOUNT IS REACHED THROUGH ITS MANAGER, and the identity's level is
// read as a user of the manager it was added to: Read-only on an operate seat
// is the wrong role, Standard is ready, and a manager that lets it list no
// users leaves the level for the first refusal to name.
//
// Mutation: query the account's users rather than the manager's, and the
// level is never read.
func TestAnAdsSeatsLevelIsReadOnTheManagerItWasAddedTo(t *testing.T) {
	t.Parallel()
	if check, _, _, _ := adsCheck(t, &ads{}); check.Status != seataccess.IdentityNone ||
		!strings.Contains(check.Said, "1234567890") {
		t.Errorf("not added: %+v", check)
	}
	f := &ads{role: "READ_ONLY", listable: true}
	check, _, _, _ := adsCheck(t, f)
	if check.Status != seataccess.IdentityWrongRole || check.Role != "Read-only" || check.Needs != "Standard" {
		t.Errorf("Read-only on an operate seat: %+v", check)
	}
	if m := f.sawManagers(); slices.Contains(m, "") {
		t.Errorf("a call went without its manager: %v", m)
	}
	if check, _, _, _ = adsCheck(t, &ads{role: "STANDARD", listable: true}); check.Status != seataccess.IdentityReady ||
		!strings.Contains(check.Said, "Ads lead reaches") {
		t.Errorf("Standard: %+v", check)
	}
	if check, _, _, _ = adsCheck(t, &ads{role: "STANDARD"}); check.Status != seataccess.IdentityReady ||
		check.Role != "" || !strings.Contains(check.Said, "a call it refuses names the role") {
		t.Errorf("level unread: %+v", check)
	}
}

// THE ADS GUIDE names the manager the identity is added to, Access and
// security's labels and the level; Google Ads has no API that adds it.
func TestTheAdsGuideNamesTheManagerAndTheLevel(t *testing.T) {
	t.Parallel()
	_, access, company, in := adsCheck(t, &ads{})
	g := access.IdentityGuide(company, in.Seat, in.Lane, in.Want)
	var text []string
	for _, s := range g.Steps {
		text = append(text, s.Text.String())
	}
	all := strings.Join(text, "\n")
	if err := g.Check(); err != nil {
		t.Errorf("the guide is not of the one form: %v", err)
	}
	for _, want := range []string{"manager account 9998887776", "Access and security", "Users tab",
		"Access level: Standard", "Add account"} {
		if !strings.Contains(all, want) {
			t.Errorf("steps %q do not say %q", all, want)
		}
	}
	if g.Make != nil {
		t.Errorf("Google Ads offers Add it for me: %+v", g.Make)
	}
}
