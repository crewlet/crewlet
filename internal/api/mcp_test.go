package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The bridge route on the App. What the bridge itself does with a call is
// internal/api/mcpbridge's; what is tested here is that the route exists,
// answers every verb the transport uses, and is reachable WITHOUT the API's
// own bearer token — because the caller is inside a sandbox and holds none.

// A NIL BRIDGE MEANS THE ROUTE IS ABSENT, not present-and-refusing. An
// endpoint that exists and 503s everything reads to an operator as broken,
// while one that is not there matches what the config says.
func TestTheBridgeRouteIsAbsentWithoutABridge(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})
	res := probe(a, http.MethodPost, mcpbridge.PathPrefix+"anything")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

// THE TOKEN IN THE PATH IS THE WHOLE CREDENTIAL. A box holds no bearer token,
// and giving it one would hand a sandbox the credential that reads the company
// — so an unminted token must reach the bridge's own 401 rather than the auth
// middleware's.
func TestTheBridgeRouteIsReachableWithoutABearerToken(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{
		Bridge: mcpbridge.New(mcpbridge.Options{Key: []byte("k"), BaseURL: "http://x"}),
	})
	res := probe(a, http.MethodPost, mcpbridge.PathPrefix+"not-a-token")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the bridge's own 401", res.StatusCode)
	}
}

// EVERY VERB THE TRANSPORT USES. Streamable HTTP is a GET for the
// server-to-client stream and a DELETE to end a session; a pattern naming only
// POST answers 405 to the other two, which an MCP client reports as a
// transport that does not support streaming rather than as a misregistered
// route.
func TestTheBridgeRouteAnswersEveryTransportVerb(t *testing.T) {
	t.Parallel()
	bridge := mcpbridge.New(mcpbridge.Options{Key: []byte("k"), BaseURL: "http://x"})
	// The app first: mounting the route is what lets the bridge open a
	// session at all.
	a := newApp(t, api.Options{Bridge: bridge})
	url := bridge.Open(&mcpbridge.Session{
		RunID: "run-1", Handle: "dev", Role: "Dev",
		Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil),
	})
	if url == "" {
		t.Fatal("no endpoint was minted")
	}
	token := url[strings.LastIndex(url, "/")+1:]

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		res := probe(a, method, mcpbridge.PathPrefix+token)
		if res.StatusCode == http.StatusMethodNotAllowed || res.StatusCode == http.StatusNotFound {
			t.Errorf("%s = %d — the route does not serve this verb", method, res.StatusCode)
		}
	}
}

// probe drives one request through the app's own handler, exactly as a box
// would reach it.
func probe(a *api.App, method, path string) *http.Response {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec.Result()
}

// THE ACT ROUTE IS MOUNTED BESIDE THE MCP ONE, behind the same guard.
//
// What the transport does with a request is internal/api/operator's; what is
// asserted here is the wiring the app owns: the route exists wherever the
// operator surface does, answers POST alone, and is ALWAYS guarded — a
// request with no token is refused by the guard before it reaches the
// handler, whatever the read posture, because every tool it serves writes.
func TestTheActRouteIsMountedBehindTheGuard(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	b.API.Auth.AllowAnonymousRead = true
	a := newApp(t, api.Options{
		Bootstrap: &b,
		Runtime:   &fakeRuntime{},
		Operator: operatorSurface(t, operator.Options{
			Work: builtin.WorkDeps{Reader: stubWorkReader{}, Actor: operator.WorkActor(nil)},
		}),
	})
	post := func(token string) (int, string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, operator.ActPathPrefix+tracker.ListWorkItemsTool,
			strings.NewReader(`{"request_id":"0f7c1a4e-9b2d-4e51-8c3a-6d7e8f9a0b1c","args":{}}`))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, r)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		code, _ := body["error"].(string)
		return rec.Code, code
	}
	if status, code := post(""); status != http.StatusUnauthorized ||
		code != string(httpjson.CodeInvalidToken) {

		t.Errorf("an act with no token answered %d %q, want the guard's 401 "+
			"even with anonymous reads open", status, code)
	}
	// THE HANDLER IS REACHED with a token: the read it names is refused by
	// the transport's own rule, which only the mounted handler can answer.
	if status, code := post("secret"); status != http.StatusBadRequest ||
		code != string(operator.CodeReadOnlyTool) {

		t.Errorf("an authenticated act answered %d %q, want the transport's own "+
			"read_only_tool — the route is not mounted", status, code)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, operator.ActPathPrefix+tracker.ListWorkItemsTool, nil)
	r.Header.Set("Authorization", "Bearer secret")
	a.ServeHTTP(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on the act route answered %d, want 405: it serves POST alone", rec.Code)
	}
}
