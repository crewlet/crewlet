package oauth_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/mcp/oauth"
)

var now = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// vendor is an authorization server on the MCP server's own origin: it
// records every form its endpoints are sent and answers what the case set.
type vendor struct {
	mu     sync.Mutex
	forms  map[string][]url.Values
	auth   map[string][]string
	answer map[string]func(w http.ResponseWriter)
}

func newVendor(t *testing.T) (*httptest.Server, *vendor) {
	t.Helper()
	v := &vendor{forms: map[string][]url.Values{}, auth: map[string][]string{},
		answer: map[string]func(http.ResponseWriter){}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			form = url.Values{"json": {string(body)}}
		}
		v.mu.Lock()
		v.forms[r.URL.Path] = append(v.forms[r.URL.Path], form)
		v.auth[r.URL.Path] = append(v.auth[r.URL.Path], r.Header.Get("Authorization"))
		answer := v.answer[r.URL.Path]
		v.mu.Unlock()
		if answer == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		answer(w)
	}))
	t.Cleanup(srv.Close)
	return srv, v
}

func (v *vendor) set(path string, status int, body string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.answer[path] = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func (v *vendor) sent(path string) []url.Values {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]url.Values(nil), v.forms[path]...)
}

func endpoints(base string) *mcp.OAuthEndpoints {
	return &mcp.OAuthEndpoints{
		Server: base + "/mcp", Resource: base + "/mcp", Issuer: base,
		Authorization: base + "/authorize", Token: base + "/token",
		Registration: base + "/register", Revocation: base + "/revoke",
		ClientIDMetadataDocument: true, PKCEMethods: []string{"S256"},
	}
}

