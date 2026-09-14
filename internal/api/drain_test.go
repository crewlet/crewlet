package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// A draining node keeps its listener for its probes and its reads, and refuses
// every request that would start work. These cases pin both halves of that
// against one app whose runtime can be told it is draining, because a gate
// that refused everything would pass the refusal half and a gate that refused
// nothing would pass the other.

// drainingApp is an authenticated, configured node with the webhook edge
// mounted on a real queue, draining or not.
func drainingApp(t *testing.T, draining bool) *api.App {
	t.Helper()
	b := closedPosture()
	b.API.Auth.AllowedOrigins = []string{"https://ops.example.com"}
	a := newApp(t, api.Options{
		Bootstrap: &b,
		Runtime: &fakeRuntime{state: api.RuntimeState{
			ShuttingDown: draining, Posture: "serve",
		}},
		Inbound: api.Inbound{
			Secrets:   func() webhooks.Secrets { return webhooks.Secrets{GitHub: "gh-secret"} },
			Publisher: memory.New(),
		},
	})
	a.SetConfigured(true)
	return a
}

// send runs one authenticated request.
func send(t *testing.T, a *api.App, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, r)
	return rec
}

// refusedForDraining reports whether a response is the drain gate's refusal.
func refusedForDraining(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		return false
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return false
	}
	return body["error"] == string(httpjson.CodeDraining)
}

// startsWork is every kind of request the gate refuses, one of each route
// family: the webhook edge (a delivery, and both of its GET landings, one of
// which acts and the other of which is refused with it), the config and
// credential writes, the setup pass, the operator writes and the operator MCP
// surface.
var startsWork = []struct{ method, path string }{
	{http.MethodPost, "/webhooks/github"},
	{http.MethodPost, "/webhooks/slack/ceo"},
	{http.MethodGet, "/webhooks/github-app?code=c&state=s"},
	{http.MethodGet, "/webhooks/slack-oauth?code=c"},
	{http.MethodPut, "/config"},
	{http.MethodPatch, "/config"},
	{http.MethodPost, "/config/reload"},
	{http.MethodPost, "/config/revisions/r1/revert"},
	{http.MethodPut, "/secrets/GITHUB_TOKEN"},
	{http.MethodDelete, "/secrets/GITHUB_TOKEN"},
	{http.MethodPost, "/setup/integrations/github/provision"},
	{http.MethodPost, "/budgets/reset"},
	{http.MethodPost, "/backup"},
	{http.MethodPost, "/work/retention/ack"},
	{http.MethodPost, "/work/ENG-1/purge"},
	{http.MethodPost, "/operator/mcp"},
}

func TestADrainRefusesEveryRequestThatWouldStartWork(t *testing.T) {
	t.Parallel()
	a := drainingApp(t, true)
	for _, req := range startsWork {
		rec := send(t, a, req.method, req.path)
		if !refusedForDraining(t, rec) {
			t.Errorf("%s %s answered %d %s during a drain, want the draining 503",
				req.method, req.path, rec.Code, rec.Body.String())
			continue
		}
		// A RETRY HINT, because the refusal is the one failure here that
		// is certain to pass: the caller belongs on another node.
		want := strconv.Itoa(int(api.DrainRetryAfter.Seconds()))
		if got := rec.Header().Get("Retry-After"); got != want {
			t.Errorf("%s %s: Retry-After = %q, want %q", req.method, req.path, got, want)
		}
	}
}

func TestNothingIsRefusedForDrainingBeforeADrain(t *testing.T) {
	t.Parallel()
	// The counterfactual. Every request above reaches its own handler on a
	// node that is not draining, whatever that handler then says: the gate
	// is the drain's, and a gate shut all the time would pass the case
	// above.
	a := drainingApp(t, false)
	for _, req := range startsWork {
		if rec := send(t, a, req.method, req.path); refusedForDraining(t, rec) {
			t.Errorf("%s %s was refused for draining on a node that is not draining",
				req.method, req.path)
		}
	}
}

