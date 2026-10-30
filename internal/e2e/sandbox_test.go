package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
	"github.com/crewlet/crewlet/internal/statelog"
)

// codingConversation is the DM line the coding gate's turn arrives on, and
// codingThread the thread within it the kick-off message sits in.
//
// A DIRECT MESSAGE, which is the one surface where the inbox partition and the
// durable conversation differ — the batch is the thread, the conversation is
// the whole line. The two values are carried apart from the turn onto the
// detached run's row and back, and on every other surface they are the same
// string: a gate that woke the seat there would pass for a frame that wrote
// either one into both, which is exactly the mis-mapping this pair invites.
const (
	codingConversation = "slack:D-coding"
	codingThread       = "root-1"
)

// conversation reads a seat's thread history straight from the node's own
// store, which is where the ledger lives.
func (n *node) conversation(t *testing.T, handle, key string) []ledger.Session {
	t.Helper()
	got, err := ledgerstore.NewConversations(n.engine.Backends().Store).
		History(context.Background(), handle, key, 0)
	if err != nil {
		t.Fatalf("conversation history for %s on %s: %v", handle, key, err)
	}
	return got
}

// GATE G6 — the golden coding turn.
//
// A real local sandbox, a real detached process, the real suspend and the real
// resume. The one stub is the model, behind the scripted CLI the seat's text
// calls run on, exactly as in the gate above: what is under test is the
// ENGINE's half of a coding turn, and a real coding CLI would test the
// vendor's.
//
// The "coding agent" here is a shell script installed as `claude` on the box's
// PATH. That makes the whole detached protocol real — a process group, a
// background job, a done marker, a findings file, an ask signal — while
// leaving what the agent DOES up to the test.

// sandboxCompanyDoc is the golden company with a code-enabled seat, filled by
// [sandboxCompany].
//
// containment `direct`, because the point is the engine's protocol and a
// container would make this suite need Docker. The path escape guards and the
// container argv have their own tests in internal/sandbox.
//
// TWO MODEL ENTRIES, because a coding run and a text call are two different
// CLIs here. The seat's text calls — onboarding, the executor that launches the
// run, the reviewer — run on the scripted entry. The RUN takes its runner from
// the seat's coding agent and its model from llm_sandbox, and those must be
// one CLI's: an entry's model is written in its own CLI's grammar, so config
// refuses a Claude Code run on an entry driving any other. So llm_sandbox names
// `coder`, a Claude Code entry none of the seat's text phases names, whose
// model the stand-in below is handed — on a state directory of its own, since
// two CLIs never share one.
const sandboxCompanyDoc = `
name: Nimbus
providers:
  llm:
%s
    coder:
      agent: claude-code
      model: sonnet
      state_dir: %q
  sandbox:
    default_run_in: direct
    default_coding_agent: claude-code
    default_pause_ttl_seconds: 1800
    local:
      state_dir: %q
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
    llm_sandbox: coder
    sandbox:
      enabled: true
      # The seat's own round cap. Nothing about the stand-in agent honours
      # it — what is under test is that a number written HERE reaches the
      # coding CLI's argv, which is the whole of "wired".
      max_turns: 7
      env:
        FAKE_AGENT_MODE: "${FAKE_AGENT_MODE}"
        FAKE_AGENT_PATH: "${FAKE_AGENT_PATH}"
        FAKE_AGENT_ARGV: "${FAKE_AGENT_ARGV}"
        FAKE_AGENT_RELEASE: "${FAKE_AGENT_RELEASE}"
        FAKE_AGENT_TOKEN: "${FAKE_AGENT_TOKEN}"
      # Only the cases that read the argv, hold the agent or keep the run's
      # git token set these three, and the stand-in reads each as possibly
      # unset (${VAR:-}). Declared optional, a case that sets none runs
      # without them; undeclared, every such case's run is refused for a
      # value it never needed.
      optional_keys: [FAKE_AGENT_ARGV, FAKE_AGENT_RELEASE, FAKE_AGENT_TOKEN]
  - name: Founder
    kind: human
    contact:
      slack_user_id: U0FOUNDER
turn_engine:
  max_iterations: 1
  max_tool_rounds: 4
`

// sandboxCompany is the coding company's document: its text calls on model's
// scripted CLI, its boxes under stateDir, and its coding runs' Claude Code
// entry on a directory of the test's own.
func sandboxCompany(t *testing.T, model *scriptedModel, stateDir string) string {
	t.Helper()
	return fmt.Sprintf(sandboxCompanyDoc, model.entry("scripted"), t.TempDir(), stateDir)
}

// codingNode is a running node whose seat can run code.
type codingNode struct {
	*node
	stateDir string
	binDir   string
	argvPath string
}

