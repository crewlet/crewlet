package mcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp/catalogue"
)

// signInServer is a remote server that wants a Bearer token, with its
// protected-resource metadata and its authorization server's metadata on its
// own origin, each answered as the case writes it.
type signInServer struct {
	srv       *httptest.Server
	challenge func(base string) string
	prm       func(base string) string
	asm       func(base string) string
	hits      atomic.Int32
}

func newSignInServer(t *testing.T) *signInServer {
	t.Helper()
	s := &signInServer{
		challenge: func(base string) string {
			return `Bearer resource_metadata="` + base + `/.well-known/oauth-protected-resource/mcp", scope="read"`
		},
		prm: func(base string) string {
			return `{"resource":"` + base + `/mcp","authorization_servers":["` + base + `"],"scopes_supported":["read","write"]}`
		},
		asm: func(base string) string {
			return `{"issuer":"` + base + `","authorization_endpoint":"` + base + `/authorize",` +
				`"registration_endpoint":"` + base + `/register","code_challenge_methods_supported":["S256"]}`
		},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, s.prm(base))
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, s.asm(base))
		case "/mcp":
			w.Header().Set("WWW-Authenticate", s.challenge(base))
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// AN ADDRESS THAT LISTS ITS TOOLS TO NO CREDENTIAL needs no sign-in.
func TestAnAddressThatListsToNoCredentialNeedsNoSignIn(t *testing.T) {
	t.Parallel()
	srv, _ := newHTTPMCPServer(t, `[{"name":"search","inputSchema":{"type":"object"}}]`)
	check := CheckAddress(t.Context(), srv.URL, discardLogger())
	if check.Auth != catalogue.AuthNone || check.OAuth != nil || len(check.Test.Tools) != 1 {
		t.Fatalf("check = %+v", check)
	}
}

// AN ADDRESS THAT CHALLENGES FOR A BEARER TOKEN is read for how it is signed
// in to: its metadata on its own origin, the operator's, read as the address
// is.
func TestAnAddressThatChallengesIsReadForItsSignIn(t *testing.T) {
	t.Parallel()
	s := newSignInServer(t)
	check := CheckAddress(t.Context(), s.srv.URL+"/mcp", discardLogger())
	if check.Auth != catalogue.AuthOAuth || check.OAuth == nil {
		t.Fatalf("check = %+v", check)
	}
	o := check.OAuth
	if o.Failure != nil {
		t.Fatalf("discovery failed: %+v", o.Failure)
	}
	if o.Issuer != s.srv.URL || o.Client != catalogue.ClientDCR || !o.PKCE ||
		strings.Join(o.Scopes, ",") != "read,write" || o.ResourceMetadata == "" {
		t.Errorf("discovery = %+v", o)
	}
}

// AN ADDRESS CHECK FOLLOWS WHAT THE SERVER NAMES ONLY THROUGH THE GUARD: a
// metadata address on another origin that is not public is refused before a
// request reaches it (blocked_address), one that is not https is not fetched
// at all, and a metadata address that redirects is not followed.
func TestAnAddressCheckFollowsWhatTheServerNamesOnlyThroughTheGuard(t *testing.T) {
	t.Parallel()
	var reached atomic.Int32
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"resource":"x","authorization_servers":["x"]}`)
	}))
	t.Cleanup(elsewhere.Close)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
	}))
	t.Cleanup(plain.Close)

	for _, tc := range []struct {
		name  string
		named string
		class FailureClass
		said  string
	}{
		{"an internal address on another origin", elsewhere.URL + "/prm", FailBlockedAddress, "public internet"},
		{"plain http on another origin", plain.URL + "/prm", FailNotMCP, "https"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSignInServer(t)
			s.challenge = func(string) string { return `Bearer resource_metadata="` + tc.named + `"` }
			check := CheckAddress(t.Context(), s.srv.URL+"/mcp", discardLogger())
			if check.OAuth == nil || check.OAuth.Failure == nil || check.OAuth.Failure.Class != tc.class {
				t.Fatalf("discovery = %+v, want class %q", check.OAuth, tc.class)
			}
			if !strings.Contains(check.OAuth.Failure.Said, tc.said) {
				t.Errorf("failure = %q, want it to say %q", check.OAuth.Failure.Said, tc.said)
			}
		})
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("an address the server named on another origin was sent %d requests", n)
	}

	// A REDIRECT IS NOT FOLLOWED, even on the server's own origin.
	s := newSignInServer(t)
	s.prm = func(string) string { return "" }
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://`+r.Host+`/prm"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, s.srv.URL+"/.well-known/oauth-protected-resource/mcp", http.StatusFound)
	}))
	t.Cleanup(redirecting.Close)
	before := s.hits.Load()
	check := CheckAddress(t.Context(), redirecting.URL+"/mcp", discardLogger())
	if check.OAuth == nil || check.OAuth.Failure == nil || !strings.Contains(check.OAuth.Failure.Said, "redirect") {
		t.Fatalf("discovery = %+v, want the redirect refused", check.OAuth)
	}
	if s.hits.Load() != before {
		t.Fatal("the redirect a metadata address answered was followed")
	}
}

// A METADATA DOCUMENT IS READ TO ITS BOUND AND FOR ITS OWN ISSUER: a larger
// one is refused, and authorization server metadata naming another issuer is
// not used.
func TestAMetadataDocumentIsBoundedAndHeldToItsIssuer(t *testing.T) {
	t.Parallel()
	large := newSignInServer(t)
	large.prm = func(string) string { return `{"resource":"` + strings.Repeat("x", maxMetadataBody) + `"}` }
	check := CheckAddress(t.Context(), large.srv.URL+"/mcp", discardLogger())
	if check.OAuth == nil || check.OAuth.Failure == nil || !strings.Contains(check.OAuth.Failure.Said, "KiB") {
		t.Fatalf("an oversized document: %+v", check.OAuth)
	}

	other := newSignInServer(t)
	other.asm = func(string) string { return `{"issuer":"https://issuer.example.com"}` }
	check = CheckAddress(t.Context(), other.srv.URL+"/mcp", discardLogger())
	if check.OAuth == nil || check.OAuth.Failure == nil || check.OAuth.Issuer != "" {
		t.Fatalf("metadata naming another issuer was used: %+v", check.OAuth)
	}
}

// A CHALLENGE'S PARAMETERS are read as RFC 9110 writes them, quoted or not.
func TestAChallengesParametersAreRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ challenge, want string }{
		{`Bearer resource_metadata="https://a.example/prm"`, "https://a.example/prm"},
		{`Bearer realm="x", resource_metadata="https://a.example/prm", scope="a b"`, "https://a.example/prm"},
		{`Bearer error=invalid_token, resource_metadata=https://a.example/prm`, "https://a.example/prm"},
		{`Basic realm="x"`, ""},
		{`Bearer realm="unterminated`, ""},
	} {
		if got := challengeParam(tc.challenge, "resource_metadata"); got != tc.want {
			t.Errorf("challengeParam(%q) = %q, want %q", tc.challenge, got, tc.want)
		}
	}
}

