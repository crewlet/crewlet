package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeProbeRuntime is a node without the ingress role, with its answers fixed.
type fakeProbeRuntime struct {
	fakeRuntime
	configured bool
	work       api.WorkState
}

func (f *fakeProbeRuntime) Configured() bool    { return f.configured }
func (f *fakeProbeRuntime) Work() api.WorkState { return f.work }

// doingItsWork is a node doing its work: linked, present, admitted.
func doingItsWork() api.WorkState {
	return api.WorkState{Presence: true, Admission: true, Admitted: true}
}

func probeHandler(t *testing.T, runtime api.ProbeRuntime) http.Handler {
	t.Helper()
	boot := config.DefaultBootstrap()
	h, err := api.Probes(api.ProbeOptions{
		Bootstrap: &boot, Runtime: runtime, NodeID: "sat-eu-1",
		Roles: []string{"seats"}, QueueBackend: "jetstream-embedded",
	})
	if err != nil {
		t.Fatalf("Probes: %v", err)
	}
	return h
}

func askProbe(t *testing.T, h http.Handler, method, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader("{}")))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s answered %d with a body that is not JSON: %q",
			method, path, rec.Code, rec.Body.String())
	}
	return rec.Code, body
}

// A NODE WITHOUT INGRESS IS READY WHILE IT IS DOING ITS WORK, and a refusal
// names the first thing it is not doing, in the one precedence the full
// surface's reasons share. Each case moves exactly one fact off a ready node,
// so a reason that stopped being checked reads as ready and fails here.
func TestAProbeNodeIsReadyOnlyWhileItIsDoingItsWork(t *testing.T) {
	t.Parallel()
	unlinked := errors.New("jetstream: this leaf has no link to any member of the fleet")
	for _, tc := range []struct {
		name       string
		configured bool
		state      api.RuntimeState
		work       func(*api.WorkState)
		want       string
		detail     string
	}{
		{name: "a node doing its work is ready", configured: true,
			state: api.RuntimeState{Posture: "serve"}},
		{name: "draining outranks everything", configured: false,
			state: api.RuntimeState{ShuttingDown: true, Posture: "stuck"},
			work:  func(w *api.WorkState) { w.Broker, w.Presence, w.Admitted = unlinked, false, false },
			want:  api.ReasonDraining},
		{name: "a broken link outranks what it causes", configured: false,
			state: api.RuntimeState{Posture: "stuck"},
			work:  func(w *api.WorkState) { w.Broker, w.Presence = unlinked, false },
			want:  api.ReasonBrokerUnlinked, detail: unlinked.Error()},
		{name: "unconfigured", configured: false,
			state: api.RuntimeState{Posture: "serve"},
			work:  func(w *api.WorkState) { w.Admitted = false },
			want:  api.ReasonUnconfigured},
		{name: "a diverged posture names itself", configured: true,
			state: api.RuntimeState{Posture: "shed"},
			work:  func(w *api.WorkState) { w.Presence = false },
			want:  "shed"},
		{name: "a node its fleet cannot see", configured: true,
			state: api.RuntimeState{Posture: "serve"},
			work:  func(w *api.WorkState) { w.Presence, w.Admitted = false, false },
			want:  api.ReasonNoPresence},
		{name: "a seats node not admitted to claim", configured: true,
			state: api.RuntimeState{Posture: "serve"},
			work:  func(w *api.WorkState) { w.Admitted = false },
			want:  api.ReasonAdmissionWithheld},
		{name: "admission that does not apply decides nothing", configured: true,
			state: api.RuntimeState{Posture: "serve"},
			work:  func(w *api.WorkState) { w.Admission, w.Admitted = false, false }},
		{name: "wait and isolated stay ready, as on the full surface", configured: true,
			state: api.RuntimeState{Posture: "isolated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			work := doingItsWork()
			if tc.work != nil {
				tc.work(&work)
			}
			h := probeHandler(t, &fakeProbeRuntime{
				fakeRuntime: fakeRuntime{state: tc.state}, configured: tc.configured, work: work,
			})
			status, body := askProbe(t, h, http.MethodGet, "/ready")
			got, present := body["reason"]
			if tc.want == "" {
				if present || status != http.StatusOK || body["ready"] != true {
					t.Errorf("status %d, body %v, want 200 and ready", status, body)
				}
				return
			}
			if got != tc.want || status != http.StatusServiceUnavailable || body["ready"] != false {
				t.Errorf("status %d, body %v, want 503 naming %q", status, body, tc.want)
			}
			if detail, _ := body["detail"].(string); detail != tc.detail {
				t.Errorf("detail = %q, want %q", detail, tc.detail)
			}
		})
	}
}

