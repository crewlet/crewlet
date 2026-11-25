package googleanalytics_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleanalytics"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

const (
	seatEmail   = "crewlet-analyst-0a1b2c@acme-crewlet-id.iam.gserviceaccount.com"
	seatToken   = "ya29.seat"
	adminToken  = "ya29.admin"
	readerToken = "ya29.reader"
)

// issued is the engine's tokens: an admin sign-in, the company's reader, and
// a seat identity made at made (zero: long ago), or not made at all.
type issued struct {
	admin, reader, unmade bool
	made                  time.Time
}

func (i issued) SeatToken(context.Context, integration.Kind, string, []string) (seataccess.IssuedSeat, error) {
	if i.unmade {
		return seataccess.IssuedSeat{Account: seatEmail}, seataccess.ErrNotIssued
	}
	return seataccess.IssuedSeat{Token: seatToken, Account: seatEmail, Made: i.made}, nil
}

func (i issued) ReaderToken(context.Context, integration.Kind) (string, error) {
	if !i.reader {
		return "", seataccess.ErrNotIssued
	}
	return readerToken, nil
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
	case path == "/v1beta/accountSummaries" && (bearer == seatToken || bearer == readerToken):
		var summaries []map[string]any
		for _, account := range slices.Sorted(maps.Keys(f.accounts)) {
			var ps []map[string]any
			for _, id := range f.accounts[account] {
				// THE READER IS ADDED AT EACH ACCOUNT: it lists every property.
				if bearer == readerToken || f.reaches(seatEmail, id) {
					ps = append(ps, map[string]any{"property": "properties/" + id, "displayName": "Site " + id})
				}
			}
			summaries = append(summaries, map[string]any{"account": account,
				"displayName": "Account " + strings.TrimPrefix(account, "accounts/"), "propertySummaries": ps})
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
      provisioner: crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com
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

// THE ANALYTICS GUIDE names Access management's labels, the email to copy,
// the box to untick and the role, and lists each property with its own page.
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
	for _, want := range []string{"Admin", "Property access management", "Add users", "Notify new users by email",
		"Role: Viewer"} {
		if !strings.Contains(all, want) {
			t.Errorf("steps %q do not say %q", all, want)
		}
	}
	if g.Role != "Viewer" || g.Make == nil || g.Make.Unavailable != "" {
		t.Errorf("guide %+v", g)
	}
	if len(g.Entries) != 1 || g.Entries[0].ID != "123" ||
		!strings.HasSuffix(g.Entries[0].AddURL, "#/p123/admin/suiteusermanagement/property") {
		t.Errorf("entries %+v", g.Entries)
	}
	if g.Wider == nil || !strings.Contains(g.Wider.Does.String(), "Account access management") {
		t.Errorf("no account-level alternative: %+v", g.Wider)
	}
}

// THE CHOOSER LISTS WHAT THE COMPANY READER SEES, each property by its name
// under its account, and takes a typed ID by the document's own rule; with no
// reader to sign in as, it says why it lists nothing rather than offering a
// typed name.
//
// Mutation: list as the seat's identity, and a seat not yet added sees nothing
// to choose from.
func TestThePropertyChooserListsWhatTheReaderSees(t *testing.T) {
	t.Parallel()
	f := newAnalytics()
	f.accounts["accounts/7"] = []string{"789"}
	_, _, access, company := identityWorld(t, "read", f, issued{reader: true})
	for _, c := range []struct {
		name   string
		tokens issued
		scope  []string
		why    string
	}{
		{"many", issued{reader: true}, []string{"789", "123", "456"}, ""},
		{"no reader", issued{}, nil, "Connect Google Cloud"},
	} {
		s, err := access.Open(t.Context(), seataccess.OpenInput{Config: company, Tokens: c.tokens})
		if err != nil {
			t.Fatal(err)
		}
		opts, err := s.Options(t.Context(), seataccess.LaneGoogleAnalytics)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(opts.Scope, c.scope) || !strings.Contains(opts.ScopeUnlisted, c.why) ||
			(c.why == "") != (opts.ScopeUnlisted == "") || opts.ScopeTyped != config.GoogleAnalyticsPropertyPattern {
			t.Errorf("%s: scope %v unlisted %q typed %q", c.name, opts.Scope, opts.ScopeUnlisted, opts.ScopeTyped)
		}
		if c.why == "" && (opts.ScopeNames["789"] != "Site 789" || opts.ScopeGroups["789"] != "Account 7" ||
			opts.ScopeGroups["123"] != "Account 9" || !opts.ScopeLive) {
			t.Errorf("%s: names %v groups %v live %v", c.name, opts.ScopeNames, opts.ScopeGroups, opts.ScopeLive)
		}
	}
	empty := newAnalytics()
	empty.accounts = map[string][]string{}
	_, _, access, company = identityWorld(t, "read", empty, issued{reader: true})
	s, _ := access.Open(t.Context(), seataccess.OpenInput{Config: company, Tokens: issued{reader: true}})
	if opts, _ := s.Options(t.Context(), seataccess.LaneGoogleAnalytics); len(opts.Scope) != 0 ||
		opts.ScopeUnlisted != "" || !opts.ScopeLive {
		t.Errorf("a reader that sees nothing: %+v", opts)
	}
}

// A ROLE NOBODY CREWLET SIGNS IN AS CAN READ IS NEVER IN SYNC: an operate
// seat whose identity reaches its property, with no Administrator signed in,
// works with its role unchecked and says which role to set; a read seat is
// checked by the listing itself, Viewer being the least role.
//
// Mutation: answer Ready where the role went unread, and the row reads In
// sync for an identity Google Analytics still holds at Viewer.
func TestARoleNobodyCanReadIsNeverInSync(t *testing.T) {
	t.Parallel()
	f := newAnalytics()
	f.bind("properties/123", seatEmail, "predefinedRoles/viewer")
	maker, in, _, _ := identityWorld(t, "operate", f, issued{})
	check, err := maker.CheckIdentity(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if check.Status != seataccess.IdentityUnchecked || !strings.Contains(check.Remedy, "Editor") ||
		!strings.Contains(check.Remedy, "Administrator's sign-in") || len(check.Entries) != 1 ||
		check.Entries[0].State != seataccess.EntryUnchecked {
		t.Errorf("operate, role unread: %+v", check)
	}
	maker, in, _, _ = identityWorld(t, "read", f, issued{})
	if check, _ = maker.CheckIdentity(t.Context(), in); check.Status != seataccess.IdentityReady ||
		check.Entries[0].State != seataccess.EntryAdded {
		t.Errorf("read, listed: %+v", check)
	}
	maker, in, _, _ = identityWorld(t, "operate", f, issued{admin: true})
	if check, _ = maker.CheckIdentity(t.Context(), in); check.Status != seataccess.IdentityWrongRole ||
		check.Entries[0].State != seataccess.EntryWrongRole {
		t.Errorf("operate, the Administrator reads Viewer: %+v", check)
	}
}

// A NEW IDENTITY IS GETTING READY, NEVER THE PERSON'S TO ADD YET: one not
// made, or made less than two minutes ago and reached nowhere, waits on
// Google, with when it can be added; one made earlier is the person's to add.
//
// Mutation: drop the settling window, and a person is sent to add an identity
// Google Analytics answers "This email doesn't match a Google Account".
func TestANewIdentityIsGettingReadyNotThePersonsToAdd(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for _, c := range []struct {
		name   string
		tokens issued
		want   seataccess.IdentityStatus
		ready  bool
	}{
		{"not made", issued{unmade: true}, seataccess.IdentityPreparing, false},
		{"made just now", issued{made: now.Add(-30 * time.Second)}, seataccess.IdentityPreparing, true},
		{"made earlier", issued{made: now.Add(-3 * time.Minute)}, seataccess.IdentityNone, false},
	} {
		maker, in, _, _ := identityWorld(t, "read", newAnalytics(), c.tokens)
		check, err := maker.CheckIdentity(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if check.Status != c.want || check.ReadyAt.IsZero() == c.ready {
			t.Errorf("%s: %s ready at %v", c.name, check.Status, check.ReadyAt)
		}
		if c.ready && !check.ReadyAt.Equal(c.tokens.made.Add(googlegrowth.IdentitySettles).UTC()) {
			t.Errorf("%s: ready at %v", c.name, check.ReadyAt)
		}
	}
}

// EACH PROPERTY STILL MISSING THE IDENTITY IS LISTED WITH ITS OWN PAGE: named
// and placed under its account from the reader's listing, so its link opens
// that property's access management, and its account's for one add there.
//
// Mutation: name entries from the identity's reads alone, and a property it
// is not added to has no name and no account in its link.
func TestEachMissingPropertyIsListedWithItsAccessPage(t *testing.T) {
	t.Parallel()
	f := newAnalytics()
	f.bind("properties/123", seatEmail, "predefinedRoles/viewer")
	maker, in, _, _ := identityWorld(t, "read", f, issued{reader: true})
	in.Want.Scope = []string{"123", "456"}
	check, err := maker.CheckIdentity(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Entries) != 2 {
		t.Fatalf("entries %+v", check.Entries)
	}
	added, missing := check.Entries[0], check.Entries[1]
	if added.State != seataccess.EntryAdded || missing.State != seataccess.EntryMissing ||
		missing.Name != "Site 456" || missing.Group != "Account 9" ||
		!strings.HasSuffix(missing.AddURL, "#/a9p456/admin/suiteusermanagement/property") ||
		!strings.HasSuffix(missing.GroupURL, "#/a9p456/admin/suiteusermanagement/account") {
		t.Errorf("added %+v missing %+v", added, missing)
	}
}
