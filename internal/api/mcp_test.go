package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/tools"
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
	url := bridge.Open(&mcpbridge.Session{
		RunID: "run-1", Handle: "dev", Role: "Dev",
		Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil),
	})
	if url == "" {
		t.Fatal("no endpoint was minted")
	}
	token := url[strings.LastIndex(url, "/")+1:]
	a := newApp(t, api.Options{Bridge: bridge})

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
