package datadog_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/datadog"
)

// region is a Datadog a client can be run against.
type region struct {
	*httptest.Server
	calls []string
	// auth records the credential pair each call carried.
	auth   []string
	handle map[string]func(w http.ResponseWriter, r *http.Request)
}

func newRegion(t *testing.T) *region {
	t.Helper()
	reg := &region{handle: map[string]func(http.ResponseWriter, *http.Request){}}
	reg.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reg.calls = append(reg.calls, r.Method+" "+r.URL.Path)
		reg.auth = append(reg.auth,
			r.Header.Get("DD-API-KEY")+"/"+r.Header.Get("DD-APPLICATION-KEY"))
		w.Header().Set("Content-Type", "application/json")
		if h, ok := reg.handle[r.URL.Path]; ok {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":["no route"]}`))
	}))
	t.Cleanup(reg.Close)
	return reg
}

// client points a real client at the fake region.
func (reg *region) client(t *testing.T) *datadog.Client {
	t.Helper()
	// The endpoint is built from the SITE, which is the point of the site
	// check, so the fake region is reached by rewriting the host on the way
	// out rather than by teaching the client about test servers.
	c, err := datadog.NewClient(datadog.ClientOptions{
		Site: "datadoghq.com",
		HTTP: &http.Client{Transport: rewriteHost{to: reg.Listener.Addr().String()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type rewriteHost struct {
	to   string
	next http.RoundTripper
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = r.to
	next := r.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(req)
}

const creds = "api-key/app-key"

var pair = datadog.Credentials{APIKey: "api-key", AppKey: "app-key"}

// A SITE IS CHECKED, NOT TRUSTED. Every later call is built from this
// hostname, so a typo is a credential that authenticates nowhere and reports
// as a rejected key rather than as the typo it is.
func TestAnUnknownSiteIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := datadog.NewClient(datadog.ClientOptions{Site: "datadog.example.com"}); err == nil {
		t.Fatal("an unknown region was accepted")
	}
	// And a real one is accepted whatever case it arrives in.
	if _, err := datadog.NewClient(datadog.ClientOptions{Site: "  DataDogHQ.EU  "}); err != nil {
		t.Fatalf("a real region was refused: %v", err)
	}
}

// BOTH KEYS TRAVEL ON EVERY CALL. They are not interchangeable — the API key
// says which organization and the application key says which user acts — and
// Datadog refuses a v2 write carrying only the first with a message that
// names neither.
func TestEveryCallCarriesBothKeys(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v2/current_user/orgs"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"attributes":{"name":"Infrado","public_id":"abc"}}]}`))
	}
	org, err := reg.client(t).VerifyCredentials(context.Background(), pair)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if org.Name != "Infrado" {
		t.Errorf("org = %+v", org)
	}
	if len(reg.auth) != 1 || reg.auth[0] != creds {
		t.Errorf("auth = %v, want both keys on the call", reg.auth)
	}
}

// A REFUSAL CARRIES DATADOG'S OWN WORDS, and its status, so the shared
// credential-rejection rule can read the number and an operator can read the
// sentence.
func TestARefusalKeepsTheStatusAndTheMessage(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v2/current_user/orgs"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["Application key is not authorized"]}`))
	}
	_, err := reg.client(t).VerifyCredentials(context.Background(), pair)
	if err == nil {
		t.Fatal("a 403 was not reported")
	}
	if got := datadog.Status(err); got != http.StatusForbidden {
		t.Errorf("Status = %d, want 403", got)
	}
	if !strings.Contains(err.Error(), "Application key is not authorized") {
		t.Errorf("err = %v, want Datadog's own message", err)
	}
}

// A PERSON IS NOT A SERVICE ACCOUNT. The filter is a substring match on
// email, so a real user can match it; returning one would let a caller
// looking for its own accounts adopt somebody's colleague and disable them.
func TestListingServiceAccountsSkipsRealUsers(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"1","attributes":{"email":"agent-sre@crewlet.local","service_account":true}},
			{"id":"2","attributes":{"email":"real.person@crewlet.local","service_account":false}}
		]}`))
	}
	got, err := reg.client(t).ListServiceAccounts(context.Background(), pair, "crewlet.local")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("accounts = %+v, want only the service account", got)
	}
}

// THE ROLE IS PART OF THE CREATE. An account that exists for a moment
// without one inherits the organization's default role, and a pass
// interrupted between two calls would leave an agent holding whatever that
// grants.
func TestAServiceAccountIsCreatedHoldingItsRole(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	var sent map[string]any
	reg.handle["/api/v2/service_accounts"] = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_, _ = w.Write([]byte(`{"data":{"id":"u1","attributes":{"email":"a@b","name":"SRE"}}}`))
	}
	if _, err := reg.client(t).CreateServiceAccount(
		context.Background(), pair, "a@b", "SRE", "role-7"); err != nil {
		t.Fatalf("create: %v", err)
	}
	data, _ := sent["data"].(map[string]any)
	rel, _ := data["relationships"].(map[string]any)
	roles, _ := rel["roles"].(map[string]any)
	list, _ := roles["data"].([]any)
	if len(list) != 1 {
		t.Fatalf("the create carried no role: %v", sent)
	}
	first, _ := list[0].(map[string]any)
	if first["id"] != "role-7" {
		t.Errorf("role = %v, want role-7", first["id"])
	}
}

// THE APP KEY'S VALUE COMES BACK ONCE. Datadog never shows it again, so a
// caller that dropped it has stranded a credential it can only delete and
// remake.
func TestAnAppKeyReturnsItsValueOnCreation(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"id":"k1","attributes":{"key":"secret-value","name":"crewlet"}}}`))
	}
	key, err := reg.client(t).CreateAppKey(context.Background(), pair, "u1", "crewlet")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if key.Key != "secret-value" || key.ID != "k1" {
		t.Errorf("key = %+v", key)
	}
}