// agentArgv is what the stand-in coding CLI was actually invoked with.
func (n *codingNode) agentArgv(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(n.argvPath)
	if err != nil {
		t.Fatalf("the coding agent recorded no invocation: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// startCoding stands up a node with a local sandbox and a fake coding CLI.
func startCoding(t *testing.T, mode string) *codingNode {
	t.Helper()
	stateDir := t.TempDir()
	binDir := installFakeAgent(t)
	// Outside the box directory, so it survives the teardown that ends the
	// turn under test.
	argv := filepath.Join(stateDir, "argv.txt")
	// All three reach the box through role.sandbox.env, which is the ONLY
	// way an external value gets there: the engine names no tool-specific
	// variable of its own. That the fake agent runs at all is therefore also
	// the proof that ${VAR} references in that block resolve.
	env := fakeAgentEnv(binDir, mode)
	env["FAKE_AGENT_ARGV"] = argv

	model := newSandboxModel(t)
	doc := sandboxCompany(t, model, stateDir)
	n := bootCompany(t, doc, model, env)
	return &codingNode{node: n, stateDir: stateDir, binDir: binDir, argvPath: argv}
}

// fakeAgentEnv is what a coding node's environment carries for the stand-in
// CLI installed in binDir to run in mode: where to copy it from, and how to
// behave.
func fakeAgentEnv(binDir, mode string) map[string]string {
	return map[string]string{
		"FAKE_AGENT_MODE": mode,
		"FAKE_AGENT_PATH": filepath.Join(binDir, "claude"),
	}
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
# file, the ask shim, the stream-json messages and the result message that
# ends them — is the real protocol.
work="$HOME/.crewlet"
init='{"type":"system","subtype":"init","session_id":"sess-1","tools":["Bash"]}'
# The invocation, recorded OUTSIDE the box: the box is torn down when the
# turn finishes, and the argv is what a test asserts about afterwards.
if [ -n "${FAKE_AGENT_ARGV:-}" ]; then
  for arg in "$@"; do printf '%s\n' "$arg"; done > "$FAKE_AGENT_ARGV"
fi
case "${FAKE_AGENT_MODE:-succeed}" in
  succeed)
    sleep 0.1
    printf 'Outcome: succeeded\nRan the suite; all green.\nOpened https://github.com/acme/api/pull/7\n' > "$work/findings.md"
    printf '%s\n' "$init"
    printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"go test ./..."}}]},"session_id":"sess-1"}'
    printf '%s\n' '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"ok","is_error":false}]},"session_id":"sess-1"}'
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"sess-1","usage":{"input_tokens":700,"output_tokens":120}}'
    ;;
  ask)
    sleep 0.1
    crewlet-ask "Which branch should I target?" --to requester
    printf '%s\n' "$init"
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"blocked","session_id":"sess-1"}'
    ;;
  held)
    # RUNS UNTIL THE TEST SAYS SO, rather than for a fixed time.
    #
    # The restart case needs this job still running when the second engine
    # recovers the seat, and a sleep cannot promise that: on a loaded
    # machine the second boot takes longer than any duration short enough
    # to keep the test quick, the job finishes first, recovery sees a
    # settled run and the seat is never parked — a hang no timeout can
    # fix, because the state it waits for will never arrive.
    #
    # So the test creates the release file when it has seen what it needed
    # to see. The ceiling is a backstop against a test that dies without
    # releasing, not a timing assumption.
    waited=0
    while [ ! -f "${FAKE_AGENT_RELEASE:-/nonexistent}" ] && [ "$waited" -lt 1200 ]; do
      sleep 0.1
      waited=$((waited + 1))
    done
    printf 'Outcome: succeeded\nfinished after the restart\n' > "$work/findings.md"
    printf '%s\n' "$init"
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"sess-1"}'
    ;;
  push)
    # A RUN THAT CHANGES WHAT IT WAS GIVEN: one commit in the checkout the
    # engine's clone step made at ./web, where the CLI starts, pushed to the
    # branch that checkout is on — by git alone, with nothing of this
    # script's: the credential, the remote and the identity are the launch
    # environment's. The token is kept OUTSIDE the box (FAKE_AGENT_TOKEN), for
    # the test to present once the run has ended.
    if [ -n "${FAKE_AGENT_TOKEN:-}" ]; then
      printf '%s' "${CREWLET_GIT_TOKEN:-}" > "$FAKE_AGENT_TOKEN"
    fi
    cd web || { echo "no checkout of web in $PWD" >&2; exit 65; }
    printf 'the fix\n' > FIX.md
    git add FIX.md && git commit -q -m 'Fix the failing test' && git push -q origin HEAD || exit 66
    printf 'Outcome: succeeded\nPushed the fix.\n' > "$work/findings.md"
    printf '%s\n' "$init"
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"sess-1"}'
    ;;
  *)
    # A MODE THIS SCRIPT DOES NOT KNOW SAYS SO. Falling through silently
    # would exit 0 with no envelope, which reaches the engine as a run
    # that produced nothing — and every test on top of it waits out its
    # budget for a state that will never arrive.
    printf 'unknown FAKE_AGENT_MODE %s\n' "${FAKE_AGENT_MODE:-}" >&2
    exit 64
    ;;
