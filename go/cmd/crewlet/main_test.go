package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/logging"
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
	var logged bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.ParseFormat("text"), &logged)

	surface, err := serveAPI(t.Context(), bootstrapFor(t, 0), nil, nil, nil, logging.Get("test"))
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
	surface, err := serveAPI(t.Context(), bootstrapFor(t, port), e, nil, nil, logging.Get("test"))
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

	surface, err := serveAPI(t.Context(), boot, e, nil, nil, logging.Get("test"))
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

// testEngine builds a real engine on an embedded stream in a temp directory.
func testEngine(t *testing.T) *engine.Engine {
	t.Helper()
	boot := bootstrapFor(t, 0)
	company, err := config.ParseCompany([]byte(companyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: boot, Company: company})
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
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(url) //nolint:noctx // a test against its own listener
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
