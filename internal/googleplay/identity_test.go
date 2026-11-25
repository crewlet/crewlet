package googleplay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/googleplay"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

const (
	seatEmail  = "crewlet-mobile-lead-0a1b2c@acme-crewlet-id.iam.gserviceaccount.com"
	seatToken  = "ya29.seat"
	adminToken = "ya29.admin"
)

// issued is the engine's tokens: the seat's identity, and the admin sign-in
// where somebody signed in.
type issued struct{ admin bool }

func (i issued) SeatToken(context.Context, integration.Kind, string, []string) (seataccess.IssuedSeat, error) {
	return seataccess.IssuedSeat{Token: seatToken, Account: seatEmail}, nil
}

func (i issued) ReaderToken(context.Context, integration.Kind) (string, error) {
	return "", seataccess.ErrNotIssued
}

func (i issued) AdminToken(context.Context, integration.Kind) (string, error) {
	if !i.admin {
		return "", seataccess.ErrNotIssued
	}
	return adminToken, nil
}

// playUsers is Play Console's users and grants: which developer account
// holds which app, each account's users, and the apps Play has not applied a
// grant to yet.
type playUsers struct {
	mu       sync.Mutex
	holds    map[string][]string
	users    map[string]map[string]map[string][]string // dev, email, app: permissions
	applying map[string]bool
	calls    []string
	// narrow is an admin Play lets list no users; expired an email whose access
	// expired, which a create refuses as existing.
	narrow  bool
	expired map[string]bool
}

func newPlayUsers() *playUsers {
	return &playUsers{holds: map[string][]string{"1234567890": {"com.acme.app"}},
		users: map[string]map[string]map[string][]string{}, applying: map[string]bool{}, expired: map[string]bool{}}
}

// grant gives email perms on app in dev, as a person inviting it would.
func (f *playUsers) grant(dev, email, app string, perms ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.users[dev] == nil {
		f.users[dev] = map[string]map[string][]string{}
	}
	if f.users[dev][email] == nil {
		f.users[dev][email] = map[string][]string{}
	}
	f.users[dev][email][app] = perms
}

func (f *playUsers) reaches(email, app string) bool {
	for _, users := range f.users {
		if _, ok := users[email][app]; ok && !f.applying[app] {
			return true
		}
	}
	return false
}

