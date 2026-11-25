package engine

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/oauth"
	"github.com/crewlet/crewlet/internal/secrets"
)

// oauthVendor is a remote MCP server and its authorization server on one
// origin, as strict as a careful vendor: a code is exchanged once, with the
// verifier its challenge was made from, and a refresh token is ROTATED at
// every renewal, a retired one presented again (a stolen grant) revoking the
// whole family.
type oauthVendor struct {
	srv *httptest.Server

	mu         sync.Mutex
	challenges map[string]string // code -> challenge
	current    string            // the one refresh token that renews
	retired    map[string]bool
	family     bool // false once revoked
	serial     int
	attempts   int
	exchanges  int
	renewals   int
	reuses     int
	revoked    []string
	accessLife int
	// presented is each Authorization the server's own address was sent; it
	// refuses every one.
	presented []string
	// authorizeAt is the authorization endpoint its metadata names in place
	// of its own, or "".
	authorizeAt string
	// registered is the client id its registration issues, scopes what its
	// metadata supports and granted the scope its token answer grants ("":
	// none named, the request granted as asked).
	registered, granted string
	scopes              []string
}

func newOAuthVendor(t *testing.T) *oauthVendor {
	t.Helper()
	v := &oauthVendor{challenges: map[string]string{}, retired: map[string]bool{}, family: true,
		accessLife: 3600, registered: "registered-client", scopes: []string{"read"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.presented = append(v.presented, r.Header.Get("Authorization"))
		v.mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://`+r.Host+
			`/.well-known/oauth-protected-resource/mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		v.mu.Lock()
		scopes := v.scopes
		v.mu.Unlock()
		writeJSONBody(w, map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base},
			"scopes_supported": scopes})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		v.mu.Lock()
		authorize := cmp.Or(v.authorizeAt, base+"/authorize")
		v.mu.Unlock()
		writeJSONBody(w, map[string]any{"issuer": base, "authorization_endpoint": authorize,
			"token_endpoint": base + "/token", "registration_endpoint": base + "/register",
			"revocation_endpoint": base + "/revoke", "client_id_metadata_document_supported": true,
			"code_challenge_methods_supported":               []string{"S256"},
			"authorization_response_iss_parameter_supported": true})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		v.mu.Lock()
		registered := v.registered
		v.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeJSONBody(w, map[string]any{"client_id": registered})
	})
	mux.HandleFunc("/token", v.token)
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		v.mu.Lock()
		v.revoked = append(v.revoked, r.PostForm.Get("token"))
		v.family = false
		v.mu.Unlock()
	})
	v.srv = httptest.NewServer(mux)
	t.Cleanup(v.srv.Close)
	return v
}

func writeJSONBody(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// authorize is the person consenting at the vendor: a code bound to the
// authorization address's challenge, as the browser carries it back.
func (v *oauthVendor) authorize(t *testing.T, address string) (code, state string) {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("the authorization address asks for %q", q.Get("code_challenge_method"))
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.serial++
	code = fmt.Sprintf("code-%d", v.serial)
	v.challenges[code] = q.Get("code_challenge")
	return code, q.Get("state")
}

func (v *oauthVendor) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	v.mu.Lock()
	defer v.mu.Unlock()
	refuse := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code,
			"error_description": "the grant is not good any more"})
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		v.attempts++
		challenge, ok := v.challenges[r.PostForm.Get("code")]
		delete(v.challenges, r.PostForm.Get("code"))
		if !ok || oauth.ChallengeOf(r.PostForm.Get("code_verifier")) != challenge ||
			r.PostForm.Get("resource") == "" {
			refuse("invalid_grant")
			return
		}
		v.exchanges++
		v.family = true
	case "refresh_token":
		presented := r.PostForm.Get("refresh_token")
		if v.retired[presented] {
			// A RETIRED TOKEN PRESENTED AGAIN: read as stolen, and the
			// whole grant goes with it.
			v.reuses++
			v.family = false
		}
		if !v.family || presented != v.current {
			refuse("invalid_grant")
			return
		}
		v.renewals++
		v.retired[presented] = true
	default:
		refuse("unsupported_grant_type")
		return
	}
	v.serial++
	v.current = fmt.Sprintf("rt-%d", v.serial)
	answer := map[string]any{"access_token": fmt.Sprintf("at-%d", v.serial), "token_type": "Bearer",
		"expires_in": v.accessLife, "refresh_token": v.current, "refresh_token_expires_in": 90 * 24 * 3600}
	if v.granted != "" {
		answer["scope"] = v.granted
	}
	writeJSONBody(w, answer)
}

func (v *oauthVendor) counts() (exchanges, renewals, reuses int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.exchanges, v.renewals, v.reuses
}

// oauthNodes are two nodes of one fleet, sharing its stores and its keyring,
// each running a company that signs in to the vendor with client.
func oauthNodes(t *testing.T, v *oauthVendor, client string) (*Engine, *Engine) {
	t.Helper()
	company := companyFor(t, `
name: Acme
mcp_servers:
  - name: vendor
    transport: http
    url: `+v.srv.URL+`/mcp
    oauth:
      client: `+client+`
`)
	fleet, backend := coordmem.NewFleet(), coordmem.New()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": []byte("crewlet-test-sealing-key-32bytes")}})
	if err != nil {
		t.Fatal(err)
	}
	boot := &config.Bootstrap{Secrets: config.Secrets{ActiveKeyID: "k1", Keys: []config.SecretKey{
		{ID: "k1", Material: base64.StdEncoding.EncodeToString([]byte("crewlet-test-signing-key-32bytes"))}}}}
	node := func(id string) *Engine {
		e := &Engine{backends: &Backends{Fleet: fleet, Coord: backend}, cipher: cipher, boot: boot,
			testNode: id, webhookBase: "https://crewlet.example.com", externalBase: "https://crewlet.example.com"}
		e.epoch.current.Store(company)
		return e
	}
	return node("node-a"), node("node-b")
}

var signingJane = iam.Actor{Name: "jane.doe", Kind: iam.ActorHuman, OperatorID: "session:jane"}

// signIn signs the company in to the vendor through a, finishing through b.
func signIn(t *testing.T, v *oauthVendor, a, b *Engine) {
	t.Helper()
	started, err := a.StartMCPSignIn(t.Context(), "vendor", signingJane)
	if err != nil {
		t.Fatalf("StartMCPSignIn: %v", err)
	}
	code, state := v.authorize(t, started.AuthorizeURL)
	if server, err := completeMCP(t.Context(), b, SignInLanding{State: state, Code: code,
		Issuer: v.srv.URL}); err != nil || server != "vendor" {
		t.Fatalf("CompleteSignIn = %q, %v", server, err)
	}
}

// A SIGN-IN'S STATE IS SIGNED AND SPENT ONCE: the browser carries the
// authorization address and nothing the engine keeps (the verifier is only
// sealed on the sign-in's record); an edited, lapsed or used state is refused
// before the vendor exchanges anything; and the one good callback, on any
// node, seals the grant, records its public half and approves the sign-in.
//
// Mutation: skip the version check that spends the state, or drop the
// signature check, and a second or forged callback exchanges a code.
func TestASignInsStateIsSignedAndSpentOnce(t *testing.T) {
	t.Parallel()
	v := newOAuthVendor(t)
	a, b := oauthNodes(t, v, "dcr")
	started, err := a.StartMCPSignIn(t.Context(), "vendor", signingJane)
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := a.backends.Fleet.OAuthSignIn(t.Context(), coord.MCPGrantFamily, "vendor")
	if err != nil || !found || record.Status != coord.OAuthSignInPending {
		t.Fatalf("the sign-in reads %+v, %v, %v", record, found, err)
	}
	plain, err := a.cipher.Decrypt(record.Sealed, signInAAD(coord.MCPGrantFamily, record.ID))
	if err != nil {
		t.Fatal(err)
	}
	var sealed signInSealed
	if err := json.Unmarshal([]byte(plain), &sealed); err != nil || len(sealed.Verifier) != 43 {
		t.Fatalf("the sealed half is %v, %v", sealed.Verifier, err)
	}
	if strings.Contains(record.Sealed, sealed.Verifier) || strings.Contains(started.AuthorizeURL, sealed.Verifier) {
		t.Fatal("the verifier is readable outside the engine's seal")
	}
	code, state := v.authorize(t, started.AuthorizeURL)

	for name, forged := range map[string]string{
		"edited":   state[:len(state)-2] + "xx",
		"unsigned": "vendor~" + record.ID.String(),
		"empty":    "",
	} {
		if _, err := completeMCP(t.Context(), b, SignInLanding{State: forged, Code: code,
			Issuer: v.srv.URL}); !errors.Is(err, refusedState) {
			t.Errorf("an %s state was not refused: %v", name, err)
		}
	}
	if exchanges, _, _ := v.counts(); exchanges != 0 {
		t.Fatalf("a refused state exchanged %d codes", exchanges)
	}
	// THE SAME STATE ARRIVING TWICE AT ONCE, on two nodes: one spends it,
	// and the other never asks the vendor to exchange the code.
	var completed, refusedAgain atomic.Int32
	var wg sync.WaitGroup
	for _, node := range []*Engine{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := completeMCP(t.Context(), node, SignInLanding{State: state, Code: code,
				Issuer: v.srv.URL})
			switch {
			case err == nil:
				completed.Add(1)
			case errors.Is(err, refusedState):
				refusedAgain.Add(1)
			default:
				t.Errorf("a callback answered %v", err)
			}
		}()
	}
	wg.Wait()
	if completed.Load() != 1 || refusedAgain.Load() != 1 {
		t.Fatalf("%d callbacks completed and %d were refused, want one of each",
			completed.Load(), refusedAgain.Load())
	}
	if _, err := completeMCP(t.Context(), a, SignInLanding{State: state, Code: code,
		Issuer: v.srv.URL}); !errors.Is(err, refusedState) {
		t.Fatalf("a later callback with the same state answered %v", err)
	}
	v.mu.Lock()
	attempts := v.attempts
	v.mu.Unlock()
	if exchanges, _, _ := v.counts(); exchanges != 1 || attempts != 1 {
		t.Fatalf("the vendor was asked to exchange %d codes and exchanged %d, want one", attempts, exchanges)
	}
	grant, found, err := a.backends.Fleet.OAuthGrant(t.Context(), coord.MCPGrantFamily, "vendor")
	if err != nil || !found || grant.ClientID != "registered-client" || grant.By != "jane.doe" ||
		grant.Issuer != v.srv.URL || grant.Expiry == nil {
		t.Fatalf("the grant record is %+v, %v, %v", grant, found, err)
	}
	held, found, err := fleetsecrets.New(a.backends.Fleet, a.cipher).Estate().Held(t.Context(), grantRow(coord.MCPGrantFamily, "vendor"))
	if err != nil || !found || !strings.Contains(held.Value, `"refresh":"rt-`) {
		t.Fatalf("the sealed grant reads found=%t (%v)", found, err)
	}
	if done, _, _ := a.backends.Fleet.OAuthSignIn(t.Context(), coord.MCPGrantFamily, "vendor"); done.Status != coord.OAuthSignInApproved ||
		done.Sealed != "" {
		t.Fatalf("the sign-in ended %+v, want approved with its sealed half gone", done)
	}

	// A STATE PAST ITS EXPIRY is refused too, on any node.
	later := time.Now().Add(SignInTTL + time.Minute)
	started, err = a.StartMCPSignIn(t.Context(), "vendor", signingJane)
	if err != nil {
		t.Fatal(err)
	}
	code, state = v.authorize(t, started.AuthorizeURL)
	b.clock = func() time.Time { return later }
	if _, err := completeMCP(t.Context(), b, SignInLanding{State: state, Code: code,
		Issuer: v.srv.URL}); !errors.Is(err, refusedState) {
		t.Fatalf("a lapsed state answered %v", err)
	}
}

// A CALLBACK FROM ANOTHER AUTHORIZATION SERVER IS REFUSED (RFC 9207): its code
// is never sent to this one's token endpoint, and the sign-in ends failed.
func TestASignInComingBackFromAnotherIssuerIsRefused(t *testing.T) {
	t.Parallel()
	v := newOAuthVendor(t)
	a, b := oauthNodes(t, v, "cimd")
	started, err := a.StartMCPSignIn(t.Context(), "vendor", signingJane)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(started.AuthorizeURL, url.QueryEscape("https://crewlet.example.com/webhooks/mcp-oauth/client-metadata.json")) {
		t.Fatalf("a cimd sign-in does not name the engine's document: %s", started.AuthorizeURL)
	}
	code, state := v.authorize(t, started.AuthorizeURL)
	var refused *SignInRefusal
	if _, err := completeMCP(t.Context(), b, SignInLanding{State: state, Code: code,
		Issuer: "https://evil.example.com"}); !errors.As(err, &refused) {
		t.Fatalf("a callback from another issuer answered %v", err)
	}
	if exchanges, _, _ := v.counts(); exchanges != 0 {
		t.Fatal("a code from another issuer was exchanged")
	}
	if done, _, _ := a.backends.Fleet.OAuthSignIn(t.Context(), coord.MCPGrantFamily, "vendor"); done.Status != coord.OAuthSignInFailed {
		t.Fatalf("the sign-in ended %+v", done)
	}
}

// TWO RENEWERS CANNOT STRAND A TOKEN: two nodes renewing one grant at once
// take the fleet's renewal lease in turn and read the refresh token the other
// just wrote, so the vendor never sees a retired one again (which revokes the
// grant) and the kept token is the one the vendor issued last.
//
// Mutation: renew without the lease, or write the rotated token without the
// version it was read at, and the vendor revokes the grant mid-run.
func TestTwoRenewersCannotStrandAToken(t *testing.T) {
	t.Parallel()
	v := newOAuthVendor(t)
	// AN ACCESS TOKEN SHORTER THAN THE RENEWAL'S LEAD, so every ask renews.
	v.accessLife = 30
	a, b := oauthNodes(t, v, "dcr")
	signIn(t, v, a, b)
	sources := []mcp.BearerSource{}
	for _, e := range []*Engine{a, b, a, b} {
		source, needed := e.bearerFor(t.Context(), "vendor")
		if needed != nil {
			t.Fatalf("a signed-in server needs a sign-in: %v", needed)
		}
		sources = append(sources, source)
	}
	var failures atomic.Int32
	var wg sync.WaitGroup
	for _, source := range sources[:2] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				if _, err := source.Token(t.Context()); err != nil {
					failures.Add(1)
					t.Errorf("a renewal failed: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if _, renewals, reuses := v.counts(); reuses != 0 || renewals != 10 || failures.Load() != 0 {
		t.Fatalf("the vendor saw %d renewals and %d reuses of a retired token, want 10 and none",
			renewals, reuses)
	}
	if sources[0] != sources[2] {
		t.Fatal("one grant on one node is two sources")
	}
	held, _, err := fleetsecrets.New(a.backends.Fleet, a.cipher).Estate().Held(t.Context(), grantRow(coord.MCPGrantFamily, "vendor"))
	if err != nil {
		t.Fatal(err)
	}
	var grant sealedGrant
	if err := json.Unmarshal([]byte(held.Value), &grant); err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	current := v.current
	v.mu.Unlock()
	if grant.Refresh != current {
		t.Fatalf("the kept refresh token is %q, and the vendor renews %q alone: the grant is stranded",
			grant.Refresh, current)
	}
}

// A REFUSED RENEWAL ASKS FOR A SIGN-IN, IN THE VENDOR'S WORDS, AND NO NODE ASKS
// THE VENDOR AGAIN until somebody signs in; the server's next pass holds it
// back for the sign-in.
func TestARefusedRenewalAsksForASignInAndTheVendorNoMore(t *testing.T) {
	t.Parallel()
	v := newOAuthVendor(t)
	a, b := oauthNodes(t, v, "dcr")
	signIn(t, v, a, b)
	v.mu.Lock()
	v.family = false
	v.mu.Unlock()
	source, _ := a.bearerFor(t.Context(), "vendor")
	if _, err := source.Token(t.Context()); !errors.Is(err, mcp.ErrSignInNeeded) ||
		!strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("a refused renewal answered %v", err)
	}
	grant, _, _ := a.backends.Fleet.OAuthGrant(t.Context(), coord.MCPGrantFamily, "vendor")
	if !grant.Failed() || !strings.Contains(grant.Failure, "invalid_grant") {
		t.Fatalf("the grant record is %+v, want the vendor's refusal", grant)
	}
	_, before, _ := v.counts()
	if _, needed := b.bearerFor(t.Context(), "vendor"); needed == nil || needed.failure == "" {
		t.Fatalf("node b's next pass would start the server: %v", needed)
	}
	stale := &oauthSource{engine: b, server: "vendor", kind: mcpGrants{}, grant: grant.Grant}
	if _, err := stale.Token(t.Context()); !errors.Is(err, mcp.ErrSignInNeeded) {
		t.Fatalf("a renewal after the refusal answered %v", err)
	}
	if _, after, _ := v.counts(); after != before {
		t.Fatal("the vendor was asked again after it refused the grant for good")
	}
}

// A SIGN-OUT REVOKES THE GRANT AT THE VENDOR AND DELETES IT HERE: the refresh
// token is sent to the revocation endpoint, the sealed row and the public
// record are gone, the server's next pass holds it back for a sign-in, and a
// second sign-out finds nobody signed in.
//
// Mutation: skip the estate delete, and the sealed row outlives the sign-out.
func TestASignOutRevokesAtTheVendorAndDeletesTheGrant(t *testing.T) {
	t.Parallel()
	v := newOAuthVendor(t)
	a, b := oauthNodes(t, v, "dcr")
	signIn(t, v, a, b)
	v.mu.Lock()
	issued := v.current
	v.mu.Unlock()
	out, err := b.SignOutMCPServer(t.Context(), "vendor", signingJane)
	if err != nil || out.Revocation != RevokedAtVendor {
		t.Fatalf("SignOutMCPServer = %+v, %v", out, err)
	}
	v.mu.Lock()
	revoked := append([]string(nil), v.revoked...)
	v.mu.Unlock()
	if len(revoked) != 1 || revoked[0] != issued {
		t.Fatalf("the vendor was asked to revoke %v, want the grant's %q", revoked, issued)
	}
	if _, found, err := fleetsecrets.New(a.backends.Fleet, a.cipher).Estate().Held(t.Context(),
		grantRow(coord.MCPGrantFamily, "vendor")); err != nil || found {
		t.Fatalf("the sealed grant outlived the sign-out: %v, %v", found, err)
	}
	if _, found, _ := a.backends.Fleet.OAuthGrant(t.Context(), coord.MCPGrantFamily, "vendor"); found {
		t.Fatal("the grant record outlived the sign-out")
	}
	if held, _ := a.HeldMCPGrant(t.Context(), "vendor"); held {
		t.Fatal("the server reads as signed in")
	}
	if _, needed := a.bearerFor(t.Context(), "vendor"); needed == nil || needed.failure != "" {
		t.Fatalf("the next pass would start the server: %v", needed)
	}
	if _, err := a.SignOutMCPServer(t.Context(), "vendor", signingJane); !errors.Is(err, ErrMCPNotSignedIn) {
		t.Fatalf("a second sign-out answered %v", err)
	}
}

// A SIGN-IN IS REFUSED BEFORE ANYBODY IS SENT ANYWHERE where it could not be
// finished: a server with no oauth block, an engine with no public address, a
// vendor offering no S256, and the company's own client whose secret is not
// set, which also holds the server back.
func TestASignInIsRefusedWhereItCouldNotBeFinished(t *testing.T) {
	t.Parallel()
	v := newOAuthVendor(t)
	a, _ := oauthNodes(t, v, "dcr")
	if _, err := a.StartMCPSignIn(t.Context(), "nothing", signingJane); !errors.Is(err, ErrNoMCPServer) {
		t.Errorf("an unknown server answered %v", err)
	}
	unaddressed := &Engine{backends: a.backends, cipher: a.cipher, boot: a.boot, testNode: "node-e"}
	unaddressed.epoch.current.Store(a.Company())
	if _, err := unaddressed.StartMCPSignIn(t.Context(), "vendor", signingJane); !errors.Is(err, ErrNoCallbackAddress) {
		t.Errorf("an engine with no public address answered %v", err)
	}

	plainPKCE := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/prm"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/prm":
			writeJSONBody(w, map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}})
		case "/.well-known/oauth-authorization-server":
			writeJSONBody(w, map[string]any{"issuer": base, "authorization_endpoint": base + "/a",
				"token_endpoint": base + "/t", "registration_endpoint": base + "/r",
				"code_challenge_methods_supported": []string{"plain"}})
		default:
			_, _ = io.WriteString(w, "{}")
		}
	}))
	t.Cleanup(plainPKCE.Close)
	weak := &Engine{backends: a.backends, cipher: a.cipher, boot: a.boot, testNode: "node-c",
		webhookBase: "https://crewlet.example.com"}
	weak.epoch.current.Store(companyFor(t, `
name: Acme
mcp_servers:
  - name: vendor
    transport: http
    url: `+plainPKCE.URL+`/mcp
    oauth:
      client: dcr
`))
	var refused *SignInRefusal
	if _, err := weak.StartMCPSignIn(t.Context(), "vendor", signingJane); !errors.As(err, &refused) ||
		!strings.Contains(refused.Reason, "S256") {
		t.Errorf("a vendor offering no S256 answered %v", err)
	}

	own := companyFor(t, `
name: Acme
mcp_servers:
  - name: vendor
    transport: http
    url: `+v.srv.URL+`/mcp
    oauth:
      client: own
      client_id: crewlet-acme
      client_secret: ${VENDOR_CLIENT_SECRET_UNSET}
`)
	ownNode := &Engine{backends: a.backends, cipher: a.cipher, boot: a.boot, testNode: "node-d",
		webhookBase: "https://crewlet.example.com"}
	ownNode.epoch.current.Store(own)
	if _, err := ownNode.StartMCPSignIn(t.Context(), "vendor", signingJane); !errors.As(err, &refused) ||
		!strings.Contains(refused.Reason, "VENDOR_CLIENT_SECRET_UNSET") {
		t.Errorf("an own client with no secret answered %v", err)
	}
	if _, missing := resolveServer(&own.Config.MCPServers[0], "", "vendor", nil, ownNode.resolver(),
		nil); missing == nil || !strings.Contains(missing.Error(), "VENDOR_CLIENT_SECRET_UNSET") {
		t.Errorf("a server whose client secret is unset is not held back: %v", missing)
	}
}

