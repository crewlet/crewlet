package api_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/version"
)

// bootFor is the boot surface of a node b describes, answering as core-1.
func bootFor(t *testing.T, b config.Bootstrap) *api.Boot {
	t.Helper()
	boot, err := api.NewBoot(&b, "core-1")
	if err != nil {
		t.Fatalf("NewBoot: %v", err)
	}
	return boot
}

// askBoot runs one request through h and decodes its JSON answer.
func askBoot(t *testing.T, h http.Handler, method, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := sendTo(t, h, method, path)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s answered %d with a body that is not JSON: %q",
			method, path, rec.Code, rec.Body.String())
	}
	return rec, body
}

// A STARTING NODE'S /health IS THE LINE A STARTUP PROBE IS SIZED BY: 503 while
// the node recovers what it holds, which the deployment bounds, and 200 from
// the moment the engine says it has, whatever the company then takes to start
// — and the body says which, and nothing about a running engine it does not
// have yet.
func TestAStartingNodesHealthTurnsOnTheRecovery(t *testing.T) {
	t.Parallel()
	boot := bootFor(t, closedPosture())
	for _, phase := range []struct {
		name   string
		mark   func()
		status int
		phase  string
	}{
		{"recovering", func() {}, http.StatusServiceUnavailable, api.PhaseRecovering},
		{"preparing", boot.Recovered, http.StatusOK, api.PhasePreparing},
	} {
		phase.mark()
		rec, body := askBoot(t, boot.Handler(), http.MethodGet, "/health")
		want := map[string]any{
			"status": api.StatusStarting, "node": "core-1",
			"version": version.String(), "phase": phase.phase,
		}
		if rec.Code != phase.status || !reflect.DeepEqual(body, want) {
			t.Errorf("%s: /health = %d %v, want %d %v", phase.name, rec.Code, body,
				phase.status, want)
		}
	}
}

