package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
)

// THE OPERATOR SURFACE READS ITS HALVES ON EVERY REQUEST.
//
// The catalogue is the native halves', met at the first company, by an apply
// long after the API serves. A server taken once at wiring would leave a node
// booted with no company without /operator/mcp for the process's life. So the
// route is mounted once and each request reads the halves: `503
// no_active_revision` before the company, the MCP transport's own answer after
// it (even with neither native half, which still gets the seat controls).
//
// Mutation: read the halves once, when the server is built, and the second
// request is the first one's 503 again.
func TestTheOperatorSurfaceReadsItsSourcePerRequest(t *testing.T) {
	t.Parallel()
	var started atomic.Bool
	operators, err := operator.New(withSeatControls(operator.Options{
		Halves: func() (operator.Halves, bool) {
			return operator.Halves{}, started.Load()
		},
		Authorize: builtin.Decide(authz.NoChart{}),
		Audit:     queuememory.New(),
	}))
	if err != nil {
		t.Fatalf("operator.New: %v", err)
	}
	a := newApp(t, api.Options{Operator: operators})
	post := func() *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, operator.MCPPath,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize",`+
				`"params":{"protocolVersion":"2025-06-18","capabilities":{},`+
				`"clientInfo":{"name":"a case","version":"1"}}}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, authed(r))
		return rec
	}

	rec := post()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not the engine's envelope: %v (%s)", err,
			rec.Body.String())
	}
	if rec.Code != http.StatusServiceUnavailable ||
		body["error"] != string(httpjson.CodeNoActiveRevision) ||
		body["detail"] != httpjson.Sentence(httpjson.NativeHalvesNotUp) {
		t.Errorf("before the company, %s answered %d %v, want 503 %s in the "+
			"halves' words", operator.MCPPath, rec.Code, body, httpjson.CodeNoActiveRevision)
	}
	poll := fmt.Sprint(httpjson.RetrySeconds(httpjson.NoActiveRevisionRetry))
	if got := rec.Header().Get("Retry-After"); got != poll {
		t.Errorf("before the company, Retry-After = %q, want the reconcile "+
			"poll's %s", got, poll)
	}

	started.Store(true)
	if rec = post(); rec.Code != http.StatusOK {
		t.Errorf("once the company is up, %s answered %d %s, want the MCP "+
			"transport's own answer to an initialize", operator.MCPPath, rec.Code,
			rec.Body.String())
	}
}