// A SIGN-IN REFUSED BEFORE ANYBODY IS SENT ANYWHERE LEAVES THE PENDING ONE: a
// vendor whose metadata now names plain http on another origin as its
// authorization endpoint is refused, and the pending sign-in can still finish.
//
// Mutation: store the sign-in before its authorize URL is made, and the
// pending one is replaced by one nobody can finish.
func TestARefusedSignInLeavesThePendingOne(t *testing.T) {
	t.Parallel()
	v := newOAuthVendor(t)
	a, _ := oauthNodes(t, v, "dcr")
	pending, err := a.StartMCPSignIn(t.Context(), "vendor", signingJane)
	if err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	v.authorizeAt = "http://elsewhere.example.com/authorize"
	v.mu.Unlock()
	var refused *SignInRefusal
	if _, err := a.StartMCPSignIn(t.Context(), "vendor", signingJane); !errors.As(err, &refused) ||
		!strings.Contains(refused.Reason, "not an https address") {
		t.Fatalf("a sign-in at a plain http endpoint elsewhere answered %v", err)
	}
	if got, err := a.MCPSignIn(t.Context(), pending.SignIn.ID); err != nil || got.Status != coord.OAuthSignInPending {
		t.Errorf("the pending sign-in after a refused one: %+v, %v", got, err)
	}
}