// /ready IS NEVER READY BEFORE THE NODE SERVES, and every other route waits,
// saying how long to and carrying the headers every answer carries — in both
// phases, since what the company configures is no more served while it starts
// than what the node holds is while it recovers.
func TestAStartingNodeIsNotReadyAndServesNothingElse(t *testing.T) {
	t.Parallel()
	boot := bootFor(t, closedPosture())
	for _, phase := range []struct {
		name string
		mark func()
	}{{"recovering", func() {}}, {"preparing", boot.Recovered}} {
		phase.mark()
		rec, ready := askBoot(t, boot.Handler(), http.MethodGet, "/ready")
		want := map[string]any{
			"ready": false, "node": "core-1", "configured": false,
			"draining": false, "posture": "", "reason": api.ReasonStarting,
		}
		if rec.Code != http.StatusServiceUnavailable || !reflect.DeepEqual(ready, want) {
			t.Errorf("%s: /ready = %d %v, want 503 %v", phase.name, rec.Code, ready, want)
		}
		for _, route := range [][2]string{
			{http.MethodGet, "/agents"}, {http.MethodGet, "/"}, {http.MethodGet, "/dashboard"},
			{http.MethodPut, "/config"}, {http.MethodPost, "/webhooks/github"},
			{http.MethodPost, "/operator/act/work_create"}, {http.MethodPost, "/health"},
		} {
			rec, body := askBoot(t, boot.Handler(), route[0], route[1])
			if rec.Code != http.StatusServiceUnavailable || body["error"] != string(httpjson.CodeStarting) {
				t.Errorf("%s: %s %s = %d %v, want 503 %q", phase.name, route[0], route[1],
					rec.Code, body, httpjson.CodeStarting)
			}
			if got, want := rec.Header().Get("Retry-After"),
				strconv.Itoa(int(api.BootRetryAfter/time.Second)); got != want {
				t.Errorf("%s: %s %s carries Retry-After %q, want %q", phase.name,
					route[0], route[1], got, want)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
				rec.Header().Get("Content-Security-Policy") == "" {
				t.Errorf("%s: %s %s carries no security headers: %v", phase.name,
					route[0], route[1], rec.Header())
			}
		}
	}
}

// A NODE TOLD TO STOP BEFORE IT SERVED ANSWERS A DRAIN'S ANSWERS, as far as a
// starting node can: shutting down outranks starting on /health as it outranks
// every status, /ready names the drain, and a route is refused as the drain
// gate refuses one — so a caller is sent to another node rather than told to
// come back to this one. /health keeps its PHASE'S status code: 503 while the
// node still recovers, since a stop does not end a recovery and the probe
// sized for one is still what bounds it, and 200 once it has.
func TestAStartingNodeThatIsStoppingAnswersAsADrain(t *testing.T) {
	t.Parallel()
	for _, phase := range []struct {
		name      string
		recovered bool
		status    int
		phase     string
	}{
		{"recovering", false, http.StatusServiceUnavailable, api.PhaseRecovering},
		{"preparing", true, http.StatusOK, api.PhasePreparing},
	} {
		boot := bootFor(t, closedPosture())
		if phase.recovered {
			boot.Recovered()
		}
		boot.Stopping()
		if rec, body := askBoot(t, boot.Handler(), http.MethodGet, "/health"); rec.Code != phase.status ||
			body["status"] != api.StatusShuttingDown || body["phase"] != phase.phase {
			t.Errorf("%s: /health = %d %v, want %d %q in phase %q", phase.name, rec.Code, body,
				phase.status, api.StatusShuttingDown, phase.phase)
		}
		if rec, body := askBoot(t, boot.Handler(), http.MethodGet, "/ready"); rec.Code != http.StatusServiceUnavailable ||
			body["reason"] != api.ReasonDraining || body["draining"] != true {
			t.Errorf("%s: /ready = %d %v, want 503 %q and draining", phase.name, rec.Code, body,
				api.ReasonDraining)
		}
		rec, body := askBoot(t, boot.Handler(), http.MethodPost, "/webhooks/github")
		if rec.Code != http.StatusServiceUnavailable || body["error"] != string(httpjson.CodeDraining) ||
			rec.Header().Get("Retry-After") != strconv.Itoa(int(api.DrainRetryAfter/time.Second)) {
			t.Errorf("%s: a delivery = %d %v (Retry-After %q), want the drain gate's 503",
				phase.name, rec.Code, body, rec.Header().Get("Retry-After"))
		}
	}
}

// EVERY ROUTE IS ANSWERED ON THE LISTENER THAT WILL SERVE IT AS IT WILL BE, and
// absent from the other exactly as it will be. Walked from the table a running
// node mounts, so a route added to it is held here too: on the listener that
// serves it, a starting node answers `503 starting`; on the other, the same 404
// the partition answers once the node serves — on the public side byte for
// byte, so the published socket says no more while the node starts than it
// does afterwards (the Crewlet Operator's own end-to-end suite asks /health of
// that socket and requires the 404).
func TestAStartingNodeAnswersEachRouteOnTheListenerThatWillServeIt(t *testing.T) {
	t.Parallel()
	a := listenerApp(t, true, false)
	b := closedPosture()
	b.API.Port = 8080
	b.API.Public = config.APIPublic{Port: 8443}
	boot := bootFor(t, b)
	sides := map[bool]struct {
		name        string
		app, starts http.Handler
	}{
		false: {"api.port", a, boot.Handler()},
		true:  {"the public listener", a.Public(), boot.Public()},
	}
	routes := a.Routes()
	if len(routes) < len(publishedRoutes)+len(adminRoutes) {
		t.Fatalf("the app mounted %d routes, fewer than the listener cases name", len(routes))
	}
	for _, pattern := range routes {
		method, path := requestFor(pattern)
		own, other := sides[auth.Public(path)], sides[!auth.Public(path)]
		if path != "/health" && path != "/ready" {
			if rec, body := askBoot(t, own.starts, method, path); rec.Code != http.StatusServiceUnavailable ||
				body["error"] != string(httpjson.CodeStarting) {
				t.Errorf("%q (%s %s) answered %d %v on %s while the node starts, want "+
					"503 %q", pattern, method, path, rec.Code, body, own.name, httpjson.CodeStarting)
			}
		}
		starting := sendTo(t, other.starts, method, path)
		serving := sendTo(t, other.app, method, path)
		if !absent(t, starting) || starting.Body.String() != serving.Body.String() {
			t.Errorf("%q (%s %s) answered %d %q on %s while the node starts, want "+
				"what it answers once the node serves: %d %q", pattern, method, path,
				starting.Code, starting.Body.String(), other.name, serving.Code,
				serving.Body.String())
		}
	}
	// THE PROBES ARE api.port's, answered there by the boot surface itself
	// and absent from the published socket as they will be.
	for _, path := range []string{"/health", "/ready"} {
		if rec := sendTo(t, boot.Public(), http.MethodGet, path); !absent(t, rec) {
			t.Errorf("GET %s answered %d on the public listener while the node starts, "+
				"want its 404", path, rec.Code)
		}
	}
}

// A NODE WITHOUT INGRESS SERVES THE NETWORK ITS PROBES AND NOTHING ELSE — a box
// off its host reaches its seats' tools through the nodes that serve the API,
// which relay its call here. So while it starts nothing but the probes answers
// a caller off the host on either socket: every other path is absent, as it is
// once the node serves, and no caller is told to come back for a route this
// node will never serve it.
func TestAStartingSeatsNodeServesOnlyItsProbes(t *testing.T) {
	t.Parallel()
	paths := []string{mcpbridge.PathPrefix + "a-token", mcpbridge.PathPrefix + "a-token/steer/notes",
		"/webhooks/github", "/otlp/a-token/v1/traces", "/agents", "/config"}
	for _, tc := range []struct {
		name   string
		public bool
	}{
		{"api.public set", true},
		{"api.public unset", false},
	} {
		b := closedPosture()
		b.Node.Roles = []string{"seats"}
		if tc.public {
			b.API.Public = config.APIPublic{Port: 8443}
		}
		boot := bootFor(t, b)
		for side, h := range map[string]http.Handler{"api.port": boot.Handler(), "api.public": boot.Public()} {
			for _, path := range paths {
				if rec := sendTo(t, h, http.MethodPost, path); !absent(t, rec) {
					t.Errorf("%s: POST %s answered %d on a seats node's %s, want its 404",
						tc.name, path, rec.Code, side)
				}
			}
		}
		// THE PROBES ARE api.port's alone, and answer there in either case.
		if rec := sendTo(t, boot.Handler(), http.MethodGet, "/health"); rec.Code != http.StatusServiceUnavailable ||
			!strings.Contains(rec.Body.String(), api.StatusStarting) {
			t.Errorf("%s: GET /health on api.port = %d %q, want the starting body",
				tc.name, rec.Code, rec.Body.String())
		}
		if rec := sendTo(t, boot.Public(), http.MethodGet, "/health"); !absent(t, rec) {
			t.Errorf("%s: GET /health on api.public = %d, want its 404", tc.name, rec.Code)
		}
	}
}

// AND A BOX ON ITS OWN HOST IS TOLD TO COME BACK. A seats node serves the two
// routes its own direct boxes dial on api.port, to this host's loopback alone
// ([api.Probes]), and a direct run outlives its node's process: while the node
// restarts, the run's calls reach the boot surface, and an absent route would
// read to its harness as a bridge that is gone rather than one about to answer. So
// a loopback caller of either route is answered `503 starting`, and every
// other path — and every caller off the host — exactly as before. A node that
// runs no seats launches no box, and tells nobody to come back for them.
//
// Mutation: drop the loopback check and the off-host rows are told to come
// back; register the routes on every node without ingress and the data node's
// row is.
func TestAStartingSeatsNodeTellsItsOwnBoxesToComeBack(t *testing.T) {
	t.Parallel()
	from := func(h http.Handler, remote, method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	boxes := []string{mcpbridge.PathPrefix + "a-token", mcpbridge.PathPrefix + "a-token/steer/notes",
		auth.OTLPPrefix + "a-token/v1/traces"}
	for _, tc := range []struct {
		name   string
		roles  []string
		remote string
		waits  bool
	}{
		{"a seats node, IPv4 loopback", []string{"seats"}, "127.0.0.1:41000", true},
		{"a seats node, IPv6 loopback", []string{"data", "seats"}, "[::1]:41000", true},
		{"a seats node, a caller off the host", []string{"seats"}, "192.0.2.7:41000", false},
		{"a node that runs no seats, loopback", []string{"data", "workers"}, "127.0.0.1:41000", false},
	} {
		b := closedPosture()
		b.Node.Roles = tc.roles
		h := bootFor(t, b).Handler()
		for _, path := range boxes {
			rec := from(h, tc.remote, http.MethodPost, path)
			switch {
			case tc.waits && (rec.Code != http.StatusServiceUnavailable ||
				!strings.Contains(rec.Body.String(), string(httpjson.CodeStarting))):
				t.Errorf("%s: POST %s = %d %q, want the starting 503", tc.name, path,
					rec.Code, rec.Body.String())
			case !tc.waits && !absent(t, rec):
				t.Errorf("%s: POST %s = %d, want its 404", tc.name, path, rec.Code)
			}
		}
		// NOTHING ELSE COMES BACK, however near the caller.
		for _, path := range []string{"/webhooks/github", "/agents", "/config"} {
			if rec := from(h, tc.remote, http.MethodPost, path); !absent(t, rec) {
				t.Errorf("%s: POST %s = %d, want its 404", tc.name, path, rec.Code)
			}
		}
	}
}

// A DASHBOARD ON AN ALLOWED ORIGIN READS A STARTING NODE'S REFUSAL, rather than
// the opaque network error a cross-origin answer with no permission on it is
// to a browser: the boot surface answers inside the same CORS layer the full
// surface does, a preflight included, and names no origin the file does not
// allow.
func TestAStartingIngressNodeAnswersInItsBrowserPosture(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	b.API.Auth.AllowedOrigins = []string{"https://dash.example.com"}
	boot := bootFor(t, b)
	ask := func(method, path, origin string, preflight bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Origin", origin)
		if preflight {
			r.Header.Set("Access-Control-Request-Method", http.MethodPut)
		}
		rec := httptest.NewRecorder()
		boot.Handler().ServeHTTP(rec, r)
		return rec
	}
	rec := ask(http.MethodPut, "/config", "https://dash.example.com", false)
	if rec.Code != http.StatusServiceUnavailable ||
		rec.Header().Get("Access-Control-Allow-Origin") != "https://dash.example.com" {
		t.Errorf("PUT /config from the allowed origin = %d with Access-Control-Allow-Origin %q, "+
			"want 503 readable by that origin", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec := ask(http.MethodOptions, "/config", "https://dash.example.com", true); rec.Code != http.StatusNoContent ||
		rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Errorf("a preflight from the allowed origin = %d %v, want the CORS layer's 204",
			rec.Code, rec.Header())
	}
	if rec := ask(http.MethodPut, "/config", "https://elsewhere.example.com", false); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("an origin the file does not allow was permitted: %q",
			rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

// lockedBuffer is a buffer the process's log sink and a test may both use.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A REFUSAL'S LOG LINE NEVER CARRIES THE RUN TOKEN A PATH DOES. The tool bridge
// and the OTLP receiver are authenticated by a signed token in their own path,
// and an executor calling back while its node restarts meets the boot
// surface's refusal; a line naming the raw path would put a live credential in
// the log. NOT PARALLEL: it installs this process's log sink for its duration,
// which only a case running alone may do.
//
// Mutation: log r.URL.Path in refuse and the token appears.
func TestAStartingNodeLogsNoRunTokenItRefuses(t *testing.T) {
	var out lockedBuffer
	logging.Configure(slog.LevelDebug, logging.FormatText, &out)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	boot := bootFor(t, closedPosture())
	for _, path := range []string{
		mcpbridge.PathPrefix + "the-secret-run-token",
		mcpbridge.PathPrefix + "the-secret-run-token/steer/notes",
		auth.OTLPPrefix + "the-secret-run-token/v1/traces",
	} {
		if rec := sendTo(t, boot.Handler(), http.MethodPost, path); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("POST %s = %d, want the starting 503 this case is about", path, rec.Code)
		}
	}
	logged := out.String()
	if !strings.Contains(logged, "api_refused_while_starting") {
		t.Fatalf("no refusal was logged, so this case asserts nothing: %q", logged)
	}
	if strings.Contains(logged, "the-secret-run-token") {
		t.Errorf("a refusal logged the run token its path carried: %q", logged)
	}
	for _, want := range []string{mcpbridge.PathPrefix + "{token}",
		mcpbridge.PathPrefix + "{token}/steer/notes", auth.OTLPPrefix + "{token}/v1/traces"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the refusals logged %q, want the route %q named with its token "+
				"replaced", logged, want)
		}
	}
}

// A PATH THAT READS ONE WAY TO A PERSON AND ANOTHER TO THE ROUTER is refused
// before anything reads it, exactly as the node refuses it once it serves —
// or `/webhooks/../config` would read to the public side as a public route.
func TestAStartingNodeRefusesANonCanonicalPath(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	b.API.Public = config.APIPublic{Port: 8443}
	boot := bootFor(t, b)
	for name, h := range map[string]http.Handler{"api.port": boot.Handler(), "public": boot.Public()} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.URL.Path = "/webhooks/../config"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if !strings.Contains(rec.Body.String(), string(httpjson.CodeNonCanonicalPath)) {
			t.Errorf("%s answered %d %q for a dot segment, want %q", name, rec.Code,
				rec.Body.String(), httpjson.CodeNonCanonicalPath)
		}
	}
}

// EVERY FIELD A STARTING NODE'S /health CARRIES IS ONE OF THE ENVELOPE'S, under
// the same name — so a script reading /health reads one shape whichever surface
// answered it — and the envelope declares each of them.
func TestAStartingNodesHealthIsPartOfTheEnvelope(t *testing.T) {
	t.Parallel()
	shared := map[string]bool{}
	envelope := reflect.TypeFor[api.Health]()
	for i := range envelope.NumField() {
		name, _, _ := strings.Cut(envelope.Field(i).Tag.Get("json"), ",")
		shared[name] = true
	}
	starting := reflect.TypeFor[api.BootHealth]()
	if starting.NumField() < 4 {
		t.Fatalf("BootHealth has %d fields; a walk that read none certifies nothing",
			starting.NumField())
	}
	for i := range starting.NumField() {
		name, _, _ := strings.Cut(starting.Field(i).Tag.Get("json"), ",")
		if !shared[name] {
			t.Errorf("BootHealth carries %q, which the health envelope does not: "+
				"one fact under two names is one a script reads wrong", name)
		}
	}
}

// THE BOOT SURFACE REFUSES WHAT IT CANNOT ANSWER AS, by name, as every surface
// here refuses a missing dependency.
func TestTheBootSurfaceRefusesAMissingDependency(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	if _, err := api.NewBoot(nil, "core-1"); err == nil {
		t.Error("a boot surface with no Tier A was built")
	}
	if _, err := api.NewBoot(&b, " "); err == nil {
		t.Error("a boot surface with no node id was built")
	}
}

// WHICH NODE BINDS api.public is one rule, read before the engine exists and
// again when it serves: an ingress node whenever the file sets one, and nothing
// else — not a node without ingress, whose seats an executor box reaches through
// the ingress nodes — and nothing on api.port 0.
func TestWhichNodesBindAPublicListener(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		roles  []string
		port   int
		public bool
		want   bool
	}{
		{"an ingress node with api.public", nil, 8080, true, true},
		{"an ingress node without it", nil, 8080, false, false},
		{"a seats node, whose seats' bridge is ingress's", []string{"seats"}, 8080, true, false},
		{"api.port 0 is the hard off switch", nil, 0, true, false},
	} {
		b := closedPosture()
		b.Node.Roles = tc.roles
		b.API.Port = tc.port
		if tc.public {
			b.API.Public = config.APIPublic{Port: 8443}
		}
		if got := api.PublicListener(&b, "core-1"); got != tc.want {
			t.Errorf("%s: PublicListener = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// ONLY AN INGRESS NODE BINDS THE ARTIFACT ORIGIN, where its file sets a port
// for it, and api.port 0 switches it off with every other listener: the rule
// `crewlet run` binds by before the boot and serves by after it.
func TestWhichNodesBindAnArtifactListener(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		roles     []string
		port      int
		artifacts int
		want      bool
	}{
		{"an ingress node with api.artifacts", nil, 8080, 8090, true},
		{"an ingress node without it", nil, 8080, 0, false},
		{"a node without ingress", []string{"data", "seats", "workers"}, 8080, 8090, false},
		{"api.port 0 is the hard off switch", nil, 0, 8090, false},
	} {
		b := closedPosture()
		b.Node.Roles = tc.roles
		b.API.Port = tc.port
		b.API.ExternalURL = "https://crewlet.example.test"
		b.API.Artifacts = config.APIArtifacts{Port: tc.artifacts,
			ExternalURL: "https://artifacts.example.test"}
		if got := api.ArtifactsListener(&b, "core-1"); got != tc.want {
			t.Errorf("%s: ArtifactsListener = %t, want %t", tc.name, got, tc.want)
		}
		if got := bootFor(t, b).Artifacts() != nil; got != tc.want {
			t.Errorf("%s: the boot surface has an artifact origin: %t, want %t", tc.name, got, tc.want)
		}
	}
}

// A STARTING ARTIFACT ORIGIN ANSWERS AS THE ORIGIN DOES: an empty refusal under
// its inert headers, never this surface's JSON, which would be a document an
// artifact page could read. A route it will serve is told to come back, with a
// starting node's Retry-After and then a drain's; a path or a method it never
// serves meets the 404 or the 405 it meets once the node serves.
func TestAStartingArtifactOriginAnswersAsTheOrigin(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	b.API.Port, b.API.ExternalURL = 8080, "https://crewlet.example.test"
	b.API.Artifacts = config.APIArtifacts{Port: 8090, ExternalURL: "https://artifacts.example.test"}
	boot := bootFor(t, b)
	origin := boot.Artifacts()
	if origin == nil {
		t.Fatal("an ingress node with api.artifacts has no starting artifact origin")
	}
	retry := func(d time.Duration) string { return strconv.Itoa(int(d / time.Second)) }
	for _, tc := range []struct {
		method, path string
		status       int
		retryAfter   string
		allow        string
	}{
		{http.MethodGet, "/t/ticket/index.html", http.StatusServiceUnavailable, retry(api.BootRetryAfter), ""},
		{http.MethodHead, "/t/ticket/_assets/logo.png", http.StatusServiceUnavailable, retry(api.BootRetryAfter), ""},
		{http.MethodGet, "/_crewlet/runtime/1/runtime.js", http.StatusServiceUnavailable, retry(api.BootRetryAfter), ""},
		{http.MethodGet, "/_crewlet/kit/1/kit.css", http.StatusServiceUnavailable, retry(api.BootRetryAfter), ""},
		{http.MethodGet, "/health", http.StatusNotFound, "", ""},
		{http.MethodGet, "/t//index.html", http.StatusNotFound, "", ""},
		{http.MethodPost, "/t/ticket/index.html", http.StatusMethodNotAllowed, "", "GET, HEAD"},
	} {
		rec := sendTo(t, origin, tc.method, tc.path)
		h := rec.Header()
		if rec.Code != tc.status || rec.Body.Len() != 0 || h.Get("Retry-After") != tc.retryAfter ||
			h.Get("Allow") != tc.allow || h.Get("Cache-Control") != "no-store" ||
			h.Get("Content-Security-Policy") == "" || h.Get("Content-Type") != "" {
			t.Errorf("%s %s = %d %q, headers %v; want an empty %d (Retry-After %q, Allow %q) "+
				"under the origin's refusal headers", tc.method, tc.path, rec.Code, rec.Body.String(),
				h, tc.status, tc.retryAfter, tc.allow)
		}
	}
	boot.Stopping()
	if rec := sendTo(t, origin, http.MethodGet, "/t/ticket/index.html"); rec.Code != http.StatusServiceUnavailable ||
		rec.Header().Get("Retry-After") != retry(api.DrainRetryAfter) {
		t.Errorf("a stopping origin = %d (Retry-After %q), want 503 with the drain's %q",
			rec.Code, rec.Header().Get("Retry-After"), retry(api.DrainRetryAfter))
	}
}