esac
`
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("install the fake agent: %v", err)
	}
	return dir
}

// bootCompany stands a node (an engine and its API) up over a company document,
// with env added to its environment ([nodeEnvironment]).
//
// The same assembly startNode does, factored out so a suite whose subject
// needs its own store directory — one that SURVIVES a restart — can supply it.
func bootCompany(t *testing.T, doc string, model *scriptedModel, env map[string]string) *node {
	t.Helper()
	dir := t.TempDir()
	return bootCompanyIn(t, doc, model, filepath.Join(dir, "crewlet.db"),
		filepath.Join(dir, "stream"), env, nil)
}

// bootCompanyIn is [bootCompany] on the store and stream paths given, its API
// served on listener where the case bound one (nil for a fresh port: see
// [wireAPI]).
func bootCompanyIn(t *testing.T, doc string, model *scriptedModel, dbPath, streamDir string,
	env map[string]string, listener net.Listener) *node {
	t.Helper()
	logs.attribute(t)
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	boot := config.DefaultBootstrap()
	withServingTierA(t, &boot)
	boot.Store.Path = dbPath
	boot.Stream.StoreDir = streamDir
	seedStore(t, &boot)

	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: &boot, Company: cfg, ActivatedAt: harnessActivation,
		Environment: nodeEnvironment(env),
		// The completion poll, sped up. It is sized in production against
		// coding jobs that run for minutes; at that cadence a test whose
		// job finishes in a moment would wait out a real tick to see it.
		SandboxPollInterval: 100 * time.Millisecond,
		// THIS TEST BINARY IS THE HOOK a node hosting code execs, as
		// startNode hands it; a company hosting none never reads it.
		GitHook: codetest.HookCommand(t),
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

	app, srv := serveAPI(t, e, &boot, func(opts *api.Options) {
		// The DURABLE record, which is what the board must read: a run
		// parked on a question waits days, and the live projection sweeps
		// long before that.
		opts.Sources.Sandbox = sandbox.NewCoordStore(e.Backends().Fleet)
	}, listener)
	return &node{engine: e, app: app, server: srv, model: model}
}

// newSandboxModel is the scripted model for a coding turn whose run_sandbox
// call names a task on another host in its brief, as every coding case here
// but the code host's does.
func newSandboxModel(t *testing.T) *scriptedModel {
	t.Helper()
	return newCodingModel(t, map[string]any{"brief": "Clone example.com/acme/api and fix the failing test"})
}

// newCodingModel is the scripted model for a coding turn whose executor calls
// run_sandbox with args.
//
// The executor calls run_sandbox on its FIRST Execute round and reports on the
// resumed one, which is what makes the suspend and the re-entry both real:
// the second Execute request is one the engine only sends after the detached
// job has finished and its result has been spliced in.
func newCodingModel(t *testing.T, args map[string]any) *scriptedModel {
	t.Helper()
	m := &scriptedModel{stateDir: t.TempDir()}
	m.listen(t, func(w http.ResponseWriter, r *http.Request) {
		call, offered, ok := readCall(w, r)
		if !ok {
			return
		}
		var reply string
		switch {
		case offered["submit_review"]:
			m.saw("review")
			reply = toolUse("submit_review", map[string]any{
				"decision": "done", "final_artifact": "The fix is up for review.",
			})
		case offered["mark_onboarded"]:
			m.saw("onboarding")
			reply = toolUse("mark_onboarded", map[string]any{"notes": "read the handbook"})
		case offered["submit_work"] && sawSandboxResult(call.Prompt):
			// The RESUMED executor round: the conversation now carries the
			// tool message the engine spliced in. Submitting is the
			// executor reporting and finishing.
			m.saw("resumed")
			reply = toolUse("submit_work", map[string]any{
				"outcome": "delivered",
				"summary": "The sandbox fixed it and opened a pull request.",
				// run_sandbox is server-backed and not a known read, so it
				// IS a delivery the engine's own record can confirm.
				"deliveries": []string{"run_sandbox"},
			})
		case offered["submit_work"]:
			m.saw("execute")
			reply = toolUse("run_sandbox", args)
		default:
			// AN AUXILIARY PASS, not a phase. The prefetch's filters reach
			// this same model with no tools offered, and counting them
			// as executor rounds made "the executor opened once" a claim
			// about the memory filter.
			m.saw("aux")
			reply = textReply("")
		}
		_, _ = io.WriteString(w, reply)
	})
	return m
}

// sawSandboxResult reports whether this prompt's conversation already carries
// the sandbox's answer — which is what makes it the resumed round.
//
// IN A TOOL RESULT, which is where the engine splices it: the run's report is
// the answer to the round's run_sandbox call, and a mention anywhere else in
// the transcript is not the run having come back.
func sawSandboxResult(prompt string) bool {
	return slices.ContainsFunc(toolResultsIn(prompt), func(result string) bool {
		return strings.Contains(result, "The sandbox coding run")
	})
}

// ---------------------------------------------------------------------
// the gate
// ---------------------------------------------------------------------

// A coding turn end to end: decide, launch, suspend, poll, collect, resume, and
// the same turn finishing with the agent's findings in hand.
func TestAGoldenCodingTurnSuspendsAndResumes(t *testing.T) {
	t.Parallel()
	n := startCoding(t, "succeed")
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	// IN A DM THREAD, as a chat message arrives. The conversation is what
	// the resumed turn owes an answer to and what its ledger entry is filed
	// under; the thread beside it is the batch this run was launched from,
	// and the row carries both.
	n.wakeInDirectThread(t, "swe", "the api test is flaking, please fix it",
		codingConversation, codingThread)

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
	}, n.diagnose)
	waitFor(t, "the run to settle", func() bool {
		return len(n.activeRuns(t)) == 0
	})

	seen := n.model.seen()
	for _, want := range []string{"execute", "resumed", "review"} {
		if !slices.Contains(seen, want) {
			t.Fatalf("the %s phase never ran; phases = %v", want, seen)
		}
	}
	// ONE turn, not two. The resumed round re-enters the conversation the
	// suspend left; a second opening round would mean the engine started a
	// fresh turn and re-derived work already done.
	if got := countOf(seen, "execute"); got != 1 {
		t.Fatalf("the executor opened %d times; a resume must re-enter. phases = %v", got, seen)
	}
	// THE RESUMED RECORD NAMES THE LAUNCH IT COLLECTED. One turn can launch
	// more than once, so a sandbox segment of a phase is identified by the
	// job the resume claimed — copied off the run's row by the engine, which
	// is the only frame holding both, and asserted end to end because the
	// row and the record are written by different processes' worth of code.
	var launch string
	waitFor(t, "the resumed phase's record to name its launch", func() bool {
		rows, _, err := n.engine.Backends().Store.Events().Phases(t.Context(), "", 60, nil, time.Now())
		if err != nil {
			t.Fatalf("phases: %v", err)
		}
		for _, row := range rows {
			var rec map[string]any
			if json.Unmarshal(row.Payload, &rec) != nil {
				continue
			}
			if rec["phase"] == "execute" && rec["backend"] == "sandbox" {
				launch, _ = rec["launch_id"].(string)
				return true
			}
		}
		return false
	})
	if launch == "" {
		t.Error("the resumed sandbox phase names no launch_id")
	}
	// THE RUN'S STREAM REACHED ITS RECORD: the transcript the runner built
	// from the stand-in's stream-json messages, and the spend its result
	// line — the stream's last, copied out by the wrapper — reported.
	// End to end because the halves are the CLI's flags, the box's shell and
	// the record's publisher, and each compiles without the others.
	var record map[string]any
	waitFor(t, "the run's own phase record", func() bool {
		rows, _, err := n.engine.Backends().Store.Events().Phases(t.Context(), "", 60, nil, time.Now())
		if err != nil {
			t.Fatalf("phases: %v", err)
		}
		for _, row := range rows {
			var rec map[string]any
			if json.Unmarshal(row.Payload, &rec) == nil && rec["phase"] == "sandbox" {
				record = rec
				return true
			}
		}
		return false
	})
	if transcript, _ := record["activity_transcript"].(string); transcript != "[tool] Bash: go test ./..." {
		t.Errorf("the run's transcript is %q; want the one tool call its stream made", transcript)
	}
	if input, _ := record["input_tokens"].(float64); input != 700 {
		t.Errorf("the run's input tokens are %v; want the 700 its result line reported", record["input_tokens"])
	}
	// The box is gone: the resumed Execute made no further run_sandbox call,
	// so the phase was done with it.
	if boxes := n.liveBoxes(t); len(boxes) != 0 {
		t.Fatalf("%d box directories survived the turn: %v", len(boxes), boxes)
	}
	// THE SEAT'S ROUND CAP REACHED THE CODING AGENT'S ARGV. This is the
	// whole of "wired": role.sandbox.max_turns, through the config, the
	// spec overlay, the launch and the runner, to the flag the CLI reads.
	// The field existed and rendered long before anything filled it in, so
	// only an end-to-end assertion tells a wired knob from a decorative one.
	argv := n.agentArgv(t)
	i := slices.Index(argv, "--max-turns")
	if i < 0 || i == len(argv)-1 {
		t.Fatalf("the coding agent was invoked uncapped: %v", argv)
	}
	if argv[i+1] != "7" {
		t.Fatalf("--max-turns %s, want the seat's own 7", argv[i+1])
	}
	// AND THE MODEL ITS llm_sandbox ENTRY NAMES, which is the half of a
	// coding run's pairing the seat's text calls cannot show: those run on
	// another entry altogether, so a run handed their model — or none, on
	// the CLI's own default — would look the same from everywhere else.
	m := slices.Index(argv, "--model")
	if m < 0 || m == len(argv)-1 || argv[m+1] != "sonnet" {
		t.Fatalf("the coding agent was not handed the coder entry's model sonnet: %v", argv)
	}
	// AND THE CONVERSATION KNOWS THE TURN HAPPENED. A turn that ends on the
	// resume path ends a thread's turn as surely as one that ends on the
	// inbox path, and only the inbox path recorded it: the thread's history
	// stopped at the moment the run detached, so the seat's next turn on it
	// read a conversation in which the coding work had never happened.
	//
	// End to end because the two halves are in different processes' worth of
	// code: the LAUNCH writes the conversation onto the run's row, and the
	// RESUME — which cannot see the trigger any more — reads it back.
	//
	// AND UNDER THE CONVERSATION RATHER THAN THE BATCH, which only a DM-shaped
	// wake can assert: the launch copies two values off the turn into two
	// fields of the row, and a crossed pair compiles — both are strings — so
	// the entry filed here is the only end-to-end proof they did not swap.
	history := n.conversation(t, "swe", codingConversation)
	if len(history) != 1 {
		t.Fatalf("the resumed turn left %d conversation entries under the DM line, "+
			"want 1: the row it reported through names the wrong conversation",
			len(history))
	}
	if batch := n.conversation(t, "swe", codingConversation+":"+codingThread); len(batch) != 0 {
		t.Errorf("the resumed turn filed %d entries under the inbox batch instead; "+
			"the seat's next turn on this DM reads the conversation and finds "+
			"nothing", len(batch))
	}
	if history[0].Reply == "" {
		t.Errorf("the entry carries no reply: %+v", history[0])
	}
	if history[0].Decision != "done" {
		t.Errorf("decision = %q, want the resumed turn's own outcome", history[0].Decision)
	}
}

// withBoxRepo is the coding company hosting code, as
// [TestACodingRunClonesPushesAndIsToldWhatItPushed] runs it: one repository,
// `web`, the coding seat's own, and the seat's code access full — a role that
// names none reads only, and a run it launches is given a repository to read.
func withBoxRepo(doc string) string {
	doc = strings.Replace(doc, "    llm_sandbox: coder\n",
		"    llm_sandbox: coder\n    code: {access: full_access}\n", 1)
	return doc + `code:
  native: true
  repos:
    - name: web
      owner: {seat: swe}
