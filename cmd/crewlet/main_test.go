package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcprelay"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

const companyYAML = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    primary:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
    llm: primary
  - name: CTO
    handle: cto
    llm: primary
`

// peopleCompanyYAML is [companyYAML] with a human seat, `founder`, for the
// cases that invite or create a person: a person holds a human seat for as
// long as they are here, so a company with none has nowhere to put anybody.
// Extended rather than edited, because every agent-only case shares
// companyYAML.
const peopleCompanyYAML = companyYAML + `  - name: Founder
    handle: founder
    kind: human
`

// configPair writes both tiers into a temp directory and returns the flags
// that point at them.
func configPair(t *testing.T, bootstrapYAML, company string) []string {
	t.Helper()
	dir := t.TempDir()
	boot := filepath.Join(dir, "crewlet.yaml")
	comp := filepath.Join(dir, "company.yaml")
	if bootstrapYAML == "" {
		bootstrapYAML = "store:\n  path: " + filepath.Join(dir, "crewlet.db") + "\n" +
			fixtureStream(dir) + fixtureKeyring
	}
	if err := os.WriteFile(boot, []byte(bootstrapYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(comp, []byte(company), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"-config", boot, "-company", comp}
}

func TestValidateReportsWhatTheConfigDescribes(t *testing.T) {
	t.Parallel()
	// Validate exists so a config can be checked without starting
	// anything, which means it must reach nothing: no broker, no store, no
	// provider. What it prints is the summary an operator uses to confirm
	// they edited the file they meant to.
	var out, errOut bytes.Buffer
	args := append([]string{"validate"}, configPair(t, "", companyYAML)...)
	if err := run(args, &out, &errOut); err != nil {
		t.Fatalf("validate: %v (stderr %s)", err, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		"Acme", "2 agent seats", "1 LLM providers", "embedded", "local",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q does not mention %q", got, want)
		}
	}
}

func TestValidateCatchesWhatASchemaCannot(t *testing.T) {
	t.Parallel()
	// The reason validate builds the epoch rather than just parsing: a
	// seat whose llm names no configured provider is well-formed YAML and
	// a valid document. It fails at the first turn, which is the worst
	// place to learn it.
	bad := strings.Replace(companyYAML, "llm: primary\n", "llm: nonexistent\n", 1)
	var out, errOut bytes.Buffer
	args := append([]string{"validate"}, configPair(t, "", bad)...)
	err := run(args, &out, &errOut)
	if err == nil {
		t.Fatal("a role naming an unconfigured provider validated")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("the error does not name the provider: %v", err)
	}
}

// A COMPANY WITH NO MODELS VALIDATES, as it does over the API and in every
// node's apply. Validate builds the epoch, and the build used to refuse an
// empty providers.llm, so the one command meant to predict a write's fate
// called invalid the company `PUT /config` had just stored and activated. Both
// forms are checked, because each prints a provider count read off the epoch's
// registry, and a company with no models has none.
func TestValidateAcceptsACompanyWithNoModels(t *testing.T) {
	t.Parallel()
	const noModels = "name: Acme\nroles:\n  - name: CEO\n    handle: ceo\n"
	for name, args := range map[string][]string{
		"one file":  {"validate", writeYAML(t, "company.yaml", noModels)},
		"two tiers": append([]string{"validate"}, configPair(t, "", noModels)...),
	} {
		var out, errOut bytes.Buffer
		if err := run(args, &out, &errOut); err != nil {
			t.Errorf("%s: a company with no models was refused: %v", name, err)
			continue
		}
		if !strings.Contains(out.String(), "0 LLM providers") {
			t.Errorf("%s: summary %q does not say the company has no provider", name, out.String())
		}
	}
}

func TestBothTiersAreReportedTogether(t *testing.T) {
	t.Parallel()
	// An operator fixing a broker URL only to be told about their org
	// chart on the next boot has been made to pay twice for one edit. It
	// is the rule each tier's own validator already follows internally.
	dir := t.TempDir()
	boot := filepath.Join(dir, "crewlet.yaml")
	comp := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(boot, []byte("stream:\n  type: kafka\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(comp, []byte("name: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	err := run([]string{"validate", "-config", boot, "-company", comp}, &out, &errOut)
	if err == nil {
		t.Fatal("two broken tiers validated")
	}
	msg := err.Error()
	if !strings.Contains(msg, "kafka") {
		t.Errorf("the bootstrap problem is missing: %v", err)
	}
	if !strings.Contains(msg, "name") {
		t.Errorf("the company problem is missing, so only the first tier was read: %v", err)
	}
}

func TestAMissingConfigNamesTheFileNotTheField(t *testing.T) {
	t.Parallel()
	// The default paths are relative, so a first run in the wrong
	// directory is the ordinary way to reach this. It must say which file
	// it could not find.
	var out, errOut bytes.Buffer
	err := run([]string{"validate",
		"-config", filepath.Join(t.TempDir(), "nope.yaml"),
		"-company", filepath.Join(t.TempDir(), "alsonope.yaml"),
	}, &out, &errOut)
	if err == nil {
		t.Fatal("a missing config validated")
	}
	if !strings.Contains(err.Error(), "nope.yaml") {
		t.Errorf("the error does not name the file: %v", err)
	}
}

func TestUsageNamesBothTiersAndTheirDefaults(t *testing.T) {
	t.Parallel()
	// The two tiers are the one thing a new operator has to understand
	// before anything else works, and the flag names alone do not say
	// which is which.
	var out bytes.Buffer
	if err := run([]string{"help"}, &out, &out); err == nil {
		t.Fatal("help returned no sentinel, so main would not treat it as help")
	}
	got := out.String()
	for _, want := range []string{"-config", "-company", "crewlet.yaml", "company.yaml", "validate"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage does not mention %q:\n%s", want, got)
		}
	}
}

func TestAnUnknownCommandIsRefusedWithUsage(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	err := run([]string{"strt"}, &out, &errOut)
	if err == nil {
		t.Fatal("an unknown command was accepted")
	}
	if !strings.Contains(err.Error(), "strt") {
		t.Errorf("the error does not name the command: %v", err)
	}
	if !strings.Contains(errOut.String(), "Usage:") {
		t.Error("an unknown command printed no usage")
	}
}

func TestNoCommandIsRefused(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if err := run(nil, &out, &errOut); err == nil {
		t.Error("an empty command line was accepted")
	}
}

func TestVersionPrints(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if err := run([]string{"version"}, &out, &errOut); err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.HasPrefix(out.String(), "crewlet ") {
		t.Errorf("version output = %q", out.String())
	}
}

func TestRunRefusesABadConfigBeforeStartingAnything(t *testing.T) {
	t.Parallel()
	// A node that boots on a bad config and discovers it at the first turn
	// has already told its peers it owns seats. This is the same check
	// validate makes, on the path that matters.
	bad := strings.Replace(companyYAML, "llm: primary\n", "llm: nonexistent\n", 1)
	var errOut bytes.Buffer
	args := append([]string{"run"}, configPair(t, "", bad)...)
	if err := run(args, &bytes.Buffer{}, &errOut); err == nil {
		t.Fatal("run started on a company whose seat names no provider")
	}
}

// bootstrapFor writes a Tier A pointing at a temp directory.
//
// WITH A KEYRING, because every record on every state log is signed under one
// and a node that has none refuses to start. It is not in
// [config.DefaultBootstrap]: a default key would be a key every deployment in
// the world shares, which is the opposite of what signing a record buys.
func bootstrapFor(t *testing.T, port int) *config.Bootstrap {
	t.Helper()
	dir := t.TempDir()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(dir, "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(dir, "stream"))
	b.API.Host = "127.0.0.1"
	b.API.Port = port
	// A COMPLETE SERVING POSTURE. Once a port is set, Tier A must carry an
	// external URL, an admin posture and a credential of its own, and every
	// guarded route needs that credential presented — so a fixture short
	// of any of them fails on its own configuration rather than on its
	// subject. [getJSON] is what presents it, carrying admin and with it
	// every app.
	if port != 0 {
		b.API.ExternalURL = "http://127.0.0.1:" + strconv.Itoa(port)
	}
	b.API.Auth.Admin = iam.AdminServe
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "fixture", Token: cliFixtureToken, Grants: []iam.Grant{iam.GrantAdmin}},
	}
	b.Secrets.ActiveKeyID = "test"
	b.Secrets.Keys = []config.SecretKey{{ID: "test", Material: testKeyMaterial(t)}}
	return &b
}

// testKeyMaterial mints one THROUGH THE MINTER `crewlet secrets keygen` uses,
// so the size, the randomness and the encoding are read off the production
// definition rather than restated here. Per call rather than a constant: two
// cases running in parallel share no key, and neither does a case that goes on
// to write one down.
func testKeyMaterial(t *testing.T) string {
	t.Helper()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("no randomness for the test keyring: %v", err)
	}
	return secrets.EncodeKey(key)
}

func TestAWorkerOnlyNodeServesNoHTTPAndSaysSo(t *testing.T) {
	t.Parallel()
	// A real posture: api.port 0 runs no dashboard, no REST API and no
	// webhook endpoint. Saying so is the point — an operator who expected
	// an integration to work should learn it here rather than from a
	// webhook that never arrives.
	//
	// The engine is not built at all: the port check comes first
	// precisely so a node that serves nothing does not pay to find out.
	// THE LOGGER IS THE TEST'S OWN, not the process root. Configuring the
	// root from a parallel test is a race with every other test in this
	// package: the CLI dispatch reconfigures it to WARN-on-stderr for
	// every non-`run` command, so whichever ran last decided where this
	// assertion's record went.
	//
	// AT WARN ONLY WHERE NOTHING ELSE WARNED. A node whose DECLARED roles
	// name ingress or seats has been told at WARN already, by the `api.port`
	// advisory its start logs, which leaves an undeclared node.roles out —
	// so that node is the one this line warns, and every other is told at
	// info. Mutation: warn on every ingress node and the declared-ingress
	// row goes red; warn on none and the undeclared row does.
	for _, tc := range []struct {
		name  string
		roles []string
		want  slog.Level
	}{
		{"undeclared roles, which are every role", nil, slog.LevelWarn},
		{"declared roles naming ingress", []string{"data", "ingress", "seats", "workers"}, slog.LevelInfo},
		{"declared roles without ingress", []string{"data", "seats", "workers"}, slog.LevelInfo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logged bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))
			boot := bootstrapFor(t, 0)
			boot.Node.Roles = tc.roles

			// NOTHING BOUND before the boot, asked of the one path every
			// listener is opened through.
			listeners, err := bindListeners(t.Context(), boot, "node-0", bindNothing(t), log)
			if err != nil || listeners != nil {
				t.Fatalf("bindListeners on api.port 0 = %v, %v; want nothing bound", listeners, err)
			}
			surface, err := serveAPI(t.Context(), boot, nil, nil, nil, nil, nil, nil, listeners, engine.DeepHost{}, log)
			if err != nil {
				t.Fatalf("serveAPI: %v", err)
			}
			if surface != nil {
				t.Error("a node with api.port 0 built an HTTP surface")
			}
			var record struct {
				Msg   string `json:"msg"`
				Level string `json:"level"`
				Hint  string `json:"hint"`
			}
			if err = json.Unmarshal(logged.Bytes(), &record); err != nil || record.Msg != "api_disabled" {
				t.Fatalf("the node did not say it serves no HTTP, in one line:\n%s", logged.String())
			}
			if record.Level != tc.want.String() {
				t.Errorf("api_disabled at %s, want %s", record.Level, tc.want)
			}
			if record.Hint == "" {
				t.Error("api_disabled does not say what the node gave up")
			}
		})
	}
}

// A NODE WHOSE ROLES LEAVE OUT INGRESS SERVES ITS PROBES ON api.port, AND
// NOTHING ELSE. An orchestrator runs every node and has to be able to restart
// one that wedged and wait on one that has not joined, so /health and /ready
// answer here; the API, the dashboard and the webhooks are a peer's, and a
// satellite placed on a private host for exactly that reason must not serve
// them.
func TestANodeWithoutTheIngressRoleServesOnlyItsProbes(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

	e := testEngine(t)
	boot := bootstrapFor(t, 0)
	boot.API.Host = "127.0.0.1"
	boot.API.Port = reservePort(t)
	boot.Node.Roles = []string{"data", "seats", "workers"}

	surface, err := serveNodeLogged(t, boot, e, log)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	if surface == nil {
		t.Fatalf("a node without the ingress role bound no listener, so no "+
			"orchestrator can probe it:\n%s", logged.String())
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	if surface.app != nil || surface.projector != nil {
		t.Error("a node without the ingress role built the whole API")
	}
	if !strings.Contains(logged.String(), "api_probes_listening") {
		t.Errorf("the node did not say what its listener serves:\n%s", logged.String())
	}

	base := "http://127.0.0.1:" + strconv.Itoa(boot.API.Port)
	health := getJSON(t, base+"/health")
	if health["node"] != e.Node().ID() || health["configured"] != true {
		t.Errorf("/health = %v, want this node, configured", health)
	}
	if roles, _ := health["roles"].([]any); len(roles) != 3 {
		t.Errorf("/health roles = %v, want the three this node runs", health["roles"])
	}
	if _, present := health["clients"]; present {
		t.Errorf("/health carries the dashboard's client count on a node that "+
			"serves no dashboard: %v", health)
	}
	if got := statusOf(t, http.MethodGet, base+"/health"); got != http.StatusOK {
		t.Errorf("GET /health = %d, want the liveness probe's 200", got)
	}
	// THE PROBE SURFACE'S JUDGEMENT, over the work this node must be doing.
	// The engine here was never started, so it holds no presence lease — a
	// fact only that judgement reads, and the one that outranks admission —
	// so a listener wired to the wrong runtime, or to the ingress answer,
	// says something else.
	if got := statusOf(t, http.MethodGet, base+"/ready"); got != http.StatusServiceUnavailable {
		t.Errorf("GET /ready = %d on a node holding no presence lease, want 503", got)
	}
	ready := getJSON(t, base+"/ready")
	if ready["node"] != e.Node().ID() || ready["ready"] != false ||
		ready["reason"] != api.ReasonNoPresence {
		t.Errorf("/ready = %v, want this node, not ready, for %q", ready, api.ReasonNoPresence)
	}
	// REFUSED, by the router's 404 or by the guard in front of it: an
	// always-guarded prefix (/config, /operator/) and every write answer 401
	// to a caller with no token before any route is looked up, exactly as the
	// full surface answers them.
	for _, route := range [][2]string{
		{http.MethodGet, "/"}, {http.MethodGet, "/dashboard"}, {http.MethodGet, "/agents"},
		{http.MethodGet, "/org"}, {http.MethodGet, "/events"},
		{http.MethodGet, "/stream/snapshot"}, {http.MethodGet, "/query/agents"},
		{http.MethodGet, "/ws/stream"}, {http.MethodGet, "/config"},
		{http.MethodPost, "/webhooks/github"}, {http.MethodPost, "/config"},
		{http.MethodPost, "/backup"}, {http.MethodPost, "/operator/mcp"},
	} {
		if got := statusOf(t, route[0], base+route[1]); got != http.StatusNotFound &&
			got != http.StatusUnauthorized {
			t.Errorf("%s %s = %d on a probe listener, want it refused", route[0], route[1], got)
		}
	}
}

// A NODE WITHOUT INGRESS SERVES ITS PROBES, AND ITS OWN BOXES' ROUTES ONLY
// WHERE IT LAUNCHES BOXES. A box off the host reaches its tools through the
// nodes that serve the API, which relay its call to the node holding the seat
// over the broker, so no dashboard or REST route is here; a box on the host is
// a seats node's own, which serves it the bridge on loopback (internal/api's
// Probes). A node that runs no seats launches none, so whatever its
// environment says about a bridge, the bridge's prefix is absent there.
//
// Mutation: hand the probe surface the bridge on every node and the data
// node's row answers the bridge's 401.
func TestANodeWithoutIngressServesItsProbesAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		roles  []string
		bridge int // what the bridge's prefix answers a caller on this host
	}{
		{[]string{"seats"}, http.StatusUnauthorized},
		{[]string{"data", "workers"}, http.StatusNotFound},
	} {
		roles := tc.roles
		var logged bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

		port := reservePort(t)
		bridge := bridgeFor(t, mcpbridge.Options{
			Material: runtoken.OneKey("k", "test-key"), BaseURL: "http://127.0.0.1:" + strconv.Itoa(port),
		})
		e := testEngineWithBridge(t, bridge)
		boot := bootstrapFor(t, port)
		boot.Node.Roles = roles

		surface, err := serveNodeLogged(t, boot, e, log)
		if err != nil {
			t.Fatalf("%v: serveAPI: %v", roles, err)
		}
		if surface == nil {
			t.Fatalf("%v: a node without ingress bound no listener for its probes:\n%s", roles, logged.String())
		}
		t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
		if surface.app != nil || surface.projector != nil {
			t.Errorf("%v: a node without the ingress role built the whole API", roles)
		}

		base := "http://127.0.0.1:" + strconv.Itoa(port)
		if got := statusOf(t, http.MethodGet, base+"/health"); got != http.StatusOK {
			t.Errorf("%v: GET /health = %d, want the liveness probe's 200", roles, got)
		}
		// AN EXEMPT PATH THIS LISTENER DOES NOT MOUNT 404s: the guard lets
		// it through to a mux that has nothing there. The bridge's prefix is
		// exempt too, and mounted — to this host — only on the seats node,
		// where a token the fleet did not sign meets the bridge's 401.
		if got := statusOf(t, http.MethodPost, base+"/dashboard"); got != http.StatusNotFound {
			t.Errorf("%v: POST /dashboard = %d on a probe listener, want 404", roles, got)
		}
		if got := statusOf(t, http.MethodPost, base+mcpbridge.PathPrefix+"not-a-token"); got != tc.bridge {
			t.Errorf("%v: POST %snot-a-token = %d on a probe listener, want %d", roles,
				mcpbridge.PathPrefix, got, tc.bridge)
		}
		// AND A GUARDED ONE IS REFUSED RATHER THAN 404, which is exactly
		// what the full surface answers now that every guarded route needs
		// a credential: the guard runs before routing, so it cannot know
		// the route is absent, and a 404 here would have disclosed that.
		if got := statusOf(t, http.MethodGet, base+"/agents"); got != http.StatusUnauthorized {
			t.Errorf("%v: GET /agents = %d on a probe listener, want the guard's 401", roles, got)
		}
	}
}

// statusOf is the status one request answers, retried while the listener is
// still coming up.
func statusOf(t *testing.T, method, url string) int {
	t.Helper()
	// A POOL OF THIS TEST'S OWN rather than http.DefaultClient's: this
	// binary holds eight httptest servers and over a hundred parallel tests,
	// and every one of their cleanups sweeps the shared pool — see
	// [github.com/crewlet/crewlet/internal/httpx/httpxtest].
	probe := httpxtest.Pool(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		res, err := probe.Do(req)
		if err == nil {
			_ = res.Body.Close()
			return res.StatusCode
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAnUnbindablePortIsReportedRatherThanIgnored(t *testing.T) {
	t.Parallel()
	// A port already in use, or one this process may not have, is a
	// configuration problem an operator has to see — not a node that
	// starts cleanly and is silently unreachable.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	port := taken.Addr().(*net.TCPAddr).Port

	e := testEngine(t)
	boot := bootstrapFor(t, port)
	surface, err := serveNode(t, boot, e)
	if err == nil {
		surface.stop(context.Background(), logging.Get("test"))
		t.Fatal("binding a port already in use reported success")
	}
	if !strings.Contains(err.Error(), "bind") {
		t.Errorf("the error does not say what failed: %v", err)
	}
}

func TestANodeServesItsOwnHealth(t *testing.T) {
	t.Parallel()
	// One process is both engine and API, sharing one broker and one
	// store. The API half is what makes the node reachable at all (every
	// inbound webhook arrives through it), so an engine that ran without it
	// would hold seats and hear nothing.
	e := testEngine(t)
	boot := bootstrapFor(t, 0)
	boot.API.Port = reservePort(t)

	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })

	body := getJSON(t, "http://"+surface.server.socket.Addr().String()+"/health")

	// The engine's own answers, which only the process running it has.
	if _, present := body["in_flight"]; !present {
		t.Errorf("the body carries no in-flight count: %v", body)
	}
	if body["started_at"] == "" || body["started_at"] == nil {
		t.Errorf("started_at = %v, want the engine's own start", body["started_at"])
	}
	if body["configured"] != true {
		t.Errorf("configured = %v: the node built an epoch and did not say so, "+
			"so it would be permanently unready", body["configured"])
	}
	if body["queue"] == "" || body["queue"] == nil {
		t.Errorf("queue = %v, want the broker named", body["queue"])
	}
}

func TestAMergedNodeSeedsItsDashboardFromItsStore(t *testing.T) {
	t.Parallel()
	// A RESTART IS NOT AN EMPTY COMPANY. Everything the live projection
	// serves, the activity feed and the spend rollup alike, used to start at
	// this process's boot beside a store that said otherwise. serveAPI is the
	// one place that seeds it, so the case runs through serveAPI: a unit test
	// of the seed alone would pass with the call deleted from the wiring.
	e := testEngine(t)
	ev := events.New(types.AgentPhaseCompleted{
		RoleName: "CEO", Agent: "a-1", TurnID: "tn-1", Phase: types.PhaseExecute,
		Model: "claude-sonnet-5", InputTokens: 20, OutputTokens: 22, TotalTokens: 42,
	}, events.TraceContext{})
	ev.Timestamp = time.Now().UTC().Add(-time.Hour)
	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("a phase completion did not render as a store row")
	}
	if err := e.Backends().Store.Events().Append(t.Context(), rec); err != nil {
		t.Fatalf("append: %v", err)
	}

	boot := bootstrapFor(t, 0)
	boot.API.Port = reservePort(t)
	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	snapshot := getJSON(t, "http://127.0.0.1:"+strconv.Itoa(boot.API.Port)+"/stream/snapshot")

	listed := false
	feed, _ := snapshot["events"].([]any)
	for _, row := range feed {
		if fields, _ := row.(map[string]any); fields["id"] == rec.ID {
			listed = true
		}
	}
	if !listed {
		t.Errorf("the snapshot's feed = %v, want the stored phase listed", feed)
	}
	rollup, _ := snapshot["tokens"].(map[string]any)
	totals, _ := rollup["totals"].(map[string]any)
	if totals["total_tokens"] != float64(42) {
		t.Errorf("the snapshot's spend totals = %v, want the stored phase's 42 tokens", totals)
	}
}

// A NODE NAMED THROUGH CREWLET_NODE_ID ANSWERS AS ITSELF, on its health body
// and in its backups.
//
// The raw `node.id` is empty on exactly the node a container orchestrator runs,
// which injects the variable and leaves the key out, and both surfaces used to
// read that raw field: /health answered as the default node-0 while the node's
// presence lease named it correctly, and a backup was keyed on a blank id, a
// trim hold every such node shared and a backup point the fleet's register
// refuses. Not parallel, because it sets the process environment.
func TestANodeNamedByTheEnvironmentAnswersAsItself(t *testing.T) {
	const name = "node-from-env"
	t.Setenv(config.NodeIDEnvVar, name)
	// THE BACKUP'S REASON IS THE LOG'S ALONE (the body says backup_failed),
	// so a red run prints it rather than discarding it.
	keepProcessLog(t)
	e := testEngine(t)
	boot := bootstrapFor(t, 0)
	boot.API.Port = reservePort(t)
	// THE PERMISSION A BACKUP TAKES. The token carried none and took a
	// backup all the same, because the route asked only whether somebody
	// resolved; it takes `admin` now.
	boot.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: "a-test-token",
		Grants: []iam.Grant{iam.GrantAdmin}}}
	if boot.Node.ID != "" {
		t.Fatalf("the bootstrap names node %q itself, so this case proves nothing",
			boot.Node.ID)
	}

	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	base := "http://127.0.0.1:" + strconv.Itoa(boot.API.Port)

	if got := getJSON(t, base+"/health")["node"]; got != name {
		t.Errorf("health node = %v, want %q from %s", got, name, config.NodeIDEnvVar)
	}

	dir := filepath.Join(t.TempDir(), "backup")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/backup?dir="+dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer a-test-token")
	// A POOL OF THIS TEST'S OWN rather than http.DefaultClient's: every
	// httptest.Server.Close in this binary sweeps the process-global pool,
	// and internal/httpx's guard fails the build over a use of it.
	res, err := httpxtest.Pool(t).Do(req)
	if err != nil {
		t.Fatalf("POST /backup: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	var manifest map[string]any
	if err := json.NewDecoder(res.Body).Decode(&manifest); err != nil {
		t.Fatalf("decode the backup answer: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /backup = %d: %v", res.StatusCode, manifest)
	}
	if manifest["node_id"] != name {
		t.Errorf("the backup is keyed on node %v, want %q", manifest["node_id"], name)
	}
}

// serveNode builds the API half of a node the way runEngine does: the
// process's chat hub beside the engine, the reconciler that owns the posture,
// the listener, then the config surface (a node with api.port 0 still needs a
// config WRITER) and the rest of what the listener serves.
func serveNode(t *testing.T, boot *config.Bootstrap, e *engine.Engine) (*httpSurface, error) {
	t.Helper()
	return serveNodeLogged(t, boot, e, logging.Get("test"))
}

// serveNodeLogged is [serveNode] logging to log, for a case that asserts what
// the node said about its listener.
func serveNodeLogged(t *testing.T, boot *config.Bootstrap, e *engine.Engine,
	log *slog.Logger,
) (*httpSurface, error) {
	t.Helper()
	surface, _, err := serveNodeWith(t, boot, e, log)
	return surface, err
}

// serveNodeWith is [serveNodeLogged] handing back the reconciler too, for a
// case that runs the reconcile loop the way runEngine does rather than serving
// a node whose epoch never moves.
func serveNodeWith(t *testing.T, boot *config.Bootstrap, e *engine.Engine,
	log *slog.Logger,
) (*httpSurface, *engine.Reconciler, error) {
	t.Helper()
	// THE PROCESS'S CHAT HUB, as `crewlet run` attaches it beside the engine.
	chatLive := api.NewEngineChatLive(t.Context(), e)
	t.Cleanup(func() { chatLive.Stop(context.WithoutCancel(t.Context())) })
	return serveNodeOver(t, boot, e, chatLive, log)
}

// serveNodeOver is [serveNodeWith] over a chat hub the case built.
func serveNodeOver(t *testing.T, boot *config.Bootstrap, e *engine.Engine,
	chatLive *api.EngineChatLive, log *slog.Logger,
) (*httpSurface, *engine.Reconciler, error) {
	t.Helper()
	reconciler := reconcilerFor(t, boot, e)
	// BOUND AS runEngine BINDS, on the socket the case reserved for each port
	// it configured rather than a second bind of a port it let go (see
	// [reservePort]) — and here after the engine has booted, which is the
	// one difference from a running node: the boot surface's own answers are
	// [TestAStartingNodeAnswersItsProbesBeforeItServes]'s.
	listeners, err := bindListeners(t.Context(), boot, e.Node().ID(), bindReserved, log)
	if err != nil {
		return nil, reconciler, err
	}
	surface, err := serveBound(t, boot, e, reconciler, chatLive, listeners, log)
	return surface, reconciler, err
}

// reconcilerFor is the reconciler that owns e's config posture, as runEngine
// builds it, under the node id boot names.
func reconcilerFor(t *testing.T, boot *config.Bootstrap, e *engine.Engine) *engine.Reconciler {
	t.Helper()
	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		t.Fatalf("node identity: %v", err)
	}
	reconciler, err := e.NewReconciler(t.Context(), engine.ReconcilerOptions{
		Store: e.Backends().Store, Fleet: e.Backends().Fleet,
		Queue: e.Backends().Queue, NodeID: nodeID,
	})
	if err != nil {
		t.Fatalf("reconciler: %v", err)
	}
	return reconciler
}

// serveBound hands listeners a case bound itself the node's surface, over the
// chat hub the case attached beside e, as [serveNodeOver] does after its own
// bind: for a case whose engine has to be built from what the bind derived, as
// runEngine builds it ([bootEngine]).
func serveBound(t *testing.T, boot *config.Bootstrap, e *engine.Engine,
	reconciler *engine.Reconciler, chatLive *api.EngineChatLive,
	listeners *nodeListeners, log *slog.Logger,
) (*httpSurface, error) {
	t.Helper()
	cipher := e.Cipher()
	// THROUGH runEngine's OWN CONSTRUCTOR, so a node this builds holds the
	// config surface exactly where a running one would — and none on a node
	// holding no estate.
	configSurface, err := companySurface(e, boot, cipher)
	if err != nil {
		t.Fatalf("config surface: %v", err)
	}
	surface, err := serveAPI(t.Context(), boot, e, reconciler, cipher, configSurface,
		chatLive.Hub(), nil, listeners, engine.DeepHost{}, log)
	if err != nil {
		// THE CALLER'S TO CLOSE until the handover, as in runEngine.
		listeners.close(context.Background(), log)
	}
	return surface, err
}

// THE CHAT HUB IS THE PROCESS'S, AND IT SERVES SOCKETS ONLY WHERE THE API IS
// MOUNTED OVER IT.
//
// It is attached beside the engine on every node, a node that serves no API
// included — a seat raises its working indicator wherever its turn runs — and
// what it says to the engine's live relay is whether this node shows anybody
// what it hears: false where no socket surface was built over it, which is
// what stops a node with no listener following a peer's batches for nobody,
// and true once the API is.
func TestTheChatHubServesSocketsOnlyWhereTheAPIIsMountedOverIt(t *testing.T) {
	t.Parallel()
	log := logging.Get("test")
	e := testEngine(t)
	chatLive := api.NewEngineChatLive(t.Context(), e)
	t.Cleanup(func() { chatLive.Stop(context.WithoutCancel(t.Context())) })

	surface, err := serveAPI(t.Context(), bootstrapFor(t, 0), e, nil, nil, nil,
		chatLive.Hub(), nil, nil, engine.DeepHost{}, log)
	if err != nil || surface != nil {
		t.Fatalf("a node with api.port 0 served %v (%v)", surface, err)
	}
	if chatLive.Hub().ServesSockets() {
		t.Fatal("a node that serves no API says its hub shows somebody what it hears")
	}

	surface, _, err = serveNodeOver(t, bootstrapFor(t, reservePort(t)), e, chatLive, log)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.WithoutCancel(t.Context()), log) })
	if !chatLive.Hub().ServesSockets() {
		t.Error("the API was mounted over the hub and the hub does not know it")
	}
	// AND IT IS STOPPED ONCE, whichever teardown reaches it first.
	chatLive.Stop(t.Context())
	chatLive.Stop(t.Context())
}

// testEngine builds a real engine on an embedded stream in a temp directory.
func testEngine(t *testing.T) *engine.Engine {
	t.Helper()
	return testEngineWithBridge(t, nil)
}

// testEngineWithBridge is testEngine holding the given tool bridge. Nil builds
// the bridge from the environment, as a node does.
func testEngineWithBridge(t *testing.T, bridge *mcpbridge.Bridge) *engine.Engine {
	t.Helper()
	return testEngineWith(t, engine.Options{Bridge: bridge})
}

// testEngineWith is testEngine built from opts, on a bootstrap of its own and
// the [companyYAML] company where opts names none.
func testEngineWith(t *testing.T, opts engine.Options) *engine.Engine {
	t.Helper()
	opts.Bootstrap = bootstrapFor(t, 0)
	seedEngineStore(t, opts.Bootstrap)
	if opts.Company == nil {
		company, err := config.ParseCompany([]byte(companyYAML))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		opts.Company = company
	}
	e, err := engine.New(t.Context(), opts)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}

// seedEngineStore writes the binary's migrated store image wherever boot's
// engine will open an estate, so the boot finds every migration applied rather
// than running the whole sequence on a fresh file — none of these cases is
// about migrating. The rule is internal/engine's SeedStore: the node estate
// always, the replicated one only where the node holds it
// ([engine.HoldsEstate]), and nothing for a scratch store, which a boot
// discards.
func seedEngineStore(t *testing.T, boot *config.Bootstrap) {
	t.Helper()
	if boot.Store.Scratch {
		return
	}
	storetest.Seed(t, store.EstateNode, boot.Store.Path)
	if engine.HoldsEstate(boot) {
		storetest.Seed(t, store.EstateReplicated,
			store.ReplicatedPath(boot.Store.Path, boot.Store.ReplicatedPath))
	}
}

// reserved is every listening socket a case of this package holds for a port
// it is about to configure a node with, by address. An address names at most
// one socket — holding it is what makes it unique — so cases running in
// parallel share the map and never an entry.
var reserved = struct {
	sync.Mutex
	held map[string]net.Listener
}{held: map[string]net.Listener{}}

// reservePort is a loopback port this case HOLDS until a node it serves binds
// that address, which [bindReserved] hands the very socket; one no node binds
// is closed when the case ends.
//
// HELD RATHER THAN FOUND FREE. A port found free and let go is one anything
// else on the machine can take before the node binds it, and the rest of the
// suite opens sockets by the thousand while a node's bind waits behind its
// dashboard's projection seed, which under the race detector takes seconds.
// That is how TestReadmittingANodeBelowTheFloorIsRefused failed on main:
// `bind 127.0.0.1:35497: … address already in use`, on a port another process
// took in that gap.
func reservePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	reserved.Lock()
	reserved.held[addr] = l
	reserved.Unlock()
	t.Cleanup(func() {
		if l := takeReserved(addr); l != nil {
			_ = l.Close()
		}
	})
	//nolint:errcheck // Listen on a TCP address always yields *TCPAddr.
	return l.Addr().(*net.TCPAddr).Port
}

// takeReserved removes and returns the socket reserved for addr, or nil.
func takeReserved(addr string) net.Listener {
	reserved.Lock()
	defer reserved.Unlock()
	l := reserved.held[addr]
	delete(reserved.held, addr)
	return l
}

// bindReserved is the [bindFunc] every node this package serves binds
// through: an address [reservePort] holds is handed that socket, once, and
// any other is bound as the binary binds it — so a case holding a port of its
// own, to see a bind refused, meets the real refusal.
func bindReserved(ctx context.Context, addr string) (net.Listener, error) {
	if l := takeReserved(addr); l != nil {
		return l, nil
	}
	return bindTCP(ctx, addr)
}

// bindNothing is the [bindFunc] for a node that must open no listener at all:
// a bind it asks for fails the case, naming the address. Every listener a
// node opens is opened through its bindFunc, so this sees each one, which a
// probe of one port after the fact could not.
func bindNothing(t *testing.T) bindFunc {
	return func(_ context.Context, addr string) (net.Listener, error) {
		t.Errorf("the node bound %s, and should bind nothing", addr)
		return nil, fmt.Errorf("bind %s: this node binds nothing", addr)
	}
}

// A RESERVED PORT IS NOBODY ELSE'S UNTIL ITS NODE SERVES ON IT. Between the
// reservation and the bind, any other socket asking for the port is refused,
// and the node then answers on it; the found-free port this replaced was
// free in exactly that gap, which is what [reservePort] says it cost.
func TestAReservedPortIsHeldUntilItsNodeServesOnIt(t *testing.T) {
	t.Parallel()
	e := testEngine(t)
	boot := bootstrapFor(t, reservePort(t))
	if l, err := net.Listen("tcp", boot.API.Addr()); err == nil {
		_ = l.Close()
		t.Fatalf("%s was free between its reservation and the node's bind, so "+
			"anything else on the machine could have taken it", boot.API.Addr())
	}
	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serveAPI on a reserved port: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	if got := surface.server.socket.Addr().String(); got != boot.API.Addr() {
		t.Errorf("the node serves on %s, want the reserved %s", got, boot.API.Addr())
	}
	if status, _ := answer(t, http.MethodGet, "http://"+boot.API.Addr()+"/health"); status != http.StatusOK {
		t.Errorf("GET /health on the reserved port = %d, want 200", status)
	}
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	// Its own pool, for [httpxtest]'s reason.
	probe := httpxtest.Pool(t)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		// THE FIXTURE'S CREDENTIAL ON EVERY CALL, exempt paths
		// included: presenting one where none is needed changes
		// nothing, and leaving it off a guarded one answers 401 with
		// an empty body, which decodes as a snapshot with no rows.
		req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if reqErr != nil {
			t.Fatalf("request %s: %v", url, reqErr)
		}
		req.Header.Set("Authorization", "Bearer "+cliFixtureToken)
		res, err := probe.Do(req)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		defer res.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
		return body
	}
	t.Fatalf("%s never answered", url)
	return nil
}

// THE RUN FLAGS THAT OVERRIDE TIER A. Their right value depends on where the
// process is running rather than on what the company is — which job this
// node does in a fleet, and where its HTTP surface binds — so they are
// flags. Everything else in Tier A belongs in the file.
func TestRunFlagsOverrideTheBootstrap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		args  []string
		file  func(*config.Bootstrap)
		check func(*testing.T, *config.Bootstrap)
	}{
		{
			name: "roles narrow what this node does",
			args: []string{"-roles", "data,ingress,workers"},
			check: func(t *testing.T, b *config.Bootstrap) {
				roles, err := b.Node.RoleSet()
				if err != nil {
					t.Fatalf("RoleSet: %v", err)
				}
				if !roles.Has(placement.RoleIngress) || !roles.Has(placement.RoleWorkers) {
					t.Errorf("roles = %v", roles)
				}
				if roles.Has(placement.RoleSeats) {
					t.Error("a node told to run ingress and workers still claims seats")
				}
			},
		},
		{
			name: "whitespace and a trailing comma are not roles",
			args: []string{"-roles", " data , "},
			check: func(t *testing.T, b *config.Bootstrap) {
				roles, err := b.Node.RoleSet()
				if err != nil {
					t.Fatalf("RoleSet: %v", err)
				}
				if len(roles) != 1 || !roles.Has(placement.RoleData) {
					t.Errorf("roles = %v, want only data", roles)
				}
			},
		},
		{
			// A FILE THAT ALREADY SAYS STATELESS takes a stateless role
			// set from the flag, which is how one image runs as either.
			name: "a stateless file takes a stateless role set",
			args: []string{"-roles", "seats"},
			file: func(b *config.Bootstrap) {
				b.Store.Scratch = true
				b.Stream.Leaf.URLs = []string{"nats-leaf://data-a.example.com:7422"}
			},
			check: func(t *testing.T, b *config.Bootstrap) {
				roles, err := b.Node.RoleSet()
				if err != nil {
					t.Fatalf("RoleSet: %v", err)
				}
				if roles.Has(placement.RoleData) || !roles.Has(placement.RoleSeats) {
					t.Errorf("roles = %v, want seats alone", roles)
				}
			},
		},
		{
			name: "the api bind is overridden",
			args: []string{"-api-host", "127.0.0.1", "-api-port", "9999"},
			check: func(t *testing.T, b *config.Bootstrap) {
				if b.API.Host != "127.0.0.1" || b.API.Port != 9999 {
					t.Errorf("api = %s:%d", b.API.Host, b.API.Port)
				}
			},
		},
		{
			// ZERO IS A REAL VALUE — "serve no HTTP at all" — which is
			// why the flag's unset sentinel cannot be zero.
			name: "port zero disables the surface",
			args: []string{"-api-port", "0"},
			check: func(t *testing.T, b *config.Bootstrap) {
				if b.API.Port != 0 {
					t.Errorf("api.port = %d, want 0", b.API.Port)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot := &config.Bootstrap{API: config.API{Host: "0.0.0.0", Port: 8080}}
			if tc.file != nil {
				tc.file(boot)
			}
			fs, roles, host, port := parseOverrides(t, tc.args)
			if err := overrideNode(boot, fs, roles, host, port); err != nil {
				t.Fatalf("overrideNode: %v", err)
			}
			tc.check(t, boot)
		})
	}
}

// A FLAG CANNOT TAKE `data` AWAY FROM A NODE WHOSE FILE KEEPS A DURABLE STORE.
// The flag is applied after the file validated, so without the role rules
// re-run here it would boot a node whose roles say stateless on a store that
// was never marked scratch — neither a data node nor a disposable one.
func TestARolesFlagThatDropsDataFromADurableNodeIsRefused(t *testing.T) {
	t.Parallel()
	boot := &config.Bootstrap{API: config.API{Host: "0.0.0.0", Port: 8080}}
	fs, roles, host, port := parseOverrides(t, []string{"-roles", "seats"})
	err := overrideNode(boot, fs, roles, host, port)
	if err == nil {
		t.Fatal("-roles seats on a durable node was accepted")
	}
	if !strings.Contains(err.Error(), "store.scratch") {
		t.Errorf("the refusal does not name the setting to change: %v", err)
	}
}

// A FLAG CANNOT PUT THE API ON THE PUBLIC LISTENER'S SOCKET, NOR TURN IT OFF
// BENEATH ONE. The file validated with its own api.port, so without the
// listener rules re-run here `-api-port 8443` would bind the admin API on the
// socket the deployment publishes, and `-api-port 0` would leave a public
// listener that no probe stands beside.
func TestAnAPIFlagThatContradictsThePublicListenerIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"the public listener's port", []string{"-api-port", "8443"}},
		{"no HTTP surface", []string{"-api-port", "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot := &config.Bootstrap{API: config.API{Host: "0.0.0.0", Port: 8080,
				Public: config.APIPublic{Port: 8443}}}
			fs, roles, host, port := parseOverrides(t, tc.args)
			err := overrideNode(boot, fs, roles, host, port)
			if err == nil {
				t.Fatalf("%v was accepted beside api.public.port 8443", tc.args)
			}
			if !strings.Contains(err.Error(), "api.public.port") {
				t.Errorf("the refusal does not name the setting it contradicts: %v", err)
			}
		})
	}
}

// THE PUBLIC LISTENER'S api.port RULE IS AN INGRESS NODE'S. The shared file sets
// api.public for its ingress peer, and the satellite the deployment guide
// starts with `-roles data,seats,workers -api-port 0` serves no probe on any
// port — so it is accepted, while the same flag under `-roles data,ingress` is
// the node answering no probe the rule exists to refuse. Both flags are read
// together, whichever order the rule would otherwise have seen them in.
func TestThePublicListenersAPIPortRuleReadsTheRolesFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		args   []string
		refuse bool
	}{
		{"a satellite with no HTTP surface", []string{"-roles", "data,seats,workers", "-api-port", "0"}, false},
		{"an ingress node with no HTTP surface", []string{"-roles", "data,ingress", "-api-port", "0"}, true},
		{"the roles alone, on a file with no API port", []string{"-roles", "data,ingress"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot := &config.Bootstrap{API: config.API{Host: "0.0.0.0", Port: 8080,
				Public: config.APIPublic{Port: 8443}}}
			if !slices.Contains(tc.args, "-api-port") {
				boot.API.Port = 0
				boot.Node.Roles = []string{"data", "seats"}
			}
			fs, roles, host, port := parseOverrides(t, tc.args)
			err := overrideNode(boot, fs, roles, host, port)
			switch {
			case tc.refuse && err == nil:
				t.Fatalf("%v was accepted beside api.public", tc.args)
			case tc.refuse && !strings.Contains(err.Error(), "api.public.port"):
				t.Errorf("the refusal does not name the setting it contradicts: %v", err)
			case !tc.refuse && err != nil:
				t.Fatalf("%v was refused: %v", tc.args, err)
			}
		})
	}
}

// AN UNSET FLAG CHANGES NOTHING, which is the whole reason each is applied
// only when it was actually given: an unset -api-port read as 0 would serve
// no HTTP at all and make every integration go deaf.
func TestUnsetRunFlagsLeaveTheBootstrapAlone(t *testing.T) {
	t.Parallel()
	boot := &config.Bootstrap{API: config.API{Host: "0.0.0.0", Port: 8080}}
	boot.Node.Roles = []string{"seats"}
	fs, roles, host, port := parseOverrides(t, nil)
	if err := overrideNode(boot, fs, roles, host, port); err != nil {
		t.Fatalf("overrideNode: %v", err)
	}
	if boot.API.Host != "0.0.0.0" || boot.API.Port != 8080 {
		t.Errorf("api = %s:%d, want the file's values", boot.API.Host, boot.API.Port)
	}
	if len(boot.Node.Roles) != 1 || boot.Node.Roles[0] != "seats" {
		t.Errorf("roles = %v, want the file's", boot.Node.Roles)
	}
}

// AN UNKNOWN ROLE IS REJECTED, not dropped. `-roles seat` would otherwise
// produce a node that runs nothing and reports itself healthy.
func TestBadRunOverridesAreRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"a misspelled role", []string{"-roles", "seat"}},
		{"a role list of only separators", []string{"-roles", " , "}},
		{"a port above the range", []string{"-api-port", "70000"}},
		{"a negative port", []string{"-api-port", "-2"}},
	} {
		boot := &config.Bootstrap{}
		fs, roles, host, port := parseOverrides(t, tc.args)
		if err := overrideNode(boot, fs, roles, host, port); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
}

// parseOverrides builds the three node-override flags runEngine builds, with
// their own defaults, so the tests exercise the same "was this flag given"
// logic the command does. The logging flags have [runFlags] beside them for
// the same reason; neither helper is the whole flag set, because a helper
// that claimed to be would have to be corrected on every flag `run` gains.
func parseOverrides(t *testing.T, args []string) (*flag.FlagSet, string, string, int) {
	t.Helper()
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	roles := fs.String("roles", "", "")
	host := fs.String("api-host", "", "")
	port := fs.Int("api-port", -1, "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return fs, *roles, *host, *port
}

// EVERY COMMAND THE BINARY DISPATCHES IS ADVERTISED BY ITS USAGE.
//
// Nothing connects the switch in runIn() to the text in usage(), so a command
// added to one and not the other works perfectly and is discoverable only by
// reading the source. A vendor CLI shipped that way for the whole of the
// rewrite: provisioning, import and resync all worked, and the only list of
// commands an operator ever sees named GitLab and Mattermost.
//
// The dispatch is read out of the source rather than exercised, because a
// switch's cases cannot be enumerated at runtime — which is the same reason
// the drift is invisible in the first place.
func TestUsageAdvertisesEveryDispatchedCommand(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := run([]string{"help"}, &out, &out); err == nil {
		t.Fatal("help returned no sentinel")
	}
	help := out.String()

	for _, cmd := range dispatchedCommands(t) {
		// The flag spellings of two commands. `crewlet --version` is a
		// convention rather than a command, and a usage list naming both
		// forms of each is noise.
		if strings.HasPrefix(cmd, "-") {
			continue
		}
		if !strings.Contains(help, "crewlet "+cmd) {
			t.Errorf("run() dispatches %q but usage never names it, so it "+
				"can only be found by reading the source:\n%s", cmd, help)
		}
	}

	// AND THE OTHER DIRECTION, which is the half this walk did not have:
	// a usage line for a command the switch does not dispatch sends an
	// operator to `unknown command`, with the binary's own help behind it.
	// That is how a command REMOVED from the switch goes on being
	// advertised, and a removal is exactly when nobody re-reads the help.
	dispatched := map[string]bool{}
	for _, cmd := range dispatchedCommands(t) {
		dispatched[cmd] = true
	}
	for _, advertised := range advertisedCommands(t, help) {
		if !dispatched[advertised] {
			t.Errorf("usage names `crewlet %s` and run() dispatches no such "+
				"command, so it answers `unknown command`", advertised)
		}
	}
}

// advertisedCommands returns the command word of every `crewlet <word>` line
// in the usage text.
func advertisedCommands(t *testing.T, help string) []string {
	t.Helper()
	// THE USAGE BLOCK ONLY, which is what `Usage:` opens: the first line
	// is `crewlet <version> — …`, and the config section below names
	// flags rather than commands.
	return advertisedWords(t, help, "crewlet ", "\nConfig:")
}

// advertisedWords returns the word after prefix on every line of a usage
// text's `Usage:` block that begins with prefix — the block ending at end.
//
// THE FIRST WORD ONLY, because a usage line names the command and then its
// shape — `crewlet budgets <cmd>`, `crewlet backup -dir PATH` — and the
// switch dispatches on the word.
func advertisedWords(t *testing.T, help, prefix, end string) []string {
	t.Helper()
	var out []string
	_, block, found := strings.Cut(help, "Usage:\n")
	if !found {
		t.Fatal("the help text has no usage block, so this half proves nothing")
	}
	block, _, _ = strings.Cut(block, end)
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		rest, found := strings.CutPrefix(line, prefix)
		if !found {
			continue
		}
		word, _, _ := strings.Cut(rest, " ")
		// The flag spellings and the usage header's own first line.
		if word == "" || strings.HasPrefix(word, "-") || strings.HasPrefix(word, "<") {
			continue
		}
		if !slices.Contains(out, word) {
			out = append(out, word)
		}
	}
	if len(out) == 0 {
		t.Fatal("the usage text names no commands, so this half proves nothing")
	}
	return out
}

// dispatchedCommands returns the case values of the command switch in runIn(),
// which [run] wraps.
func dispatchedCommands(t *testing.T) []string {
	t.Helper()
	return switchCases(t, "main.go", "runIn", "cmd")
}

// switchCases returns the string case values of the switch on the variable
// tag inside the function fn of file — a dispatch its usage text has to agree
// with.
//
// THE SWITCH ON THAT VARIABLE ONLY, never every case in the function: a
// dispatcher's arms may hold switches of their own, and a string case of one
// of those is not a command.
func switchCases(t *testing.T, file, fn, tag string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	var commands []string
	ast.Inspect(parsed, func(n ast.Node) bool {
		decl, ok := n.(*ast.FuncDecl)
		if !ok || decl.Name.Name != fn {
			return true
		}
		ast.Inspect(decl.Body, func(n ast.Node) bool {
			dispatch, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			if on, ok := dispatch.Tag.(*ast.Ident); !ok || on.Name != tag {
				return true
			}
			for _, stmt := range dispatch.Body.List {
				clause := stmt.(*ast.CaseClause)
				for _, expr := range clause.List {
					lit, ok := expr.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquoting %s: %v", lit.Value, err)
					}
					commands = append(commands, value)
				}
			}
			return true
		})
		return false
	})

	if len(commands) == 0 {
		t.Fatalf("found no cases of the switch on %s in %s(), so this test "+
			"proves nothing", tag, fn)
	}
	return commands
}

// --- validate: the positional form, -tier and -json --------------------- //

// writeYAML drops one document in a temp dir and returns its path.
func writeYAML(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// THE WORST BUG THIS COMMAND HAD. Go's flag package stops at the first
// non-flag token, so `crewlet validate company.yaml -json` parsed ZERO flags,
// discarded both tokens, and validated ./crewlet.yaml and ./company.yaml
// instead — printing a success line about files it never opened. A fix loop
// reading that converges on nothing.
func TestValidateReadsTheFileItWasGiven(t *testing.T) {
	t.Parallel()
	bad := writeYAML(t, "company.yaml", "name: Acme\nnonsense: true\n")
	var out, errOut bytes.Buffer
	err := run([]string{"validate", bad}, &out, &errOut)
	if err == nil {
		t.Fatalf("a broken document validated: %s", out.String())
	}
	if !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("the error does not name the field in the file given: %v", err)
	}
}

// A TIER A DOCUMENT WHOSE BROKER CANNOT HOLD A FRESH NODE IS SAID AT VALIDATE,
// through the same list a node logs at every start ([engine.BootstrapWarnings]):
// stream.store_max_bytes accepts a value the boot then refuses.
func TestValidateWarnsOfABrokerLimitBelowTheFloor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeYAML(t, "crewlet.yaml", "store:\n  path: "+filepath.Join(dir, "c.db")+"\n"+
		"stream:\n  store_dir: "+filepath.Join(dir, "s")+"\n  store_max_bytes: 6442450944\n"+
		fixtureKeyring)
	var out, errOut bytes.Buffer
	if err := run([]string{"validate", path, "-json"}, &out, &errOut); err != nil {
		t.Fatalf("validate: %v (%s)", err, out.String())
	}
	var got struct {
		Warnings []config.Warning `json:"warnings"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("the -json output is not JSON: %v\n%s", err, out.String())
	}
	for _, w := range got.Warnings {
		if w.Path == "stream.store_max_bytes" && strings.Contains(w.Message, "refuses to boot") {
			return
		}
	}
	t.Errorf("no advisory names stream.store_max_bytes below the floor: %+v", got.Warnings)
}

