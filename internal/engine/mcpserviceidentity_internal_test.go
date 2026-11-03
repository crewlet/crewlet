package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/secrets"
)

// serviceVendor is a remote MCP server and its authorization server on one
// origin, issuing access tokens to one client by the client credentials grant
// and serving MCP requests that carry a token it issued, challenging every
// other.
type serviceVendor struct {
	srv *httptest.Server

	mu      sync.Mutex
	id      string
	secret  string
	refuse  string // the error every ask is refused with, "" to issue
	life    int    // expires_in of each token
	tokenAt string // the token endpoint the metadata names, "" for its own
	asks    []url.Values
	basics  []string
	issued  map[string]bool
	serial  int
	carried []string
}

const (
	serviceClientID = "crewlet-svc"
	// serviceSecret is the fake secret the vendor issues tokens to.
	serviceSecret = "svc-s3cr3t-0123456789"
)

func newServiceVendor(t *testing.T) *serviceVendor {
	t.Helper()
	v := &serviceVendor{id: serviceClientID, secret: serviceSecret, life: 3600, issued: map[string]bool{}}
	server := sdk.NewServer(&sdk.Implementation{Name: "vendor", Version: "1.0.0"}, nil)
	server.AddTool(&sdk.Tool{Name: "search", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	serve := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true})
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		v.mu.Lock()
		v.carried = append(v.carried, bearer)
		ok := v.issued[bearer]
		v.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://`+r.Host+
				`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		serve.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		writeJSONBody(w, map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		v.mu.Lock()
		token := v.tokenAt
		v.mu.Unlock()
		if token == "" {
			token = base + "/token"
		}
		writeJSONBody(w, map[string]any{"issuer": base, "token_endpoint": token,
			"grant_types_supported":                 []string{"client_credentials"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"}})
	})
	mux.HandleFunc("/token", v.token)
	v.srv = httptest.NewServer(mux)
	t.Cleanup(v.srv.Close)
	return v
}

func (v *serviceVendor) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id, secret, _ := r.BasicAuth()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.asks = append(v.asks, r.PostForm)
	v.basics = append(v.basics, id)
	refuse := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		writeJSONBody(w, map[string]string{"error": code,
			"error_description": "client " + id + " presented " + secret + ", which is not good"})
	}
	switch {
	case v.refuse != "":
		refuse(v.refuse)
		return
	case r.PostForm.Get("grant_type") != "client_credentials" || id != v.id || secret != v.secret:
		refuse("invalid_client")
		return
	}
	v.serial++
	token := fmt.Sprintf("at-%d", v.serial)
	v.issued[token] = true
	writeJSONBody(w, map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": v.life})
}

func (v *serviceVendor) asked() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.asks)
}

func (v *serviceVendor) set(f func(v *serviceVendor)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	f(v)
}

// serviceClock is a clock a case moves by hand.
type serviceClock struct{ at atomic.Pointer[time.Time] }

func newServiceClock() *serviceClock {
	c := &serviceClock{}
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	c.at.Store(&start)
	return c
}

func (c *serviceClock) now() time.Time { return *c.at.Load() }

func (c *serviceClock) advance(d time.Duration) {
	next := c.now().Add(d)
	c.at.Store(&next)
}

// serviceNodes are two nodes of one fleet, sharing its stores, its keyring and
// its clock, each running a company whose server signs in as a service
// identity, its entry's oauth block carrying lines beside the grant.
func serviceNodes(t *testing.T, v *serviceVendor, lines string) (*Engine, *Engine, *serviceClock) {
	t.Helper()
	company := companyFor(t, `
name: Acme
mcp_servers:
  - name: vendor
    transport: http
    url: `+v.srv.URL+`/mcp
    oauth:
      grant: client_credentials
      client_id: `+serviceClientID+`
      client_secret: ${VENDOR_CLIENT_SECRET}
`+lines)
	fleet, backend := coordmem.NewFleet(), coordmem.New()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": []byte("crewlet-test-sealing-key-32bytes")}})
	if err != nil {
		t.Fatal(err)
	}
	clock := newServiceClock()
	node := func(id string) *Engine {
		e := &Engine{backends: &Backends{Fleet: fleet, Coord: backend}, cipher: cipher, testNode: id,
			clock: clock.now, environ: config.MapSource{"VENDOR_CLIENT_SECRET": serviceSecret}}
		e.epoch.current.Store(company)
		return e
	}
	return node("node-a"), node("node-b"), clock
}

// entryOf is node e's configured entry for the vendor's server.
func entryOf(e *Engine) *config.MCPServer { return &e.Company().Config.MCPServers[0] }

// sourceOf is the token source a pass on e starts the vendor's server with.
func sourceOf(t *testing.T, e *Engine) mcp.BearerSource {
	t.Helper()
	source, needed := e.tokenSourceFor(t.Context(), entryOf(e), e.resolver())
	if needed != nil {
		t.Fatalf("a service identity's server is held back for a sign-in: %v", needed)
	}
	return source
}

// A SERVICE IDENTITY IS ISSUED ITS TOKEN AT USE AND KEEPS NOTHING: with nobody
// signed in, a pass starts the server with a source that signs in as the
// company's client (the grant, the server as the resource, the configured
// scopes, the secret by basic authentication) the first time a request needs a
// token, holds it for the next, and is the same source at the next pass; each
// node is issued its own; and no grant, sealed row or sign-in is written.
//
// Mutation: start the server through the person's grant ([Engine.bearerFor])
// and it is held back for a sign-in nobody makes.
func TestAServiceIdentityIsIssuedItsTokenAtUseAndKeepsNothing(t *testing.T) {
	t.Parallel()
	v := newServiceVendor(t)
	a, b, _ := serviceNodes(t, v, "      scopes: [read]\n")
	source := sourceOf(t, a)
	if v.asked() != 0 {
		t.Fatal("a pass asked for a token before any request needed one")
	}
	first, err := source.Token(t.Context())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	again, err := source.Token(t.Context())
	if err != nil || again != first || v.asked() != 1 {
		t.Fatalf("a held token was asked for again: %q then %q, %d asks, %v", first, again, v.asked(), err)
	}
	v.mu.Lock()
	ask, basic := v.asks[0], v.basics[0]
	v.mu.Unlock()
	if ask.Get("grant_type") != "client_credentials" || ask.Get("resource") != v.srv.URL+"/mcp" ||
		ask.Get("scope") != "read" || ask.Has("client_secret") || basic != serviceClientID {
		t.Fatalf("the ask sent %v as %q", ask, basic)
	}
	if sourceOf(t, a) != source {
		t.Fatal("the next pass made a new source, which restarts the server, with nothing changed")
	}
	other, err := sourceOf(t, b).Token(t.Context())
	if err != nil || other == first || v.asked() != 2 {
		t.Fatalf("node b was handed %q (%v), want a token of its own", other, err)
	}

	if _, found, err := a.backends.Fleet.MCPGrant(t.Context(), "vendor"); err != nil || found {
		t.Fatalf("a service identity recorded a grant: %t, %v", found, err)
	}
	if _, found, err := fleetsecrets.New(a.backends.Fleet, a.cipher).Estate().Held(t.Context(),
		grantRow("vendor")); err != nil || found {
		t.Fatalf("a service identity sealed a grant: %t, %v", found, err)
	}
	if signIns, err := a.backends.Fleet.ListMCPSignIns(t.Context()); err != nil || len(signIns) != 0 {
		t.Fatalf("a service identity left sign-ins %v, %v", signIns, err)
	}
}

// A REFUSED SERVICE IDENTITY IS HELD, NOT ASKED AGAIN BY EVERY REQUEST: the
// vendor's refusal is the server's failure, classed and in its words, never
// quoting the secret; every request within the restart schedule's wait is
// answered it without the vendor being asked; the first after the wait asks
// again; and new credentials ask at once.
//
// Mutation: drop the held failure, and each request asks the vendor again.
func TestARefusedServiceIdentityIsHeldUntilItsWaitOrNewCredentials(t *testing.T) {
	t.Parallel()
	v := newServiceVendor(t)
	a, _, clock := serviceNodes(t, v, "")
	v.set(func(v *serviceVendor) { v.refuse = "invalid_client" })
	source := sourceOf(t, a)
	_, err := source.Token(t.Context())
	if mcp.ClassOf(err) != mcp.FailCredentialRejected || !strings.Contains(err.Error(), "invalid_client") ||
		!strings.Contains(err.Error(), "oauth.client_id") {
		t.Fatalf("a refused service identity answered %v (class %q)", err, mcp.ClassOf(err))
	}
	if strings.Contains(err.Error(), serviceSecret) {
		t.Fatalf("the refusal quotes the secret the vendor echoed: %v", err)
	}
	for range 3 {
		if _, again := source.Token(t.Context()); again == nil || again.Error() != err.Error() {
			t.Fatalf("a request within the wait answered %v", again)
		}
	}
	if _, again := source.Rejected(t.Context(), "at-anything"); again == nil {
		t.Fatal("a 401 within the wait was answered a token")
	}
	if n := v.asked(); n != 1 {
		t.Fatalf("the vendor was asked %d times within the wait, want once", n)
	}

	clock.advance(time.Hour)
	if _, err := source.Token(t.Context()); err == nil || v.asked() != 2 {
		t.Fatalf("after the wait the vendor was not asked again: %d asks, %v", v.asked(), err)
	}
	if _, err := source.Token(t.Context()); err == nil || v.asked() != 2 {
		t.Fatalf("the refusal after the wait was not held: %d asks, %v", v.asked(), err)
	}

	rotated := config.NewResolver(config.MapSource{"VENDOR_CLIENT_SECRET": "svc-rotated-9876543210"})
	fresh := a.serviceSource(entryOf(a), rotated)
	if fresh == source {
		t.Fatal("new credentials kept the source that holds the old ones' refusal")
	}
	if _, err := fresh.Token(t.Context()); err == nil || v.asked() != 3 {
		t.Fatalf("new credentials were not asked about at once: %d asks, %v", v.asked(), err)
	}

	v.set(func(v *serviceVendor) { v.refuse = "" })
	clock.advance(time.Hour)
	if token, err := source.Token(t.Context()); err != nil || token == "" {
		t.Fatalf("credentials the vendor takes again were refused after the wait: %v", err)
	}
}

// A 401 RENEWS A SERVICE IDENTITY'S TOKEN ONCE, AND A TOKEN NEAR ITS END IS
// RENEWED AHEAD OF IT: a refusal of a token issued within the fresh window
// answers the same token (a renewal would not change it); a refusal after it
// is one new token, and every request the old one was refused on is answered
// that one; and a token within the renewal's lead of its end is replaced
// before it is sent.
//
// Mutation: renew on every refusal, and one refused token costs a vendor ask
// per request.
func TestA401RenewsAServiceIdentitysTokenOnce(t *testing.T) {
	t.Parallel()
	v := newServiceVendor(t)
	a, _, clock := serviceNodes(t, v, "")
	source := sourceOf(t, a)
	first, err := source.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if same, err := source.Rejected(t.Context(), first); err != nil || same != first || v.asked() != 1 {
		t.Fatalf("a token refused within the fresh window was renewed: %q, %d asks, %v", same, v.asked(), err)
	}
	clock.advance(oauthFreshWindow + time.Second)
	second, err := source.Rejected(t.Context(), first)
	if err != nil || second == first || v.asked() != 2 {
		t.Fatalf("a refused token was not renewed once: %q, %d asks, %v", second, v.asked(), err)
	}
	if again, err := source.Rejected(t.Context(), first); err != nil || again != second || v.asked() != 2 {
		t.Fatalf("a second refusal of the old token renewed again: %q, %d asks, %v", again, v.asked(), err)
	}
	clock.advance(time.Hour - oauthRenewAhead)
	if near, err := source.Token(t.Context()); err != nil || near == second || v.asked() != 3 {
		t.Fatalf("a token within the renewal's lead was sent: %q, %d asks, %v", near, v.asked(), err)
	}
}

// A SERVICE IDENTITY IS NEVER SIGNED IN TO, AND RAISES NO SIGN-IN ALARM: a
// sign-in is refused naming why, a grant a person made before the server
// changed its grant is neither the source it starts with nor a sign-in
// condition, and its credential's failures are its instances'.
func TestAServiceIdentityIsNeverSignedInToAndRaisesNoSignInAlarm(t *testing.T) {
	t.Parallel()
	v := newServiceVendor(t)
	a, _, _ := serviceNodes(t, v, "")
	if _, err := a.StartMCPSignIn(t.Context(), "vendor", signingJane); !errors.Is(err, ErrMCPServiceIdentity) {
		t.Fatalf("a sign-in to a service identity answered %v", err)
	}
	stale := coord.MCPGrant{Server: "vendor", Grant: uuid.New(), Client: "dcr", ClientID: "registered",
		Issuer: v.srv.URL, Resource: v.srv.URL + "/mcp", By: signingJane.Name, ByKind: signingJane.Kind,
		At: a.now()}
	if _, _, err := a.backends.Fleet.CreateMCPGrant(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	if source, ok := sourceOf(t, a).(*oauthSource); !ok || source.service == nil {
		t.Fatalf("a service identity's server starts with a person's grant: %#v", source)
	}
	conds := mcpConditions(a.Company(), nil, map[string]coord.MCPGrant{"vendor": stale})
	if len(conds) != 1 || conds[0].SignIn != nil {
		t.Fatalf("a service identity has a sign-in condition: %+v", conds)
	}
}

// A TEST OF A SERVICE IDENTITY SIGNS IN WITH WHAT IT TESTS: the saved server
// is tested with a token issued for the test, which reaches the server; a test
// with another secret is refused as the credential, in the vendor's words; and
// neither touches the source the running server holds.
func TestATestOfAServiceIdentitySignsInWithWhatItTests(t *testing.T) {
	t.Parallel()
	v := newServiceVendor(t)
	a, _, _ := serviceNodes(t, v, "")
	running := sourceOf(t, a)
	report := a.testServer(t.Context(), entryOf(a), nil, nil, nil, nil, nil, nil)
	if !report.Serving() || v.asked() != 1 {
		t.Fatalf("the saved server's test: %+v, %d asks", report.Failure, v.asked())
	}
	v.mu.Lock()
	carried := append([]string(nil), v.carried...)
	v.mu.Unlock()
	if !strings.HasPrefix(carried[len(carried)-1], "at-") {
		t.Fatalf("the server was sent %q, want the token issued for the test", carried[len(carried)-1])
	}
	wrong := config.NewResolver(config.MapSource{"VENDOR_CLIENT_SECRET": "svc-wrong-0123456789"})
	report = a.testServer(t.Context(), entryOf(a), nil, nil, wrong, nil, nil, nil)
	if report.Serving() || report.Failure.Class != mcp.FailCredentialRejected ||
		!strings.Contains(report.Failure.Said, "invalid_client") ||
		strings.Contains(report.Failure.Said, "svc-wrong-0123456789") {
		t.Fatalf("a test with the wrong secret: %+v", report.Failure)
	}
	if sourceOf(t, a) != running {
		t.Fatal("a test replaced the running server's source")
	}
}

// A SERVICE IDENTITY'S TOKEN ENDPOINT IS DIALLED ONLY THROUGH THE GUARD: one
// the server's metadata names on another origin at an internal address is sent
// nothing, and the server's failure says the address was blocked.
func TestAServiceIdentitysTokenEndpointIsDialledOnlyThroughTheGuard(t *testing.T) {
	t.Parallel()
	var reached atomic.Int32
	internal := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Add(1)
	}))
	t.Cleanup(internal.Close)
	v := newServiceVendor(t)
	v.set(func(v *serviceVendor) { v.tokenAt = internal.URL + "/token" })
	a, _, _ := serviceNodes(t, v, "")
	_, err := sourceOf(t, a).Token(t.Context())
	if mcp.ClassOf(err) != mcp.FailBlockedAddress {
		t.Fatalf("an internal token endpoint answered %v (class %q)", err, mcp.ClassOf(err))
	}
	if reached.Load() != 0 || v.asked() != 0 {
		t.Fatal("a token endpoint the guard refuses was sent the secret")
	}
}
