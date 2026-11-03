package webhooks_test

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/mcp/oauth"
)

// scriptedSignIns finishes the sign-in whose state is "good" and refuses every
// other, recording what it was handed.
type scriptedSignIns struct {
	mu     sync.Mutex
	landed []webhooks.MCPLanding
	noDoc  bool
}

func (s *scriptedSignIns) CompleteSignIn(_ context.Context, l webhooks.MCPLanding) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.landed = append(s.landed, l)
	if l.State != "good" {
		return "", &webhooks.MCPSignInRefusal{Status: http.StatusBadRequest,
			Reason: "this sign-in link is not one this engine issued, has expired, or was already used",
			Remedy: "Start the sign-in again from the server's page"}
	}
	return "linear", nil
}

func (s *scriptedSignIns) ClientMetadata() (oauth.Metadata, bool) {
	if s.noDoc {
		return oauth.Metadata{}, false
	}
	return oauth.ClientMetadata("https://crewlet.example.com/webhooks/mcp-oauth/client-metadata.json",
		"https://crewlet.example.com", "https://crewlet.example.com/webhooks/mcp-oauth"), true
}

func (s *scriptedSignIns) seen() []webhooks.MCPLanding {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]webhooks.MCPLanding(nil), s.landed...)
}

func mcpLanding(t *testing.T, e *edge, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/webhooks/mcp-oauth?"+query, nil)
	res := httptest.NewRecorder()
	e.mux.ServeHTTP(res, req)
	return res
}

// THE CALLBACK TRUSTS NOTHING BUT THE ENGINE'S VERDICT ON THE STATE, AND SHOWS
// NOTHING IT WAS SENT: a finished sign-in names the server the signed state
// named, a refused one says why in the engine's words, and neither page carries
// the code, the state or what the vendor said; the page is never cached and
// tells the next page nothing of the address it arrived on.
//
// Mutation: render the code or the vendor's description, or drop no-store, and
// this is red.
func TestTheSignInCallbackShowsNothingItWasSent(t *testing.T) {
	t.Parallel()
	signIns := &scriptedSignIns{}
	e := newEdge(t, func(o *webhooks.Options) { o.MCPSignIns = signIns })

	res := mcpLanding(t, e, "code=the-secret-code-0123456789&state=good&iss=https%3A%2F%2Fauth.example.com")
	page := html.UnescapeString(res.Body.String())
	if res.Code != http.StatusOK || !strings.Contains(page, "Signed in to linear") {
		t.Fatalf("a finished sign-in = %d:\n%s", res.Code, page)
	}
	got := signIns.seen()
	if len(got) != 1 || got[0].Code != "the-secret-code-0123456789" || got[0].Issuer != "https://auth.example.com" {
		t.Fatalf("the engine was handed %+v", got)
	}

	res = mcpLanding(t, e, "error=access_denied&error_description=%3Cscript%3Ealert(1)%3C%2Fscript%3E&state=forged")
	page = html.UnescapeString(res.Body.String())
	if res.Code != http.StatusBadRequest || !strings.Contains(page, "not one this engine issued") {
		t.Fatalf("a refused sign-in = %d:\n%s", res.Code, page)
	}
	for _, res := range []*httptest.ResponseRecorder{res,
		mcpLanding(t, e, "code=the-secret-code-0123456789&state=good")} {
		body := res.Body.String()
		for _, sent := range []string{"the-secret-code", "forged", "<script>", "access_denied"} {
			if strings.Contains(body, sent) {
				t.Errorf("the page shows %q, which the address carried:\n%s", sent, body)
			}
		}
		if res.Header().Get("Cache-Control") != "no-store" || res.Header().Get("Referrer-Policy") != "no-referrer" ||
			res.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("the page's headers are %v", res.Header())
		}
	}
}

// A LANDING NO SIGN-IN RETURNS TO IS REFUSED BEFORE THE ENGINE IS ASKED: no
// state, or a value past the bounds a real sign-in stays within.
func TestALandingNoSignInReturnsToIsNeverHandedOn(t *testing.T) {
	t.Parallel()
	signIns := &scriptedSignIns{}
	e := newEdge(t, func(o *webhooks.Options) { o.MCPSignIns = signIns })
	for _, query := range []string{
		"code=abc",
		"code=abc&state=" + strings.Repeat("s", 2<<10),
		"code=" + strings.Repeat("c", 16<<10) + "&state=good",
		"error=" + strings.Repeat("e", 100) + "&state=good",
	} {
		if res := mcpLanding(t, e, query); res.Code != http.StatusBadRequest {
			t.Errorf("%.40s... = %d, want 400", query, res.Code)
		}
	}
	if got := signIns.seen(); len(got) != 0 {
		t.Fatalf("the engine was handed %d landings no sign-in returns to", len(got))
	}
	unwired := newEdge(t)
	if res := mcpLanding(t, unwired, "code=abc&state=good"); res.Code != http.StatusNotImplemented {
		t.Errorf("an engine that finishes no sign-in answered %d", res.Code)
	}
}

// THE CLIENT METADATA DOCUMENT IS PUBLIC AND NAMES THE CALLBACK ALONE: a public
// client (no secret, PKCE its proof) whose one redirect is the engine's
// callback, served where the engine has a public address and nowhere else.
func TestTheClientMetadataDocumentNamesTheCallbackAlone(t *testing.T) {
	t.Parallel()
	signIns := &scriptedSignIns{}
	e := newEdge(t, func(o *webhooks.Options) { o.MCPSignIns = signIns })
	req := httptest.NewRequest(http.MethodGet, "/webhooks/mcp-oauth/client-metadata.json", nil)
	res := httptest.NewRecorder()
	e.mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK || res.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("the document = %d %v", res.Code, res.Header())
	}
	var doc map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["client_id"] != "https://crewlet.example.com/webhooks/mcp-oauth/client-metadata.json" ||
		doc["token_endpoint_auth_method"] != "none" {
		t.Errorf("the document is %v", doc)
	}
	redirects, _ := doc["redirect_uris"].([]any)
	if len(redirects) != 1 || redirects[0] != "https://crewlet.example.com/webhooks/mcp-oauth" {
		t.Errorf("the document's redirects are %v", redirects)
	}
	signIns.noDoc = true
	res = httptest.NewRecorder()
	e.mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/webhooks/mcp-oauth/client-metadata.json", nil))
	if res.Code != http.StatusNotFound {
		t.Errorf("an engine with no public address served a document: %d", res.Code)
	}
}
