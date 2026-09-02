package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/store"
)

func cli(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errs bytes.Buffer
	err := run(args, &out, &errs)
	return out.String(), errs.String(), err
}

// -check REPORTS AND APPLIES NOTHING. A command that migrated while
// answering "what would you migrate" could never answer it.
func TestMigrateCheckReportsPendingWithoutApplying(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)

	out, _, err := cli(t, "migrate", "-config", cfg, "-check")
	if err == nil {
		// NON-ZERO IS THE POINT: this is what a deploy gate calls, and a
		// gate that reported pending work and exited 0 stops nothing.
		t.Fatal("a database with pending migrations exited 0")
	}
	if !strings.Contains(out, "pending") {
		t.Errorf("output = %q", out)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "index.db")); statErr == nil {
		// Reading may create the file, but it must not create the
		// schema — which the next assertion proves.
		applied, pending, perr := store.Pending(context.Background(),
			filepath.Join(dir, "index.db"), store.Options{})
		if perr != nil {
			t.Fatalf("Pending: %v", perr)
		}
		if len(applied) != 0 || len(pending) == 0 {
			t.Errorf("-check applied %d migration(s)", len(applied))
		}
	}
}

// MIGRATING IS WHAT OPENING DOES, so the command reports rather than
// reimplements — a second migrator is one that can disagree with the engine
// about what "applied" means.
func TestMigrateAppliesAndThenIsQuiet(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)

	out, _, err := cli(t, "migrate", "-config", cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !strings.Contains(out, "applied") {
		t.Errorf("output = %q", out)
	}

	// A SECOND RUN IS A NO-OP and says so, which is what makes it safe in
	// a deploy script that runs on every rollout.
	out, _, err = cli(t, "migrate", "-config", cfg)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if !strings.Contains(out, "up to date") {
		t.Errorf("second run = %q", out)
	}

	// And -check now passes, which is the gate a deploy actually reads.
	if _, _, err = cli(t, "migrate", "-config", cfg, "-check"); err != nil {
		t.Errorf("-check failed on a migrated database: %v", err)
	}
}

// --- the budget commands ---------------------------------------------------

// These talk to a RUNNING NODE, not to a file, because the counter they act
// on is the fleet's and lives in the coordination store. So the fake here is
// a node: an HTTP server answering the two routes the commands call.

