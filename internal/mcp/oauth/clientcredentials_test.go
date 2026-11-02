package oauth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/mcp/oauth"
)

// service is the company's own client signing in as a service identity.
var service = oauth.Client{Kind: catalogue.ClientOwn, ID: "crewlet-svc", Secret: "svc-s3cr3t-value"}

// A SERVICE IDENTITY ASKS FOR ITS SERVER AND ITS CONFIGURED SCOPES, AND KEEPS
// NOTHING BUT THE ACCESS TOKEN: the request names the client credentials grant,
// the server as the resource (RFC 8707) and the scopes as configured (none
// where none are), the secret goes by basic authentication alone, and a refresh
// token the answer carries anyway is dropped, since a token that runs out is
// asked for again.
func TestAServiceIdentityAsksForItsServerAndKeepsOnlyTheAccessToken(t *testing.T) {
	t.Parallel()
	srv, v := newVendor(t)
	ep := endpoints(srv.URL)
	v.set("/token", 200, `{"access_token":"at-1","token_type":"bearer","expires_in":3600,`+
		`"refresh_token":"rt-unwanted","refresh_token_expires_in":86400,"scope":"read write"}`)
	tokens, err := oauth.ClientCredentials(t.Context(), ep, service, []string{"read", "write"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.Access != "at-1" || !tokens.Expiry.Equal(now.Add(time.Hour)) {
		t.Fatalf("tokens = %+v", tokens)
	}
	if tokens.Refresh != "" || !tokens.RefreshExpiry.IsZero() {
		t.Fatalf("a service identity kept a refresh token: %+v", tokens)
	}
	form := v.sent("/token")[0]
	if form.Get("grant_type") != "client_credentials" || form.Get("resource") != srv.URL+"/mcp" ||
		form.Get("scope") != "read write" {
		t.Fatalf("the request sent %v", form)
	}
	if form.Get("client_secret") != "" || form.Has("code_verifier") || form.Has("refresh_token") {
		t.Fatalf("the request carried more than the grant: %v", form)
	}
	v.mu.Lock()
	basic := v.auth["/token"][0]
	v.mu.Unlock()
	user, pass, ok := (&http.Request{Header: http.Header{"Authorization": {basic}}}).BasicAuth()
	if !ok || user != "crewlet-svc" || pass != "svc-s3cr3t-value" {
		t.Fatalf("basic credentials = %q %t", user, ok)
	}

	if _, err := oauth.ClientCredentials(t.Context(), ep, service, nil, now); err != nil {
		t.Fatal(err)
	}
	if form := v.sent("/token")[1]; form.Has("scope") {
		t.Fatalf("no configured scope asked for %q, want the client's default", form.Get("scope"))
	}
	if _, err := oauth.ClientCredentials(t.Context(), ep, oauth.Client{ID: "public"}, nil, now); err == nil {
		t.Fatal("a client with no secret was signed in as a service identity")
	}
}

// THE SECRET GOES ONLY BY A METHOD THE SERVER OFFERS: basic where it offers it
// (preferred over the body), the body where that is all it offers, and nowhere
// where it offers neither, which no request reaches the server for.
func TestAServiceIdentitysSecretGoesOnlyByAMethodTheServerOffers(t *testing.T) {
	t.Parallel()
	srv, v := newVendor(t)
	ep := endpoints(srv.URL)
	v.set("/token", 200, `{"access_token":"at","token_type":"Bearer"}`)

	ep.TokenAuthMethods = []string{"client_secret_post", "client_secret_basic"}
	if _, err := oauth.ClientCredentials(t.Context(), ep, service, nil, now); err != nil {
		t.Fatal(err)
	}
	if form := v.sent("/token")[0]; form.Get("client_secret") != "" {
		t.Fatal("a server offering basic was sent the secret in the body")
	}

	ep.TokenAuthMethods = []string{"client_secret_post"}
	if _, err := oauth.ClientCredentials(t.Context(), ep, service, nil, now); err != nil {
		t.Fatal(err)
	}
	if form := v.sent("/token")[1]; form.Get("client_secret") != service.Secret || form.Get("client_id") != service.ID {
		t.Fatalf("a body-only server was sent %v", form)
	}
	v.mu.Lock()
	sentBasic := v.auth["/token"][1]
	v.mu.Unlock()
	if sentBasic != "" {
		t.Fatal("a body-only server was also sent basic credentials")
	}

	ep.TokenAuthMethods = []string{"private_key_jwt", "tls_client_auth"}
	if err := oauth.CheckClientCredentials(ep); err == nil || !strings.Contains(err.Error(), "private_key_jwt") {
		t.Fatalf("a server taking no secret was checked as %v", err)
	}
	_, err := oauth.ClientCredentials(t.Context(), ep, service, nil, now)
	if err == nil || strings.Contains(err.Error(), service.Secret) {
		t.Fatalf("a server taking no secret answered %v", err)
	}
	if n := len(v.sent("/token")); n != 2 {
		t.Fatalf("a server taking no secret was sent a request (%d in all)", n)
	}
}

// A SERVER THAT ISSUES NO TOKEN TO A SERVICE IDENTITY IS REFUSED BEFORE THE
// SECRET IS SENT: metadata naming no token endpoint, or listing its grants
// without client credentials, is refused naming the way out; metadata listing
// no grants is asked, since RFC 8414's silence is common among servers that
// issue them.
func TestAServerThatIssuesNoServiceTokenIsRefusedBeforeTheSecretIsSent(t *testing.T) {
	t.Parallel()
	ep := endpoints("https://mcp.example.com")
	if err := oauth.CheckClientCredentials(ep); err != nil {
		t.Fatalf("metadata listing no grants: %v", err)
	}
	ep.GrantTypes = []string{"authorization_code", "client_credentials"}
	if err := oauth.CheckClientCredentials(ep); err != nil {
		t.Fatalf("metadata listing client credentials: %v", err)
	}
	ep.GrantTypes = []string{"authorization_code", "refresh_token"}
	if err := oauth.CheckClientCredentials(ep); err == nil || !strings.Contains(err.Error(), "authorization_code") {
		t.Fatalf("metadata without client credentials: %v", err)
	}
	ep.GrantTypes, ep.Token = nil, ""
	if err := oauth.CheckClientCredentials(ep); err == nil {
		t.Fatal("metadata naming no token endpoint was accepted")
	}
	if err := oauth.CheckClientCredentials(nil); err == nil {
		t.Fatal("unread metadata was accepted")
	}
}

// THE SECRET NEVER REACHES A TOKEN ENDPOINT THE GUARD REFUSES: one the metadata
// points at an internal address on another origin, or at plain http there, is
// sent nothing, and no error carries the secret.
func TestAServiceIdentitysSecretNeverReachesAnAddressTheGuardRefuses(t *testing.T) {
	t.Parallel()
	var reached atomic.Int32
	internal := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Add(1)
	}))
	t.Cleanup(internal.Close)
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Add(1)
	}))
	t.Cleanup(plain.Close)

	ep := endpoints("https://mcp.example.com")
	ep.Token = internal.URL + "/token"
	_, err := oauth.ClientCredentials(t.Context(), ep, service, nil, now)
	if !errors.Is(err, httpx.ErrNotPublic) || strings.Contains(err.Error(), service.Secret) {
		t.Fatalf("an internal token endpoint answered %v, want the guard's refusal", err)
	}
	ep.Token = plain.URL + "/token"
	_, err = oauth.ClientCredentials(t.Context(), ep, service, nil, now)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("a plain-http token endpoint on another origin answered %v", err)
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("a token endpoint the guard refuses was sent %d requests", n)
	}
}