// PKCE S256 ONLY: a proof key is 256 bits as 43 base64url characters, its
// challenge is the verifier's SHA-256, every authorization address carries
// S256, and an authorization server that does not offer S256 is refused
// before a person is sent anywhere, `plain` included.
func TestOnlyPKCES256IsEverSentAndAServerWithoutItIsRefused(t *testing.T) {
	t.Parallel()
	key, err := oauth.NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(key.Verifier) != 43 || strings.Trim(key.Verifier,
		"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
		t.Fatalf("verifier %q is not 43 base64url characters", key.Verifier)
	}
	// RFC 7636, appendix B.
	if got := oauth.ChallengeOf("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got !=
		"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("challenge = %q, want RFC 7636's", got)
	}
	again, _ := oauth.NewPKCE()
	if again.Verifier == key.Verifier {
		t.Fatal("two proof keys are the same")
	}

	ep := endpoints("https://mcp.example.com")
	raw, err := oauth.AuthorizeURL(ep, oauth.Client{ID: "https://crewlet.example.com/client"},
		"https://crewlet.example.com/webhooks/mcp-oauth", "signed-state", key.Challenge,
		[]string{"read", "offline_access"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	for name, want := range map[string]string{
		"response_type": "code", "code_challenge_method": "S256", "code_challenge": key.Challenge,
		"state": "signed-state", "resource": "https://mcp.example.com/mcp",
		"redirect_uri": "https://crewlet.example.com/webhooks/mcp-oauth",
		"scope":        "read offline_access", "client_id": "https://crewlet.example.com/client",
	} {
		if q.Get(name) != want {
			t.Errorf("the authorization address's %s = %q, want %q", name, q.Get(name), want)
		}
	}
	if strings.Contains(raw, key.Verifier) {
		t.Fatal("the authorization address carries the verifier")
	}

	for _, methods := range [][]string{nil, {"plain"}, {"plain", "s256"}} {
		ep := endpoints("https://mcp.example.com")
		ep.PKCEMethods = methods
		if err := oauth.CheckSignIn(ep, catalogue.ClientCIMD); err == nil ||
			!strings.Contains(err.Error(), "S256") {
			t.Errorf("a server offering %v was not refused for S256: %v", methods, err)
		}
	}
	if err := oauth.CheckSignIn(endpoints("https://mcp.example.com"), catalogue.ClientCIMD); err != nil {
		t.Fatalf("a server offering S256 was refused: %v", err)
	}
}

// A REGISTRATION KIND THE SERVER DOES NOT SUPPORT IS REFUSED, naming the kind
// to configure instead.
func TestARegistrationKindTheServerDoesNotSupportIsRefused(t *testing.T) {
	t.Parallel()
	ep := endpoints("https://mcp.example.com")
	ep.ClientIDMetadataDocument, ep.Registration = false, ""
	if err := oauth.CheckSignIn(ep, catalogue.ClientCIMD); err == nil || !strings.Contains(err.Error(), "dcr") {
		t.Errorf("cimd at a server reading no document: %v", err)
	}
	if err := oauth.CheckSignIn(ep, catalogue.ClientDCR); err == nil || !strings.Contains(err.Error(), "cimd") {
		t.Errorf("dcr at a server offering no registration: %v", err)
	}
	if err := oauth.CheckSignIn(ep, catalogue.ClientOwn); err != nil {
		t.Errorf("the company's own client was refused: %v", err)
	}
	ep.Authorization = "javascript:alert(1)"
	if _, err := oauth.AuthorizeURL(ep, oauth.Client{ID: "c"}, "r", "s", "c", nil); err == nil {
		t.Error("an authorization endpoint that is no https address was handed to a browser")
	}
}

// AN EXCHANGE SENDS THE VERIFIER, THE CALLBACK AND THE RESOURCE, and keeps the
// grant only where it carries a refresh token: one with nothing to renew it is
// refused rather than kept for an hour.
func TestAnExchangeSendsTheVerifierAndKeepsOnlyARenewableGrant(t *testing.T) {
	t.Parallel()
	srv, v := newVendor(t)
	ep := endpoints(srv.URL)
	v.set("/token", 200, `{"access_token":"at-1","token_type":"bearer","expires_in":3600,
		"refresh_token":"rt-1","refresh_token_expires_in":7776000,"scope":"read write"}`)
	got, err := oauth.Exchange(t.Context(), ep, oauth.Client{ID: "cid"}, "the-code", "the-verifier",
		"https://crewlet.example.com/webhooks/mcp-oauth", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Access != "at-1" || got.Refresh != "rt-1" || !got.Expiry.Equal(now.Add(time.Hour)) ||
		!got.RefreshExpiry.Equal(now.Add(90*24*time.Hour)) || strings.Join(got.Scopes, " ") != "read write" {
		t.Fatalf("tokens = %+v", got)
	}
	form := v.sent("/token")[0]
	for name, want := range map[string]string{
		"grant_type": "authorization_code", "code": "the-code", "code_verifier": "the-verifier",
		"resource": srv.URL + "/mcp", "client_id": "cid",
		"redirect_uri": "https://crewlet.example.com/webhooks/mcp-oauth",
	} {
		if form.Get(name) != want {
			t.Errorf("the exchange sent %s=%q, want %q", name, form.Get(name), want)
		}
	}

	v.set("/token", 200, `{"access_token":"at-2","token_type":"Bearer","expires_in":3600}`)
	if _, err := oauth.Exchange(t.Context(), ep, oauth.Client{ID: "cid"}, "c", "v", "r",
		now); !errors.Is(err, oauth.ErrNoRefreshToken) {
		t.Fatalf("a grant with no refresh token was kept: %v", err)
	}
	v.set("/token", 200, `{"access_token":"at-3","token_type":"mac","refresh_token":"rt"}`)
	if _, err := oauth.Exchange(t.Context(), ep, oauth.Client{ID: "cid"}, "c", "v", "r", now); err == nil {
		t.Fatal("a token that is not a Bearer token was kept")
	}
}

// A REFUSAL IS THE VENDOR'S OWN WORDS, BOUNDED AND REDACTED, and only a refusal
// waiting does not end is read as the grant being gone.
func TestARefusalIsTheVendorsWordsBoundedAndRedacted(t *testing.T) {
	t.Parallel()
	srv, v := newVendor(t)
	ep := endpoints(srv.URL)
	long := strings.Repeat("the grant was revoked ", 100)
	v.set("/token", 400, `{"error":"invalid_grant","error_description":"token `+
		`ghp_0123456789abcdefghijklmnopqrstuvwxyzAB was revoked. `+long+`"}`)
	_, err := oauth.Refresh(t.Context(), ep, oauth.Client{ID: "cid"}, "rt-old", now)
	var refused *oauth.Error
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want an *oauth.Error", err)
	}
	if refused.Code != "invalid_grant" || !refused.Definitive() || refused.Status != 400 {
		t.Fatalf("refusal = %+v", refused)
	}
	if strings.Contains(refused.Description, "ghp_0123456789") {
		t.Fatalf("the description carries the credential the vendor quoted: %q", refused.Description)
	}
	if len(refused.Description) > 512 {
		t.Fatalf("the description is %d bytes", len(refused.Description))
	}
	if form := v.sent("/token")[0]; form.Get("grant_type") != "refresh_token" ||
		form.Get("refresh_token") != "rt-old" || form.Get("resource") != srv.URL+"/mcp" {
		t.Fatalf("the renewal sent %v", form)
	}
	if strings.Contains(err.Error(), "rt-old") {
		t.Fatalf("the refusal quotes the refresh token sent: %v", err)
	}

	v.set("/token", 503, `upstream down`)
	_, err = oauth.Refresh(t.Context(), ep, oauth.Client{ID: "cid"}, "rt-old", now)
	if !errors.As(err, &refused) || refused.Definitive() {
		t.Fatalf("a 503 was read as the grant being gone: %v", err)
	}
}

// A CONFIDENTIAL CLIENT AUTHENTICATES BY BASIC unless the server offers only
// the body, and a public client names itself in the body alone.
func TestAClientAuthenticatesAsTheServerOffers(t *testing.T) {
	t.Parallel()
	srv, v := newVendor(t)
	ep := endpoints(srv.URL)
	v.set("/token", 200, `{"access_token":"at","token_type":"Bearer","refresh_token":"rt"}`)
	own := oauth.Client{Kind: catalogue.ClientOwn, ID: "id:with%", Secret: "s3cr3t"}
	if _, err := oauth.Refresh(t.Context(), ep, own, "rt", now); err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	basic := v.auth["/token"][0]
	v.mu.Unlock()
	req := http.Request{Header: http.Header{"Authorization": {basic}}}
	user, pass, ok := req.BasicAuth()
	if !ok || user != url.QueryEscape("id:with%") || pass != "s3cr3t" {
		t.Fatalf("basic credentials = %q %q %t", user, pass, ok)
	}
	if form := v.sent("/token")[0]; form.Get("client_secret") != "" {
		t.Fatal("a basic client also sent its secret in the body")
	}
	ep.TokenAuthMethods = []string{"client_secret_post"}
	if _, err := oauth.Refresh(t.Context(), ep, own, "rt", now); err != nil {
		t.Fatal(err)
	}
	if form := v.sent("/token")[1]; form.Get("client_secret") != "s3cr3t" || form.Get("client_id") != "id:with%" {
		t.Fatalf("a post client sent %v", form)
	}
}

// EVERY ENDPOINT ON ANOTHER ORIGIN IS REACHED THROUGH THE GUARD, over https
// alone: a token endpoint the metadata points at an internal address is never
// sent the code, the verifier or a refresh token.
func TestAnEndpointOnAnotherOriginIsReachedOnlyThroughTheGuard(t *testing.T) {
	t.Parallel()
	var reached atomic.Int32
	internal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
	}))
	t.Cleanup(internal.Close)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
	}))
	t.Cleanup(plain.Close)

	ep := endpoints("https://mcp.example.com")
	ep.Token = internal.URL + "/token"
	_, err := oauth.Exchange(t.Context(), ep, oauth.Client{ID: "cid"}, "code", "verifier", "r", now)
	if !errors.Is(err, httpx.ErrNotPublic) {
		t.Fatalf("an internal token endpoint answered %v, want the guard's refusal", err)
	}
	ep.Token = plain.URL + "/token"
	if _, err := oauth.Refresh(t.Context(), ep, oauth.Client{ID: "cid"}, "rt", now); err == nil ||
		!strings.Contains(err.Error(), "https") {
		t.Fatalf("a plain-http token endpoint on another origin answered %v", err)
	}
	ep.Revocation = internal.URL + "/revoke"
	if _, err := oauth.Revoke(t.Context(), ep, oauth.Client{ID: "cid"}, "rt", "refresh_token"); !errors.Is(err,
		httpx.ErrNotPublic) {
		t.Fatalf("an internal revocation endpoint answered %v", err)
	}
	ep.Registration = internal.URL + "/register"
	if _, err := oauth.Register(t.Context(), ep, oauth.ClientMetadata("", "b", "r")); !errors.Is(err,
		httpx.ErrNotPublic) {
		t.Fatalf("an internal registration endpoint answered %v", err)
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("an endpoint the guard refuses was sent %d requests", n)
	}
}

