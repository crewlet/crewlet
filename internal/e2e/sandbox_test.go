package e2e

import (
	"context"
	"encoding/json"
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
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
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
    # Long enough that the engine can be stopped while the job runs, short
    # enough that the test does not wait out a real coding job.
    sleep 3
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

	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: &boot, Company: cfg,
		// The completion poll, sped up. It is sized in production against
		// coding jobs that run for minutes; at that cadence a test whose
		// job finishes in a moment would wait out a real tick to see it.
		SandboxPollInterval: 100 * time.Millisecond,
	})
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
			// The DURABLE record, which is what the board must read: a
			// run parked on a question waits days, and the live
			// projection sweeps long before that.
			Sandbox: sandbox.NewSQLStore(e.Backends().Store),
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

// The board an operator looks at when a run needs somebody.
//
// The DURABLE record, over the same query channel every other read uses: a run
// parked on a question waits days, and the live projection sweeps long before
// that — so the states that most need a person were the ones least likely to
// be on screen.
func TestAParkedRunReachesTheBoardAnOperatorReads(t *testing.T) {
	n := startCoding(t, "ask")
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	n.wake(t, "swe", "the api test is flaking, please fix it")

	waitFor(t, "the parked run to reach the board", func() bool {
		for _, row := range n.board(t) {
			if row["status"] == sandbox.StatusAwaiting {
				return true
			}
		}
		return false
	})
	var parked map[string]any
	for _, row := range n.board(t) {
		if row["status"] == sandbox.StatusAwaiting {
			parked = row
		}
	}
	if parked["question"] != "Which branch should I target?" {
		t.Fatalf("question = %v", parked["question"])
	}
	if parked["agent_handle"] != "swe" || parked["coding_agent"] != "claude-code" {
		t.Fatalf("row = %v", parked)
	}
	if parked["box_exists"] != true || parked["paused_at"] == "" {
		t.Fatalf("the held snapshot is invisible, so nobody can see what is being paid for: %v", parked)
	}
	// The suspended conversation is by far the largest column in the row,
	// and every prompt in it is already reachable through the event store.
	for _, key := range []string{"execute_state", "messages"} {
		if _, leaked := parked[key]; leaked {
			t.Fatalf("%q reached a board that renders one line per run", key)
		}
	}
}

// board reads the sandbox-runs answer over the query surface, exactly as the
// dashboard does.
func (n *codingNode) board(t *testing.T) []map[string]any {
	t.Helper()
	res, err := http.Get(n.server.URL + "/query/sandbox_runs")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /query/sandbox_runs = %d", res.StatusCode)
	}
	var payload struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return payload.Runs
}

// THE RESTART LEG of the gate. A coding job outlives the engine that started
// it, so the state that matters is the row: a fresh process picks the run up
// on the seat's next claim, drives it to completion, and re-enters the SAME
// conversation the dead process suspended.
func TestAnEngineRestartMidRunStillFinishesTheSameTurn(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("the local sandbox needs a POSIX shell")
	}
	stateDir := t.TempDir()
	binDir := installFakeAgent(t)
	t.Setenv("FAKE_AGENT_MODE", "slow")
	t.Setenv("FAKE_AGENT_PATH", filepath.Join(binDir, "claude"))

	// ONE store and ONE stream directory across both processes, which is
	// what makes this a restart rather than a fresh company.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crewlet.db")
	streamDir := filepath.Join(dir, "stream")

	model := newSandboxModel(t)
	doc := fmt.Sprintf(sandboxCompanyDoc, model.url, stateDir)

	first := bootCompanyIn(t, doc, model, dbPath, streamDir)
	firstNode := &codingNode{node: first, stateDir: stateDir, binDir: binDir}
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(first.engine.Node().Host().Held(), "swe")
	})
	first.wake(t, "swe", "the api test is flaking, please fix it")

	// The run is launched and its conversation persisted — the two facts
	// the restart depends on.
	waitFor(t, "the run to be recorded with its conversation", func() bool {
		for _, run := range firstNode.activeRuns(t) {
			if run.Status == sandbox.StatusRunning && len(run.ExecuteState) > 0 {
				return true
			}
		}
		return false
	})
	launched := firstNode.runFor(t, sandbox.StatusRunning)

	// The engine goes away with the job still running. Its box does NOT: a
	// detached run is reparented to init and keeps going, which is the
	// whole reason the row exists.
	first.engine.Stop(context.Background())

	second := bootCompanyIn(t, doc, model, dbPath, streamDir)
	secondNode := &codingNode{node: second, stateDir: stateDir, binDir: binDir}
	waitFor(t, "the new engine to claim the seat", func() bool {
		return slices.Contains(second.engine.Node().Host().Held(), "swe")
	})
	// Recovery re-marked the seat busy from the row, so its mail is parked
	// rather than starting a second turn beside the running job.
	waitFor(t, "the recovered run to park the seat", func() bool {
		return second.engine.AwaitingSandbox("swe")
	})

	// And it drives the same run to completion, re-entering the
	// conversation the dead process suspended.
	waitFor(t, "the recovered run to resume its turn", func() bool {
		return slices.Contains(model.seen(), "resumed")
	})
	waitFor(t, "the recovered run to settle", func() bool {
		return len(secondNode.activeRuns(t)) == 0
	})
	if got := countOf(model.seen(), "plan"); got != 1 {
		t.Fatalf("Plan ran %d times across the restart; the resume must not re-plan. phases = %v",
			got, model.seen())
	}
	_ = launched
}

