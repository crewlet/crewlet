package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
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
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

const companyYAML = `
name: Acme
providers:
  llm:
    primary:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
roles:
  - name: CEO
    handle: ceo
    llm: primary
  - name: CTO
    handle: cto
    llm: primary
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
			"stream:\n  store_dir: " + filepath.Join(dir, "stream") + "\n"
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
	for _, want := range []string{"Acme", "2 agent seats", "1 LLM providers", "embedded", "local"} {
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
func bootstrapFor(t *testing.T, port int) *config.Bootstrap {
	t.Helper()
	dir := t.TempDir()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(dir, "crewlet.db")
	b.Stream.StoreDir = filepath.Join(dir, "stream")
	b.API.Host = "127.0.0.1"
	b.API.Port = port
	return &b
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
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

	surface, err := serveAPI(t.Context(), bootstrapFor(t, 0), nil, nil, nil, nil, log)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	if surface != nil {
		t.Error("a node with api.port 0 built an HTTP surface")
	}
	if !strings.Contains(logged.String(), "api_disabled") {
		t.Errorf("the node did not say it serves no HTTP:\n%s", logged.String())
	}
}

// A node whose roles leave out ingress binds nothing, even with api.port set.
// The role was validated and advertised to peers while serveAPI read only the
// port, so a seats-only satellite opened a listener it was placed on a private
// host to avoid. The port is left free and the node says why.
func TestANodeWithoutTheIngressRoleBindsNoListener(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

	e := testEngine(t)
	boot := bootstrapFor(t, 0)
	boot.API.Host = "127.0.0.1"
	boot.API.Port = freePort(t)
	boot.Node.Roles = []string{"seats", "workers"}

	surface, err := serveAPI(t.Context(), boot, e, nil, nil, nil, log)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	if surface != nil {
		surface.stop(context.Background(), logging.Get("test"))
		t.Fatal("a node without the ingress role built an HTTP surface")
	}
	if !strings.Contains(logged.String(), "api_not_started") {
		t.Errorf("the node did not say why it serves no HTTP:\n%s", logged.String())
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(boot.API.Port)))
	if err != nil {
		t.Fatalf("api.port is held although the node serves no HTTP: %v", err)
	}
	_ = listener.Close()
}

// A SEATS NODE WITHOUT INGRESS STILL SERVES ITS OWN TOOL BRIDGE, and nothing
// else. A bridged session lives in the process that opened it, so the box of an
// agent-mode seat can reach only the node running that seat; gating the bridge
// on ingress with the rest of the API launched boxes whose every tool call found
// nothing listening. The listener carries the bridge's own refusal for a bad
// token, and no dashboard, probe or REST route.
func TestASeatsNodeWithoutIngressServesOnlyItsToolBridge(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

	port := freePort(t)
	bridge := mcpbridge.New(mcpbridge.Options{
		Key: []byte("test-key"), BaseURL: "http://127.0.0.1:" + strconv.Itoa(port),
	})
	e := testEngineWithBridge(t, bridge)
	boot := bootstrapFor(t, port)
	boot.Node.Roles = []string{"seats"}

	surface, err := serveAPI(t.Context(), boot, e, nil, nil, nil, log)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	if surface == nil {
		t.Fatalf("a seats node with a bridge URL bound no listener, so its "+
			"agent-mode boxes can reach no tools:\n%s", logged.String())
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	if surface.app != nil || surface.projector != nil {
		t.Error("a node without the ingress role built the whole API")
	}
	if !bridge.Mounted() {
		t.Error("the listener did not mount the bridge, so no session can open")
	}
	if !strings.Contains(logged.String(), "api_bridge_listening") {
		t.Errorf("the node did not say what its listener serves:\n%s", logged.String())
	}

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	status := func(method, path string) int {
		t.Helper()
		// A POOL OF THIS TEST'S OWN rather than http.DefaultClient's: this
		// binary holds eight httptest servers and over a hundred parallel
		// tests, and every one of their cleanups sweeps the shared pool —
		// see [github.com/crewlet/crewlet/internal/httpx/httpxtest].
		probe := httpxtest.Pool(t)
		deadline := time.Now().Add(10 * time.Second)
		for {
			req, reqErr := http.NewRequestWithContext(t.Context(), method, base+path,
				strings.NewReader("{}"))
			if reqErr != nil {
				t.Fatal(reqErr)
			}
			res, doErr := probe.Do(req)
			if doErr == nil {
				_ = res.Body.Close()
				return res.StatusCode
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s: %v", method, path, doErr)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if got := status(http.MethodPost, mcpbridge.PathPrefix+"not-a-token"); got != http.StatusUnauthorized {
		t.Errorf("POST %snot-a-token = %d, want the bridge's own 401", mcpbridge.PathPrefix, got)
	}
	for _, path := range []string{"/health", "/dashboard", "/agents"} {
		if got := status(http.MethodGet, path); got != http.StatusNotFound {
			t.Errorf("GET %s = %d on a bridge-only listener, want 404", path, got)
		}
	}
}

// A NODE THAT RUNS NO SEATS BINDS NOTHING FOR A BRIDGE, whatever its
// environment says. It opens no session, so a listener there could only answer
// every box with 401, and a workers node placed on a private host would open a
// port for nothing.
func TestANodeRunningNoSeatsBindsNoBridgeListener(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

	port := freePort(t)
	e := testEngineWithBridge(t, mcpbridge.New(mcpbridge.Options{
		Key: []byte("test-key"), BaseURL: "http://127.0.0.1:" + strconv.Itoa(port),
	}))
	boot := bootstrapFor(t, port)
	boot.Node.Roles = []string{"workers"}

	surface, err := serveAPI(t.Context(), boot, e, nil, nil, nil, log)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	if surface != nil {
		surface.stop(context.Background(), logging.Get("test"))
		t.Fatal("a node that runs no seats bound a listener for a bridge it never uses")
	}
	if !strings.Contains(logged.String(), "api_not_started") {
		t.Errorf("the node did not say why it serves no HTTP:\n%s", logged.String())
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("api.port is held although the node serves no HTTP: %v", err)
	}
	_ = listener.Close()
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
	surface, err := serveAPI(t.Context(), bootstrapFor(t, port), e, nil, nil, nil, logging.Get("test"))
	if err == nil {
		surface.stop(context.Background(), logging.Get("test"))
		t.Fatal("binding a port already in use reported success")
	}
	if !strings.Contains(err.Error(), "bind") {
		t.Errorf("the error does not say what failed: %v", err)
	}
}

func TestAMergedNodeServesItsOwnHealth(t *testing.T) {
	t.Parallel()
	// One process is both engine and API, sharing one broker and one
	// store. The API half is what makes the node reachable at all — every
	// inbound webhook arrives through it — so an engine that ran without
	// it would hold seats and hear nothing.
	e := testEngine(t)
	boot := bootstrapFor(t, 0)
	boot.API.Port = freePort(t)

	surface, err := serveAPI(t.Context(), boot, e, nil, nil, nil, logging.Get("test"))
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })

	base := "http://" + surface.server.Addr
	if base == "http://" {
		base = "http://127.0.0.1:" + strconv.Itoa(boot.API.Port)
	}
	body := getJSON(t, base+"/health")

	// The engine's own answers, which only a co-located process has.
	if body["engine"] != true {
		t.Errorf("engine = %v, want true on a merged node", body["engine"])
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
	boot.API.Port = freePort(t)
	surface, err := serveAPI(t.Context(), boot, e, nil, nil, nil, logging.Get("test"))
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

// testEngine builds a real engine on an embedded stream in a temp directory.
func testEngine(t *testing.T) *engine.Engine {
	t.Helper()
	return testEngineWithBridge(t, nil)
}

// testEngineWithBridge is testEngine holding the given tool bridge. Nil builds
// the bridge from the environment, as a node does.
func testEngineWithBridge(t *testing.T, bridge *mcpbridge.Bridge) *engine.Engine {
	t.Helper()
	boot := bootstrapFor(t, 0)
	company, err := config.ParseCompany([]byte(companyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: boot, Company: company, Bridge: bridge})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	// Its own pool, for [httpxtest]'s reason.
	probe := httpxtest.Pool(t)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res, err := probe.Get(url) //nolint:noctx // a test against its own listener
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
		check func(*testing.T, *config.Bootstrap)
	}{
		{
			name: "roles narrow what this node does",
			args: []string{"-roles", "ingress,workers"},
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
			args: []string{"-roles", " seats , "},
			check: func(t *testing.T, b *config.Bootstrap) {
				roles, err := b.Node.RoleSet()
				if err != nil {
					t.Fatalf("RoleSet: %v", err)
				}
				if len(roles) != 1 || !roles.Has(placement.RoleSeats) {
					t.Errorf("roles = %v, want only seats", roles)
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
			boot.Node.Roles = []string{"seats"}
			fs, roles, host, port := parseOverrides(t, tc.args)
			if err := overrideNode(boot, fs, roles, host, port); err != nil {
				t.Fatalf("overrideNode: %v", err)
			}
			tc.check(t, boot)
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
// Nothing connects the switch in run() to the text in usage(), so a command
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
}

// dispatchedCommands returns the case values of the command switch in run().
func dispatchedCommands(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}

	var commands []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
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
			return true
		})
		return false
	})

	if len(commands) == 0 {
		t.Fatal("found no command cases in run(), so this test proves nothing")
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
				"stream:\n  store_dir: " + filepath.Join(dir, "s") + "\n", "bootstrap"},
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
	if !strings.Contains(err.Error(), "auto, company, bootstrap") {
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
// than by its name: two seats of one name are one line of text and one
// problem beside each seat, at the name each one wrote.
func TestTheJSONOutputLocatesAnOrgRuleAtEachSeat(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(companyYAML, "  - name: CTO\n", "  - name: CEO\n", 1)
	got, raw, _ := validateJSON(t, doc)
	var paths []string
	for _, p := range got.Problems {
		if strings.Contains(p.Message, "duplicate seat name") {
			paths = append(paths, p.Path+"@"+p.Seat)
			if p.Kind != "conflict" {
				t.Errorf("kind = %q, want conflict", p.Kind)
			}
		}
	}
	if want := []string{"roles[0].name@ceo", "roles[1].name@cto"}; !slices.Equal(paths, want) {
		t.Errorf("duplicate problems at %v, want %v\n%s", paths, want, raw)
	}
}

// THE PROSE SAYS EACH THING ONCE, AND WHERE. A rule the org model reports
// names its seat in words, so the path leads the line; two seats sharing a
// name are one message, printed once and led by both paths rather than
// repeated for the second seat. A warning is printed too, and fails nothing.
func TestTheProseOutputLeadsEachMessageWithItsPaths(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(companyYAML, "  - name: CTO\n", "  - name: CEO\n", 1) +
		"units:\n  - name: Platform\n    lead: Ghost\n"
	path := writeYAML(t, "company.yaml", doc)
	var out, errOut bytes.Buffer
	err := run([]string{"validate", path}, &out, &errOut)
	if err == nil {
		t.Fatalf("a document with a duplicate seat name validated: %s", out.String())
	}
	if n := strings.Count(err.Error(), "duplicate seat name"); n != 1 {
		t.Errorf("the duplicate is printed %d times, want once:\n%v", n, err)
	}
	if !strings.Contains(err.Error(), "roles[0].name, roles[1].name: duplicate seat name") {
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
// AN ADVISORY RIDES THE SAME LIST (a unit written with no id is valid and
// still worth knowing before it is applied), and the two are told apart by
// Kind, so a consumer branches on the field rather than on the prose. They
// arrive in [config.Company.Warnings]' own order: what is broken first, what
// could be better after it.
func TestTheJSONOutputCarriesWarnings(t *testing.T) {
	t.Parallel()
	doc := companyYAML + "units:\n  - name: Platform\n    lead: Ghost\n"
	got, raw, _ := validateJSON(t, doc)
	if !got.Valid {
		t.Fatalf("a dangling lead failed validation: %s", raw)
	}
	want := []config.Warning{{
		Kind: config.WarningDanglingReference, Ref: "lead",
		Path: "units[0].lead", Segments: config.Path{"units", 0, "lead"},
		Unit: "Platform", From: "Platform", To: "Ghost",
	}, {
		Kind: config.WarningAdvisory,
		Path: "units[0].id", Segments: config.Path{"units", 0, "id"},
		Unit: "Platform",
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
	doc := companyYAML + "units:\n  - name: Platform\n    lead: Ghost\n"
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
		config.WarningAdvisory + " units[0].id",
	}
	if !slices.Equal(located, want) {
		t.Errorf("warnings = %v, want %v\n%s", located, want, out.String())
	}
}

// AN OPERATOR CAN TELL THE TWO APART IN PROSE. Both tiers' advisories share
// the warning list with the references now, and under one undifferentiated
// `warning:` line a dangling lead, which is broken and has to be corrected,
// reads exactly like a unit with no id, which is a choice with a consequence.
// The kind leads the line, so the list can be skimmed by a person and grepped
// by a CI step.
func TestTheProseOutputNamesEachWarningsKind(t *testing.T) {
	t.Parallel()
	doc := companyYAML + "units:\n  - name: Platform\n    lead: Ghost\n"
	var out, errOut bytes.Buffer
	args := append([]string{"validate"}, configPair(t, "", doc)...)
	if err := run(args, &out, &errOut); err != nil {
		t.Fatalf("validate: %v\n%s", err, errOut.String())
	}
	for _, want := range []string{
		"warning (advisory): retention.backup_owner: ",
		"warning (dangling reference): units[0].lead: ",
		"warning (advisory): units[0].id: ",
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
			Path: "roles[0].name", Message: "duplicate seat name"},
		want: "warning (admission): roles[0].name: duplicate seat name",
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
// -company bootstraps an empty store; -import-company replaces what the fleet
// is running. Any precedence between them is a silent guess about which the
// operator meant, made about the flag that overwrites a live company.
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
