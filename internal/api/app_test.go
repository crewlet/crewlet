package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/config"
)

var clock = time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

// fakeRuntime is a co-located engine's answers, fixed.
type fakeRuntime struct {
	state api.RuntimeState
	tools []api.ToolInfo
}

func (f *fakeRuntime) Snapshot(context.Context) api.RuntimeState { return f.state }
func (f *fakeRuntime) Tools() []api.ToolInfo                     { return f.tools }

// ShuttingDown answers from the same state Snapshot does, which is the
// contract: one fact, two ways to read it.
func (f *fakeRuntime) ShuttingDown() bool { return f.state.ShuttingDown }

func newApp(t *testing.T, opts api.Options) *api.App {
	t.Helper()
	if opts.Bootstrap == nil {
		b := config.DefaultBootstrap()
		opts.Bootstrap = &b
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return clock }
	}
	a := api.New(opts)
	t.Cleanup(a.Stop)
	return a
}

// get runs one request and returns the status and decoded body.
func get(t *testing.T, a *api.App, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	res := rec.Result()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return res.StatusCode, body
}

// --- liveness ------------------------------------------------------------ //

func TestHealthAnswersWhileTheProcessIsAlive(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{QueueBackend: "jetstream"})
	status, body := get(t, a, "/health")

	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	// Unconfigured until a revision applies, and saying so is the point:
	// an engine with no active revision drops every inbound webhook and
	// used to look exactly like a healthy idle one.
	if body["status"] != api.StatusUnconfigured || body["configured"] != false {
		t.Errorf("body = %v, want unconfigured", body)
	}
	if body["node"] != config.DefaultNodeID {
		t.Errorf("node = %v", body["node"])
	}
	if body["queue"] != "jetstream" {
		t.Errorf("queue = %v", body["queue"])
	}
}

func TestAConfiguredNodeSaysSo(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})
	a.SetConfigured(true)
	_, body := get(t, a, "/health")
	if body["status"] != api.StatusOK || body["configured"] != true {
		t.Errorf("body = %v, want ok and configured", body)
	}
}

func TestAStandaloneAPIOmitsWhatItCannotKnow(t *testing.T) {
	t.Parallel()
	// ABSENT and ZERO are different answers. Without the distinction a
	// dashboard renders a confident zero for both, and reports an idle
	// company during an outage.
	a := newApp(t, api.Options{})
	_, body := get(t, a, "/health")

	if body["engine"] != false {
		t.Errorf("engine = %v, want false", body["engine"])
	}
	for _, absent := range []string{"in_flight", "shutting_down", "applied_epoch", "seats"} {
		if _, present := body[absent]; present {
			t.Errorf("a standalone API answered %q = %v", absent, body[absent])
		}
	}
}

func TestAMergedNodeReportsWhatOnlyItCanKnow(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Runtime: &fakeRuntime{state: api.RuntimeState{
		InFlight: 3, Posture: "serve", AppliedEpoch: 41,
		StartedAt: "2026-06-14T11:00:00Z", Seats: []string{"ceo", "cto"},
	}}})
	a.SetConfigured(true)
	_, body := get(t, a, "/health")

	if body["engine"] != true {
		t.Errorf("engine = %v", body["engine"])
	}
	if body["in_flight"] != float64(3) || body["applied_epoch"] != float64(41) {
		t.Errorf("body = %v", body)
	}
	seats, _ := body["seats"].([]any)
	if len(seats) != 2 {
		t.Errorf("seats = %v", body["seats"])
	}
	// Separate from the API's own start: on the standalone deployment they
	// are two processes on two clocks, and one merged uptime would be the
	// two-different-windows error in a new place.
	if body["engine_started_at"] == body["started_at"] {
		t.Error("the engine's start was merged with the API's")
	}
}

