package setupapi_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
)

// fakeAnalytics is the engine's turn-on, keeping what it was handed.
type fakeAnalytics struct {
	got []appstoreconnect.AnalyticsEnable
}

func (f *fakeAnalytics) EnableAnalytics(_ context.Context, in appstoreconnect.AnalyticsEnable) (
	[]appstoreconnect.AnalyticsApp, error) {
	f.got = append(f.got, in)
	return []appstoreconnect.AnalyticsApp{{App: "111", State: appstoreconnect.AnalyticsOn},
		{App: "222", State: appstoreconnect.AnalyticsFailed, Detail: "App Store Connect answered 403"}}, nil
}

// A PASTED ADMIN KEY TURNS APP STORE ANALYTICS ON FOR AN ADMINISTRATOR IN
// PERSON, AT A STEP-UP, and goes no further than the turn-on: the engine is
// handed the key and the person's seat, the answer names the apps and tells
// the person to revoke the key, and never repeats it. A machine token, a
// person past the step-up window or holding no seat, and a node that reaches
// no App Store Connect turn nothing on.
//
// Mutation: mount the route on the connect verb, and the machine token turns
// analytics on.
func TestAPastedAdminKeyTurnsAppStoreAnalyticsOnInPerson(t *testing.T) {
	t.Parallel()
	const key = "-----BEGIN PRIVATE KEY-----\nMIGTAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBHkwdwIBAQQg\n-----END PRIVATE KEY-----\n"
	body, _ := json.Marshal(map[string]string{"connection": "main", "key_id": "P1A2S3T4E5", "private_key": key})
	engine := &fakeAnalytics{}
	svc := newService(t, setupapi.Options{WebhookBase: fixtureWebhookBase, AppStoreAnalytics: engine,
		Now: func() time.Time { return pinned }})
	mux := http.NewServeMux()
	mount(t, mux, svc)
	send := func(p iam.Principal) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/setup/integrations/app_store_connect/analytics",
			strings.NewReader(string(body)))
		req = req.WithContext(iam.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	admin := iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
		Seat: "founder", Grants: iam.AllGrants, ReauthAt: time.Now().Add(time.Hour)}
	rec := send(admin)
	if rec.Code != http.StatusOK || len(engine.got) != 1 || engine.got[0].By != "founder" ||
		engine.got[0].PrivateKey != key || engine.got[0].KeyID != "P1A2S3T4E5" {
		t.Fatalf("the administrator's turn-on answered %d %s, handed %+v", rec.Code, rec.Body, engine.got)
	}
	if strings.Contains(rec.Body.String(), "PRIVATE KEY") || !strings.Contains(rec.Body.String(), "Revoke") {
		t.Errorf("the answer is %s", rec.Body)
	}
	stale := admin
	stale.ReauthAt = time.Now().Add(-time.Minute)
	seatless := admin
	seatless.Seat = ""
	// A MACHINE TOKEN BOUND TO A SEAT, so only the verb's in-person bar refuses it.
	machine := iam.Principal{ID: uuid.New(), Login: iam.TokenLogin("ops"), Kind: iam.KindMachine,
		Stage: iam.StageActive, Seat: "founder", Grants: iam.AllGrants, ReauthAt: time.Now().Add(time.Hour)}
	for name, p := range map[string]iam.Principal{"a machine token": machine, "a stale proof": stale,
		"a person holding no seat": seatless} {
		if rec := send(p); rec.Code != http.StatusForbidden || len(engine.got) != 1 {
			t.Errorf("%s answered %d %s", name, rec.Code, rec.Body)
		}
	}
	none := newService(t, setupapi.Options{WebhookBase: fixtureWebhookBase, Now: func() time.Time { return pinned }})
	bare := http.NewServeMux()
	mount(t, bare, none)
	req := httptest.NewRequest(http.MethodPost, "/setup/integrations/app_store_connect/analytics",
		strings.NewReader(string(body)))
	req = req.WithContext(iam.WithPrincipal(req.Context(), admin))
	got := httptest.NewRecorder()
	bare.ServeHTTP(got, req)
	if got.Code != http.StatusServiceUnavailable {
		t.Errorf("a node reaching no App Store Connect answered %d %s", got.Code, got.Body)
	}
}