`
}

// A CODING RUN CLONES THE REPOSITORY IT NAMES AND PUSHES AS ITS RUN, end to
// end through a real engine serving git: the executor names `web` in
// run_sandbox's `repos`; the launch resolves it into the run's grant and hands
// the box its token, the code host's address (CREWLET_BOX_GIT_URL, this node's
// own listener) and its git configuration; the engine's own setup steps clone
// it; and the stand-in coding CLI commits and pushes with nothing of its own —
// so the push reaching the code host at all is the proof that the token, the
// credential helper, the base and the identity agree.
//
// Asserted from the code host's own record, never the box's word: the push
// row names the seat as an agent, received over git, made by THIS run's
// launch with its coding agent, onto the seat's default branch for a turn on
// no work item (`seats/swe/run-<the turn id's last 12 hex>`); the commit the
// row moved the branch to, read back from the code host, carries the run's
// three trailers; and once the run has ended its token admits nothing.
// (A `direct` box on the node that serves git: warned, never refused.)
//
// Mutations: a launch whose RunEnv sets no trailer variables → the commit
// carries no trailer, red; boxGitEnv without the credential helper → the
// clone step fails the launch and no push row is written, red; the run's
// token minted for another launch → the clone is refused `run_superseded`, red.
func TestACodingRunClonesPushesAndIsToldWhatItPushed(t *testing.T) {
	t.Parallel()
	// THE NODE'S OWN ADDRESS, BOUND BEFORE IT BOOTS: CREWLET_BOX_GIT_URL is
	// read once at boot, and the box dials it from the same host, so this
	// listener is both the API's and the box's way to the code host.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the node's listener: %v", err)
	}
	stateDir := t.TempDir()
	binDir := installFakeAgent(t)
	tokenPath := filepath.Join(stateDir, "git-token")
	env := fakeAgentEnv(binDir, "push")
	env["FAKE_AGENT_TOKEN"] = tokenPath
	base := "http://" + listener.Addr().String() + "/box/git"
	env[sandbox.BoxGitURLVar] = base

	model := newCodingModel(t, map[string]any{
		"brief": "Fix the failing test in web",
		"repos": []any{map[string]any{"repo": "web"}},
	})
	doc := withBoxRepo(sandboxCompany(t, model, stateDir))
	dir := t.TempDir()
	n := &codingNode{node: bootCompanyIn(t, doc, model, filepath.Join(dir, "crewlet.db"),
		filepath.Join(dir, "stream"), env, listener), stateDir: stateDir, binDir: binDir}
	repo := awaitRepo(t, "web", code.RepoStatusActive, n.node)
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	n.wakeInDirectThread(t, "swe", "the web test is failing, please fix it", codingConversation, codingThread)

	var run sandbox.PendingRun
	waitFor(t, "the run to be launched with its grant", func() bool {
		runs := n.activeRuns(t)
		if len(runs) != 1 || len(runs[0].Repos) != 1 {
			return false
		}
		run = runs[0]
		return true
	}, n.diagnose)
	if got := run.Repos[0]; got.Repo != "web" || got.RepoID != repo.ID || got.Access != sandbox.RepoWrite {
		t.Fatalf("the run was given %+v; want web (%s) to write", got, repo.ID)
	}
	waitFor(t, "the suspended turn to be resumed", func() bool {
		return slices.Contains(n.model.seen(), "resumed")
	}, n.diagnose)
	waitFor(t, "the run to settle", func() bool { return len(n.activeRuns(t)) == 0 })

	// THE PUSH, AS THE CODE HOST RECORDED IT.
	seam, ok := engine.OperatorCode(n.engine)
	if !ok {
		t.Fatal("the node serves no code operations")
	}
	answer, err := seam.Read(t.Context(), code.ReadQuery{Kind: code.ReadRunPushes, Limit: 10,
		RunPushes: &code.RunPushesQuery{Run: run.LaunchID, Repos: []string{repo.ID}, After: repo.Position},
		Fresh:     statelog.Freshness{Level: statelog.ReadLinearizable}})
	if err != nil {
		t.Fatalf("read the run's pushes: %v", err)
	}
	if len(answer.Pushes) != 1 {
		t.Fatalf("the run is recorded as %d pushes, want its one: %+v", len(answer.Pushes), answer.Pushes)
	}
	push := answer.Pushes[0]
	branch := "refs/heads/seats/swe/run-" + run.TurnID[len(run.TurnID)-12:]
	switch {
	case push.Via != code.ViaGit || push.ActorKind != string(code.PrincipalAgent) || push.ActorSeat != "swe":
		t.Errorf("the push is recorded via %s by %s %q; want via git by the agent swe", push.Via,
			push.ActorKind, push.ActorSeat)
	case push.Run.RunID != run.LaunchID || push.Run.CodingAgent != "claude-code":
		t.Errorf("the push is recorded as run %q by %q; want this launch, %s, by claude-code", push.Run.RunID,
			push.Run.CodingAgent, run.LaunchID)
	case len(push.Commands) != 1 || push.Commands[0].Ref != branch || push.Commands[0].Result != code.ResultOK:
		t.Fatalf("the push moved %+v; want %s created", push.Commands, branch)
	}

	// THE COMMIT, READ BACK FROM THE CODE HOST, CARRIES THE RUN'S TRAILERS:
	// each asked as a literal of the pushed commit's whole message.
	for _, trailer := range []string{"Crewlet-Seat: swe", "Crewlet-Run: " + run.LaunchID,
		"Crewlet-Work-Item: none"} {
		got, err := seam.GitRead(t.Context(), code.GitQuery{Kind: code.GitLog, RepoID: repo.ID,
			Asker: code.Principal{Kind: code.PrincipalAgent, Seat: "swe"},
			Log:   &code.LogQuery{Rev: push.Commands[0].New, Grep: trailer, Limit: 1}})
		if err != nil {
			t.Fatalf("read the pushed commit's history: %v", err)
		}
		if got.Log == nil || len(got.Log.Commits) != 1 || got.Log.Commits[0].OID != push.Commands[0].New {
			t.Errorf("the pushed commit %s does not carry %q", push.Commands[0].New, trailer)
		}
	}

	// AND ITS TOKEN ADMITS NOTHING ONCE THE RUN HAS ENDED.
	token, err := os.ReadFile(tokenPath)
	if err != nil || len(token) == 0 {
		t.Fatalf("the coding CLI kept no git token (%v)", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		base+"/web.git/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("run", string(token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ask with the ended run's token: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "ended") {
		t.Errorf("an ended run's token is answered %d %q; want 403 saying the run ended", resp.StatusCode, body)
	}
}

