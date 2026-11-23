package appstoreconnect_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// apple answers as App Store Connect does by each key's role: a key it does
// not know is 401, a read the key's role may not make 403, as the operation
// table's roles say.
type apple struct {
	mu    sync.Mutex
	roles map[string]string
	calls []string
}

func (a *apple) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	kid := kidOf(r)
	a.calls = append(a.calls, r.Method+" "+r.URL.Path+" "+kid)
	role, ok := a.roles[kid]
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errors": [{"status": "401", "code": "NOT_AUTHORIZED", "title": "no"}]}`)
		return
	}
	if r.URL.Path == "/v1/apps" {
		_, _ = io.WriteString(w, `{"data": [{"type": "apps", "id": "111", "attributes": {"name": "Notes"}}]}`)
		return
	}
	for _, op := range appstoreconnect.Catalog().Operations {
		if op.Method != r.Method || !templateOf(op.Path).MatchString(r.URL.Path) {
			continue
		}
		if !growth.Permits(op, []string{role}) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"errors": [{"status": "403", "code": "FORBIDDEN_ERROR", "title": "no"}]}`)
			return
		}
		if op.ID == "financeReports_getCollection" {
			// A READ THE ROLE MAY MAKE, refused for its missing filters.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"errors": [{"status": "400", "code": "PARAMETER_ERROR.REQUIRED", "title": "filter"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data": []}`)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"errors": [{"status": "404", "code": "NOT_FOUND", "title": "no"}]}`)
}

func templateOf(path string) *regexp.Regexp {
	return regexp.MustCompile("^" + regexp.MustCompile(`\\\{[A-Za-z]+\\\}`).ReplaceAllString(regexp.QuoteMeta(path),
		`[^/]+`) + "$")
}

// identityRig is a product over one team whose company key is Marketing, the
// agent seat growth at level, and the access session that keeps its keys.
type identityRig struct {
	apple   *apple
	product *appstoreconnect.Product
	session seataccess.IdentityMaker
	sealed  map[string]string
	company *config.Company
}

func newIdentityRig(t *testing.T, level string) *identityRig {
	t.Helper()
	a := &apple{roles: map[string]string{"2X9R4HXF34": appstoreconnect.RoleMarketing}}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	on := true
	companyKey := p8(t)
	rig := &identityRig{apple: a, sealed: map[string]string{"${K}": companyKey}}
	rig.company = &config.Company{Name: "Example",
		Integrations: config.Integrations{AppStoreConnect: &config.AppStoreConnect{Enabled: true,
			Connections: map[string]config.AppStoreConnectTeam{"main": {IssuerID: "57246542-96fe-1a63-e053-0824d011072a",
				Keys: config.AppStoreConnectKeys{
					Marketing: &config.AppStoreConnectKey{KeyID: "2X9R4HXF34", PrivateKey: "${K}"}}}}}},
		Roles: []config.Role{{Name: "Growth Marketer", Handle: "growth", Integrations: config.RoleIntegrations{
			AppStoreConnect: &config.RoleAppStoreConnect{Enabled: &on, Level: level, Apps: []string{"111"},
				Identity: "own"}}}},
	}
	var mu sync.Mutex
	rig.product = appstoreconnect.New(appstoreconnect.Deps{
		Company: func() *config.Company { return rig.company },
		Resolve: func(v string) string {
			mu.Lock()
			defer mu.Unlock()
			if got, ok := rig.sealed[v]; ok {
				return got
			}
			if strings.HasPrefix(v, "${") {
				return ""
			}
			return v
		},
		HTTP: srv.Client(), Base: srv.URL, Audit: &audit{t: t}, Leases: &leases{},
		Now: func() time.Time { return time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC) },
	})
	vendor, ok := seataccess.Lookup(integration.KindAppStoreConnect)
	if !ok {
		t.Fatal("App Store Connect registers no access vendor")
	}
	sess, err := vendor.Open(t.Context(), seataccess.OpenInput{Config: rig.company,
		Reads: rig.product.SeatReads(t.Context(), true)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	maker, ok := sess.(seataccess.IdentityMaker)
	if !ok {
		t.Fatal("the App Store Connect session keeps no identity")
	}
	rig.session = maker
	return rig
}

func (rig *identityRig) keep(t *testing.T, value, file string, records map[string]seataccess.Record,
	level string) (seataccess.IdentityKept, error) {
	t.Helper()
	return rig.session.KeepIdentity(t.Context(), seataccess.IdentityInput{
		Seat: seataccess.SeatRef{Handle: "growth", Name: "Growth Marketer"}, Lane: seataccess.LaneAppStoreConnect,
		Want: seataccess.Desired{Enabled: true, Level: level, Scope: []string{"111"}}, Records: records,
		Way: seataccess.WayHandedIn, Value: value, FileName: file,
		Sinks: map[string]string{"KEY": "APP_STORE_CONNECT_KEY_GROWTH",
			"RETIRING_KEY": "APP_STORE_CONNECT_RETIRING_KEY_GROWTH"},
	})
}

// A HANDED-IN KEY IS KEPT ONLY AT THE ROLE ITS LEVEL NEEDS, IN THE COMPANY'S
// TEAM: Crewlet reads its role from Apple's answers alone, refuses Admin for any
// agent, warns where a role reaches more than the level needs, and names
// another team's (or a revoked) key as the wrong team.
//
// Mutation: drop the Admin case, and an agent signs with a key that manages
// the team's users; treat a 400 as a refusal, and every Finance key reads as
// another role.
func TestAHandedInKeyIsCheckedBeforeItIsKept(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, role, level string
		status            seataccess.IdentityStatus
		broader           bool
	}{
		{"Marketing for read", appstoreconnect.RoleMarketing, "read", seataccess.IdentityReady, false},
		{"App Manager for operate", appstoreconnect.RoleAppManager, "operate", seataccess.IdentityReady, false},
		{"App Manager for read", appstoreconnect.RoleAppManager, "read", seataccess.IdentityReady, true},
		{"Admin, never", appstoreconnect.RoleAdmin, "read", seataccess.IdentityWrongRole, false},
		{"Marketing for operate", appstoreconnect.RoleMarketing, "operate", seataccess.IdentityWrongRole, false},
		{"Finance for read", appstoreconnect.RoleFinance, "read", seataccess.IdentityWrongRole, false},
		{"another team's", "", "read", seataccess.IdentityWrongTeam, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rig := newIdentityRig(t, c.level)
			if c.role != "" {
				rig.apple.roles["7KQ4M2ZX9P"] = c.role
			}
			kept, err := rig.keep(t, p8(t), "AuthKey_7KQ4M2ZX9P.p8", nil, c.level)
			if err != nil {
				t.Fatalf("KeepIdentity: %v", err)
			}
			if kept.Check.Status != c.status || kept.Check.Broader != c.broader {
				t.Fatalf("%s: status %s broader %v (%s), want %s broader %v", c.name, kept.Check.Status,
					kept.Check.Broader, kept.Check.Said, c.status, c.broader)
			}
			if c.status == seataccess.IdentityReady {
				if kept.ID != "7KQ4M2ZX9P" || kept.Name != "crewlet-growth" || kept.Check.Team != "main" {
					t.Fatalf("kept %+v, want key 7KQ4M2ZX9P named crewlet-growth in main", kept)
				}
				if c.role != "" && kept.Check.Role != map[string]string{appstoreconnect.RoleMarketing: "Marketing",
					appstoreconnect.RoleAppManager: "App Manager"}[c.role] {
					t.Fatalf("role read back %q", kept.Check.Role)
				}
			}
		})
	}
}

// A KEY IS ONE AGENT'S: one another agent keeps, a company key, a file that is
// no .p8 key, or one with no key ID is refused before Apple is asked.
func TestAKeyIsOneAgentsAlone(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t, "read")
	rig.apple.roles["7KQ4M2ZX9P"] = appstoreconnect.RoleMarketing
	other := map[string]seataccess.Record{"qa": {Handle: "qa", Lanes: map[seataccess.Lane]seataccess.LaneRecord{
		seataccess.LaneAppStoreConnect: {Identity: &seataccess.IdentityRecord{ID: "7KQ4M2ZX9P"}}}}}
	for name, c := range map[string]struct {
		value, file string
		records     map[string]seataccess.Record
	}{
		"another agent's": {p8(t), "AuthKey_7KQ4M2ZX9P.p8", other},
		"the company's":   {p8(t), "AuthKey_2X9R4HXF34.p8", nil},
		"not a key":       {"hello", "AuthKey_7KQ4M2ZX9P.p8", nil},
		"no key ID":       {p8(t), "key.p8", nil},
	} {
		before := len(rig.apple.calls)
		_, err := rig.keep(t, c.value, c.file, c.records, "read")
		if _, ok := errors.AsType[*seataccess.Refusal](err); !ok {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
		if len(rig.apple.calls) != before {
			t.Errorf("%s: Apple was asked about a key Crewlet refuses", name)
		}
	}
}

// A PASS FINDS A REVOKED KEY REFUSED, and a replaced one gone once Apple
// refuses it, so the box stops asking for its revoke.
func TestAPassFindsARevokedKeyAndAReplacedOneGone(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t, "read")
	rig.apple.roles["7KQ4M2ZX9P"] = appstoreconnect.RoleMarketing
	rig.sealed["${APP_STORE_CONNECT_KEY_GROWTH}"] = p8(t)
	rig.sealed["${APP_STORE_CONNECT_RETIRING_KEY_OLDKEY0001_GROWTH}"] = p8(t)
	in := seataccess.IdentityInput{Seat: seataccess.SeatRef{Handle: "growth"}, Lane: seataccess.LaneAppStoreConnect,
		Want: seataccess.Desired{Enabled: true, Level: "read", Scope: []string{"111"}},
		Record: &seataccess.IdentityRecord{ID: "7KQ4M2ZX9P", Check: &seataccess.IdentityCheck{
			Status: seataccess.IdentityReady, Team: "main"}, Retiring: []seataccess.RetiringIdentity{{ID: "OLDKEY0001"}}},
		Sinks: map[string]string{"KEY": "APP_STORE_CONNECT_KEY_GROWTH",
			"RETIRING_KEY_OLDKEY0001": "APP_STORE_CONNECT_RETIRING_KEY_OLDKEY0001_GROWTH"}}
	check, err := rig.session.CheckIdentity(t.Context(), in)
	if err != nil || check.Status != seataccess.IdentityReady || len(check.RetiringGone) != 1 {
		t.Fatalf("a working key with its old one revoked: %+v, %v", check, err)
	}
	rig.apple.roles["OLDKEY0001"] = appstoreconnect.RoleMarketing
	delete(rig.apple.roles, "7KQ4M2ZX9P")
	check, err = rig.session.CheckIdentity(t.Context(), in)
	if err != nil || check.Status != seataccess.IdentityRefused || len(check.RetiringGone) != 0 {
		t.Fatalf("a revoked key with its old one still live: %+v, %v", check, err)
	}
}

// ONLY A PERSON REVOKES A TEAM KEY: giving one up keeps it sealed as retiring
// and asks for the revoke at Apple's Team Keys page.
func TestGivingAKeyUpAsksForItsRevoke(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t, "read")
	rig.sealed["${APP_STORE_CONNECT_KEY_GROWTH}"] = "the sealed key"
	dropped, err := rig.session.DropIdentity(context.Background(), seataccess.IdentityInput{
		Seat: seataccess.SeatRef{Handle: "growth"}, Record: &seataccess.IdentityRecord{ID: "7KQ4M2ZX9P"},
		Sinks: map[string]string{"KEY": "APP_STORE_CONNECT_KEY_GROWTH"}})
	if err != nil || dropped.Waiting == nil || dropped.Waiting.ActionURL != appstoreconnect.TeamKeysURL ||
		!strings.Contains(dropped.Waiting.Detail, "7KQ4M2ZX9P") {
		t.Fatalf("drop: %+v, %v", dropped, err)
	}
	if len(dropped.Minted) != 1 || dropped.Minted[0].Field != "RETIRING_KEY_7KQ4M2ZX9P" || dropped.Minted[0].Value != "the sealed key" {
		t.Fatalf("the given-up key is not kept as retiring: %+v", dropped.Minted)
	}
}

// The sheet links Team Keys on the first step, which names it, and draws no
// separate button for it.
func TestTheSheetLinksTeamKeysOnTheStepThatNamesIt(t *testing.T) {
	t.Parallel()
	v, ok := seataccess.Lookup(integration.KindAppStoreConnect)
	if !ok {
		t.Fatal("App Store Connect registers no access vendor")
	}
	guider, ok := v.(seataccess.IdentityGuider)
	if !ok {
		t.Fatal("App Store Connect words no set-up sheet")
	}
	g := guider.IdentityGuide(&config.Company{Name: "Example Co"}, seataccess.SeatRef{Handle: "growth"},
		seataccess.LaneAppStoreConnect, seataccess.Desired{Enabled: true})
	if err := g.Check(); err != nil {
		t.Fatalf("the guide is not of the one form: %v", err)
	}
	first := g.Steps[0].Text
	if g.Link != nil || len(first.URLs()) != 1 || first.URLs()[0] != appstoreconnect.TeamKeysURL ||
		first.String() != "Open Users and Access > Integrations > Team Keys." {
		t.Errorf("step 1 is %q linking %v, and the button is %+v", first.String(), first.URLs(), g.Link)
	}
}