func refuse(w http.ResponseWriter, code int, status string) {
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"status":%q,"message":"refused"}}`, code, status)
}

// perms is what email holds on app in dev, and has whether it is a user there.
func (f *playUsers) perms(dev, email, app string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.users[dev][email][app])
}

func (f *playUsers) has(dev, email string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.users[dev][email]
	return ok
}

// sawUsers reports whether anything reached Play's users and grants.
func (f *playUsers) sawUsers() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, "/developers/") })
}

func (f *playUsers) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/androidpublisher/v3/"), "/")
	switch {
	case len(parts) == 3 && parts[0] == "applications" && parts[2] == "deviceTierConfigs" && r.Method == http.MethodGet:
		if bearer != seatToken || !f.reaches(seatEmail, parts[1]) {
			refuse(w, 403, "PERMISSION_DENIED")
			return
		}
		_, _ = w.Write([]byte(`{"deviceTierConfigs":[]}`))
	case parts[0] == "developers" && (bearer != adminToken || f.narrow):
		refuse(w, 403, "PERMISSION_DENIED")
	case len(parts) == 3 && r.Method == http.MethodGet:
		var users []map[string]any
		for email, apps := range f.users[parts[1]] {
			var grants []map[string]any
			for app, perms := range apps {
				grants = append(grants, map[string]any{"packageName": app, "appLevelPermissions": perms,
					"name": "developers/" + parts[1] + "/users/" + email + "/grants/" + app})
			}
			state := "ACCESS_GRANTED"
			if f.expired[email] {
				state = "ACCESS_EXPIRED"
			}
			users = append(users, map[string]any{"email": email, "accessState": state, "grants": grants})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"users": users})
	case len(parts) == 3 && r.Method == http.MethodPost:
		var u struct{ Email string }
		_ = json.NewDecoder(r.Body).Decode(&u)
		if _, exists := f.users[parts[1]][u.Email]; exists {
			refuse(w, 409, "ALREADY_EXISTS")
			return
		}
		if f.users[parts[1]] == nil {
			f.users[parts[1]] = map[string]map[string][]string{}
		}
		f.users[parts[1]][u.Email] = map[string][]string{}
		_, _ = w.Write([]byte(`{"email":"` + u.Email + `"}`))
	case len(parts) == 4 && r.Method == http.MethodDelete:
		delete(f.users[parts[1]], parts[3])
		_, _ = w.Write([]byte(`{}`))
	case len(parts) == 5 && r.Method == http.MethodPost:
		var g struct {
			PackageName         string
			AppLevelPermissions []string
		}
		_ = json.NewDecoder(r.Body).Decode(&g)
		if !slices.Contains(f.holds[parts[1]], g.PackageName) {
			refuse(w, 404, "NOT_FOUND")
			return
		}
		f.users[parts[1]][parts[3]][g.PackageName] = g.AppLevelPermissions
		delete(f.expired, parts[3])
		_, _ = w.Write([]byte(`{}`))
	case len(parts) == 6 && r.Method == http.MethodPatch:
		if r.URL.Query().Get("updateMask") != "appLevelPermissions" {
			refuse(w, 400, "INVALID_ARGUMENT")
			return
		}
		var g struct{ AppLevelPermissions []string }
		_ = json.NewDecoder(r.Body).Decode(&g)
		f.users[parts[1]][parts[3]][parts[5]] = g.AppLevelPermissions
		_, _ = w.Write([]byte(`{}`))
	default:
		refuse(w, 404, "NOT_FOUND")
	}
}

// identityWorld is Acme with one Play seat at level, its admin sign-in
// configured, and Play Console as f holds it.
func identityWorld(t *testing.T, level string, f *playUsers, devs ...string) (*config.Company, googlegrowth.SeatAccess) {
	t.Helper()
	if len(devs) == 0 {
		devs = []string{"1234567890"}
	}
	quoted := make([]string, len(devs))
	for i, d := range devs {
		quoted[i] = `"` + d + `"`
	}
	company, err := config.ParseCompanyDocument([]byte(`
name: Acme
integrations:
  google_cloud:
    enabled: true
    provisioning:
      provisioner: crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com
      provider: projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine
      federation_key: "${GOOGLE_CLOUD_FEDERATION_KEY}"
  google_play:
    enabled: true
    apps: ["com.acme.app"]
    developer_accounts: [` + strings.Join(quoted, ", ") + `]
    oauth: {client_id: 123456789012-abc.apps.googleusercontent.com, client_secret: "${GOOGLE_OAUTH_CLIENT_SECRET}"}
roles:
  - name: Mobile lead
    handle: mobile-lead
    integrations:
      google_play: {enabled: true, level: ` + level + `, apps: ["com.acme.app"]}