// AND A LEFTOVER IS REFUSED rather than ignored, which is the only way an
// operator finds out they typed something the command cannot honour.
func TestValidateRefusesMoreThanOneDocument(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	err := run([]string{"validate", "one.yaml", "two.yaml"}, &out, &errOut)
	if err == nil {
		t.Fatal("two positional documents were accepted")
	}
	if !strings.Contains(err.Error(), "at most one") {
		t.Errorf("error = %v", err)
	}
}

// THE TIER IS DETECTED FROM THE KEYS, not from the filename — the one thing
// this has to get right is the case where the operator named the file
// something else.
func TestValidateDetectsWhichTierADocumentIs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
		want string
	}{
		{"a company under any name", companyYAML, "company"},
		{"a bootstrap under any name",
			"store:\n  path: " + filepath.Join(dir, "c.db") + "\n" +
				fixtureStream(dir) + fixtureKeyring,
			"bootstrap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeYAML(t, "some-other-name.yaml", tc.body)
			var out, errOut bytes.Buffer
			if err := run([]string{"validate", path, "-json"}, &out, &errOut); err != nil {
				t.Fatalf("validate: %v (%s)", err, out.String())
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("the -json output is not JSON: %v\n%s", err, out.String())
			}
			if got["tier"] != tc.want {
				t.Errorf("tier = %v, want %s", got["tier"], tc.want)
			}
			if got["valid"] != true {
				t.Errorf("valid = %v: %s", got["valid"], out.String())
			}
		})
	}
}