// The clarification park AND the answer that ends it: the agent asks, the seat
// is freed, the person replies, and the SAME turn resumes.
//
// END TO END BECAUSE NOTHING SHORTER REACHES THE BUG. The answer is an ordinary
// chat message arriving at a seat with nothing holding it, and every layer in
// its way has to agree it might be an answer: the coordinator has to say a
// question is open, the screening has to offer the delivery before a turn
// consumes it, and the match has to find the row from the conversation the
// reply arrived on. A test that injected the screening's conditions by hand
// asserted the last two and assumed the first — which was the one that was
// false, for the whole life of the feature, so the answer ran as an unrelated
// turn and the box waited out its pause TTL.
func TestACodingRunThatAsksAQuestionParksAndResumesOnTheAnswer(t *testing.T) {
	t.Parallel()
	n := startCoding(t, "ask")
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	// IN A DM THREAD, which is the shape that stranded every clarification:
	// the run parks under the thread it was asked in, and a person answering
	// on the DM line arrives in a different batch entirely. Matched on the
	// batch the two never meet; matched on the conversation — one line
	// however it is threaded — the answer arrives.
	n.wakeInDirectThread(t, "swe", "the api test is flaking, please fix it",
		codingConversation, codingThread)

	waitFor(t, "the run to park on its question", func() bool {
		for _, run := range n.activeRuns(t) {
			if run.Status == sandbox.StatusAwaiting && run.Question != "" {
				return true
			}
		}
		return false
	}, n.diagnose)
	parked := n.runFor(t, sandbox.StatusAwaiting)
	if parked.Question != "Which branch should I target?" {
		t.Fatalf("question = %q", parked.Question)
	}
	if parked.Audience != "requester" {
		t.Fatalf("audience = %q", parked.Audience)
	}
	// The seat is FREE while it waits: a person can take days, and the
	// answer arrives on the seat's own inbox.
	if n.engine.SeatHeldBySandbox("swe") {
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

	// THE PERSON ANSWERS, top-level on the DM line rather than back in the
	// thread — a different inbox batch from the one the run parked under, and
	// the same conversation.
	n.wakeInConversation(t, "swe", "target main, please", codingConversation)

	waitFor(t, "the answer to resume the suspended turn", func() bool {
		return slices.Contains(n.model.seen(), "resumed")
	}, n.diagnose)
	waitFor(t, "the answered run to settle", func() bool {
		return len(n.activeRuns(t)) == 0
	}, n.diagnose)

	// ONE TURN, not two. This is the assertion the whole path turns on: the
	// answer re-entered the suspended Execute loop rather than opening a
	// fresh executor round, which is what a seat that treated the reply as
	// an ordinary message would have done — planning the work again with the
	// parked run still waiting beside it.
	if got := countOf(n.model.seen(), "execute"); got != 1 {
		t.Fatalf("the executor opened %d times; the answer started a new turn "+
			"instead of resuming the one that asked. phases = %v",
			got, n.model.seen())
	}
	// What the resumed round was handed is the spliced answer and not a
	// fresh brief, which is what the "resumed" phase itself records: that
	// branch of the scripted model fires only for a request whose
	// conversation already carries the run_sandbox reply. That the reply
	// carries the person's own words is pinned where the text is built, in
	// internal/sandbox's own suite.
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func (n *codingNode) activeRuns(t *testing.T) []sandbox.PendingRun {
	t.Helper()
	runs, err := sandbox.NewCoordStore(n.engine.Backends().Fleet).ListActive(t.Context())
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

// diagnose renders everything a stalled coding turn is stuck on.
//
// Every field here is one somebody reading a CI log had to guess at: which
// state the run reached (a launching run has not suspended yet; a failed one
// was settled by a tail that could not resume it), whether its conversation
// was ever written, whether its box still exists, and how far the model got.
// It reads the DURABLE record rather than the projection, so it answers after
// the run has been settled just as well as while it is live.
func (n *codingNode) diagnose() string {
	// EVERY ROW, not ListActive. That read is the operator board's and
	// excludes `failed` and `done` by construction — which is precisely the
	// row a destroyed turn leaves behind, so a diagnostic built on it prints
	// "0 active run(s)" for the most likely stall and stops. The raw fleet
	// records are decoded here rather than widening the store's contract
	// with a list-everything a test is the only caller of.
	records, err := n.engine.Backends().Fleet.SandboxRuns(context.Background())
	if err != nil {
		return fmt.Sprintf("the run records could not be read: %v", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "phases seen = %v; boxes on disk = %v; %d run record(s)",
		n.model.seen(), n.liveBoxesQuietly(), len(records))
	store := sandbox.NewCoordStore(n.engine.Backends().Fleet)
	for _, record := range records {
		var run struct {
			Status    string    `json:"status"`
			LaunchID  string    `json:"launch_id"`
			SandboxID string    `json:"sandbox_id"`
			CommandID string    `json:"command_id"`
			Question  string    `json:"question"`
			PausedAt  time.Time `json:"paused_at"`
		}
		if err := json.Unmarshal(record.Value, &run); err != nil {
			fmt.Fprintf(&b, "\n  turn=%s <undecodable: %v>", record.Key, err)
			continue
		}
		// THE CONVERSATION IS KEPT BESIDE THE RECORD, under its launch, so
		// whether one was written is asked of the store.
		conversation := "none"
		switch state, found, err := store.Conversation(context.Background(), record.Key, run.LaunchID); {
		case err != nil:
			conversation = "<unreadable: " + err.Error() + ">"
		case found:
			conversation = fmt.Sprintf("%d bytes", len(state))
		}
		fmt.Fprintf(&b, "\n  turn=%s status=%s launch=%s sandbox=%q command=%q "+
			"conversation=%s question=%q paused_at=%s",
			record.Key, run.Status, run.LaunchID, run.SandboxID, run.CommandID,
			conversation, run.Question, run.PausedAt)
	}
	return b.String()
}

// hasConversation reports whether run's launch has its suspended conversation
// kept beside its record — the fact a restart depends on.
func (n *codingNode) hasConversation(t *testing.T, run sandbox.PendingRun) bool {
	t.Helper()
	_, found, err := sandbox.NewCoordStore(n.engine.Backends().Fleet).Conversation(t.Context(),
		run.TurnID, run.LaunchID)
	if err != nil {
		t.Fatalf("Conversation(%s): %v", run.TurnID, err)
	}
	return found
}

// liveBoxesQuietly is [codingNode.liveBoxes] for a diagnostic, which must
// report what it finds rather than fail the test it is explaining.
func (n *codingNode) liveBoxesQuietly() []string {
	entries, err := os.ReadDir(filepath.Join(n.stateDir, "boxes"))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
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
	t.Parallel()
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
	// The suspended conversation is by far the largest thing a run keeps
	// (beside its row), and every prompt in it is already reachable through
	// the event store.
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
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		n.server.URL+"/query/sandbox_runs", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	res, err := n.server.Client().Do(present(req))
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
	t.Parallel()
	stateDir := t.TempDir()
	binDir := installFakeAgent(t)
	env := fakeAgentEnv(binDir, "held")
	// THE JOB RUNS UNTIL THIS FILE APPEARS. The restart has to happen while
	// it is still running, and how long a second engine takes to boot is a
	// property of the machine rather than of the design.
	release := filepath.Join(t.TempDir(), "release")
	env["FAKE_AGENT_RELEASE"] = release

	// ONE store and ONE stream directory across both processes, which is
	// what makes this a restart rather than a fresh company.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crewlet.db")
	streamDir := filepath.Join(dir, "stream")

	model := newSandboxModel(t)
	doc := sandboxCompany(t, model, stateDir)

	first := bootCompanyIn(t, doc, model, dbPath, streamDir, env, nil)
	firstNode := &codingNode{node: first, stateDir: stateDir, binDir: binDir}
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(first.engine.Node().Host().Held(), "swe")
	})
	first.wake(t, "swe", "the api test is flaking, please fix it")

	// The run is launched and its conversation persisted — the two facts
	// the restart depends on.
	waitFor(t, "the run to be recorded with its conversation", func() bool {
		for _, run := range firstNode.activeRuns(t) {
			if run.Status == sandbox.StatusRunning && firstNode.hasConversation(t, run) {
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

	second := bootCompanyIn(t, doc, model, dbPath, streamDir, env, nil)
	secondNode := &codingNode{node: second, stateDir: stateDir, binDir: binDir}
	waitFor(t, "the new engine to claim the seat", func() bool {
		return slices.Contains(second.engine.Node().Host().Held(), "swe")
	})
	// Recovery re-marked the seat busy from the row, so its mail is parked
	// rather than starting a second turn beside the running job.
	waitFor(t, "the recovered run to park the seat", func() bool {
		return second.engine.SeatHeldBySandbox("swe")
	})

	// The job may finish now: the restart happened while it was running,
	// which is the whole staging this case needed.
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("release the held agent: %v", err)
	}

	// And it drives the same run to completion, re-entering the
	// conversation the dead process suspended.
	waitFor(t, "the recovered run to resume its turn", func() bool {
		return slices.Contains(model.seen(), "resumed")
	})
	waitFor(t, "the recovered run to settle", func() bool {
		return len(secondNode.activeRuns(t)) == 0
	})
	if got := countOf(model.seen(), "execute"); got != 1 {
		t.Fatalf("the executor opened %d times across the restart; the resume must "+
			"re-enter. phases = %v", got, model.seen())
	}
	_ = launched
}

// THE CONTAINER LEG. Same protocol, real host isolation — and the mode whose
// in-box paths match a remote backend's, so a path a step or a brief names
// under the home reads the same in both. (A setup FILE at a system path is
// refused here exactly as in `direct`: a container box writes files only
// under its home mount.)
//
// Skipped where no container runtime is usable rather than dropped: it is the
// half of the gate a workstation cannot always run, and a suite that quietly
// tested only the easy mode would report a gate it had not met.
func TestTheContainerModeRunsTheSameProtocol(t *testing.T) {
	t.Parallel()
	runtime := usableContainerRuntime(t)
	if runtime == "" {
		t.Skip("no usable container runtime; the direct mode covers the protocol here")
	}
	image := os.Getenv("CREWLET_TEST_SANDBOX_IMAGE")
	if image == "" {
		// PINNED, like every image docker-compose.yml names. `alpine:3` is a
		// floating tag, and CLAUDE.md's rule against those is not about
		// tidiness: a green run under one is a claim about a build nobody can
		// name afterwards, and the day the tag moves this gate's subject
		// changes with no commit to point at. 3.24.1 is the version `3`
		// resolves to today (Docker Hub puts 3, 3.24, 3.24.1 and latest on one
		// digest); nothing bumps a literal in Go source, so a failure here that
		// names the image IS the bump signal.
		image = "alpine:3.24.1"
	}
	local, err := sandbox.NewLocal(sandbox.LocalOptions{
		Placement: sandbox.Container, StateDir: t.TempDir(),
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
	t.Parallel()
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
	stateDir := t.TempDir()
	binDir := installFakeAgent(t)

	model := newSandboxModel(t)
	doc := strings.Replace(
		sandboxCompany(t, model, stateDir),
		"default_pause_ttl_seconds: 1800",
		fmt.Sprintf("default_pause_ttl_seconds: %d", pauseTTL), 1)
	n := bootCompany(t, doc, model, fakeAgentEnv(binDir, mode))
	return &codingNode{node: n, stateDir: stateDir, binDir: binDir}
}