// LIVENESS STAYS 200 THROUGH ANYTHING THE PROCESS SURVIVES, a drain and a dead
// link included — an orchestrator that read either as death would kill a node
// that is finishing its turns or waiting out a broker restart — and its body
// is the node's half of the health envelope: who it is, what it runs, and what
// it holds, with nothing that describes a dashboard it does not serve.
func TestAProbeNodesHealthIsTheNodesHalfOfTheEnvelope(t *testing.T) {
	t.Parallel()
	nodes := 3
	runtime := &fakeProbeRuntime{
		fakeRuntime: fakeRuntime{
			state: api.RuntimeState{
				ShuttingDown: true, Posture: "serve", AppliedEpoch: 41,
				InFlight: 2, Seats: []string{"eu-support"}, StartedAt: "2026-10-09T08:00:00Z",
				ConfigAttempt: &api.ConfigAttempt{Epoch: 41, RevisionID: "rev-41", Status: "ok"},
			},
			fleet: api.FleetState{LiveNodes: &nodes, Alarms: []statelog.Kind{}},
		},
		configured: true,
		work:       api.WorkState{Broker: errors.New("down")},
	}
	status, body := askProbe(t, probeHandler(t, runtime), http.MethodGet, "/health")
	if status != http.StatusOK {
		t.Fatalf("/health = %d through a drain with the link down, want 200", status)
	}
	want := map[string]any{
		"status": api.StatusShuttingDown, "node": "sat-eu-1", "roles": []any{"seats"},
		"configured": true, "queue": "jetstream-embedded", "in_flight": float64(2),
		"shutting_down": true, "posture": "serve", "applied_epoch": float64(41),
		"config_attempt": map[string]any{"epoch": float64(41), "revision_id": "rev-41",
			"status": "ok"},
		"seats": []any{"eu-support"}, "started_at": "2026-10-09T08:00:00Z",
		"nodes": float64(3), "alarms": map[string]any{"count": float64(0)},
		// AN OBJECT EVEN WHERE NOTHING WAS ARMED, as on the full surface.
		"identity_duty_seconds": map[string]any{},
	}
	for field, value := range want {
		if !reflect.DeepEqual(body[field], value) {
			t.Errorf("/health %s = %v, want %v", field, body[field], value)
		}
	}
	for _, field := range []string{"clients", "event_history_seconds",
		"spend_history_seconds", "seeded_from"} {
		if _, present := body[field]; present {
			t.Errorf("/health carries %q on a node that serves no dashboard", field)
		}
	}
	// AND THE FIELDS IT SHARES WITH THE ENVELOPE ARE THE ENVELOPE'S, under
	// the same names: a script reading one reads the other.
	shared := map[string]bool{}
	envelope := reflect.TypeFor[api.Health]()
	for i := range envelope.NumField() {
		name, _, _ := strings.Cut(envelope.Field(i).Tag.Get("json"), ",")
		shared[name] = true
	}
	node := reflect.TypeFor[api.ProbeHealth]()
	for i := range node.NumField() {
		name, _, _ := strings.Cut(node.Field(i).Tag.Get("json"), ",")
		if !shared[name] && name != "roles" {
			t.Errorf("ProbeHealth carries %q, which the health envelope does not: "+
				"one fact under two names is one a script reads wrong", name)
		}
	}
}

// THE PROBE SURFACE SERVES THE PROBES AND NOTHING ELSE. A satellite is placed
// on a private host so that it terminates no traffic: every route the full
// surface serves is refused here, by the router or by the guard in front of
// it, and with no bridge configured the bridge's prefix is absent too.
func TestTheProbeSurfaceServesNothingButItsProbes(t *testing.T) {
	t.Parallel()
	h := probeHandler(t, &fakeProbeRuntime{configured: true, work: doingItsWork()})
	for _, route := range [][2]string{
		{http.MethodGet, "/"}, {http.MethodGet, "/dashboard"}, {http.MethodGet, "/agents"},
		{http.MethodGet, "/org"}, {http.MethodGet, "/tools"}, {http.MethodGet, "/events"},
		{http.MethodGet, "/query/viewer"}, {http.MethodGet, "/stream/snapshot"},
		{http.MethodGet, "/ws/stream"}, {http.MethodGet, "/config"},
		{http.MethodGet, "/secrets"}, {http.MethodGet, "/mcp/a-token"},
		{http.MethodPost, "/webhooks/github"}, {http.MethodPost, "/config"},
		{http.MethodPost, "/backup"}, {http.MethodPost, "/operator/mcp"},
		{http.MethodPost, "/otlp/a-token/v1/traces"}, {http.MethodPost, "/mcp/a-token"},
	} {
		status, body := askProbe(t, h, route[0], route[1])
		if status != http.StatusNotFound && status != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want it refused", route[0], route[1], status)
		}
		if body["error"] == nil {
			t.Errorf("%s %s refused with no error code: %v", route[0], route[1], body)
		}
	}
	for _, path := range []string{"/health", "/ready"} {
		if status, _ := askProbe(t, h, http.MethodGet, path); status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 on a node doing its work", path, status)
		}
	}
}