// A DOCUMENT THAT IS NEITHER IS AN ERROR NAMING THE FLAG, never a guess.
// Guessing wrong reports every field of the file as invalid, and an operator
// reading that cannot tell it from a genuinely broken document.
func TestValidateRefusesADocumentItCannotClassify(t *testing.T) {
	t.Parallel()
	path := writeYAML(t, "mystery.yaml", "colour: blue\n")
	var out, errOut bytes.Buffer
	err := run([]string{"validate", path}, &out, &errOut)
	if err == nil {
		t.Fatal("an unclassifiable document validated")
	}
	if !strings.Contains(err.Error(), "-tier") {
		t.Errorf("the error does not name the remedy: %v", err)
	}
}

// -tier OVERRIDES THE DETECTION, which is what makes the refusal above
// actionable rather than a dead end.
func TestTheTierFlagOverridesDetection(t *testing.T) {
	t.Parallel()
	// A document that is VALID as a company, so the flag is the only thing
	// that can make it fail: detection would pick company and pass.
	path := writeYAML(t, "c.yaml", companyYAML)
	var out, errOut bytes.Buffer
	if err := run([]string{"validate", path}, &out, &errOut); err != nil {
		t.Fatalf("the document is not valid as a company: %v (%s)", err, out.String())
	}
	out.Reset()
	if err := run([]string{"validate", path, "-tier", "bootstrap"}, &out, &errOut); err == nil {
		t.Fatalf("a company document validated as a bootstrap: %s", out.String())
	}
}