func TestHealthStaysOKThroughADrain(t *testing.T) {
	t.Parallel()
	// An orchestrator watching liveness must not SIGKILL a node that is
	// finishing its in-flight turns.
	a := newApp(t, api.Options{Runtime: &fakeRuntime{state: api.RuntimeState{
		InFlight: 2, ShuttingDown: true, Posture: "serve",
	}}})
	a.SetConfigured(true)
	status, body := get(t, a, "/health")

	if status != http.StatusOK {
		t.Errorf("status = %d during a drain, want 200", status)
	}
	if body["status"] != api.StatusShuttingDown {
		t.Errorf("status field = %v, want shutting_down", body["status"])
	}
}

func TestDrainingOutranksEveryOtherStatus(t *testing.T) {
	t.Parallel()
	// A draining engine is draining first, whatever else is true of it.
	a := newApp(t, api.Options{Runtime: &fakeRuntime{state: api.RuntimeState{
		ShuttingDown: true, Posture: "stuck",
	}}})
	// Not configured either — the lowest-precedence claim of all.
	if _, body := get(t, a, "/health"); body["status"] != api.StatusShuttingDown {
		t.Errorf("status = %v, want shutting_down to outrank both", body["status"])
	}
}

func TestUnconfiguredOutranksAPosture(t *testing.T) {
	t.Parallel()
	// The documented precedence, and the one /ready's reason follows. A
	// node with no active revision refuses every delivery whatever it
	// concluded about the fleet's epoch, and a status that named the
	// posture instead would send an operator to read a control-plane error
	// on a node that has no company at all.
	for _, posture := range []string{"shed", "stuck", "isolated"} {
		a := newApp(t, api.Options{Runtime: &fakeRuntime{
			state: api.RuntimeState{Posture: posture},
		}})
		if _, body := get(t, a, "/health"); body["status"] != api.StatusUnconfigured {
			t.Errorf("unconfigured and %q: status = %v, want unconfigured", posture, body["status"])
		}
	}
}

func TestADivergedPostureBecomesTheStatus(t *testing.T) {
	t.Parallel()
	// WHY a node left rotation, on the probe that stays 200: "draining"
	// and "cannot apply epoch 41" call for opposite responses.
	for _, posture := range []string{"shed", "stuck", "isolated"} {
		a := newApp(t, api.Options{Runtime: &fakeRuntime{
			state: api.RuntimeState{Posture: posture},
		}})
		a.SetConfigured(true)
		if _, body := get(t, a, "/health"); body["status"] != posture {
			t.Errorf("posture %q: status = %v", posture, body["status"])
		}
	}
	// And the two that are ordinary do NOT become a status.
	for _, posture := range []string{"serve", "wait"} {
		a := newApp(t, api.Options{Runtime: &fakeRuntime{
			state: api.RuntimeState{Posture: posture},
		}})
		a.SetConfigured(true)
		if _, body := get(t, a, "/health"); body["status"] != api.StatusOK {
			t.Errorf("posture %q: status = %v, want ok", posture, body["status"])
		}
	}
}

// --- readiness ----------------------------------------------------------- //

func TestReadinessNeedsAConfiguredNode(t *testing.T) {
	t.Parallel()
	// An unconfigured node cannot verify a webhook signature, and taking
	// it out of rotation is how a fleet avoids answering with a node that
	// would only reject the delivery.
	a := newApp(t, api.Options{})
	if status, body := get(t, a, "/ready"); status != http.StatusServiceUnavailable || body["ready"] != false {
		t.Errorf("status = %d body = %v, want 503", status, body)
	}
	a.SetConfigured(true)
	if status, body := get(t, a, "/ready"); status != http.StatusOK || body["ready"] != true {
		t.Errorf("status = %d body = %v, want 200", status, body)
	}
}

func TestADrainLeavesRotationImmediately(t *testing.T) {
	t.Parallel()
	// The split from /health is what lets a node leave rotation the moment
	// a drain starts while still reporting itself alive for the minutes
	// its turns need to finish.
	a := newApp(t, api.Options{Runtime: &fakeRuntime{state: api.RuntimeState{
		ShuttingDown: true, Posture: "serve",
	}}})
	a.SetConfigured(true)
	status, body := get(t, a, "/ready")
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d during a drain, want 503", status)
	}
	if body["draining"] != true {
		t.Errorf("body = %v", body)
	}
}

