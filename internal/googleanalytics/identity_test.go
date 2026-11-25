package googleanalytics_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleanalytics"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

const (
	seatEmail  = "crewlet-analyst-0a1b2c@acme-crewlet-id.iam.gserviceaccount.com"
	seatToken  = "ya29.seat"
	adminToken = "ya29.admin"
)

type issued struct{ admin bool }

func (i issued) SeatToken(context.Context, integration.Kind, string, []string) (string, string, error) {
	return seatToken, seatEmail, nil
}

func (i issued) AdminToken(context.Context, integration.Kind) (string, error) {
	if !i.admin {
		return "", seataccess.ErrNotIssued
	}
	return adminToken, nil
}

// analytics is Google Analytics' access: each binding by its parent (a
// property or an account), and which account holds which property.
type analytics struct {
	mu       sync.Mutex
	accounts map[string][]string // account: properties
	bindings map[string]map[string][]string
	writes   []string
}

func newAnalytics() *analytics {
	return &analytics{accounts: map[string][]string{"accounts/9": {"123", "456"}},
		bindings: map[string]map[string][]string{}}
}

// bind gives email roles on parent, as a person adding it would.
func (f *analytics) bind(parent, email string, roles ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bindings[parent] == nil {
		f.bindings[parent] = map[string][]string{}
	}
	f.bindings[parent][email] = roles
}

func (f *analytics) rolesOn(parent string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.bindings[parent][seatEmail])
}

func (f *analytics) wrote() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// reaches reports whether email holds any binding on property id or its account.
func (f *analytics) reaches(email, id string) bool {
	if len(f.bindings["properties/"+id][email]) > 0 {
		return true
	}
	for account, props := range f.accounts {
		if slices.Contains(props, id) && len(f.bindings[account][email]) > 0 {
			return true
		}
	}
	return false
}

func (f *analytics) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	path := r.URL.Path
	switch {
	case path == "/v1beta/accountSummaries" && bearer == seatToken:
		var summaries []map[string]any
		for account, props := range f.accounts {
			var ps []map[string]any
			for _, id := range props {
				if f.reaches(seatEmail, id) {
					ps = append(ps, map[string]any{"property": "properties/" + id, "displayName": "Site " + id})
				}
			}
			summaries = append(summaries, map[string]any{"account": account, "propertySummaries": ps})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accountSummaries": summaries})
	case bearer != adminToken:
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"refused"}}`))
	case strings.HasSuffix(path, "/accessBindings") && r.Method == http.MethodGet:
		parent := strings.TrimSuffix(strings.TrimPrefix(path, "/v1alpha/"), "/accessBindings")
		var out []map[string]any
		for email, roles := range f.bindings[parent] {
			out = append(out, map[string]any{"name": parent + "/accessBindings/" + email, "user": email,
				"roles": roles})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accessBindings": out})
	case strings.HasSuffix(path, "/accessBindings") && r.Method == http.MethodPost:
		parent := strings.TrimSuffix(strings.TrimPrefix(path, "/v1alpha/"), "/accessBindings")
		var b struct {
			User  string
			Roles []string
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if f.bindings[parent] == nil {
			f.bindings[parent] = map[string][]string{}
		}
		f.bindings[parent][b.User] = b.Roles
		f.writes = append(f.writes, "create "+parent)
		_, _ = w.Write([]byte(`{}`))
	case strings.Contains(path, "/accessBindings/"):
		parent, email, _ := strings.Cut(strings.TrimPrefix(path, "/v1alpha/"), "/accessBindings/")
		if r.Method == http.MethodDelete {
			delete(f.bindings[parent], email)
			f.writes = append(f.writes, "delete "+parent)
		} else {
			var b struct{ Roles []string }
			_ = json.NewDecoder(r.Body).Decode(&b)
			f.bindings[parent][email] = b.Roles
			f.writes = append(f.writes, "patch "+parent)
		}
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"error":{"code":404,"status":"NOT_FOUND","message":"%s"}}`, path)
	}
}