// fakeNode stands in for a running engine.
type fakeNode struct {
	server  *httptest.Server
	budgets []byte
	durable bool

	// seen records what the last reset was asked to clear, so a test can
	// assert the scope actually reached the node rather than only that
	// the command printed something plausible.
	seen    string
	resets  int
	tokens  []string
	cleared []string
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	n := &fakeNode{durable: true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /query/budgets", func(w http.ResponseWriter, r *http.Request) {
		n.tokens = append(n.tokens, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if n.budgets != nil {
			_, _ = w.Write(n.budgets)
			return
		}
		_, _ = fmt.Fprintf(w, `{"durable":%t,"org":{"max_tokens":0,"durable_used":0},"seats":[]}`,
			n.durable)
	})
	mux.HandleFunc("POST /budgets/reset", func(w http.ResponseWriter, r *http.Request) {
		n.tokens = append(n.tokens, r.Header.Get("Authorization"))
		n.seen = r.URL.Query().Get("scope")
		n.resets++
		body, err := json.Marshal(map[string]any{
			"cleared": len(n.cleared), "scopes": n.cleared,
		})
		if err != nil {
			t.Errorf("marshal: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

// bootstrapForNode writes a Tier A config naming this fake node's address and
// one token, which is what the commands read their defaults from.
func bootstrapForNode(t *testing.T, node *fakeNode) string {
	t.Helper()
	return bootstrapForURL(t, node.server.URL)
}

// bootstrapForURL is the same, for any test server. Split out because more
// than one kind of fake node needs it and a second copy of this would be one
// that drifts.
func bootstrapForURL(t *testing.T, serverURL string) string {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(serverURL, "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", serverURL, err)
	}
	dir := t.TempDir()
	body := fmt.Sprintf("node:\n  id: cli-test\nstore:\n  path: %s\n"+
		"api:\n  host: %s\n  port: %s\n  auth:\n    tokens:\n"+
		"      - id: ops\n        token: t0ken\n",
		filepath.Join(dir, "index.db"), host, port)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	return path
}

// A COUNTER NOBODY COULD READ IS NOT ZERO SPEND. The query surface says so
// with `durable: false`, and printing a table of zeros for it would draw a
// company at 0% of its budget when the truth is that nobody looked.
func TestBudgetsShowRefusesToPrintZerosForAnUnreadableCounter(t *testing.T) {
	node := newFakeNode(t)
	node.durable = false
	cfg := bootstrapForNode(t, node)

	_, _, err := cli(t, "budgets", "show", "-config", cfg)
	if err == nil {
		t.Fatal("an unreadable counter was printed as spend")
	}
	if !strings.Contains(err.Error(), "durable") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

func TestBudgetsShowListsWhatEachScopeSpent(t *testing.T) {
	node := newFakeNode(t)
	node.budgets = []byte(`{"durable":true,
	  "org":{"max_tokens":10000,"durable_used":1200,"durable_updated_at":"2026-08-01T00:00:00Z"},
	  "seats":[{"handle":"swe","agent_id":"a1","max_tokens":500,"durable_used":300,
	            "durable_updated_at":"2026-08-01T00:00:00Z"},
	           {"handle":"uncapped","agent_id":"a2","max_tokens":0,"durable_used":700},
	           {"handle":"idle","agent_id":"a3","max_tokens":0,"durable_used":0}]}`)
	cfg := bootstrapForNode(t, node)

	out, _, err := cli(t, "budgets", "show", "-config", cfg)
	if err != nil {
		t.Fatalf("budgets show: %v", err)
	}
	if !strings.Contains(out, "org") || !strings.Contains(out, "1200") {
		t.Errorf("the org counter is missing: %q", out)
	}
	if !strings.Contains(out, "swe") || !strings.Contains(out, "300") {
		t.Errorf("the seat's own spend is missing: %q", out)
	}
	// A CAP OF 0 IS UNLIMITED, matching the config. Printing a literal 0
	// in that column would read as the exact opposite of what it means.
	if !strings.Contains(out, "unlimited") {
		t.Errorf("an uncapped scope was not named as unlimited: %q", out)
	}
	// And a seat with no cap and no spend contributes nothing: a
	// permanent zero row per seat buries the seats that matter in a
	// company of any size.
	if strings.Contains(out, "idle") {
		t.Errorf("a seat with nothing to report was printed: %q", out)
	}
}

// A RESET NAMES WHAT IT CLEARED. A count alone leaves an operator unable to
// tell "reset the seat I meant" from "reset a scope that was already empty".
func TestBudgetsResetNamesTheScopesItCleared(t *testing.T) {
	node := newFakeNode(t)
	node.cleared = []string{"agent:swe"}
	cfg := bootstrapForNode(t, node)

	out, _, err := cli(t, "budgets", "reset", "-config", cfg, "-scope", "agent:swe")
	if err != nil {
		t.Fatalf("budgets reset: %v", err)
	}
	if !strings.Contains(out, "agent:swe") {
		t.Errorf("output = %q", out)
	}
	// SCOPED MEANS SCOPED, and the scope has to reach the NODE — a
	// command that printed the right thing while clearing every counter
	// would re-arm a company somebody had stopped on purpose.
	if node.seen != "agent:swe" {
		t.Errorf("the node was asked to clear %q, want the scope the operator named", node.seen)
	}
}

// RESETTING NOTHING SAYS SO rather than reporting a success that did not
// happen.
func TestResettingAScopeThatIsNotThereSaysSo(t *testing.T) {
	node := newFakeNode(t)
	cfg := bootstrapForNode(t, node)

	out, _, err := cli(t, "budgets", "reset", "-config", cfg, "-scope", "agent:nobody")
	if err != nil {
		t.Fatalf("budgets reset: %v", err)
	}
	if !strings.Contains(out, "Nothing to reset") {
		t.Errorf("output = %q", out)
	}
}

// THE TOKEN IS SENT. A reset is a guarded write, so a command that dropped
// the token would fail against every deployment that has auth on — which is
// every deployment that is not a laptop.
func TestABudgetCommandSendsTheConfiguredToken(t *testing.T) {
	node := newFakeNode(t)
	cfg := bootstrapForNode(t, node)

	if _, _, err := cli(t, "budgets", "reset", "-config", cfg); err != nil {
		t.Fatalf("budgets reset: %v", err)
	}
	if len(node.tokens) == 0 || node.tokens[len(node.tokens)-1] != "Bearer t0ken" {
		t.Errorf("Authorization = %v, want the config's token", node.tokens)
	}
}

// AN EXPORTED TOKEN WINS over the config's. An operator who exported one
// meant that one, and a checked-in config's ${VAR} resolves to the same place
// anyway.
func TestAnExportedTokenBeatsTheConfigs(t *testing.T) {
	node := newFakeNode(t)
	cfg := bootstrapForNode(t, node)
	t.Setenv(APITokenEnv, "exported")

	if _, _, err := cli(t, "budgets", "reset", "-config", cfg); err != nil {
		t.Fatalf("budgets reset: %v", err)
	}
	if got := node.tokens[len(node.tokens)-1]; got != "Bearer exported" {
		t.Errorf("Authorization = %q, want the exported token", got)
	}
}

// A NODE THAT IS NOT THERE says what to do about it. "connection refused"
// alone sends an operator to the network; the actual answer is usually that
// the engine is not running, because that is where this state lives.
func TestReachingANodeThatIsDownExplainsItself(t *testing.T) {
	dir := t.TempDir()
	body := fmt.Sprintf("node:\n  id: cli-test\nstore:\n  path: %s\n"+
		"api:\n  host: 127.0.0.1\n  port: 1\n", filepath.Join(dir, "index.db"))
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := cli(t, "budgets", "show", "-config", cfg)
	if err == nil {
		t.Fatal("a command against a node that is not listening succeeded")
	}
	if !strings.Contains(err.Error(), "RUNNING engine") {
		t.Errorf("the error does not name the likely cause: %v", err)
	}
}

// A CONFIG THAT SERVES NO HTTP has no node to ask, and saying so beats
// dialling port 0 and reporting whatever the network layer makes of it.
func TestAConfigWithNoHTTPSurfaceSaysThereIsNoNode(t *testing.T) {
	dir := t.TempDir()
	body := fmt.Sprintf("node:\n  id: cli-test\nstore:\n  path: %s\n",
		filepath.Join(dir, "index.db"))
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := cli(t, "budgets", "show", "-config", cfg)
	if err == nil {
		t.Fatal("a config with api.port 0 was dialled anyway")
	}
	if !strings.Contains(err.Error(), "api.port") {
		t.Errorf("the error does not name the field to change: %v", err)
	}
}

// A WILDCARD BIND IS NOT AN ADDRESS. `host: 0.0.0.0` says "every interface",
// which nothing can dial — so the default has to become the loopback one,
// which is the interface a command running beside the config is on.
func TestAWildcardBindResolvesToSomethingDialable(t *testing.T) {
	for _, host := range []string{"", "0.0.0.0", "::", "[::]"} {
		got, err := nodeBaseURL(&config.Bootstrap{
			API: config.API{Host: host, Port: 8080},
		})
		if err != nil {
			t.Fatalf("host %q: %v", host, err)
		}
		if got != "http://127.0.0.1:8080" {
			t.Errorf("host %q resolved to %q, want the loopback address", host, got)
		}
	}
}

func TestBudgetsRejectsAnUnknownSubcommand(t *testing.T) {
	node := newFakeNode(t)
	cfg := bootstrapForNode(t, node)
	if _, _, err := cli(t, "budgets", "explode", "-config", cfg); err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}
}

var _ = time.Now

// TWO CONFIG DOCUMENTS IS A REFUSAL, and it must be reachable.
//
// The guard used to be conjoined with "a positional subject was already
// taken", which made it unreachable on the exact input it was written for:
// `crewlet migrate a.yaml b.yaml` puts both names in the tail with no
// subject, so neither branch fired and the command fell through to
// ./crewlet.yaml — migrating, for real and without -check, a database the
// operator never named.
func TestMigrateRefusesLeftoverPositionalArguments(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	other := filepath.Join(dir, "other.yaml")
	if err := os.WriteFile(other, []byte("node:\n  id: other\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"two positionals after the flag", []string{"migrate", "-check", cfg, other}},
		{"two positionals before the flag", []string{"migrate", cfg, other, "-check"}},
		{"two positionals, no flag at all", []string{"migrate", cfg, other}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := cli(t, tc.args...)
			if err == nil {
				t.Fatal("two config documents were accepted; the command silently " +
					"acted on ./crewlet.yaml instead of either file named")
			}
			if !strings.Contains(err.Error(), "at most one config document") {
				t.Errorf("error = %v, want the one-document refusal", err)
			}
		})
	}
}

// AND NAMING IT TWICE, once positionally and once with -config, is refused
// rather than silently resolved — they would have to agree and nothing
// checks that they do. `run` already had this guard; migrate did not.
func TestMigrateRefusesAConfigNamedTwice(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)

	_, _, err := cli(t, "migrate", cfg, "-config", cfg, "-check")
	if err == nil {
		t.Fatal("the config document was accepted twice over")
	}
	if !strings.Contains(err.Error(), "named twice") {
		t.Errorf("error = %v, want the named-twice refusal", err)
	}
}

// The single positional still WORKS, so the cases above are the refusal
// firing rather than positional support being removed.
func TestMigrateAcceptsASinglePositionalConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)

	if _, _, err := cli(t, "migrate", cfg); err != nil {
		t.Fatalf("a single positional config was refused: %v", err)
	}
	applied, pending, err := store.Pending(context.Background(),
		filepath.Join(dir, "index.db"), store.Options{})
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(applied) == 0 || len(pending) != 0 {
		t.Errorf("applied %d, pending %d: the named document was not the one migrated",
			len(applied), len(pending))
	}
}
