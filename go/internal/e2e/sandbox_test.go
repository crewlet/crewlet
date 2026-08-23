package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// GATE G6 — the golden coding turn.
//
// A real local sandbox, a real detached process, the real suspend and the real
// resume. The one stub is the vendor endpoint, exactly as in the gate above:
// what is under test is the ENGINE's half of a coding turn, and a real coding
// CLI would test the vendor's.
//
// The "coding agent" here is a shell script installed as `claude` on the box's
// PATH. That makes the whole detached protocol real — a process group, a
// background job, a done marker, a findings file, an ask signal — while
// leaving what the agent DOES up to the test.

// sandboxCompany adds a code-enabled seat to the golden company.
//
// containment `direct`, because the point is the engine's protocol and a
// container would make this suite need Docker. The path escape guards and the
// container argv have their own tests in internal/sandbox.
const sandboxCompanyDoc = `
name: Nimbus
providers:
  llm:
    scripted:
      type: anthropic
      model: claude-golden
      base_url: %s
      api_keys: ["${CREWLET_TEST_KEY}"]
  sandbox:
    type: local
    default_coding_agent: claude-code
    default_pause_ttl_seconds: 1800
    local:
      containment: direct
      state_dir: %s
    setup:
      # A REAL setup step, which is also how the stand-in coding CLI gets
      # onto the agent's PATH. The box's PATH inside the run wrapper is the
      # login shell's, so a CLI has to be somewhere the wrapper prepends —
      # and the shim directory is exactly that.
      - name: fake-coding-agent
        commands:
          - 'mkdir -p "$HOME/.crewlet/bin" && cp "$FAKE_AGENT_PATH" "$HOME/.crewlet/bin/claude" && chmod +x "$HOME/.crewlet/bin/claude"'
        brief: A stand-in coding CLI is installed.
roles:
  - name: SWE
    handle: swe
    llm: scripted
    sandbox:
      enabled: true
      env:
        FAKE_AGENT_MODE: "${FAKE_AGENT_MODE}"
        FAKE_AGENT_PATH: "${FAKE_AGENT_PATH}"
  - name: Founder
    kind: human
    contact:
      slack_user_id: U0FOUNDER
turn_engine:
  max_iterations: 1
  max_tool_rounds: 4
  plan_max_tool_rounds: 3
`

// codingNode is a running node whose seat can run code.
type codingNode struct {
	*node
	stateDir string
	binDir   string
}

// startCoding stands up a node with a local sandbox and a fake coding CLI.
func startCoding(t *testing.T, mode string) *codingNode {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("the local sandbox needs a POSIX shell")
	}
	stateDir := t.TempDir()
	binDir := installFakeAgent(t)
	// Both reach the box through role.sandbox.env, which is the ONLY way an
	// external value gets there: the engine names no tool-specific variable
	// of its own. That the fake agent runs at all is therefore also the
	// proof that ${VAR} references in that block resolve.
	t.Setenv("FAKE_AGENT_MODE", mode)
	t.Setenv("FAKE_AGENT_PATH", filepath.Join(binDir, "claude"))

	model := newSandboxModel(t)
	doc := fmt.Sprintf(sandboxCompanyDoc, model.url, stateDir)
	n := bootCompany(t, doc, model)
	return &codingNode{node: n, stateDir: stateDir, binDir: binDir}
}

// installFakeAgent writes a `claude` that behaves like a headless coding CLI.
//
// It reads FAKE_AGENT_MODE from the run env — which is how the test steers it,
// and incidentally proves role.sandbox.env reaches the box — and writes the
// findings file the brief asked for, or an ask signal, or nothing at all.
func installFakeAgent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
# A stand-in for a headless coding CLI. Everything it touches — the findings
# file, the ask shim, the JSON envelope — is the real protocol.
work="$HOME/.crewlet"
case "${FAKE_AGENT_MODE:-succeed}" in
  succeed)
    sleep 0.1
    printf 'Outcome: succeeded\nRan the suite; all green.\nOpened https://github.com/acme/api/pull/7\n' > "$work/findings.md"
    printf '{"result":"done","subtype":"success","session_id":"sess-1","usage":{"input_tokens":700,"output_tokens":120}}\n'
    ;;
  ask)
    sleep 0.1
    crewlet-ask "Which branch should I target?" --to requester
    printf '{"result":"blocked","subtype":"success"}\n'
    ;;
  slow)
    # Long enough that the engine can be stopped mid-run.
    sleep 30
    printf 'Outcome: succeeded\nfinished after the restart\n' > "$work/findings.md"
    printf '{"result":"done","subtype":"success"}\n'
    ;;