// world is Acme with one Analytics seat at level on property 123.
func identityWorld(t *testing.T, level string, f *analytics, tokens issued) (seataccess.IdentityMaker,
	seataccess.IdentityInput, googlegrowth.SeatAccess, *config.Company) {
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
  google_analytics:
    enabled: true
    oauth: {client_id: 123456789012-abc.apps.googleusercontent.com, client_secret: "${GOOGLE_OAUTH_CLIENT_SECRET}"}
roles:
  - name: Analyst
    handle: analyst
    integrations:
      google_analytics: {enabled: true, level: ` + level + `, properties: ["123"]}
`))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := seataccess.Lookup(googleanalytics.Kind)
	access := *v.(*googlegrowth.SeatAccess)
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	access.HTTP = &http.Client{Transport: httpxtest.Rewrite(t, srv)}
	s, err := access.Open(t.Context(), seataccess.OpenInput{Config: company, Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := seataccess.DesiredOf(seataccess.SeatsOf(company)[0].Role, seataccess.LaneGoogleAnalytics)
	return s.(seataccess.IdentityMaker), seataccess.IdentityInput{Seat: seataccess.SeatRef{Handle: "analyst",
		Name: "Analyst"}, Lane: seataccess.LaneGoogleAnalytics, Want: want}, access, company
}

// A PROPERTY IS REACHED WHERE THE IDENTITY'S ACCOUNT SUMMARIES LIST IT, and
// its role is the admin's read of its binding there and on the account, which
// every property inherits.
//
// Mutation: read the property's binding alone, and a role given on the
// account reads as none.
func TestAnAnalyticsRoleIsReadOnThePropertyAndItsAccount(t *testing.T) {
	t.Parallel()
	f := newAnalytics()
	maker, in, _, _ := identityWorld(t, "operate", f, issued{admin: true})
	check, err := maker.CheckIdentity(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if check.Status != seataccess.IdentityNone || !strings.Contains(check.Said, "123") {
		t.Fatalf("not added: %+v", check)
	}
	f.bind("properties/123", seatEmail, "predefinedRoles/viewer")
	if check, _ = maker.CheckIdentity(t.Context(), in); check.Status != seataccess.IdentityWrongRole ||
		check.Role != "Viewer" || check.Needs != "Editor" {
		t.Fatalf("Viewer on an operate seat: %+v", check)
	}
	f.bind("accounts/9", seatEmail, "predefinedRoles/editor")
	if check, _ = maker.CheckIdentity(t.Context(), in); check.Status != seataccess.IdentityReady {
		t.Errorf("Editor on the account: %+v", check)
	}
}

// ADD IT FOR ME BINDS THE LEVEL'S ROLE where the identity holds none at or
// above it, keeping a data restriction its binding carries, and removing the
// seat deletes the binding.
//
// Mutation: patch the need alone, and the No Cost Metrics restriction is lost.
func TestAddItForMeBindsTheLevelsRoleAndKeepsARestriction(t *testing.T) {
	t.Parallel()
	f := newAnalytics()
	maker, in, _, _ := identityWorld(t, "read", f, issued{admin: true})
	in.Way = seataccess.WayMade
	kept, err := maker.KeepIdentity(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Check.Status != seataccess.IdentityReady || !slices.Equal(f.rolesOn("properties/123"),
		[]string{"predefinedRoles/viewer"}) {
		t.Fatalf("made: %+v, bound %v", kept.Check, f.rolesOn("properties/123"))
	}

	g := newAnalytics()
	g.bind("properties/123", seatEmail, "predefinedRoles/viewer", "predefinedRoles/no-cost-data")
	maker, in, _, _ = identityWorld(t, "operate", g, issued{admin: true})
	in.Way = seataccess.WayMade
	if _, err := maker.KeepIdentity(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if got := g.rolesOn("properties/123"); !slices.Contains(got, "predefinedRoles/editor") ||
		!slices.Contains(got, "predefinedRoles/no-cost-data") || slices.Contains(got, "predefinedRoles/viewer") {
		t.Errorf("patched to %v", got)
	}
	if _, err := maker.DropIdentity(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if got := g.rolesOn("properties/123"); got != nil {
		t.Errorf("the binding stays after the seat is off: %v", got)
	}
}

// A ROLE HELD AT OR ABOVE THE NEED IS LEFT ALONE: Add it for me writes
// nothing where the account already gives the identity Editor.
//
// Mutation: bind every property, and Analytics gets a binding nobody needed.
func TestAddItForMeWritesNothingTheIdentityAlreadyHolds(t *testing.T) {
	t.Parallel()
	f := newAnalytics()
	f.bind("accounts/9", seatEmail, "predefinedRoles/editor")
	maker, in, _, _ := identityWorld(t, "operate", f, issued{admin: true})
	in.Way = seataccess.WayMade
	if _, err := maker.KeepIdentity(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if w := f.wrote(); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
}

// THE ANALYTICS GUIDE names Access management's labels, the property, the
// email to copy and the role.
func TestTheAnalyticsGuideNamesThePropertyAndTheRole(t *testing.T) {
	t.Parallel()
	_, in, access, company := identityWorld(t, "read", newAnalytics(), issued{})
	g := access.IdentityGuide(company, in.Seat, in.Lane, in.Want)
	var text []string
	for _, s := range g.Steps {
		text = append(text, s.Text.String())
	}
	all := strings.Join(text, "\n")
	if err := g.Check(); err != nil {
		t.Errorf("the guide is not of the one form: %v", err)
	}
	for _, want := range []string{"Admin", "property 123", "Property access management", "Add users",
		"Role: Viewer"} {
		if !strings.Contains(all, want) {
			t.Errorf("steps %q do not say %q", all, want)
		}
	}
	if g.Role != "Viewer" || g.Make == nil || g.Make.Unavailable != "" {
		t.Errorf("guide %+v", g)
	}
}
