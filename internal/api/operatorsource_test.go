package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
)

// THE OPERATOR'S MCP SURFACE ASKS ITS SOURCE ON EVERY REQUEST, AND SAYS WHICH
// OF ITS TWO ABSENCES IT IS.
//
// The surface is the native halves', which a node meets at its first company
// — by an apply, long after its API started serving. It was a server taken
// ONCE when the API was wired, so a node that booted with no company served
// no /operator/mcp for the life of the process. Now the route is mounted once
// and each request reads the source: before the company it is `503
// no_active_revision` with the reconcile poll as its wait and the halves'
// words, and once the company has come up keeping nothing this surface could
// manage it is the route's absence, in the mux's own `404 no_route` — one
// App, the same route, told apart only by what the source says NOW.
//
// Mutation: read the source once, when the route is mounted, and the second
// request is the first one's 503 again.
func TestTheOperatorSurfaceReadsItsSourcePerRequest(t *testing.T) {
	t.Parallel()
	var started atomic.Bool
	a := newApp(t, api.Options{
		Operator: func() (*opsmcp.Server, bool) { return nil, started.Load() },
	})
	post := func() (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodPost, opsmcp.Path,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))))
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("the answer is not the engine's envelope: %v (%s)", err,
				rec.Body.String())
		}
		return rec, body
	}

	rec, body := post()
	if rec.Code != http.StatusServiceUnavailable ||
		body["error"] != string(httpjson.CodeNoActiveRevision) ||
		body["detail"] != httpjson.NativeHalvesNotUp {
		t.Errorf("before the company, %s answered %d %v, want 503 %s in the "+
			"halves' words", opsmcp.Path, rec.Code, body, httpjson.CodeNoActiveRevision)
	}
	poll := fmt.Sprint(httpjson.RetrySeconds(httpjson.NoActiveRevisionRetry))
	if got := rec.Header().Get("Retry-After"); got != poll {
		t.Errorf("before the company, Retry-After = %q, want the reconcile "+
			"poll's %s", got, poll)
	}

	started.Store(true)
	rec, body = post()
	if rec.Code != http.StatusNotFound || body["error"] != string(httpjson.CodeNoRoute) {
		t.Errorf("once the company keeps nothing this surface manages, %s "+
			"answered %d %v, want the route's absence, 404 %s", opsmcp.Path,
			rec.Code, body, httpjson.CodeNoRoute)
	}
}