`))
	if err != nil {
		t.Fatal(err)
	}
	v, ok := seataccess.Lookup(googleplay.Kind)
	if !ok {
		t.Fatal("Google Play registers no seat access")
	}
	access := v.(googlegrowth.SeatAccess)
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	access.HTTP = &http.Client{Transport: httpxtest.Rewrite(t, srv)}
	return company, access
}

// session opens the seat's lane with the engine's tokens.
func session(t *testing.T, company *config.Company, access googlegrowth.SeatAccess, tokens issued) (
	seataccess.IdentityMaker, seataccess.IdentityInput) {
	t.Helper()
	s, err := access.Open(t.Context(), seataccess.OpenInput{Config: company, Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := seataccess.DesiredOf(seataccess.SeatsOf(company)[0].Role, seataccess.LaneGooglePlay)
	return s.(seataccess.IdentityMaker), seataccess.IdentityInput{Seat: seataccess.SeatRef{Handle: "mobile-lead",
		Name: "Mobile lead"}, Lane: seataccess.LaneGooglePlay, Want: want}
}

// A PLAY SEAT NEEDS ITS PERSON UNTIL ITS IDENTITY REACHES EVERY APP: not
// invited, the check says so and names the app; invited, it works with its
// role unchecked where nobody signed in as a Play Console admin.
//
// Mutation: answer ready where an app refuses the identity, and the first
// check reads ready.
func TestAPlaySeatNeedsYouUntilItsIdentityIsInvitedToItsApps(t *testing.T) {
	t.Parallel()
	f := newPlayUsers()
	company, access := identityWorld(t, "read", f)
	maker, in := session(t, company, access, issued{})
	check, err := maker.CheckIdentity(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if check.Status != seataccess.IdentityNone || !strings.Contains(check.Said, "com.acme.app") ||
		!strings.Contains(check.Said, "not added") || check.ID != seatEmail {
		t.Fatalf("not invited: %+v", check)
	}
	f.grant("1234567890", seatEmail, "com.acme.app", "CAN_VIEW_NON_FINANCIAL_DATA")
	if check, err = maker.CheckIdentity(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if check.Status != seataccess.IdentityUnchecked || check.Role != "" || !strings.Contains(check.Said, "a Play Console admin") {
		t.Errorf("invited, no admin: %+v", check)
	}
}

// THE ADMIN READS THE ROLE: a seat holding less than its level needs is the
// wrong role, naming what it holds and needs; Admin holds every permission,
// ready with a warning below administer and without one at it.
//
// Mutation: compare held against need's first permission alone, and the
// operate seat holding only View app information reads ready.
func TestThePlayAdminReadsTheRoleAndAShortOneIsTheWrongRole(t *testing.T) {
	t.Parallel()
	f := newPlayUsers()
	f.grant("1234567890", seatEmail, "com.acme.app", "CAN_VIEW_NON_FINANCIAL_DATA", "CAN_REPLY_TO_REVIEWS")
	company, access := identityWorld(t, "operate", f)
	maker, in := session(t, company, access, issued{admin: true})
	check, err := maker.CheckIdentity(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if check.Status != seataccess.IdentityWrongRole || !strings.Contains(check.Role, "Reply to reviews") ||
		!strings.Contains(check.Needs, "Manage store presence") || !strings.Contains(check.Remedy, "Give it") {
		t.Fatalf("short of operate: %+v", check)
	}
	f.grant("1234567890", seatEmail, "com.acme.app", "CAN_MANAGE_PERMISSIONS")
	if check, _ = maker.CheckIdentity(t.Context(), in); check.Status != seataccess.IdentityReady || !check.Broader ||
		!strings.Contains(check.Said, "more than its level needs") {
		t.Errorf("Admin on an operate seat: %+v", check)
	}

	adminCompany, adminAccess := identityWorld(t, "administer", f)
	maker, in = session(t, adminCompany, adminAccess, issued{admin: true})
	if check, _ = maker.CheckIdentity(t.Context(), in); check.Status != seataccess.IdentityReady || check.Broader {
		t.Errorf("Admin on an administer seat: %+v", check)
	}
}

// ADD IT FOR ME INVITES EXACTLY THE LEVEL'S PERMISSIONS, keeping any the
// identity holds beyond them, and the check that follows reads it ready.
//
// Mutation: send the need alone on a grant that exists, and the financial
// permission it held is dropped.
func TestAddItForMeInvitesTheLevelsPermissionsAndKeepsMore(t *testing.T) {
	t.Parallel()
	f := newPlayUsers()
	company, access := identityWorld(t, "operate", f)
	maker, in := session(t, company, access, issued{admin: true})
	in.Way = seataccess.WayMade
	kept, err := maker.KeepIdentity(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Check.Status != seataccess.IdentityReady || kept.Way != seataccess.WayMade || kept.ID != seatEmail {
		t.Fatalf("made: %+v", kept)
	}
	got := f.perms("1234567890", seatEmail, "com.acme.app")
	for _, p := range []string{"CAN_VIEW_NON_FINANCIAL_DATA", "CAN_REPLY_TO_REVIEWS", "CAN_MANAGE_PUBLIC_LISTING",
		"CAN_MANAGE_TRACK_APKS", "CAN_MANAGE_TRACK_USERS", "CAN_MANAGE_PUBLIC_APKS"} {
		if !slices.Contains(got, p) {
			t.Errorf("granted %v, missing %s", got, p)
		}
	}
	if slices.Contains(got, "CAN_MANAGE_PERMISSIONS") || slices.Contains(got, "CAN_MANAGE_ORDERS") {
		t.Errorf("granted %v, more than operate needs", got)
	}

	g := newPlayUsers()
	g.grant("1234567890", seatEmail, "com.acme.app", "CAN_VIEW_FINANCIAL_DATA")
	company, access = identityWorld(t, "read", g)
	maker, in = session(t, company, access, issued{admin: true})
	in.Way = seataccess.WayMade
	if _, err := maker.KeepIdentity(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if got := g.perms("1234567890", seatEmail, "com.acme.app"); !slices.Contains(got, "CAN_VIEW_FINANCIAL_DATA") ||
		!slices.Contains(got, "CAN_VIEW_NON_FINANCIAL_DATA") {
		t.Errorf("a grant it held became %v", got)
	}
}

// ADD IT FOR ME FINDS THE ACCOUNT HOLDING THE APP, and a user it made in an
// account holding none of the seat's apps is removed again.
//
// Mutation: skip the removal, and the first account keeps a stray user.
func TestAddItForMeLeavesNoStrayUserInAnotherAccount(t *testing.T) {
	t.Parallel()
	f := newPlayUsers()
	f.holds = map[string][]string{"1111111111": {"com.acme.games"}, "2222222222": {"com.acme.app"}}
	company, access := identityWorld(t, "read", f, "1111111111", "2222222222")
	maker, in := session(t, company, access, issued{admin: true})
	in.Way = seataccess.WayMade
	if _, err := maker.KeepIdentity(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if f.has("1111111111", seatEmail) {
		t.Error("a user is left in the account that holds none of the seat's apps")
	}
	if got := f.perms("2222222222", seatEmail, "com.acme.app"); !slices.Contains(got, "CAN_VIEW_NON_FINANCIAL_DATA") {
		t.Errorf("the account holding the app granted %v", got)
	}
}

// PLAY STILL APPLYING A GRANT IS NOT A PERSON'S MISSING STEP: the admin shows
// the identity added though Play does not let it in yet, and the check says so.
//
// Mutation: read every unreached app as not added, and the person is told to
// add what is added.
func TestAGrantPlayIsStillApplyingIsSaidSo(t *testing.T) {
	t.Parallel()
	f := newPlayUsers()
	f.grant("1234567890", seatEmail, "com.acme.app", "CAN_VIEW_NON_FINANCIAL_DATA")
	f.applying["com.acme.app"] = true
	company, access := identityWorld(t, "read", f)
	maker, in := session(t, company, access, issued{admin: true})
	check, err := maker.CheckIdentity(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if check.Status != seataccess.IdentityNone || !strings.Contains(check.Said, "does not let it in yet") ||
		strings.Contains(check.Said, "not added") || check.Remedy != "" {
		t.Errorf("applying: %+v", check)
	}
}

// WITHOUT AN ADMIN SIGN-IN, ADD IT FOR ME IS A PERSON'S STEP, refused before
// Play with where to sign in, and the guide says so before anybody presses it.
//
// Mutation: treat a missing grant as a fault, and the keep errors.
func TestWithoutAnAdminSignInAddItForMeIsAPersonsStep(t *testing.T) {
	t.Parallel()
	f := newPlayUsers()
	company, access := identityWorld(t, "read", f)
	maker, in := session(t, company, access, issued{})
	in.Way = seataccess.WayMade
	_, err := maker.KeepIdentity(t.Context(), in)
	r, ok := errors.AsType[*seataccess.Refusal](err)
	if !ok || !strings.Contains(r.Said, "a Play Console admin") || r.Actor != seataccess.ByYou {
		t.Fatalf("keep without an admin: %v", err)
	}
	if f.sawUsers() {
		t.Error("Play's users were reached without an admin")
	}

	company.Integrations.GooglePlay.OAuth = nil
	g := access.IdentityGuide(company, in.Seat, in.Lane, in.Want)
	if g.Make == nil || !strings.Contains(g.Make.Unavailable, "signs in on the Settings tab") {
		t.Errorf("guide without an OAuth client: %+v", g.Make)
	}
}

// TURNING THE SEAT OFF TAKES ITS IDENTITY OUT OF PLAY where an admin signed
// in; without one, nothing is asked of Play.
//
// Mutation: drop nothing, and the user stays in Play Console.
func TestTurningTheSeatOffRemovesItsPlayUser(t *testing.T) {
	t.Parallel()
	f := newPlayUsers()
	f.grant("1234567890", seatEmail, "com.acme.app", "CAN_VIEW_NON_FINANCIAL_DATA")
	company, access := identityWorld(t, "read", f)
	maker, in := session(t, company, access, issued{})
	if _, err := maker.DropIdentity(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if !f.has("1234567890", seatEmail) {
		t.Fatal("removed without an admin sign-in")
	}
	maker, in = session(t, company, access, issued{admin: true})
	if _, err := maker.DropIdentity(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if f.has("1234567890", seatEmail) {
		t.Error("the identity is still a Play Console user")
	}
}

// THE GUIDE IS PLAY CONSOLE'S OWN WORDS: its page, the identity's email to
// copy, the seat's apps and the permissions its level needs, by their labels.
//
// Mutation: name the catalog's short label for View app information, and the
// step no longer matches the box Play Console shows.
func TestThePlayGuideNamesTheEmailTheAppsAndThePermissions(t *testing.T) {
	t.Parallel()
	company, access := identityWorld(t, "read", newPlayUsers())
	_, in := session(t, company, access, issued{})
	g := access.IdentityGuide(company, in.Seat, in.Lane, in.Want)
	var text []string
	var copied string
	for _, s := range g.Steps {
		text = append(text, s.Text.String())
		if s.Copy != nil {
			copied = s.Copy.Value
		}
	}
	all := strings.Join(text, "\n")
	for _, want := range []string{"Users and permissions", "Invite new users", "com.acme.app",
		"View app information (read-only)", "Invite user"} {
		if !strings.Contains(all, want) {
			t.Errorf("steps %q do not say %q", all, want)
		}
	}
	if !strings.HasPrefix(copied, "crewlet-mobile-lead-") || !strings.HasSuffix(copied,
		"@acme-crewlet-id.iam.gserviceaccount.com") || g.Name != copied {
		t.Errorf("email to copy %q, name %q", copied, g.Name)
	}
	if err := g.Check(); err != nil {
		t.Errorf("the guide is not of the one form: %v", err)
	}
	if first := g.Steps[0].Text.URLs(); g.Link != nil || len(first) != 1 ||
		first[0] != "https://play.google.com/console/developers/users-and-permissions" || g.Make == nil ||
		g.Make.Unavailable != "" || g.Back.Kind != seataccess.BackNothing {
		t.Errorf("guide %+v", g)
	}
}

// AN ADMIN WHO SEES TOO LITTLE LEAVES THE ROLE UNREAD, never the check
// failed: a sign-in Play lets list no users reads the identity as reaching
// its apps with its role unchecked, never ready.
//
// Mutation: hand the admin read's 403 back as the check's fault, and every
// pass fails to read the identity.
func TestAnAdminWhoSeesTooLittleLeavesThePlayRoleUnread(t *testing.T) {
	t.Parallel()
	f := newPlayUsers()
	f.grant("1234567890", seatEmail, "com.acme.app", "CAN_VIEW_NON_FINANCIAL_DATA")
	f.narrow = true
	company, access := identityWorld(t, "read", f)
	maker, in := session(t, company, access, issued{admin: true})
	check, err := maker.CheckIdentity(t.Context(), in)
	if err != nil || check.Status != seataccess.IdentityUnchecked || check.Role != "" {
		t.Errorf("a narrow admin: %+v, %v", check, err)
	}
}

// AN IDENTITY WHOSE PLAY ACCESS EXPIRED IS GRANTED AGAIN: Play refuses to
// create a user that exists, so Add it for me grants the one it has.
//
// Mutation: hand the create's 409 back, and the person is told to sign in again.
func TestAddItForMeGrantsAnExpiredUserAgain(t *testing.T) {
	t.Parallel()
	f := newPlayUsers()
	f.grant("1234567890", seatEmail, "com.acme.other")
	f.expired[seatEmail] = true
	company, access := identityWorld(t, "read", f)
	maker, in := session(t, company, access, issued{admin: true})
	in.Way = seataccess.WayMade
	kept, err := maker.KeepIdentity(t.Context(), in)
	if err != nil || kept.Check.Status != seataccess.IdentityReady {
		t.Errorf("an expired user: %+v, %v", kept.Check, err)
	}
}