// A REFUSED /ready SAYS WHY, in one field, in the precedence the health status
// uses. Three fields that each say part of it leave a load balancer's log of a
// failed probe to be decoded by whoever reads it, and they decode it wrong the
// day a node is both draining and diverged.
func TestARefusedReadinessNamesItsReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		configured bool
		state      api.RuntimeState
		want       string
	}{
		{"draining outranks everything", false,
			api.RuntimeState{ShuttingDown: true, Posture: "stuck"}, api.ReasonDraining},
		{"unconfigured outranks a posture", false,
			api.RuntimeState{Posture: "shed"}, api.ReasonUnconfigured},
		{"a diverged posture names itself", true,
			api.RuntimeState{Posture: "shed"}, "shed"},
		{"stuck names itself", true,
			api.RuntimeState{Posture: "stuck"}, "stuck"},
		{"a ready node names nothing", true,
			api.RuntimeState{Posture: "wait"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newApp(t, api.Options{Runtime: &fakeRuntime{state: tc.state}})
			a.SetConfigured(tc.configured)
			status, body := get(t, a, "/ready")
			got, present := body["reason"]
			if tc.want == "" {
				if present || status != http.StatusOK {
					t.Errorf("status %d, reason %v, want 200 and no reason", status, got)
				}
				return
			}
			if got != tc.want || status != http.StatusServiceUnavailable {
				t.Errorf("status %d, reason %v, want 503 and %q", status, got, tc.want)
			}
		})
	}
}

func TestOnlyShedAndStuckTakeANodeOutOfRotation(t *testing.T) {
	t.Parallel()
	// Both exclusions are load-bearing. wait is ordinary propagation
	// during a rollout, so failing readiness there would make every
	// successful rollout a fleet-wide outage; isolated means NO node
	// applied the revision, so taking this one out would take the fleet
	// out over one bad revision.
	for posture, wantReady := range map[string]bool{
		"serve": true, "wait": true, "isolated": true,
		"shed": false, "stuck": false,
	} {
		a := newApp(t, api.Options{Runtime: &fakeRuntime{
			state: api.RuntimeState{Posture: posture},
		}})
		a.SetConfigured(true)
		status, body := get(t, a, "/ready")
		if body["ready"] != wantReady {
			t.Errorf("posture %q: ready = %v, want %v", posture, body["ready"], wantReady)
		}
		wantStatus := http.StatusServiceUnavailable
		if wantReady {
			wantStatus = http.StatusOK
		}
		if status != wantStatus {
			t.Errorf("posture %q: status = %d, want %d", posture, status, wantStatus)
		}
	}
}

func TestAStandaloneAPIIsReadyOnceConfigured(t *testing.T) {
	t.Parallel()
	// With no engine to ask there is nothing to be draining or diverged
	// about, and refusing traffic on that basis would take every
	// standalone API permanently out of rotation.
	a := newApp(t, api.Options{})
	a.SetConfigured(true)
	if status, body := get(t, a, "/ready"); status != http.StatusOK {
		t.Errorf("status = %d body = %v", status, body)
	}
}

// --- the guard ----------------------------------------------------------- //

func TestTheProbesAreReachableWithoutAToken(t *testing.T) {
	t.Parallel()
	// An orchestrator has no token, and a liveness check that 401s is a
	// liveness check that fails.
	b := config.DefaultBootstrap()
	b.API.Auth.AllowAnonymousRead = false
	b.API.Auth.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	a := newApp(t, api.Options{Bootstrap: &b})
	a.SetConfigured(true)

	for _, path := range []string{"/health", "/ready"} {
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s was guarded", path)
		}
	}
}