// AN UNKNOWN TIER IS REFUSED NAMING THE SET.
func TestValidateRefusesAnUnknownTier(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	err := run([]string{"validate", "x.yaml", "-tier", "middle"}, &out, &errOut)
	if err == nil {
		t.Fatal("an unknown tier ran")
	}
	if !strings.Contains(err.Error(), `-tier "middle" is not one of "auto", "company" or "bootstrap"`) {
		t.Errorf("the error does not name the set: %v", err)
	}
}

// validateJSON runs `crewlet validate <file> -json` and decodes the payload
// into the config package's own problem and warning types, which is the
// contract: a loop written against the API reads the CLI's output unchanged.
func validateJSON(t *testing.T, body string) (payload struct {
	Valid    bool             `json:"valid"`
	Problems []config.Problem `json:"problems"`
	Warnings []config.Warning `json:"warnings"`
}, raw string, stderr string) {
	t.Helper()
	path := writeYAML(t, "company.yaml", body)
	var out, errOut bytes.Buffer
	runErr := run([]string{"validate", path, "-json"}, &out, &errOut)
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("the -json output is not JSON: %v\n%s", err, out.String())
	}
	if (runErr == nil) != payload.Valid {
		t.Errorf("exit disposition %v disagrees with valid=%v", runErr, payload.Valid)
	}
	// BOTH LISTS ARE ALWAYS ARRAYS, so a consumer iterates them without a
	// nil check.
	var shape map[string]any
	if err := json.Unmarshal(out.Bytes(), &shape); err != nil {
		t.Fatal(err)
	}
	for _, list := range []string{"problems", "warnings"} {
		if _, isArray := shape[list].([]any); !isArray {
			t.Errorf("%s is %v, want an array: %s", list, shape[list], out.String())
		}
	}
	return payload, out.String(), errOut.String()
}