func TestADrainingNodeStillServesItsProbesAndItsReads(t *testing.T) {
	t.Parallel()
	a := drainingApp(t, true)

	// Liveness stays 200, which is the whole reason the listener outlives
	// the drain: an orchestrator that could not reach it would kill the
	// node in the middle of the turns the drain exists to finish.
	if rec := send(t, a, http.MethodGet, "/health"); rec.Code != http.StatusOK {
		t.Errorf("/health answered %d during a drain, want 200", rec.Code)
	} else if !strings.Contains(rec.Body.String(), api.StatusShuttingDown) {
		t.Errorf("/health does not say the node is shutting down: %s", rec.Body.String())
	}

	// Readiness answers from its own handler, with the reason, rather than
	// from the gate: it is a read, and its 503 is the one that steers
	// traffic.
	rec := send(t, a, http.MethodGet, "/ready")
	var ready map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ready)
	if rec.Code != http.StatusServiceUnavailable || ready["reason"] != api.ReasonDraining {
		t.Errorf("/ready answered %d %v during a drain, want 503 naming the drain",
			rec.Code, ready)
	}

	// Every other read keeps answering, which is how an operator watches
	// the drain: the dashboard, the REST reads and the query surface.
	for _, path := range []string{"/agents", "/org", "/dashboard", "/query/stream"} {
		if rec := send(t, a, http.MethodGet, path); rec.Code != http.StatusOK {
			t.Errorf("GET %s answered %d during a drain, want 200", path, rec.Code)
		}
	}
	// And the reads of a guarded surface, and a preflight, are reads too.
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/config"},
		{http.MethodOptions, "/config"},
		{http.MethodGet, "/operator/mcp"},
	} {
		if rec := send(t, a, req.method, req.path); refusedForDraining(t, rec) {
			t.Errorf("%s %s was refused for draining, and it starts nothing",
				req.method, req.path)
		}
	}
}

func TestADrainKeepsTheEdgesOfTheRunsItCannotWaitFor(t *testing.T) {
	t.Parallel()
	// The sandbox bridge and the telemetry edge carry the tool calls and
	// the spans of coding runs that started before the drain. A detached
	// run outlives the turn that started it, so the drain never waits on
	// one: refusing these shortens no drain and only breaks a run against
	// the node still holding its bridge session and its receiver.
	a := drainingApp(t, true)
	for _, req := range []struct{ method, path string }{
		{http.MethodPost, "/mcp/run-token"},
		{http.MethodDelete, "/mcp/run-token"},
		{http.MethodPost, "/otlp/run-token/v1/traces"},
	} {
		if rec := send(t, a, req.method, req.path); refusedForDraining(t, rec) {
			t.Errorf("%s %s was refused for draining: an in-flight run lost its "+
				"edge", req.method, req.path)
		}
	}
}

func TestADrainRefusalCarriesASentenceForThePersonWhoSentIt(t *testing.T) {
	t.Parallel()
	// The CLI and the dashboard both render `detail` and `hint` beside the
	// code, and a bare "draining" tells an operator what happened without
	// telling them what to do about it.
	rec := send(t, drainingApp(t, true), http.MethodPut, "/config")
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["detail"] == "" || body["hint"] == "" {
		t.Errorf("the refusal carries no detail or hint: %v", body)
	}
}

func TestACrossOriginDashboardCanReadADrainRefusal(t *testing.T) {
	t.Parallel()
	// The gate sits INSIDE the browser posture. Outside it, a permitted
	// origin's write would be refused with no allow-origin header, and the
	// browser would report an opaque network failure instead of the drain.
	a := drainingApp(t, true)
	r := httptest.NewRequest(http.MethodPatch, "/config", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Origin", "https://ops.example.com")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, r)
	if !refusedForDraining(t, rec) {
		t.Fatalf("PATCH /config answered %d during a drain", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://ops.example.com" {
		t.Errorf("the refusal permitted origin %q, so the browser cannot read it", got)
	}
}

func TestACredentialIsStillTheFirstQuestionDuringADrain(t *testing.T) {
	t.Parallel()
	// Inside the guard: a write with no token is unauthorized whatever the
	// node is doing, so a drain tells an anonymous caller nothing a guarded
	// route would not.
	a := drainingApp(t, true)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/config", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated write answered %d during a drain, want 401", rec.Code)
	}
}

func TestAProcessWithNoEngineHasNothingToDrain(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b})
	if rec := send(t, a, http.MethodPost, "/budgets/reset"); refusedForDraining(t, rec) {
		t.Error("a process with no engine refused a write for draining")
	}
}