// A SEATS NODE SERVES ITS OWN BOXES THEIR TWO ROUTES, TO THIS HOST ALONE. A
// `direct` box is a process of the node's host, handed the node's api.port on
// loopback for its seat's tools and its telemetry, so the probe surface serves
// the bridge and the receiver there — to a caller on loopback, read from the
// connection. To anybody else they are absent, whatever the method, exactly as
// on a node that serves neither: the network still reaches the probes alone.
// And on loopback each is the real route: a token the fleet did not sign is
// refused 401 before anything runs, and a run's own token is forwarded.
//
// Mutation: serve the routes to every caller and the off-host rows reach them
// (a 401, or a forwarded export); mount them behind the host check one route
// at a time and the off-host GET meets the route's 405 rather than its absence.
func TestTheProbeSurfaceServesItsOwnBoxesOnThisHostAlone(t *testing.T) {
	t.Parallel()
	forwarded := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	material := runtoken.OneKey("k", "test-key")
	bridge, err := mcpbridge.New(mcpbridge.Options{Material: material, HostURL: "http://127.0.0.1:8080"})
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := sandbox.BuildOtelReceiver(func(name string) string {
		return map[string]string{sandbox.OtelUpstreamEndpointVar: upstream.URL}[name]
	}, material, "http://127.0.0.1:8080")
	if err != nil || receiver == nil {
		t.Fatalf("the receiver: %v, %v", receiver, err)
	}
	export := strings.TrimPrefix(receiver.RunEnv("trace-abc", "span", "turn-1", "engineer", true,
		time.Hour)["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:8080")
	if !strings.HasPrefix(export, auth.OTLPPrefix) {
		t.Fatalf("a box on the host was handed no export path of this node's: %q", export)
	}
	boot := config.DefaultBootstrap()
	h, err := api.Probes(api.ProbeOptions{
		Bootstrap: &boot, Runtime: &fakeProbeRuntime{configured: true, work: doingItsWork()},
		NodeID: "seats-1", Roles: []string{"seats"}, QueueBackend: "jetstream-leaf",
		Bridge: bridge, OtelReceiver: receiver,
	})
	if err != nil {
		t.Fatalf("Probes: %v", err)
	}
	ask := func(remote, method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader("spans"))
		r.RemoteAddr = remote
		r.Header.Set("Content-Type", "application/x-protobuf")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	code := func(rec *httptest.ResponseRecorder) string {
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		s, _ := body["error"].(string)
		return s
	}

	// OFF THE HOST: absent, every method, every route under both prefixes.
	for _, route := range [][2]string{
		{http.MethodPost, mcpbridge.PathPrefix + "a-token"},
		{http.MethodGet, mcpbridge.PathPrefix + "a-token"},
		{http.MethodDelete, mcpbridge.PathPrefix + "a-token"},
		{http.MethodGet, mcpbridge.PathPrefix + "a-token/steer/notes"},
		{http.MethodPost, export + "/v1/traces"},
		{http.MethodGet, export + "/v1/traces"},
	} {
		rec := ask("192.0.2.7:41000", route[0], route[1])
		if rec.Code != http.StatusNotFound || code(rec) != string(httpjson.CodeNoRoute) {
			t.Errorf("off the host, %s %s = %d %q, want the route's absence", route[0], route[1],
				rec.Code, code(rec))
		}
	}
	select {
	case path := <-forwarded:
		t.Fatalf("an export from off the host was forwarded to %s", path)
	default:
	}

	// ON THE HOST: the real routes, over either loopback family.
	for _, remote := range []string{"127.0.0.1:41000", "[::1]:41000"} {
		for _, path := range []string{mcpbridge.PathPrefix + "forged-token",
			mcpbridge.PathPrefix + "forged-token/steer/notes", auth.OTLPPrefix + "forged-token/v1/traces"} {
			rec := ask(remote, http.MethodPost, path)
			if rec.Code != http.StatusUnauthorized || code(rec) != string(httpjson.CodeInvalidToken) {
				t.Errorf("from %s, POST %s = %d %q, want a forged token refused", remote, path,
					rec.Code, code(rec))
			}
		}
		if rec := ask(remote, http.MethodPost, export+"/v1/traces"); rec.Code != http.StatusOK {
			t.Fatalf("from %s, a run's own export = %d %s, want it accepted", remote, rec.Code,
				rec.Body.String())
		}
		select {
		case path := <-forwarded:
			if path != "/v1/traces" {
				t.Errorf("the export was forwarded to %s, want /v1/traces", path)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("an accepted export never reached the upstream")
		}
	}

	// AND THE PROBES ARE UNCHANGED, to everybody.
	for _, path := range []string{"/health", "/ready"} {
		if rec := ask("192.0.2.7:41000", http.MethodGet, path); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 on a node doing its work", path, rec.Code)
		}
	}
}

// EVERY DEPENDENCY IS REQUIRED and refused by name, as [api.New] refuses its
// own: a probe surface built around a nil would answer with a node it cannot
// describe.
func TestTheProbeSurfaceRefusesAMissingDependency(t *testing.T) {
	t.Parallel()
	boot := config.DefaultBootstrap()
	runtime := &fakeProbeRuntime{}
	for name, opts := range map[string]api.ProbeOptions{
		"Tier A":  {Runtime: runtime, NodeID: "n"},
		"runtime": {Bootstrap: &boot, NodeID: "n"},
		"node id": {Bootstrap: &boot, Runtime: runtime},
	} {
		if _, err := api.Probes(opts); err == nil {
			t.Errorf("a probe surface with no %s was built", name)
		}
	}
}
