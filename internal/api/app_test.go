package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

var clock = time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

// sharedEvents is the ONE event log every case that does not seed its own
// history shares.
//
// Shared rather than opened per case, because opening a store runs its whole
// migration set over two estates, and this package builds an app in most of its
// cases: a store apiece is minutes of the suite under -race for a log the cases
// that use it never read. The ones whose subject IS the history
// ([seededApp]) open their own and write to it, so nothing writes to this one.
var sharedEvents *store.EventLog

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite is TestMain's body as a function, so the store's cleanup runs:
// os.Exit skips deferred calls.
func runSuite(m *testing.M) int {
	dir, err := os.MkdirTemp("", "api-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "api test: temp dir:", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(dir) }()

	db, err := store.Open(context.Background(), filepath.Join(dir, "api.db"), store.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "api test: store.Open:", err)
		return 2
	}
	defer func() { _ = db.Close() }()
	sharedEvents = db.Events()
	return m.Run()
}

// fakeRuntime is the engine's answers, fixed.
type fakeRuntime struct {
	state api.RuntimeState
	tools []api.ToolInfo
}

func (f *fakeRuntime) Snapshot(context.Context) api.RuntimeState { return f.state }
func (f *fakeRuntime) Tools() []api.ToolInfo                     { return f.tools }

// ShuttingDown answers from the same state Snapshot does, which is the
// contract: one fact, two ways to read it.
func (f *fakeRuntime) ShuttingDown() bool { return f.state.ShuttingDown }

// noRoutes is a surface that mounts nothing, for the cases that are not about
// /config, /secrets or /setup.
type noRoutes struct{}

func (noRoutes) Routes(*http.ServeMux) {}

// noAppFlow is a GitHub App completer that completes nothing.
type noAppFlow struct{}

func (noAppFlow) Complete(context.Context, string, string) (string, error) { return "", nil }
func (noAppFlow) InstallURL(string) string                                 { return "" }

// active is a company revision that is active, for the cases about a
// configured node.
func active() func() *config.Company {
	company := &config.Company{Name: "Acme"}
	return func() *config.Company { return company }
}

// newApp builds the app over whatever a case names, filling each required
// dependency it leaves unset with an inert one: no active revision, an engine
// holding nothing, and surfaces that mount nothing. A case names only what it
// is about.
func newApp(t *testing.T, opts api.Options) *api.App {
	t.Helper()
	a, err := api.New(withRequired(t, opts))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	t.Cleanup(a.Stop)
	return a
}

func withRequired(t *testing.T, opts api.Options) api.Options {
	t.Helper()
	if opts.Bootstrap == nil {
		b := config.DefaultBootstrap()
		opts.Bootstrap = &b
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return clock }
	}
	if opts.Runtime == nil {
		opts.Runtime = &fakeRuntime{state: api.RuntimeState{Posture: "serve"}}
	}
	if opts.Sources.Company == nil {
		opts.Sources.Company = func() *config.Company { return nil }
	}
	if opts.Sources.Events == nil {
		opts.Sources.Events = sharedEvents
	}
	if opts.Sources.NodeID == "" {
		opts.Sources.NodeID = config.DefaultNodeID
	}
	fleet := coordmemory.NewFleet()
	if opts.Inbound.Publisher == nil {
		opts.Inbound.Publisher = queuememory.New()
	}
	if opts.Inbound.Claims == nil {
		opts.Inbound.Claims = fleet
	}
	if opts.Inbound.Secrets == nil {
		opts.Inbound.Secrets = func() webhooks.Secrets { return webhooks.Secrets{} }
	}
	if opts.Inbound.AppFlow == nil {
		opts.Inbound.AppFlow = noAppFlow{}
	}
	if opts.Config == nil {
		opts.Config = noRoutes{}
	}
	if opts.Secrets == nil {
		opts.Secrets = noRoutes{}
	}
	if opts.Setup == nil {
		opts.Setup = noRoutes{}
	}
	if opts.Budgets == nil {
		opts.Budgets = fleet
	}
	if opts.Retention == nil {
		opts.Retention = fleet
	}
	if opts.Capacity == nil {
		opts.Capacity = &fakeStateLog{}
	}
	if opts.Backup == nil {
		opts.Backup = &fakeBackup{}
	}
	return opts
}