// THE ANALYTICS CARD READS WHAT THE TURN-ON SENDS: the answer and each app's
// line, held both ways over the answer as the route writes it, and the states
// to the engine's closed set.
//
// Mutation: drop `revoke` from SetupAppStoreAnalyticsAnswer, and this is red.
func TestTheAnalyticsCardReadsWhatTheTurnOnSends(t *testing.T) {
	t.Parallel()
	svc := newService(t, setupapi.Options{WebhookBase: fixtureWebhookBase, AppStoreAnalytics: &fakeAnalytics{},
		Now: func() time.Time { return pinned }})
	mux := http.NewServeMux()
	mount(t, mux, svc)
	req := httptest.NewRequest(http.MethodPost, "/setup/integrations/app_store_connect/analytics",
		strings.NewReader(`{"connection": "main", "key_id": "P1A2S3T4E5", "private_key": "x"}`))
	req = req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{ID: uuid.New(), Login: "jane.doe",
		Kind: iam.KindPerson, Stage: iam.StageActive, Seat: "founder", Grants: iam.AllGrants,
		ReauthAt: time.Now().Add(time.Hour)}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var answer map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("the turn-on answered %d %s", rec.Code, rec.Body)
	}
	tree := clientsource.Tree(t)
	both := func(iface string, sent map[string]any) {
		members, err := clientsource.Interface(tree, iface)
		if err != nil {
			t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
		}
		declared := map[string]bool{}
		for _, m := range members {
			declared[m.Name] = true
		}
		got, want := slices.Sorted(maps.Keys(declared)), slices.Sorted(maps.Keys(sent))
		if !slices.Equal(got, want) {
			t.Errorf("%s declares %v, the route sends %v", iface, got, want)
		}
	}
	both("SetupAppStoreAnalyticsAnswer", answer)
	apps, _ := answer["apps"].([]any)
	failed, _ := apps[1].(map[string]any)
	both("SetupAppStoreAnalyticsApp", failed)
	states, err := clientsource.Union(tree, "SetupAppStoreAnalyticsState")
	if slices.Sort(states); err != nil || !slices.Equal(states, slices.Sorted(slices.Values(appstoreconnect.AnalyticsStates()))) {
		t.Errorf("SetupAppStoreAnalyticsState declares %v (%v), the engine sends %v", states, err,
			appstoreconnect.AnalyticsStates())
	}
}

// ascCompany is companyDoc with an App Store Connect team holding two keys.
const ascCompany = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {"app_store_connect": {"enabled": true, "connections": {"main": {
    "issuer_id": "57246542-96fe-1a63-e053-0824d011072a",
    "keys": {"marketing": {"key_id": "2X9R4HXF34", "private_key": "${APP_STORE_CONNECT_MAIN_MARKETING_KEY}"},
             "admin": {"key_id": "A1B2C3D4E5", "private_key": "${APP_STORE_CONNECT_MAIN_ADMIN_KEY}"}}}}}},
  "roles": [{"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu"}]
}`

// ONE ACTION REMOVES AN APP STORE CONNECT KEY: the slot leaves the company
// document in one revision, which the next call reads, and then its sealed
// .p8 leaves the store; a connection's last key, or a key nobody connected,
// is refused and nothing moves.
//
// Mutation: skip the Unset, and the Admin key stays sealed after its removal.
func TestOneActionRemovesAnAppStoreConnectKey(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedDocument(t, ascCompany)
	for name, value := range map[string]string{"APP_STORE_CONNECT_MAIN_ADMIN_KEY": "sealed admin key",
		"APP_STORE_CONNECT_MAIN_MARKETING_KEY": "sealed marketing key"} {
		if err := s.vault.Set(t.Context(), name, value, secrets.Author{}, secrets.SourceSetup, pinned); err != nil {
			t.Fatal(err)
		}
	}
	base := s.active(t).ID
	res := s.do(t, http.MethodDelete, "/setup/integrations/app_store_connect/keys/main/admin", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", res.Code, res.Body)
	}
	if after := s.active(t); after.ParentID != base || !strings.Contains(after.Summary, "admin key") {
		t.Errorf("the removal landed %+v on %s", after, base)
	}
	slots := s.activeCompany(t).Integrations.AppStoreConnect.Connections["main"].Keys.Slots()
	if len(slots) != 1 || slots[0].Name != "marketing" {
		t.Errorf("the connection holds %+v, want the marketing key alone", slots)
	}
	if _, held := s.vault.get("APP_STORE_CONNECT_MAIN_ADMIN_KEY"); held {
		t.Error("the removed Admin key is still sealed")
	}
	for path, want := range map[string]int{
		"/setup/integrations/app_store_connect/keys/main/marketing": http.StatusConflict,
		"/setup/integrations/app_store_connect/keys/main/finance":   http.StatusNotFound,
		"/setup/integrations/app_store_connect/keys/eu/marketing":   http.StatusNotFound,
	} {
		if res := s.do(t, http.MethodDelete, path, "", nil); res.Code != want {
			t.Errorf("DELETE %s = %d %s, want %d", path, res.Code, res.Body, want)
		}
	}
	if _, held := s.vault.get("APP_STORE_CONNECT_MAIN_MARKETING_KEY"); !held {
		t.Error("a refused removal removed the last key's seal")
	}
}