// A REVOCATION NAMES THE TOKEN AND ITS KIND, and a server that publishes no
// revocation endpoint is told apart from one that revoked.
func TestARevocationIsSentWhereTheServerOffersOne(t *testing.T) {
	t.Parallel()
	srv, v := newVendor(t)
	ep := endpoints(srv.URL)
	v.set("/revoke", 200, ``)
	revoked, err := oauth.Revoke(t.Context(), ep, oauth.Client{ID: "cid"}, "rt-1", "refresh_token")
	if err != nil || !revoked {
		t.Fatalf("Revoke = %t, %v", revoked, err)
	}
	if form := v.sent("/revoke")[0]; form.Get("token") != "rt-1" || form.Get("token_type_hint") != "refresh_token" {
		t.Fatalf("the revocation sent %v", form)
	}
	ep.Revocation = ""
	if revoked, err := oauth.Revoke(t.Context(), ep, oauth.Client{ID: "cid"}, "rt-1", "refresh_token"); err != nil ||
		revoked {
		t.Fatalf("a server with no revocation endpoint answered revoked=%t (%v)", revoked, err)
	}
}

// A REGISTRATION SENDS A PUBLIC CLIENT'S METADATA, and keeps what it was
// issued.
func TestARegistrationSendsAPublicClientsMetadata(t *testing.T) {
	t.Parallel()
	srv, v := newVendor(t)
	ep := endpoints(srv.URL)
	v.set("/register", 201, `{"client_id":"issued-1","client_secret":"issued-secret"}`)
	doc := oauth.ClientMetadata("", "https://crewlet.example.com", "https://crewlet.example.com/webhooks/mcp-oauth")
	got, err := oauth.Register(t.Context(), ep, doc)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "issued-1" || got.Secret != "issued-secret" || got.Kind != catalogue.ClientDCR {
		t.Fatalf("registered = %+v", got)
	}
	var sent oauth.Metadata
	if err := json.Unmarshal([]byte(v.sent("/register")[0].Get("json")), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.TokenEndpointAuthMethod != "none" || sent.ClientID != "" ||
		strings.Join(sent.RedirectURIs, ",") != "https://crewlet.example.com/webhooks/mcp-oauth" ||
		strings.Join(sent.GrantTypes, ",") != "authorization_code,refresh_token" {
		t.Fatalf("the registration sent %+v", sent)
	}
}

// THE SCOPES ASKED FOR are the configured ones, else the server's own, with
// offline_access beside them where the authorization server lists it.
func TestTheScopesAskedForAreTheConfiguredOrTheServersOwn(t *testing.T) {
	t.Parallel()
	ep := endpoints("https://mcp.example.com")
	ep.Scopes, ep.ResourceScopes, ep.OfflineAccess = []string{"read", "write"}, true, true
	if got := strings.Join(oauth.Scopes(nil, ep), " "); got != "read write offline_access" {
		t.Errorf("no configured scopes asked for %q", got)
	}
	if got := strings.Join(oauth.Scopes([]string{"read"}, ep), " "); got != "read offline_access" {
		t.Errorf("configured scopes asked for %q", got)
	}
	ep.ResourceScopes, ep.OfflineAccess = false, false
	if got := oauth.Scopes(nil, ep); len(got) != 0 {
		t.Errorf("an authorization server's own list was asked for whole: %q", got)
	}
}