// A GRANT GOES ONLY TO THE ADDRESS IT WAS ISSUED FOR: a configuration write
// that moves the server to another origin, or off the resource's path, holds
// the server back for a sign-in at its new address rather than handing it the
// company's token, a saved test included; moved back, the grant serves again.
//
// Mutation: skip the resource check, and the token is handed to the new
// address.
func TestAGrantGoesOnlyToTheAddressItWasIssuedFor(t *testing.T) {
	t.Parallel()
	v := newOAuthVendor(t)
	a, b := oauthNodes(t, v, "dcr")
	signIn(t, v, a, b)
	original := a.Company()
	if _, needed := a.bearerFor(t.Context(), "vendor"); needed != nil {
		t.Fatalf("the signed-in server needs a sign-in: %v", needed)
	}
	for _, moved := range []string{"https://elsewhere.example.com/mcp", v.srv.URL + "/other"} {
		a.epoch.current.Store(companyFor(t, `
name: Acme
mcp_servers:
  - name: vendor
    transport: http
    url: `+moved+`
    oauth:
      client: dcr
`))
		if _, needed := a.bearerFor(t.Context(), "vendor"); needed == nil || needed.moved == "" {
			t.Fatalf("a server moved to %s was handed the grant: %v", moved, needed)
		}
		if a.testBearer(t.Context(), "vendor") != nil {
			t.Fatalf("a test of the server moved to %s carries the grant", moved)
		}
		stale := &oauthSource{engine: a, server: "vendor", kind: mcpGrants{}, grant: mustGrant(t, a).Grant}
		if _, err := stale.Token(t.Context()); !errors.Is(err, mcp.ErrSignInNeeded) {
			t.Fatalf("a renewal for the server moved to %s answered %v", moved, err)
		}
	}
	a.epoch.current.Store(original)
	if _, needed := a.bearerFor(t.Context(), "vendor"); needed != nil {
		t.Fatalf("the server back at its address needs a sign-in: %v", needed)
	}
}