func TestTheGuardIsMountedEvenWithNoTierA(t *testing.T) {
	t.Parallel()
	// Tier A supplies the posture, never the existence of a check.
	a := newApp(t, api.Options{Bootstrap: nil})
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/config/revisions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d with no Tier A, want 401", rec.Code)
	}
	if a.Guard() == nil {
		t.Error("no guard was built")
	}
}

func TestAnUnknownRouteIsNotFound(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	// 404 rather than 401: an anonymous read posture lets the request
	// through the guard, and the mux then has nothing for it.
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestTheConfiguredFlagIsSafeUnderConcurrentUse(t *testing.T) {
	t.Parallel()
	// The config refresher sets it from its own goroutine while every
	// health probe reads it.
	a := newApp(t, api.Options{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			a.SetConfigured(i%2 == 0)
		}
	}()
	for range 200 {
		get(t, a, "/health")
	}
	<-done
}

func TestTheNodeIDNamesTheProcessThatAnswered(t *testing.T) {
	t.Parallel()
	// The field that turns "the config apply failed" into "the config
	// apply failed on node-2" once a load balancer sits in front of more
	// than one process — and the only way a caller can tell which one it
	// reached.
	b := config.DefaultBootstrap()
	b.Node.ID = "node-2"
	a := newApp(t, api.Options{Bootstrap: &b})

	if _, body := get(t, a, "/health"); body["node"] != "node-2" {
		t.Errorf("health node = %v, want node-2", body["node"])
	}
	if _, body := get(t, a, "/ready"); body["node"] != "node-2" {
		t.Errorf("ready node = %v, want node-2", body["node"])
	}
}

func TestAnUnnamedNodeTakesTheDefault(t *testing.T) {
	t.Parallel()
	// The counterfactual: a single-process deployment names nothing, and
	// an empty node field would tell a reader less than a default does.
	a := newApp(t, api.Options{})
	if _, body := get(t, a, "/health"); body["node"] != config.DefaultNodeID {
		t.Errorf("node = %v, want the default", body["node"])
	}
}

// closedPosture is Tier A with reads guarded, for the cases that check what
// stays reachable anyway.
func closedPosture() config.Bootstrap {
	b := config.DefaultBootstrap()
	b.API.Auth.AllowAnonymousRead = false
	b.API.Auth.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	return b
}

// THE APP ANSWERS A PREFLIGHT WITHOUT A CREDENTIAL.
//
// The browser posture has to wrap the credential one, and that is not a style
// choice: a preflight is an `OPTIONS` the browser sends itself with no
// Authorization header, because it will not attach one until it has been told
// the origin is permitted. Wired inside the guard, every preflight to a
// guarded route answers 401, the browser reports a CORS failure, and the real
// request is never sent — so the allow-list would be as unreachable as it was
// when nothing read it at all.
func TestAPreflightToAGuardedRouteIsAnswered(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.API.Auth.AllowedOrigins = []string{"https://ops.example.com"}
	b.API.Auth.Tokens = []config.APIToken{{ID: "founder", Token: "s3cret"}}
	a := newApp(t, api.Options{Bootstrap: &b})

	// `/config` is one of the two prefixes never eligible for
	// allow_anonymous_read, so it is exactly the route whose preflight the
	// guard would answer 401.
	r := httptest.NewRequest(http.MethodOptions, "/config", nil)
	r.Header.Set("Origin", "https://ops.example.com")
	r.Header.Set("Access-Control-Request-Method", "PATCH")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, r)

	if rec.Code == http.StatusUnauthorized {
		t.Fatal("the preflight was answered by the guard, which the browser " +
			"cannot put a token on — so every cross-origin write to a guarded " +
			"route fails before it is sent")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://ops.example.com" {
		t.Errorf("the preflight permitted %q — an allow-list nothing stamps "+
			"onto a response is one the browser never sees", got)
	}
	if n := a.CORS().Origins(); n != 1 {
		t.Errorf("the app reports %d permitted origin(s), want 1", n)
	}
}