// THE -json PAYLOAD CARRIES A LOCATED, CLASSIFIED PROBLEM PER FAILURE, which is
// the whole reason an authoring loop uses it: prose it would have to parse
// converges on whatever the prose happened to say.
func TestTheJSONOutputCarriesAPathPerProblem(t *testing.T) {
	t.Parallel()
	got, _, stderr := validateJSON(t,
		strings.Replace(companyYAML, "llm: primary\n", "llm: nonexistent\n", 1))
	if got.Valid {
		t.Errorf("valid = true on a broken document")
	}
	want := config.Problem{
		Path: "roles[0].llm", Segments: config.Path{"roles", 0, "llm"},
		Kind: "unknown_value", Seat: "ceo",
	}
	if len(got.Problems) != 1 {
		t.Fatalf("problems = %+v, want exactly one", got.Problems)
	}
	p := got.Problems[0]
	if p.Path != want.Path || !reflect.DeepEqual(p.Segments, want.Segments) ||
		p.Kind != want.Kind || p.Seat != want.Seat {
		t.Errorf("problem = %+v, want %+v", p, want)
	}
	// THE MESSAGE IS THE WHOLE LINE, path and all, exactly as the refusal's
	// text carries it.
	if !strings.HasPrefix(p.Message, "roles[0].llm: ") || !strings.Contains(p.Message, "nonexistent") {
		t.Errorf("message = %q, want the full rendered line", p.Message)
	}
	// AND NOTHING IS ECHOED ON STDERR: a second copy of what the payload
	// already carries is what makes a machine consumer's log unreadable.
	if strings.Contains(stderr, "nonexistent") {
		t.Errorf("the problem was printed twice: %s", stderr)
	}
}

