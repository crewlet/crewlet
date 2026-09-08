package datadog_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	reg.handle["/api/v1/org"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"orgs":[{"name":"Acme","public_id":"abc"}]}`))
	}
	org, err := reg.client(t).VerifyCredentials(context.Background(), pair)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if org.Name != "Acme" {
		t.Errorf("org = %+v", org)
	}
	if len(reg.auth) != 1 || reg.auth[0] != creds {
		t.Errorf("auth = %v, want both keys on the call", reg.auth)
	}
}

// THE VERIFY CALL NAMES A ROUTE DATADOG SERVES.
//
// This is the one thing the fake cannot check for itself: it answers whatever
// path the client asks for, so the suite passed for as long as the client
// asked for `/api/v2/current_user/orgs`, which Datadog does not serve and
// answers 404. A correct key pair failed to verify, and the pass reported the
// organization credentials refused. Pinning the path here is what makes the
// fake's agreement mean something.
func TestVerifyAsksForTheOrganizationRoute(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	asked := ""
	reg.handle["/api/v1/org"] = func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		_, _ = w.Write([]byte(`{"orgs":[{"name":"Acme","public_id":"p1"}]}`))
	}
	if _, err := reg.client(t).VerifyCredentials(context.Background(), pair); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if asked != "/api/v1/org" {
		t.Errorf("asked = %q, want /api/v1/org", asked)
	}
}

// A REFUSAL CARRIES DATADOG'S OWN WORDS, and its status, so the shared
// credential-rejection rule can read the number and an operator can read the
// sentence.
func TestARefusalKeepsTheStatusAndTheMessage(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v1/org"] = func(w http.ResponseWriter, _ *http.Request) {
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

// A SERVICE ACCOUNT ON PAGE TWO EXISTS.
//
// The listing read one page and stopped, so an account past it read as
// ABSENT — and both callers act on absence: the reconcile creates a second
// identity on top of a live account, and the teardown walks away from one it
// was asked to remove. The threshold is lower than a hundred service
// accounts, because Datadog's filter is a substring match on email and every
// PERSON under the same domain consumes a slot on page one before the
// service-account check drops them.
func TestListingServiceAccountsWalksEveryPage(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	var asked []string
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page[number]")
		asked = append(asked, page)
		if page == "0" {
			// A FULL page, which is what says there may be more.
			rows := make([]string, 0, 100)
			for i := range 100 {
				rows = append(rows, fmt.Sprintf(
					`{"id":"p0-%d","attributes":{"email":"filler-%d@crewlet.local","service_account":false}}`,
					i, i))
			}
			_, _ = w.Write([]byte(`{"data":[` + strings.Join(rows, ",") + `]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[
			{"id":"late","attributes":{"email":"agent-sre@crewlet.local","service_account":true}}
		]}`))
	}

	got, err := reg.client(t).ListServiceAccounts(context.Background(), pair, "crewlet.local")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(asked) < 2 {
		t.Fatalf("asked for pages %v; a full page was taken as the whole set", asked)
	}
	if len(got) != 1 || got[0].ID != "late" {
		t.Fatalf("accounts = %+v; the account on page two was reported absent", got)
	}
}

// AND A LISTING THAT NEVER CONVERGES RAISES rather than returning what it
// has. A short list reaching a caller is what creates a duplicate identity on
// top of a live account, so the ceiling must be an error.
func TestAListingThatNeverEndsIsAnError(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	rows := make([]string, 0, 100)
	for i := range 100 {
		rows = append(rows, fmt.Sprintf(
			`{"id":"%d","attributes":{"email":"a%d@crewlet.local","service_account":true}}`,
			i, i))
	}
	full := `{"data":[` + strings.Join(rows, ",") + `]}`
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(full))
	}

	if _, err := reg.client(t).ListServiceAccounts(
		context.Background(), pair, "crewlet.local"); err == nil {
		t.Fatal("a listing that never ends returned a short list as if complete")
	}
}

// A ROLE PAST THE FIRST PAGE EXISTS TOO, and this one is sharper: roleIDOf
// needs an EXACT match among what comes back and refuses the whole pass when
// it finds none, accusing the operator's config of naming a role their
// organization in fact has.
func TestListingRolesWalksEveryPage(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v2/roles"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page[number]") == "0" {
			rows := make([]string, 0, 100)
			for i := range 100 {
				rows = append(rows, fmt.Sprintf(
					`{"id":"p0-%d","attributes":{"name":"Crewlet Read Only %d"}}`, i, i))
			}
			_, _ = w.Write([]byte(`{"data":[` + strings.Join(rows, ",") + `]}`))
			return
		}
		_, _ = w.Write([]byte(
			`{"data":[{"id":"exact","attributes":{"name":"Crewlet"}}]}`))
	}

	got, err := reg.client(t).ListRoles(context.Background(), pair, "Crewlet")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, role := range got {
		if role.Name == "Crewlet" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the exact role on page two was not read: %d roles", len(got))
	}
}

// A NON-JSON REFUSAL DOES NOT PUT A WHOLE RESPONSE BODY INTO AN ERROR.
//
// That error becomes Finding.Detail, which integration.Observe stores WITHOUT
// truncation into a State the fleet writes to one coordination key shared
// with every other integration. A proxy's HTML page or a gateway 502 —
// exactly what this client's read cap exists for — is the answer least likely
// to be the JSON the decoder expects.
func TestARefusalDetailIsBounded(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>" + strings.Repeat("padding ", 100_000) + "</html>"))
	}

	_, err := reg.client(t).ListServiceAccounts(context.Background(), pair, "crewlet.local")
	if err == nil {
		t.Fatal("a 502 was not reported")
	}
	if len(err.Error()) > 4096 {
		t.Errorf("the error is %d bytes; a response body is being pasted into "+
			"a value the fleet stores", len(err.Error()))
	}
}