// EVERY DEPENDENCY THE ENGINE SUPPLIES IS REQUIRED, and a missing one is
// refused by name.
//
// A nil is a wiring mistake, because the engine beside every API holds all of
// them, and an answer built around it would look deliberate (a health body
// missing the engine's fields, a 503, an absent /config). The constructor is the
// one place it can surface before an operator is misled.
func TestNewRefusesEveryMissingDependencyByName(t *testing.T) {
	t.Parallel()
	_, err := api.New(api.Options{})
	if err == nil {
		t.Fatal("an app wired to nothing was built")
	}
	for _, field := range []string{
		"Runtime", "Sources.Company", "Sources.Events", "Sources.NodeID",
		"Inbound.Publisher", "Inbound.Claims", "Inbound.Secrets", "Inbound.AppFlow",
		"Config", "Secrets", "Setup", "Budgets", "Retention", "Capacity", "Backup",
	} {
		if !strings.Contains(err.Error(), "Options."+field) {
			t.Errorf("the refusal does not name Options.%s: %v", field, err)
		}
	}
	// And the counterfactual: a complete set builds. Without it a refusal
	// that named every field for any input would pass the case above.
	a, err := api.New(withRequired(t, api.Options{}))
	if err != nil {
		t.Fatalf("a complete set was refused: %v", err)
	}
	a.Stop()
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

// THE PAGING FLOOR IS ON THE WIRE, because a reader who has paged to the
// bottom of the log has to be told they reached the floor rather than that the
// company went quiet. Three dashboard screens restated the number as literal
// copy while nothing carried it, so a change to the store's retention would
// have left all three lying with nothing to catch it.
func TestHealthCarriesTheEventPagingFloor(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})
	_, body := get(t, a, "/health")

	want := store.EventHistory.Seconds()
	if body["event_history_seconds"] != want {
		t.Errorf("event_history_seconds = %v, want %v", body["event_history_seconds"], want)
	}
}

func TestAConfiguredNodeSaysSo(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Sources: queries.Sources{Company: active()}})
	_, body := get(t, a, "/health")
	if body["status"] != api.StatusOK || body["configured"] != true {
		t.Errorf("body = %v, want ok and configured", body)
	}
}

// CONFIGURED IS READ OFF THE LIVE EPOCH, not remembered. An apply that brings a
// node its first revision flips it with nothing to call, and a revision that
// stops being active flips it back.
func TestConfiguredFollowsTheLiveEpoch(t *testing.T) {
	t.Parallel()
	var current *config.Company
	a := newApp(t, api.Options{Sources: queries.Sources{
		Company: func() *config.Company { return current },
	}})
	if a.Configured() {
		t.Fatal("a node with no active revision reported configured")
	}
	current = &config.Company{Name: "Acme"}
	if !a.Configured() {
		t.Error("the first revision applied and the node still reads unconfigured")
	}
}

// THE ENGINE'S FIELDS ARE ALWAYS ON THE BODY, and a zero is a real zero.
//
// Every process that serves the API runs the engine that answers them, so an
// idle node says 0 in flight and an empty seat list rather than leaving a client
// to guess what an absence means.
func TestAnIdleNodeReportsItsZerosRatherThanOmittingThem(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})
	_, body := get(t, a, "/health")

	if _, present := body["engine"]; present {
		t.Errorf("the body still carries the engine flag: %v", body["engine"])
	}
	if _, present := body["engine_started_at"]; present {
		t.Errorf("the body still carries a second start time: %v", body["engine_started_at"])
	}
	for field, want := range map[string]any{
		"in_flight": float64(0), "shutting_down": false, "applied_epoch": float64(0),
	} {
		if got, present := body[field]; !present || got != want {
			t.Errorf("%s = %v (present %v), want %v", field, got, present, want)
		}
	}
	if seats, ok := body["seats"].([]any); !ok || len(seats) != 0 {
		t.Errorf("seats = %#v, want an empty list", body["seats"])
	}
}