// THE CONTAINER LEG. Same protocol, real host isolation — and the mode whose
// in-box paths match a remote backend's, so a setup step that provisions a
// system path works there and is refused in `direct`.
//
// Skipped where no container runtime is usable rather than dropped: it is the
// half of the gate a workstation cannot always run, and a suite that quietly
// tested only the easy mode would report a gate it had not met.
func TestTheContainerModeRunsTheSameProtocol(t *testing.T) {
	runtime := usableContainerRuntime(t)
	if runtime == "" {
		t.Skip("no usable container runtime; the direct mode covers the protocol here")
	}
	image := os.Getenv("CREWLET_TEST_SANDBOX_IMAGE")
	if image == "" {
		image = "alpine:3"
	}
	local, err := sandbox.NewLocal(sandbox.LocalOptions{
		Containment: sandbox.Container, StateDir: t.TempDir(),
		Image: image, Runtime: filepath.Base(runtime),
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(context.Background()) })

	// The in-box home matches a remote backend's, which is what makes a
	// setup step written for one work on the other.
	if box.Home() != sandbox.DefaultHome {
		t.Fatalf("home = %q, want %q", box.Home(), sandbox.DefaultHome)
	}
	runner := codingagent.NewClaudeCode()
	if err := runner.Install(t.Context(), box); err != nil {
		t.Fatalf("Install: %v", err)
	}
	p := codingagent.PathsFor(box)

	// A file written from the host side of the mount is visible inside.
	if err := box.WriteFile(t.Context(), p.WorkDir()+"/probe", []byte("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	res, err := box.Exec(t.Context(), "cat "+p.WorkDir()+"/probe", sandbox.ExecOptions{})
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("Exec = %+v, %v", res, err)
	}
	// And the ask shim runs, which is the one part of the protocol that is
	// a script the engine wrote into somebody else's image.
	if _, err := box.Exec(t.Context(),
		`PATH='`+p.BinDir()+`':"$PATH" crewlet-ask "which branch?" --to team`,
		sandbox.ExecOptions{}); err != nil {
		t.Fatalf("the ask shim: %v", err)
	}
	blob, err := box.ReadFile(t.Context(), p.Ask())
	if err != nil || !strings.Contains(string(blob), "which branch?") {
		t.Fatalf("ask.json = %q, %v", blob, err)
	}
}

// usableContainerRuntime returns a runtime that can actually start a
// container, or "". Present-on-PATH is not enough: a daemon can be installed
// and unreachable, which is the ordinary case in a container-in-container CI.
func usableContainerRuntime(t *testing.T) string {
	t.Helper()
	found, err := sandbox.ResolveContainerRuntime("auto")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, found, "info").Run(); err != nil {
		return ""
	}
	return found
}

// THE RESEED LEG. A person can take days, and a paused box is held and paid
// for the whole time — so the reaper reclaims it past its TTL. The run is NOT
// over: the answer can still arrive, and the work re-seeds from the branch the
// agent pushed, which was always the durable half.
func TestAnExpiredPauseReclaimsTheBoxAndLeavesTheRunWaiting(t *testing.T) {
	n := startCodingWithPause(t, "ask", 1)
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	n.wake(t, "swe", "the api test is flaking, please fix it")

	waitFor(t, "the run to park with its box held", func() bool {
		for _, run := range n.activeRuns(t) {
			if run.Status == sandbox.StatusAwaiting && run.SandboxID != "" {
				return true
			}
		}
		return false
	})

	// Past the TTL, the reaper takes the box and the run moves to reseed.
	waitFor(t, "the pause to expire", func() bool {
		for _, run := range n.activeRuns(t) {
			if run.Status == sandbox.StatusReseed {
				return true
			}
		}
		return false
	})
	reseeded := n.runFor(t, sandbox.StatusReseed)
	if reseeded.SandboxID != "" {
		t.Fatalf("the row still names a box that was reclaimed: %q", reseeded.SandboxID)
	}
	if reseeded.Question == "" {
		t.Fatal("the question was lost, so the answer has nothing to match")
	}
	// The run is still on the board, still answerable — which is the whole
	// point: a reseed used to look exactly like work that had finished.
	found := false
	for _, row := range n.board(t) {
		if row["status"] == sandbox.StatusReseed {
			found = true
			if row["box_exists"] != false {
				t.Fatalf("the board claims a box that is gone: %v", row)
			}
		}
	}
	if !found {
		t.Fatal("a reseeded run has no surface, so nobody knows it is waiting")
	}
	// And the box really is gone from disk, not merely forgotten.
	waitFor(t, "the box to be removed", func() bool { return len(n.liveBoxes(t)) == 0 })
}

// startCodingWithPause stands a node up whose paused boxes expire after
// pauseTTL seconds, so a test can watch the reaper without waiting half an
// hour for the production value.
func startCodingWithPause(t *testing.T, mode string, pauseTTL int) *codingNode {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("the local sandbox needs a POSIX shell")
	}
	stateDir := t.TempDir()
	binDir := installFakeAgent(t)
	t.Setenv("FAKE_AGENT_MODE", mode)
	t.Setenv("FAKE_AGENT_PATH", filepath.Join(binDir, "claude"))

	model := newSandboxModel(t)
	doc := strings.Replace(
		fmt.Sprintf(sandboxCompanyDoc, model.url, stateDir),
		"default_pause_ttl_seconds: 1800",
		fmt.Sprintf("default_pause_ttl_seconds: %d", pauseTTL), 1)
	return &codingNode{node: bootCompany(t, doc, model), stateDir: stateDir, binDir: binDir}
}