// A RULE THE ORG MODEL CHECKS IS LOCATED TOO, by the seat it is about rather
// than by its name: two seats declaring one handle are one line of text and
// one problem beside each seat, at the handle each one wrote.
func TestTheJSONOutputLocatesAnOrgRuleAtEachSeat(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(companyYAML, "    handle: cto\n", "    handle: ceo\n", 1)
	got, raw, _ := validateJSON(t, doc)
	var paths []string
	for _, p := range got.Problems {
		if strings.Contains(p.Message, "duplicate handle") {
			paths = append(paths, p.Path+"@"+p.Seat)
			if p.Kind != "conflict" {
				t.Errorf("kind = %q, want conflict", p.Kind)
			}
		}
	}
	if want := []string{"roles[0].handle@ceo", "roles[1].handle@ceo"}; !slices.Equal(paths, want) {
		t.Errorf("duplicate problems at %v, want %v\n%s", paths, want, raw)
	}
}

// THE PROSE SAYS EACH THING ONCE, AND WHERE. A rule the org model reports
// names its seat in words, so the path leads the line; two seats sharing a
// handle are one message, printed once and led by both paths rather than
// repeated for the second seat. A warning is printed too, and fails nothing.
func TestTheProseOutputLeadsEachMessageWithItsPaths(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(companyYAML, "    handle: cto\n", "    handle: ceo\n", 1) +
		"units:\n  - name: Platform\n    channel: platform\n    lead: ghost\n"
	path := writeYAML(t, "company.yaml", doc)
	var out, errOut bytes.Buffer
	err := run([]string{"validate", path}, &out, &errOut)
	if err == nil {
		t.Fatalf("a document with a duplicate handle validated: %s", out.String())
	}
	if n := strings.Count(err.Error(), "duplicate handle"); n != 1 {
		t.Errorf("the duplicate is printed %d times, want once:\n%v", n, err)
	}
	if !strings.Contains(err.Error(), "roles[0].handle, roles[1].handle: duplicate handle") {
		t.Errorf("the duplicate is not led by both paths:\n%v", err)
	}
	if !strings.Contains(out.String(), "warning (dangling reference): units[0].lead: ") {
		t.Errorf("the dangling lead is not printed as a dangling-reference warning: %q",
			out.String())
	}
}

// A REFERENCE THAT RESOLVES TO NOTHING IS A WARNING, located where it was
// written, and it fails nothing: the engine runs a company assembled in
// pieces, and a gate refusing one would refuse every intermediate state.
//
// EACH CARRIES ITS Kind, so a consumer branches on the field rather than on
// the prose, and they arrive in [config.Company.Warnings]' own order: what is
// broken first, what could be better after it.
//
// THE LEAD IS A HANDLE, which is what makes `ghost` a misspelling rather than
// a name somebody wrote: nothing resolves a seat by its display name.
//
// AND A COMPANY OF AGENTS ALONE IS SAID TO BE ONE, after it: it declares no
// human seat, so nobody can be invited into it or created in it — which is
// right for a company its agents run alone and worth knowing for any other.
func TestTheJSONOutputCarriesWarnings(t *testing.T) {
	t.Parallel()
	// THE CHANNEL KEEPS THIS CASE ABOUT THE TWO KINDS. A Chatlet
	// company whose units declare none raises a third advisory of its own,
	// and a fixture collecting it would assert a warning set rather than
	// the pair this case names.
	doc := companyYAML + "units:\n  - name: Platform\n    channel: platform\n    lead: ghost\n"
	got, raw, _ := validateJSON(t, doc)
	if !got.Valid {
		t.Fatalf("a dangling lead failed validation: %s", raw)
	}
	want := []config.Warning{{
		Kind: config.WarningDanglingReference, Ref: "lead",
		Path: "units[0].lead", Segments: config.Path{"units", 0, "lead"},
		Unit: "Platform", From: "Platform", To: "ghost",
	}, {
		Kind: config.WarningAdvisory, Path: "roles",
		Segments: config.Path{"roles"},
	}}
	if len(got.Warnings) != len(want) {
		t.Fatalf("warnings = %+v, want %d: %s", got.Warnings, len(want), raw)
	}
	for i, w := range want {
		g := got.Warnings[i]
		// THE SENTENCE ITSELF IS NOT PINNED, because it is prose and it
		// gets reworded. Its PRESENCE is: a warning with no message is one
		// nobody can act on.
		if g.Message == "" {
			t.Errorf("warnings[%d] carries no message: %+v", i, g)
		}
		g.Message = ""
		if !reflect.DeepEqual(g, w) {
			t.Errorf("warnings[%d] = %+v, want %+v", i, g, w)
		}
	}
}