// A VENDOR'S REFUSAL OF A SERVICE IDENTITY IS ITS OWN WORDS, BOUNDED AND
// REDACTED, the secret withheld wherever the vendor echoes it although it looks
// like no credential, and tells a credential it will never take from a wait
// that may pass.
func TestAServiceIdentitysRefusalIsTheVendorsWordsAndNeverTheSecret(t *testing.T) {
	t.Parallel()
	srv, v := newVendor(t)
	ep := endpoints(srv.URL)
	v.set("/token", 401, `{"error":"invalid_client","error_description":"client `+
		url.QueryEscape(service.ID)+` presented svc-s3cr3t-value, which is wrong. `+
		strings.Repeat("check the client ", 80)+`"}`)
	_, err := oauth.ClientCredentials(t.Context(), ep, service, nil, now)
	var refused *oauth.Error
	if !errors.As(err, &refused) || refused.Code != "invalid_client" || !refused.Definitive() {
		t.Fatalf("err = %v", err)
	}
	if len(refused.Description) > 512 {
		t.Fatalf("the description is %d bytes", len(refused.Description))
	}
	if strings.Contains(err.Error(), service.Secret) || !strings.Contains(refused.Description, "[withheld]") {
		t.Fatalf("the refusal quotes the secret the vendor echoed: %q", refused.Description)
	}
	v.set("/token", 429, `{"error":"slow_down"}`)
	_, err = oauth.ClientCredentials(t.Context(), ep, service, nil, now)
	if !errors.As(err, &refused) || refused.Definitive() || refused.Status != 429 {
		t.Fatalf("a rate limit was read as %v", err)
	}
}