func mustGrant(t *testing.T, e *Engine) coord.OAuthGrant {
	t.Helper()
	g, found, err := e.backends.Fleet.OAuthGrant(t.Context(), coord.MCPGrantFamily, "vendor")
	if err != nil || !found {
		t.Fatalf("the grant record: %v, %v", found, err)
	}
	return g
}

// A VALUE TEST SENDS THE GRANT ONLY TO THE ADDRESS THIS NODE RUNS: the entry
// sent with the test (the fleet's active revision, ahead of this node's apply)
// may name another address than the grant was checked against, and is then
// tested without the company's token.
//
// Mutation: compare the address under test with the sent entry's own, and the
// token is handed to the new address.
func TestAValueTestSendsTheGrantOnlyToTheAddressThisNodeRuns(t *testing.T) {
	t.Parallel()
	v := newOAuthVendor(t)
	a, b := oauthNodes(t, v, "dcr")
	signIn(t, v, a, b)
	var sent atomic.Value
	var reached atomic.Int32
	sent.Store("")
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		sent.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(elsewhere.Close)

	moved := a.Company().Config.MCPServers[0]
	moved.URL = elsewhere.URL + "/mcp"
	report := a.testServer(t.Context(), &moved, nil, nil, nil, nil, nil, a.testBearer(t.Context(), "vendor"))
	if report.Serving() || reached.Load() == 0 || sent.Load() != "" {
		t.Fatalf("an entry moved ahead of the apply was tested with %q; want no token at the new address",
			sent.Load())
	}

	// THE ADDRESS THIS NODE RUNS is sent the grant's token, as the vendor's
	// server records.
	here := a.Company().Config.MCPServers[0]
	a.testServer(t.Context(), &here, nil, nil, nil, nil, nil, a.testBearer(t.Context(), "vendor"))
	v.mu.Lock()
	presented := slices.Clone(v.presented)
	v.mu.Unlock()
	if !slices.ContainsFunc(presented, func(h string) bool { return strings.HasPrefix(h, "Bearer ") }) {
		t.Fatalf("the address this node runs was presented %q, want the grant's token", presented)
	}
}