// A SIGN-IN'S ENDPOINTS ARE READ BY THE SAME CHECK, as published: the
// addresses a sign-in is made at, which no answer carries, beside the
// redacted words one does.
func TestAnAddressCheckReadsTheEndpointsASignInIsMadeAt(t *testing.T) {
	t.Parallel()
	s := newSignInServer(t)
	s.asm = func(base string) string {
		return `{"issuer":"` + base + `","authorization_endpoint":"` + base + `/authorize",` +
			`"token_endpoint":"` + base + `/token","revocation_endpoint":"` + base + `/revoke",` +
			`"client_id_metadata_document_supported":true,"code_challenge_methods_supported":["S256"],` +
			`"authorization_response_iss_parameter_supported":true,"scopes_supported":["read","offline_access"]}`
	}
	check := CheckAddress(t.Context(), s.srv.URL+"/mcp", discardLogger())
	ep := check.OAuth.Endpoints
	if check.OAuth.Failure != nil || ep == nil {
		t.Fatalf("discovery = %+v", check.OAuth)
	}
	base := s.srv.URL
	if ep.Server != base+"/mcp" || ep.Resource != base+"/mcp" || ep.Issuer != base ||
		ep.Authorization != base+"/authorize" || ep.Token != base+"/token" ||
		ep.Revocation != base+"/revoke" || !ep.ClientIDMetadataDocument || !ep.IssuerInResponse ||
		!ep.OfflineAccess || !ep.ResourceScopes || strings.Join(ep.Scopes, ",") != "read,write" {
		t.Errorf("endpoints = %+v", ep)
	}
}

// THE RESOURCE IS THE SERVER'S OWN: metadata naming another origin, or a path
// that is not the server's or one above it, is refused before anything in it
// is used, so no token is asked for, or bound to, somebody else's server.
func TestAResourceIsTheServersOwn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		resource, server string
		want             bool
	}{
		{"https://mcp.example.com/mcp", "https://mcp.example.com/mcp", true},
		{"https://mcp.example.com", "https://mcp.example.com/mcp", true},
		{"https://mcp.example.com/", "https://mcp.example.com/v1/mcp", true},
		{"https://MCP.example.com:443/mcp/", "https://mcp.example.com/mcp", true},
		{"https://mcp.example.com/other", "https://mcp.example.com/mcp", false},
		{"https://mcp.example.com/mc", "https://mcp.example.com/mcp", false},
		{"https://evil.example.com/mcp", "https://mcp.example.com/mcp", false},
		{"http://mcp.example.com/mcp", "https://mcp.example.com/mcp", false},
		{"https://mcp.example.com/mcp?tenant=x", "https://mcp.example.com/mcp", false},
		{"", "https://mcp.example.com/mcp", false},
	} {
		if got := ResourceServes(tc.resource, tc.server); got != tc.want {
			t.Errorf("ResourceServes(%q, %q) = %t, want %t", tc.resource, tc.server, got, tc.want)
		}
	}
	s := newSignInServer(t)
	s.prm = func(base string) string {
		return `{"resource":"https://evil.example.com/mcp","authorization_servers":["` + base + `"]}`
	}
	check := CheckAddress(t.Context(), s.srv.URL+"/mcp", discardLogger())
	if check.OAuth == nil || check.OAuth.Failure == nil || check.OAuth.Endpoints != nil ||
		!strings.Contains(check.OAuth.Failure.Said, "not the server") {
		t.Fatalf("metadata naming another resource = %+v", check.OAuth)
	}
}
