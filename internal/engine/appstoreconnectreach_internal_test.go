package engine

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
	"github.com/crewlet/crewlet/internal/setup"
)

// ascTeam is a fake App Store Connect team with two apps, counting each full
// listing of them.
type ascTeam struct {
	mu    sync.Mutex
	lists int
}

func (a *ascTeam) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/v1/apps" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("limit") != "1" {
		a.mu.Lock()
		a.lists++
		a.mu.Unlock()
	}
	_, _ = io.WriteString(w, `{"data": [
		{"type": "apps", "id": "222", "attributes": {"name": "Trail Maps", "bundleId": "com.example.maps"}},
		{"type": "apps", "id": "111", "attributes": {"name": "Field Notes", "bundleId": "com.example.notes"}}]}`)
}

func (a *ascTeam) listed() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lists
}

// ascDoc is a company connecting one team by its issuer, whose Growth agent is
// on with app 111.
const ascDoc = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {"app_store_connect": {"enabled": true, "connections": {"main": {
    "issuer_id": "57246542-96fe-1a63-e053-0824d011072a"}}}},
  "seat_access_migrated": ["app_store_connect"],
  "roles": [{"name": "Growth", "handle": "growth", "llm": "zulu",
    "integrations": {"app_store_connect": {"enabled": true, "level": "read", "apps": ["111"]}}}]
}`

// ascEngine is an engine running ascDoc whose App Store Connect product calls
// a fake team, its Growth agent holding its own Marketing key there.
func ascEngine(t *testing.T) (*Engine, *Company, *ascTeam) {
	t.Helper()
	company := specCompany(t, ascDoc)
	e := &Engine{backends: &Backends{Fleet: memory.NewFleet()}}
	e.epoch.current.Store(company)
	team := &ascTeam{}
	srv := httptest.NewServer(team)
	t.Cleanup(srv.Close)
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	key := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	product := appstoreconnect.New(appstoreconnect.Deps{
		Company: func() *config.Company { return company.Config },
		Resolve: func(v string) string {
			if v == "${ASC_KEY}" {
				return key
			}
			return v
		},
		HTTP: srv.Client(), Base: srv.URL,
		Identity: func(context.Context, string) (seataccess.Signer, error) {
			return seataccess.Signer{Own: true, Record: &seataccess.IdentityRecord{ID: "7KQ4M2ZX9P",
				Credential: &seataccess.Credential{Variable: "ASC_KEY"},
				Check: &seataccess.IdentityCheck{Status: seataccess.IdentityReady, Role: "Marketing",
					Team: "main"}}}, nil
		},
	})
	e.growthOnce.Do(func() { e.growth = []builtin.GrowthProduct{product} })
	return e, company, team
}

// THE AGENTS TAB OFFERS THE TEAM'S APPS BY NAME, as an agent's own key lists
// them: the access routes' offer hands the product's read to the session, so
// the chooser lists each app with its name and bundle ID, and the reach stays
// open.
//
// Mutation: drop the reads from seatOffers' session, and the chooser lists
// nothing to tick.
func TestTheAgentsTabOffersTheTeamsAppsByName(t *testing.T) {
	t.Parallel()
	e, company, _ := ascEngine(t)
	offers := e.seatOffers(t.Context(), integration.KindAppStoreConnect, company.Config, company.Org, "rev-1", false)
	if len(offers) != 1 {
		t.Fatalf("App Store Connect offers %d lanes", len(offers))
	}
	o := offers[0]
	if !slices.Equal(o.Scope, []string{"111", "222"}) || !o.ScopeOpen || o.ScopeNames["111"] != "Field Notes" ||
		o.ScopeDetails["222"] != "com.example.maps" {
		t.Fatalf("the chooser offers %v named %v with %v (open %v)", o.Scope, o.ScopeNames, o.ScopeDetails,
			o.ScopeOpen)
	}
}

// A PASS READS THE APPS AGAIN, so Check now shows a new app at once, and hands
// what it read to the sync that follows it.
//
// Mutation: read the held list in the pass, and Apple is not asked again.
func TestAPassReadsTheTeamsAppsAgain(t *testing.T) {
	t.Parallel()
	e, _, team := ascEngine(t)
	if r := e.AppStoreConnectReach(); len(r) != 1 || r[0].Read {
		t.Fatalf("before any read the card holds %+v", r)
	}
	if reads := e.seatReads(t.Context(), integration.KindAppStoreConnect); reads == nil || team.listed() != 1 {
		t.Fatalf("the first read handed %v after %d lists", reads, team.listed())
	}
	findings, reads, err := (&appStoreConnectPass{engine: e}).run(t.Context(), setup.PassInput{})
	if err != nil || len(findings) != 0 {
		t.Fatalf("the pass: %v, %+v", err, findings)
	}
	r, ok := reads.(appstoreconnect.Reads)
	if !ok || len(r.Reach.Apps()) != 2 || team.listed() != 2 {
		t.Fatalf("the pass handed %v after %d lists, want both apps read again", reads, team.listed())
	}
	if held := e.AppStoreConnectReach(); len(held) != 1 || len(held[0].Apps) != 2 {
		t.Fatalf("after the pass the card holds %+v", held)
	}
}

// A SYNC NO PASS STARTED (a change on the Agents tab) names the seat's apps as
// the product last read them, which the agent's own access is worded in.
//
// Mutation: drop the sync's fallback to the product's read, and the ledger
// records the app by its bare ID.
func TestASyncFromTheAgentsTabNamesTheSeatsApps(t *testing.T) {
	t.Parallel()
	e, _, _ := ascEngine(t)
	report, err := e.syncSeatAccess(t.Context(), integration.KindAppStoreConnect,
		seatSync{sink: seataccesstest.NewSink()})
	if err != nil {
		t.Fatal(err)
	}
	lane := report.Records["growth"].Lanes[seataccess.LaneAppStoreConnect]
	if lane.Words == nil || lane.Words.ScopeNames["111"] != "Field Notes" {
		t.Fatalf("the sync recorded the seat's apps as %+v (findings %+v)", lane.Words, report.Findings)
	}
}