// completeMCP finishes an MCP sign-in through e, answering the server it named.
func completeMCP(ctx context.Context, e *Engine, l SignInLanding) (string, error) {
	landed, err := e.CompleteSignIn(ctx, l)
	return landed.Name, err
}

// A SIGN-IN TO A SERVER THAT MOVED TO A SERVICE IDENTITY UNDER IT is refused
// before any code is exchanged, and tells the person there is nothing to do:
// the engine issues that server's tokens itself.
//
// Mutation: drop the service identity's own remedy, and the refusal asks for
// an oauth block the server already has.
func TestASignInToAServerNowAServiceIdentityHasNothingToDo(t *testing.T) {
	t.Parallel()
	v := newOAuthVendor(t)
	a, b := oauthNodes(t, v, "dcr")
	started, err := a.StartMCPSignIn(t.Context(), "vendor", signingJane)
	if err != nil {
		t.Fatal(err)
	}
	code, state := v.authorize(t, started.AuthorizeURL)
	b.epoch.current.Store(companyFor(t, `
name: Acme
mcp_servers:
  - name: vendor
    transport: http
    url: `+v.srv.URL+`/mcp
    oauth:
      grant: client_credentials
      client_id: crewlet-service
      client_secret: ${VENDOR_CLIENT_SECRET}
`))
	_, err = completeMCP(t.Context(), b, SignInLanding{State: state, Code: code, Issuer: v.srv.URL})
	var refused *SignInRefusal
	if !errors.As(err, &refused) || !strings.HasPrefix(refused.Remedy, "Nothing to do") ||
		!strings.Contains(refused.Reason, "service identity") {
		t.Fatalf("the sign-in answered %v, want the service identity's remedy", err)
	}
	if exchanges, _, _ := v.counts(); exchanges != 0 {
		t.Fatalf("the vendor exchanged %d codes for a server that takes no grant", exchanges)
	}
}