esac
`
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("install the fake agent: %v", err)
	}
	return dir
}

// bootCompany stands a merged node up over a company document.
//
// The same assembly startWith does, factored out so a suite whose subject
// needs its own store directory — one that SURVIVES a restart — can supply it.
func bootCompany(t *testing.T, doc string, model *scriptedModel) *node {
	t.Helper()
	dir := t.TempDir()
	return bootCompanyIn(t, doc, model, filepath.Join(dir, "crewlet.db"), filepath.Join(dir, "stream"))
}

func bootCompanyIn(t *testing.T, doc string, model *scriptedModel, dbPath, streamDir string) *node {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	boot := config.DefaultBootstrap()
	boot.Store.Path = dbPath
	boot.Stream.StoreDir = streamDir

	e, err := engine.New(t.Context(), engine.Options{Bootstrap: &boot, Company: cfg})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			e.Stop(context.Background())
		}
	})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}

	app := api.New(api.Options{
		Bootstrap:    &boot,
		QueueBackend: e.Backends().Queue.Backend(),
		Sources: queries.Sources{
			Events:  e.Backends().Store.Events(),
			Company: func() *config.Company { return cfg },
		},
		HealthInterval: tickInterval,
	})
	app.SetConfigured(true)
	app.Start(t.Context())
	t.Cleanup(app.Stop)

	projector := observe.NewProjector(e.Backends().Queue, app.Stream())
	if err := projector.Start(t.Context()); err != nil {
		t.Fatalf("projector: %v", err)
	}
	t.Cleanup(func() { projector.Stop(context.Background()) })

	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	return &node{engine: e, app: app, server: srv, model: model}
}

// sandboxModel is the scripted endpoint for a coding turn.
//
// The executor calls run_sandbox on its FIRST Execute round and reports on the
// resumed one, which is what makes the suspend and the re-entry both real:
// the second Execute request is one the engine only sends after the detached
// job has finished and its result has been spliced in.
func newSandboxModel(t *testing.T) *scriptedModel {
	t.Helper()
	m := &scriptedModel{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		offered := offeredTools(raw)
		var reply string
		switch {
		case offered["submit_review"]:
			m.saw("review")
			reply = toolUse("submit_review", map[string]any{
				"decision": "done", "final_artifact": "The fix is up for review.",
			})
		case offered["submit_plan"]:
			m.saw("plan")
			reply = toolUse("submit_plan", map[string]any{
				"decision": "plan", "reasoning": "Hand the code work to a sandbox.",
				"tools_needed":     []string{"run_sandbox"},
				"steps":            []map[string]string{{"intent": "fix", "approach": "sandbox"}},
				"success_criteria": []string{"the suite passes"},
			})
		case offered["mark_onboarded"]:
			m.saw("onboarding")
			reply = toolUse("mark_onboarded", map[string]any{"notes": "read the handbook"})
		case sawSandboxResult(raw):
			// The RESUMED Execute round: the conversation now carries the
			// tool message the engine spliced in. Answering with prose is
			// the executor reporting and finishing.
			m.saw("resumed")
			reply = textReply("The sandbox fixed it and opened a pull request.")
		default:
			m.saw("execute")
			reply = toolUse("run_sandbox", map[string]any{
				"brief": "Clone example.com/acme/api and fix the failing test",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	m.url = srv.URL
	return m
}

// sawSandboxResult reports whether this request's conversation already carries
// the sandbox's answer — which is what makes it the resumed round.
func sawSandboxResult(raw []byte) bool {
	return strings.Contains(string(raw), "The sandbox coding run")
}

// ---------------------------------------------------------------------
// the gate
// ---------------------------------------------------------------------

// A coding turn end to end: plan, launch, suspend, poll, collect, resume, and
// the same turn finishing with the agent's findings in hand.
func TestAGoldenCodingTurnSuspendsAndResumes(t *testing.T) {
	n := startCoding(t, "succeed")
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	n.wake(t, "swe", "the api test is flaking, please fix it")

	// The SUSPEND: the turn ends with a row that outlives it.
	waitFor(t, "a detached run to be recorded", func() bool {
		return len(n.activeRuns(t)) == 1
	})
	// While it is running, the seat's mail is PARKED rather than consumed:
	// a coding job outlasts any ack window.
	waitFor(t, "the seat to be parked on its run", func() bool {
		return slices.Contains(n.model.seen(), "execute")
	})

	// The RESUME: the waiter detects the completion and the engine re-enters
	// the same conversation, which the model can only answer once the
	// sandbox's result is in it.
	waitFor(t, "the suspended turn to be resumed", func() bool {
		return slices.Contains(n.model.seen(), "resumed")
	})
	waitFor(t, "the run to settle", func() bool {
		return len(n.activeRuns(t)) == 0
	})

	seen := n.model.seen()
	for _, want := range []string{"plan", "execute", "resumed", "review"} {
		if !slices.Contains(seen, want) {
			t.Fatalf("the %s phase never ran; phases = %v", want, seen)
		}
	}
	// ONE turn, not two. The resumed round re-enters the conversation the
	// suspend left; a second Plan would mean the engine started a fresh turn
	// and re-derived a plan for work already done.
	if got := countOf(seen, "plan"); got != 1 {
		t.Fatalf("Plan ran %d times; a resume must not re-plan. phases = %v", got, seen)
	}
	// The box is gone: the resumed Execute made no further run_sandbox call,
	// so the phase was done with it.
	if boxes := n.liveBoxes(t); len(boxes) != 0 {
		t.Fatalf("%d box directories survived the turn: %v", len(boxes), boxes)
	}
}

// The clarification park: the agent asks, the seat is freed, and the answer
// resumes the same turn.
func TestACodingRunThatAsksAQuestionParksAndResumesOnTheAnswer(t *testing.T) {
	n := startCoding(t, "ask")
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	n.wake(t, "swe", "the api test is flaking, please fix it")

	waitFor(t, "the run to park on its question", func() bool {
		for _, run := range n.activeRuns(t) {
			if run.Status == sandbox.StatusAwaiting && run.Question != "" {
				return true
			}
		}
		return false
	})
	parked := n.runFor(t, sandbox.StatusAwaiting)
	if parked.Question != "Which branch should I target?" {
		t.Fatalf("question = %q", parked.Question)
	}
	if parked.Audience != "requester" {
		t.Fatalf("audience = %q", parked.Audience)
	}
	// The seat is FREE while it waits: a person can take days, and the
	// answer arrives on the seat's own inbox.
	if n.engine.AwaitingSandbox("swe") {
		t.Fatal("a seat waiting on a person cannot receive their answer while parked")
	}
	// The box is held paused for an exact resume, with its TTL now ticking
	// for the reaper.
	if parked.SandboxID == "" {
		t.Fatal("the box was torn down, so the answer cannot resume the checkout")
	}
	if parked.PausedAt.IsZero() {
		t.Fatal("the row does not record that a snapshot is being held")
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func (n *codingNode) activeRuns(t *testing.T) []sandbox.PendingRun {
	t.Helper()
	runs, err := sandbox.NewSQLStore(n.engine.Backends().Store).ListActive(t.Context())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	return runs
}

func (n *codingNode) runFor(t *testing.T, status string) sandbox.PendingRun {
	t.Helper()
	for _, run := range n.activeRuns(t) {
		if run.Status == status {
			return run
		}
	}
	t.Fatalf("no run in %q", status)
	return sandbox.PendingRun{}
}

// liveBoxes is what the local backend still has on disk.
func (n *codingNode) liveBoxes(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(n.stateDir, "boxes"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func countOf(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}