func TestANodeReportsWhatOnlyTheEngineCanKnow(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{
		Sources: queries.Sources{Company: active()},
		Runtime: &fakeRuntime{state: api.RuntimeState{
			InFlight: 3, Posture: "serve", AppliedEpoch: 41,
			StartedAt: "2026-06-14T11:00:00Z", Seats: []string{"ceo", "cto"},
		}},
	})
	_, body := get(t, a, "/health")

	if body["in_flight"] != float64(3) || body["applied_epoch"] != float64(41) {
		t.Errorf("body = %v", body)
	}
	seats, _ := body["seats"].([]any)
	if len(seats) != 2 {
		t.Errorf("seats = %v", body["seats"])
	}
	// The ENGINE's start is the node's, and the only start the body names.
	if body["started_at"] != "2026-06-14T11:00:00Z" {
		t.Errorf("started_at = %v, want the engine's own start", body["started_at"])
	}
}

// A STRANDED SEAT IS ON THE HEALTH BODY, with how long it has been stranded.
//
// The seat host computed the age and nothing served it, while the seat
// ownership page told operators to alert on `unproven_seconds`. A seat whose
// teardown failed is held by this node's lease and run by nobody, and until
// this field existed the only trace of that was a periodic log line.
func TestAStrandedSeatIsReportedWithHowLongItHasBeenStranded(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{
		Runtime: &fakeRuntime{state: api.RuntimeState{
			Posture: "serve", Seats: []string{"ceo"},
			Unproven: map[string]time.Duration{"ceo": 90 * time.Second},
		}},
		Sources: queries.Sources{Company: active()},
	})
	_, body := get(t, a, "/health")

	stranded, ok := body["unproven_seconds"].(map[string]any)
	if !ok {
		t.Fatalf("unproven_seconds = %v, want a map of seat to seconds", body["unproven_seconds"])
	}
	if stranded["ceo"] != float64(90) {
		t.Errorf("unproven_seconds[ceo] = %v, want 90", stranded["ceo"])
	}
}

// And absent when nothing is stranded, so an alert on its presence does not
// fire on every healthy node.
func TestAHealthyNodeReportsNoStrandedSeats(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{
		Runtime: &fakeRuntime{state: api.RuntimeState{
			Posture: "serve", Seats: []string{"ceo"},
		}},
		Sources: queries.Sources{Company: active()},
	})
	_, body := get(t, a, "/health")
	if _, present := body["unproven_seconds"]; present {
		t.Errorf("unproven_seconds = %v on a node with nothing stranded", body["unproven_seconds"])
	}
}

func TestHealthStaysOKThroughADrain(t *testing.T) {
	t.Parallel()
	// An orchestrator watching liveness must not SIGKILL a node that is
	// finishing its in-flight turns.
	a := newApp(t, api.Options{
		Sources: queries.Sources{Company: active()},
		Runtime: &fakeRuntime{state: api.RuntimeState{
			InFlight: 2, ShuttingDown: true, Posture: "serve",
		}},
	})
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
		a := newApp(t, api.Options{
			Sources: queries.Sources{Company: active()},
			Runtime: &fakeRuntime{state: api.RuntimeState{Posture: posture}},
		})
		if _, body := get(t, a, "/health"); body["status"] != posture {
			t.Errorf("posture %q: status = %v", posture, body["status"])
		}
	}
	// And the two that are ordinary do NOT become a status.
	for _, posture := range []string{"serve", "wait"} {
		a := newApp(t, api.Options{
			Sources: queries.Sources{Company: active()},
			Runtime: &fakeRuntime{state: api.RuntimeState{Posture: posture}},
		})
		if _, body := get(t, a, "/health"); body["status"] != api.StatusOK {
			t.Errorf("posture %q: status = %v, want ok", posture, body["status"])
		}
	}
}

