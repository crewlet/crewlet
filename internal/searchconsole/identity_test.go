package searchconsole_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/searchconsole"
	"github.com/crewlet/crewlet/internal/seataccess"
)

const identityEmail = "crewlet-growth-lead-0a1b2c@acme-crewlet-id.iam.gserviceaccount.com"

// issued is the engine's tokens; Search Console takes no admin sign-in.
type issued struct{}

func (issued) SeatToken(context.Context, integration.Kind, string, []string) (seataccess.IssuedSeat, error) {
	return seataccess.IssuedSeat{Token: "ya29.seat", Account: identityEmail}, nil
}

func (issued) ReaderToken(context.Context, integration.Kind) (string, error) {
	return "", seataccess.ErrNotIssued
}

func (issued) AdminToken(context.Context, integration.Kind) (string, error) {
	return "", seataccess.ErrNotIssued
}

// sites is the identity's list of sites, each with its permission level.
type sites struct {
	mu     sync.Mutex
	levels map[string]string
}

func (f *sites) set(site, level string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.levels[site] = level
}

func (f *sites) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/webmasters/v3/sites" || r.Header.Get("Authorization") != "Bearer ya29.seat" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var entries []map[string]string
	for site, level := range f.levels {
		entries = append(entries, map[string]string{"siteUrl": site, "permissionLevel": level})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"siteEntry": entries})
}

// checkAt checks a seat at level on sc-domain:acme.example against f.
func checkAt(t *testing.T, level string, f *sites) (seataccess.IdentityCheck, googlegrowth.SeatAccess,
	*config.Company, seataccess.IdentityInput) {
	t.Helper()
	company, err := config.ParseCompanyDocument([]byte(`
name: Acme
integrations:
  google_cloud:
    enabled: true
    provisioning:
      provisioner: crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com
      provider: projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine
      federation_key: "${GOOGLE_CLOUD_FEDERATION_KEY}"
  search_console:
    enabled: true
roles:
  - name: Growth lead
    handle: growth-lead
    integrations:
      search_console: {enabled: true, level: ` + level + `, sites: ["sc-domain:acme.example"]}
`))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := seataccess.Lookup(searchconsole.Kind)
	access := *v.(*googlegrowth.SeatAccess)
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	access.HTTP = &http.Client{Transport: httpxtest.Rewrite(t, srv)}
	s, err := access.Open(t.Context(), seataccess.OpenInput{Config: company, Tokens: issued{}})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := seataccess.DesiredOf(seataccess.SeatsOf(company)[0].Role, seataccess.LaneSearchConsole)
	in := seataccess.IdentityInput{Seat: seataccess.SeatRef{Handle: "growth-lead", Name: "Growth lead"},
		Lane: seataccess.LaneSearchConsole, Want: want}
	check, err := s.(seataccess.IdentityMaker).CheckIdentity(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	return check, access, company, in
}

// THE IDENTITY'S OWN LIST OF SITES IS ITS REACH AND ITS ROLE: a property
// missing or unverified is not added, a Restricted user on an operate seat is
// the wrong role, and a Full user is ready.
//
// Mutation: read siteUnverifiedUser as reached, and an unverified property
// reads as the wrong role rather than not added.
func TestASearchConsoleSeatIsCheckedOnItsOwnListOfSites(t *testing.T) {
	t.Parallel()
	f := &sites{levels: map[string]string{}}
	for _, c := range []struct {
		level, permission string
		want              seataccess.IdentityStatus
	}{
		{"operate", "", seataccess.IdentityNone},
		{"operate", "siteUnverifiedUser", seataccess.IdentityNone},
		{"operate", "siteRestrictedUser", seataccess.IdentityWrongRole},
		{"operate", "siteFullUser", seataccess.IdentityReady},
		{"read", "siteOwner", seataccess.IdentityReady},
		{"administer", "siteFullUser", seataccess.IdentityWrongRole},
	} {
		if c.permission != "" {
			f.set("sc-domain:acme.example", c.permission)
		}
		check, _, _, _ := checkAt(t, c.level, f)
		if check.Status != c.want {
			t.Errorf("%s at %q: %+v, want %s", c.level, c.permission, check, c.want)
		}
		if c.level == "read" && c.permission == "siteOwner" && !check.Broader {
			t.Errorf("an Owner on a read seat is not warned of: %+v", check)
		}
	}
}

// THE GUIDE IS AN OWNER'S STEPS with Search Console's labels, and offers no
// Add it for me: Search Console has no API that adds a user.
func TestTheSearchConsoleGuideIsAnOwnersStepsOnly(t *testing.T) {
	t.Parallel()
	_, access, company, in := checkAt(t, "operate", &sites{levels: map[string]string{}})
	g := access.IdentityGuide(company, in.Seat, in.Lane, in.Want)
	var text []string
	for _, s := range g.Steps {
		text = append(text, s.Text.String())
	}
	all := strings.Join(text, "\n")
	if err := g.Check(); err != nil {
		t.Errorf("the guide is not of the one form: %v", err)
	}
	for _, want := range []string{"Settings > Users and permissions", "Add user", "Permission: Full user"} {
		if !strings.Contains(all, want) {
			t.Errorf("steps %q do not say %q", all, want)
		}
	}
	// EACH PROPERTY IS LISTED WITH ITS OWN USERS PAGE.
	if len(g.Entries) == 0 || g.Entries[0].ID != "sc-domain:acme.example" ||
		g.Entries[0].AddURL != "https://search.google.com/search-console/users?resource_id=sc-domain%3Aacme.example" {
		t.Errorf("entries %+v", g.Entries)
	}
	if g.Make != nil || !strings.Contains(g.Note.String(), "Indexing API needs Owner") {
		t.Errorf("guide %+v", g)
	}
}