// THE TWO-FLAG FORM CARRIES THE COMPANY'S WARNINGS TOO. It is the form a CI
// step runs over a deployment's pair of files, and a dangling lead reported
// by `crewlet validate company.yaml` but not by `crewlet validate -config
// crewlet.yaml -company company.yaml` would be a misspelling the gate never
// shows.
//
// TIER A'S OWN ADVISORIES LEAD THE LIST, which is the order the two files are
// named in, and the company's follow in their own. Both files' warnings on
// one run is the same rule both files' problems follow: an operator who fixes
// one file and hears about the other on the next run has paid twice for one
// edit.
func TestTheTwoFileFormCarriesWarnings(t *testing.T) {
	t.Parallel()
	doc := companyYAML + "units:\n  - name: Platform\n    channel: platform\n    lead: ghost\n"
	var out, errOut bytes.Buffer
	args := append([]string{"validate", "-json"}, configPair(t, "", doc)...)
	if err := run(args, &out, &errOut); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	var got struct {
		Valid    bool             `json:"valid"`
		Warnings []config.Warning `json:"warnings"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("the -json output is not JSON: %v\n%s", err, out.String())
	}
	if !got.Valid {
		t.Errorf("valid = false for a document whose only faults are warnings: %s", out.String())
	}
	// KIND WITH PATH, because either one alone lets the other move: a path
	// under the wrong kind is a misclassified warning, and a kind at the
	// wrong path is one no editor can jump to.
	located := make([]string, 0, len(got.Warnings))
	for _, w := range got.Warnings {
		if w.Message == "" {
			t.Errorf("the warning at %q carries no message: %+v", w.Path, w)
		}
		located = append(located, w.Kind+" "+w.Path)
	}
	want := []string{
		config.WarningAdvisory + " retention.backup_owner",
		config.WarningDanglingReference + " units[0].lead",
		// A COMPANY OF AGENTS ALONE: no human seat to invite anybody onto.
		config.WarningAdvisory + " roles",
	}
	if !slices.Equal(located, want) {
		t.Errorf("warnings = %v, want %v\n%s", located, want, out.String())
	}
}

// AN OPERATOR CAN TELL THE KINDS APART IN PROSE. Both tiers' advisories
// share the warning list with the references, and under one undifferentiated
// `warning:` line a dangling lead, which is broken and has to be corrected,
// reads exactly like a declined fsync, which is a choice with a consequence.
// The kind leads the line, so the list can be skimmed by a person and grepped
// by a CI step.
func TestTheProseOutputNamesEachWarningsKind(t *testing.T) {
	t.Parallel()
	doc := companyYAML + "units:\n  - name: Platform\n    channel: platform\n    lead: ghost\n"
	var out, errOut bytes.Buffer
	args := append([]string{"validate"}, configPair(t, "", doc)...)
	if err := run(args, &out, &errOut); err != nil {
		t.Fatalf("validate: %v\n%s", err, errOut.String())
	}
	for _, want := range []string{
		"warning (advisory): retention.backup_owner: ",
		"warning (dangling reference): units[0].lead: ",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the output does not carry %q:\n%s", want, out.String())
		}
	}
}

// EVERY KIND HAS WORDS, and one this build does not classify renders as its
// wire value rather than as an empty pair of brackets: the label is the only
// thing on the line that says how much the warning matters.
func TestAWarningLineCarriesItsKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		w    config.Warning
		want string
	}{{
		name: "a dangling reference",
		w: config.Warning{Kind: config.WarningDanglingReference,
			Path: "units[0].lead", Message: "no seat holds that name"},
		want: "warning (dangling reference): units[0].lead: no seat holds that name",
	}, {
		name: "an admission rule",
		w: config.Warning{Kind: config.WarningAdmission,
			Path: "units[0].id", Message: "duplicate unit key"},
		want: "warning (admission): units[0].id: duplicate unit key",
	}, {
		name: "an advisory",
		w: config.Warning{Kind: config.WarningAdvisory,
			Path: "stream.sync", Message: "an acknowledged write may lag the disk"},
		want: "warning (advisory): stream.sync: an acknowledged write may lag the disk",
	}, {
		name: "a kind this build does not classify",
		w:    config.Warning{Kind: "invented", Path: "stream.sync", Message: "something"},
		want: "warning (invented): stream.sync: something",
	}, {
		name: "no kind at all",
		w:    config.Warning{Path: "stream.sync", Message: "something"},
		want: "warning: stream.sync: something",
	}, {
		// The path is not printed twice when the message already opens
		// with it, which is prose's own rule. The kind still leads.
		name: "a message that already opens with its path",
		w: config.Warning{Kind: config.WarningAdvisory,
			Path: "stream.sync", Message: "stream.sync: something"},
		want: "warning (advisory): stream.sync: something",
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := warningLine(c.w); got != c.want {
				t.Errorf("warningLine = %q, want %q", got, c.want)
			}
		})
	}
}

// A FAILED -json VALIDATION EXITS NON-ZERO, or every CI gate built on
// `crewlet validate x.yaml -json || exit 1` passes unconditionally.
func TestAFailedJSONValidationStillFails(t *testing.T) {
	t.Parallel()
	bad := writeYAML(t, "company.yaml", "name: Acme\nnonsense: true\n")
	var out, errOut bytes.Buffer
	if err := run([]string{"validate", bad, "-json"}, &out, &errOut); err == nil {
		t.Fatalf("a broken document exited zero: %s", out.String())
	}
}

// --- run: the positional Tier A path ------------------------------------ //

// `crewlet run /etc/crewlet.yaml -debug` used to parse no flags at all,
// discard the path, and boot from ./crewlet.yaml — or from nothing — without
// ever mentioning the file the operator named.
func TestRunReadsTheConfigItWasGiven(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "not-here.yaml")
	var out, errOut bytes.Buffer
	err := run([]string{"run", missing}, &out, &errOut)
	if err == nil {
		t.Fatal("a missing config booted")
	}
	if !strings.Contains(err.Error(), "not-here.yaml") {
		t.Errorf("the error does not name the file given: %v", err)
	}
}

// NAMING IT TWICE IS REFUSED. They would have to agree and nothing checks
// that they do, so the second one silently winning is the worst answer.
func TestRunRefusesTheConfigNamedTwice(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	err := run([]string{"run", "a.yaml", "-config", "b.yaml"}, &out, &errOut)
	if err == nil {
		t.Fatal("two config paths were accepted")
	}
	if !strings.Contains(err.Error(), "named twice") {
		t.Errorf("error = %v", err)
	}
}

// THE COMMONEST FIRST-RUN FAILURE: this repo's own quickstart and example
// call the Tier A document config.yaml, while the binary's default is
// crewlet.yaml. Naming the neighbour costs one stat and saves the diagnosis.
func TestAMissingDefaultNamesTheFileSittingBesideIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	beside := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(beside, []byte("store:\n  path: x.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := run([]string{"validate",
		"-config", filepath.Join(dir, "crewlet.yaml"),
		"-company", filepath.Join(dir, "company.yaml")}, &out, &errOut)
	if err == nil {
		t.Fatal("a missing config validated")
	}
	if !strings.Contains(err.Error(), "config.yaml is there") {
		t.Errorf("the neighbour was not named: %v", err)
	}
}

// THE TWO TIER B FLAGS MEAN OPPOSITE THINGS, so naming both is refused.
//
// -company bootstraps a company that does not exist yet; -import-company
// replaces what the fleet is running. Any precedence between them is a silent
// guess about which the operator meant, made about the flag that overwrites a
// live company.
func TestNamingBothTierBFlagsIsRefused(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	err := run([]string{"run", "-company", "a.yaml", "-import-company", "b.yaml"},
		&out, &errOut)
	if err == nil {
		t.Fatal("both Tier B flags were accepted")
	}
	for _, want := range []string{"-company", "-import-company", "opposite"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// A TIER B PATH THE OPERATOR TYPED AND THAT IS NOT THERE IS A TYPO, whichever
// flag carried it. Booting past it would give them a node quietly running
// something other than what they named.
func TestANamedTierBFileThatIsMissingIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	boot := filepath.Join(dir, "crewlet.yaml")
	if err := os.WriteFile(boot, []byte("node:\n  id: n1\nstore:\n  path: "+
		filepath.Join(dir, "n.db")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-company", "-import-company"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			err := run([]string{"run", "-config", boot, flag,
				filepath.Join(dir, "nope.yaml")}, &out, &errOut)
			if err == nil {
				t.Fatalf("%s named a missing file and was accepted", flag)
			}
			if !strings.Contains(err.Error(), "nope.yaml") {
				t.Errorf("the error does not name the file: %v", err)
			}
		})
	}
}

// USAGE NAMES BOTH, because nothing connects the flag registration to the help
// text and a flag an operator is never told about is one they never reach for
// — which for -import-company means reaching for the one that silently does
// nothing instead.
func TestUsageNamesBothTierBFlags(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	usage(&buf)
	for _, want := range []string{"-company", "-import-company"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("usage never names %q:\n%s", want, buf.String())
		}
	}
}

// A COMMAND THAT TALKS TO A NODE AUTHENTICATES WITH CREWLET_API_TOKEN AND
// NOTHING ELSE — and the help, the flag set and the 401 all say so.
//
// The node commands registered a `-token` flag, documented, beside a comment
// saying the token was never a flag: a token typed as an argument is in the
// shell history, in `ps` and in any CI log that echoes the command. So the
// flag was the one way to use this CLI that leaked the credential it carried,
// and a 401 had two sources to send an operator to. The flag is refused as
// unknown now, the variable is the token whatever else is on the line, and
// the help names the variable because nothing else does.
//
// NOT PARALLEL, and handed the PROCESS's environment: that is its subject, and
// it is what every run of the binary reads ([run]).
func TestTheNodeCredentialIsTheEnvironmentAndNeverAFlag(t *testing.T) {
	t.Setenv(apiTokenEnv, "from-the-environment")

	var help bytes.Buffer
	usage(&help)
	if !strings.Contains(help.String(), apiTokenEnv) {
		t.Errorf("usage never names %s, the one credential a node command "+
			"sends:\n%s", apiTokenEnv, help.String())
	}

	var stderr bytes.Buffer
	if _, err := nodeClientFor(config.EnvSource{}, []string{"-api", "http://127.0.0.1:1", "-token", "typed"},
		"budgets show", &stderr, nil); err == nil {
		t.Error("-token was accepted: a credential typed on the command line " +
			"is in the shell history and in ps")
	}
	client, err := nodeClientFor(config.EnvSource{}, []string{"-api", "http://127.0.0.1:1"},
		"budgets show", &stderr, nil)
	if err != nil {
		t.Fatalf("nodeClientFor: %v", err)
	}
	if client.token != "from-the-environment" {
		t.Errorf("token = %q, want %s's value", client.token, apiTokenEnv)
	}

	// AND THE 401 NAMES ONE SOURCE, because there is one.
	msg, ok := credentialRefusal(http.StatusUnauthorized, nil, false)
	if !ok || !strings.Contains(msg, apiTokenEnv) || strings.Contains(msg, "-token") {
		t.Errorf("the no-token refusal says %q: want %s named and no flag", msg, apiTokenEnv)
	}
}

// A WORKER-ONLY NODE STILL HAS A CONFIG WRITER.
//
// The writer was installed inside serveAPI, after its early return for
// `api.port: 0` — so a node with no HTTP surface never got one. The reconcile
// loop is armed regardless and is a FLEET SINGLETON, so it lands on exactly
// that node as readily as on any other, and everything needing the writer
// then failed for the life of the process: a disconnect answered "no config
// surface is wired on this node" over an error whose own comment calls that
// state momentary, a GitHub seat's discovered installation could never be
// recorded, and the Atlassian pass repeated its "set cloud_id by hand" note
// on every tick, for ever.
//
// It is a SOURCE assertion because there is nothing at runtime to watch: the
// bug is which function the call sits in, and a call in the wrong one still
// compiles, still runs, and produces a node that looks healthy.
func TestTheConfigWriterIsInstalledOutsideTheHTTPSurface(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}

	var installedIn []string
	for _, decl := range parsed.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)
			if isSel && sel.Sel.Name == "UseConfigWriter" {
				installedIn = append(installedIn, fn.Name.Name)
			}
			return true
		})
	}

	if len(installedIn) != 1 {
		t.Fatalf("UseConfigWriter is called from %v; there is one writer and it "+
			"is installed once", installedIn)
	}
	if installedIn[0] == "serveAPI" {
		t.Error("the config writer is installed inside serveAPI, which returns " +
			"early for api.port 0 — a worker-only node would run the reconcile " +
			"loop with no way to write the company document")
	}
}

// TestEveryNilMeansTrustOptionIsWiredAtTheOneAPICall.
//
// [api.Options] has a class of field whose nil is a REAL configuration rather
// than a missing wire: the API treats it as "this node has nothing to gate on"
// and serves. `Estate` is the one shipped today — nil is honest for a company
// on Jira and Confluence, which runs no state-log domain at all — and that is
// exactly what makes it dangerous here. An option the binary forgets to pass
// compiles, boots, serves every route, and reports a healthy node whose
// replicated rows have a hole nothing will fill. There is no runtime symptom
// to watch for, because the absence IS the permissive answer.
//
// A SOURCE ASSERTION for the same reason
// [TestTheConfigWriterIsInstalledOutsideTheHTTPSurface] is one: the bug is a
// field that is not in a composite literal, and nothing about the running
// process distinguishes "deliberately nil" from "nobody wired it". The list
// is a declaration — a new option of this class is added here in the same
// change that adds it to the API, or the next one inherits the gap.
func TestEveryNilMeansTrustOptionIsWiredAtTheOneAPICall(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}

	passed := map[string]bool{}
	calls := 0
	ast.Inspect(parsed, func(n ast.Node) bool {
		lit, isLit := n.(*ast.CompositeLit)
		if !isLit {
			return true
		}
		sel, isSel := lit.Type.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != "Options" {
			return true
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		if !isIdent || pkg.Name != "api" {
			return true
		}
		calls++
		for _, elt := range lit.Elts {
			kv, isKV := elt.(*ast.KeyValueExpr)
			if !isKV {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok {
				passed[key.Name] = true
			}
		}
		return true
	})

	if calls != 1 {
		t.Fatalf("main.go builds api.Options %d times; this gate reads the one "+
			"the binary serves from, and several would let a second call skip "+
			"what the first passes", calls)
	}
	for _, field := range []string{"Estate"} {
		if !passed[field] {
			t.Errorf("api.Options.%s is not passed at main.go's api.New call. "+
				"Its nil means \"nothing to gate on\", so an unwired one serves "+
				"confidently out of whatever state this node happens to hold — "+
				"there is no symptom to notice and nothing else checks it",
				field)
		}
	}
}

// EVERY FLAG `run` REGISTERS APPEARS IN ITS OWN USAGE SYNOPSIS.
//
// That synopsis is the only help an operator sees at the moment they got the
// arguments wrong, and a flag missing from it is one they do not reach for.
// Its own comment records that it has been short THREE times — first the three
// logging flags, which are exactly what somebody diagnosing this reaches for,
// then -import-company, then -log-file — which is three silent regressions in
// one string with nothing watching it. This is what watches it.
//
// A SOURCE ASSERTION because there is nothing at runtime to catch: a missing
// flag parses, runs and works, and the only symptom is somebody not knowing it
// exists.
//
// -config is the one exclusion, and the synopsis says why: its value is the
// leading positional, and naming it a second time is the error being reported.
func TestRunsUsageSynopsisNamesEveryFlagItRegisters(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}

	var registered []string
	var synopsis string
	for _, decl := range parsed.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || fn.Name.Name != "runEngine" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall || len(call.Args) == 0 {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			switch sel.Sel.Name {
			case "String", "Bool", "Int", "Duration", "Float64":
				if name, ok := stringLiteral(call.Args[0]); ok {
					registered = append(registered, name)
				}
			}
			// The synopsis is the concatenated literal handed to the
			// Fprintln that starts "usage: crewlet run".
			if sel.Sel.Name == "Fprintln" && len(call.Args) == 2 {
				if text, ok := concatLiterals(call.Args[1]); ok &&
					strings.HasPrefix(text, "usage: crewlet run") {
					synopsis = text
				}
			}
			return true
		})
	}

	// THE CONTROLS. A walk that found no flags, or no synopsis, passes
	// every assertion below and protects nothing.
	if len(registered) < 8 {
		t.Fatalf("found %d flags on run's set (%v); the walk is not reading it",
			len(registered), registered)
	}
	if synopsis == "" {
		t.Fatal("run's usage synopsis was not found; this case is reading the " +
			"wrong function or the string was reshaped")
	}

	for _, name := range registered {
		if name == "config" {
			continue
		}
		if !strings.Contains(synopsis, "-"+name) {
			t.Errorf("-%s is registered by `run` and missing from its usage "+
				"synopsis, so an operator who needs it is never told it "+
				"exists:\n%s", name, synopsis)
		}
	}
}

// stringLiteral is an unquoted string literal, or false for anything else.
func stringLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// concatLiterals flattens a `"a" + "b" + "c"` chain, which is how a long
// message is written in this tree. Anything else in the chain gives up rather
// than guessing.
func concatLiterals(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		return stringLiteral(v)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		left, okL := concatLiterals(v.X)
		right, okR := concatLiterals(v.Y)
		return left + right, okL && okR
	}
	return "", false
}

// bridgeFor is [mcpbridge.New] for a keyring a case expects to be usable.
func bridgeFor(t *testing.T, opts mcpbridge.Options) *mcpbridge.Bridge {
	t.Helper()
	bridge, err := mcpbridge.New(opts)
	if err != nil {
		t.Fatalf("mcpbridge.New: %v", err)
	}
	return bridge
}

// AN ENTRY'S HARNESS PROFILE IS JUDGED BY THE VALIDATOR, NOT ONLY BY THE EPOCH
// BUILD — so the refusal is located at the field to change. Built-only, an
// unloadable profile came back with an empty path and an `engine:` prefix, and
// a key with no variable to go in validated clean.
func TestValidateLocatesAnEntrysProfileRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, entry, path string }{
		{"an unloadable profile", "      harness: custom\n", "providers.llm.sub.overrides"},
		{"an api key with nowhere to go",
			"      harness: hermes\n      auth: {mode: api-key, api_key: \"${OPENROUTER_API_KEY}\"}\n",
			"providers.llm.sub.auth.mode"},
		{"a profile with no model flag",
			"      harness: codex\n      overrides: {model_args: []}\n",
			"providers.llm.sub.overrides.model_args"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := "name: Acme\nproviders:\n  sandbox: {fake: true}\n  llm:\n    sub:\n      model: m\n" + tc.entry
			path := writeYAML(t, "company.yaml", doc)
			var out, errOut bytes.Buffer
			if err := run([]string{"validate", path, "-tier", "company", "-json"}, &out, &errOut); err == nil {
				t.Fatalf("the entry validated: %s", out.String())
			}
			var report struct {
				Problems []struct {
					Path string `json:"path"`
				} `json:"problems"`
			}
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatalf("decode: %v (%s)", err, out.String())
			}
			if len(report.Problems) == 0 || report.Problems[0].Path != tc.path {
				t.Errorf("problems = %+v, want one at %s", report.Problems, tc.path)
			}
		})
	}
}

// THE RELAY IS DISPATCHED UNDER THE NAME A HARNESS IS HANDED. The argv the engine
// writes into a harness's MCP configuration is built from [mcprelay.Command], and the
// switch names its cases as literals (which is how this suite reads the
// binary's command list) — so a rename on one side would leave every harness
// starting a command the binary answers `unknown command` to, and every seat
// without its tools.
//
// And run as itself: the bytes the engine's end sends reach stdout and
// nothing else does, with no logging set up in front of it.
func TestTheRelayIsDispatchedUnderItsOwnName(t *testing.T) {
	t.Parallel()
	if !slices.Contains(dispatchedCommands(t), mcprelay.Command) {
		t.Fatalf("runIn() dispatches no %q, the command every harness is told to start",
			mcprelay.Command)
	}

	dir, err := os.MkdirTemp("", "relay") //nolint:usetesting // t.TempDir is named for the test, past a socket path's bound
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("from the engine\n"))
		_ = conn.Close()
	}()

	var stdout, stderr bytes.Buffer
	if err := run([]string{mcprelay.Command, path}, &stdout, &stderr); err != nil {
		t.Fatalf("the relay failed: %v (stderr %q)", err, stderr.String())
	}
	if got := stdout.String(); got != "from the engine\n" {
		t.Errorf("stdout = %q, want exactly what the engine sent", got)
	}
}