// A GRANT THE FLEET COULD NOT RECORD IS REFUSED BEFORE ANYBODY CONSENTS, and
// a scope a vendor grants unasked past what a record keeps is left out of it:
// a registration issuing a client id past coord.MaxGrantAddressBytes, or
// metadata naming a scope to ask for past coord.MaxGrantScopeBytes, is
// refused as the sign-in starts, naming the field, with no code exchanged;
// a token answer granting such a scope beside the one asked for completes
// the consent the person gave, recording what was asked for.
//
// Mutation: drop grantUnkeepable from startSignIn, or keptScopes from the
// record, and its case reaches consent or fails after it.
func TestAGrantTheFleetCouldNotKeepIsRefusedBeforeAnybodyConsents(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, field string
		vendor      func(v *oauthVendor)
	}{
		{"a long client id", "client id", func(v *oauthVendor) {
			v.registered = strings.Repeat("c", coord.MaxGrantAddressBytes+1)
		}},
		{"a long scope to ask for", "scope to ask for", func(v *oauthVendor) {
			v.scopes = []string{"read", strings.Repeat("s", coord.MaxGrantScopeBytes+1)}
		}},
	} {
		v := newOAuthVendor(t)
		c.vendor(v)
		a, _ := oauthNodes(t, v, "dcr")
		_, err := a.StartMCPSignIn(t.Context(), "vendor", signingJane)
		var refusal *SignInRefusal
		if !errors.As(err, &refusal) || refusal.Status != http.StatusConflict ||
			!strings.Contains(refusal.Reason, c.field) || !strings.Contains(refusal.Reason, v.srv.URL) {
			t.Errorf("%s: the sign-in started with %v, want it refused naming the %s and the server",
				c.name, err, c.field)
		}
		if exchanges, _, _ := v.counts(); exchanges != 0 {
			t.Errorf("%s: the vendor exchanged %d codes", c.name, exchanges)
		}
	}

	// THIS ENGINE'S OWN ADDRESS as a cimd client id is named as that, with
	// the setting that shortens it.
	cimd := newOAuthVendor(t)
	a, _ := oauthNodes(t, cimd, "cimd")
	a.webhookBase = "https://crewlet.example.com/" + strings.Repeat("p", coord.MaxGrantAddressBytes)
	_, err := a.StartMCPSignIn(t.Context(), "vendor", signingJane)
	var refusal *SignInRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "client metadata address") ||
		!strings.Contains(refusal.Remedy, "external_url") {
		t.Errorf("a cimd sign-in past the bound started with %v, want it refused naming this engine's address", err)
	}

	// ITS SIGN-IN CALLBACK, which the grant records, likewise.
	long := newOAuthVendor(t)
	a, _ = oauthNodes(t, long, "dcr")
	a.webhookBase = "https://crewlet.example.com/" + strings.Repeat("p", coord.MaxGrantAddressBytes)
	_, err = a.StartMCPSignIn(t.Context(), "vendor", signingJane)
	refusal = nil
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "sign-in callback") ||
		!strings.Contains(refusal.Remedy, "external_url") {
		t.Errorf("a sign-in whose callback is past the bound started with %v, want it refused naming this "+
			"engine's address", err)
	}

	v := newOAuthVendor(t)
	v.granted = "read " + strings.Repeat("s", coord.MaxGrantScopeBytes+1)
	a, b := oauthNodes(t, v, "dcr")
	signIn(t, v, a, b)
	grant, found, err := a.backends.Fleet.OAuthGrant(t.Context(), coord.MCPGrantFamily, "vendor")
	if err != nil || !found || !slices.Equal(grant.Scopes, []string{"read"}) {
		t.Errorf("the grant whose answer added a long scope reads %v (found %t, %v), want it recorded with "+
			"the scope asked for", grant.Scopes, found, err)
	}
}