func TestAnUnconfiguredNodeSaysSoOverItsPosture(t *testing.T) {
	t.Parallel()
	// The declared precedence puts unconfigured above a posture, and a node
	// whose FIRST apply failed is both: it has no revision, so it discards
	// every inbound webhook, and it reports a diverged posture about the
	// epoch it could not reach. The status names the first; the posture
	// still travels in its own field.
	for _, posture := range []string{"shed", "stuck", "isolated"} {
		a := newApp(t, api.Options{Runtime: &fakeRuntime{
			state: api.RuntimeState{Posture: posture},
		}})
		_, body := get(t, a, "/health")
		if body["status"] != api.StatusUnconfigured {
			t.Errorf("posture %q on an unconfigured node: status = %v, want unconfigured",
				posture, body["status"])
		}
		if body["posture"] != posture {
			t.Errorf("posture field = %v, want %q kept beside the status", body["posture"], posture)
		}
	}
}

// --- readiness ----------------------------------------------------------- //

func TestReadinessNeedsAConfiguredNode(t *testing.T) {
	t.Parallel()
	// An unconfigured node cannot verify a webhook signature, and taking
	// it out of rotation is how a fleet avoids answering with a node that
	// would only reject the delivery.
	var current *config.Company
	a := newApp(t, api.Options{Sources: queries.Sources{
		Company: func() *config.Company { return current },
	}})
	if status, body := get(t, a, "/ready"); status != http.StatusServiceUnavailable || body["ready"] != false {
		t.Errorf("status = %d body = %v, want 503", status, body)
	}
	current = &config.Company{Name: "Acme"}
	if status, body := get(t, a, "/ready"); status != http.StatusOK || body["ready"] != true {
		t.Errorf("status = %d body = %v, want 200", status, body)
	}
}

func TestADrainLeavesRotationImmediately(t *testing.T) {
	t.Parallel()
	// The split from /health is what lets a node leave rotation the moment
	// a drain starts while still reporting itself alive for the minutes
	// its turns need to finish.
	a := newApp(t, api.Options{
		Sources: queries.Sources{Company: active()},
		Runtime: &fakeRuntime{state: api.RuntimeState{ShuttingDown: true, Posture: "serve"}},
	})
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
			sources := queries.Sources{Company: func() *config.Company { return nil }}
			if tc.configured {
				sources.Company = active()
			}
			a := newApp(t, api.Options{
				Runtime: &fakeRuntime{state: tc.state}, Sources: sources,
			})
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
		a := newApp(t, api.Options{
			Sources: queries.Sources{Company: active()},
			Runtime: &fakeRuntime{state: api.RuntimeState{Posture: posture}},
		})
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

// --- the guard ----------------------------------------------------------- //

func TestTheProbesAreReachableWithoutAToken(t *testing.T) {
	t.Parallel()
	// An orchestrator has no token, and a liveness check that 401s is a
	// liveness check that fails.
	b := config.DefaultBootstrap()
	b.API.Auth.AllowAnonymousRead = false
	b.API.Auth.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	a := newApp(t, api.Options{Bootstrap: &b, Sources: queries.Sources{Company: active()}})

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

// THE NODE ID NAMES THE PROCESS THAT ANSWERED, and it is the RESOLVED one.
//
// The field that turns "the config apply failed" into "the config apply failed
// on node-2" once a load balancer sits in front of more than one process, and
// the only way a caller can tell which one it reached. It used to be read off
// the raw `node.id` field, which is empty on a node named through
// CREWLET_NODE_ID: every such node answered as the default, node-0, while its
// presence lease and the fleet view named it correctly. So the Bootstrap here
// names a DIFFERENT node, and the body must not report it.
func TestTheNodeIDNamesTheProcessThatAnswered(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Node.ID = "the-raw-field"
	a := newApp(t, api.Options{Bootstrap: &b, Sources: queries.Sources{NodeID: "node-2"}})

	if _, body := get(t, a, "/health"); body["node"] != "node-2" {
		t.Errorf("health node = %v, want the resolved node-2", body["node"])
	}
	if _, body := get(t, a, "/ready"); body["node"] != "node-2" {
		t.Errorf("ready node = %v, want the resolved node-2", body["node"])
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
