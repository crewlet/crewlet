package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
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
// resume. Every agent seat's executor is its harness's own run, so this is
// the path every turn takes; what this gate adds is the box's whole life seen
// from outside — its argv, its stream, its park on a question, the reaper, and
// a restart in the middle of a run.
//
// The harness is the stand-in of codingcli_test.go: this binary, installed
// as `claude` on the box's PATH by the catalogue's setup step, asking the same
// scripted model the seat's phases on this host do. That makes the whole detached
// protocol real — a process group, a background job, a done marker, a result
// line, an ask signal, the bridge — while leaving what the agent DOES up to
// the test, through role.sandbox.env ([codingModeEnv]).

// sandboxCompanyDoc is the golden coding company, filled by [sandboxCompany]:
// one agent seat whose executor runs in a `direct` box (see [codingSandbox] for
// why direct), on a pause TTL a case may shorten.
const sandboxCompanyDoc = `
name: Nimbus
providers:
%s    default_pause_ttl_seconds: 1800
  llm:
%s%sroles:
  - name: SWE
    handle: swe
    llm: [scripted, coder]
    sandbox:
      # The seat's own round cap. What is under test is that a number
      # written HERE reaches the harness's argv, which is the whole of
      # "wired".
      max_turns: 7
      env:
        FAKE_AGENT_MODE: "${FAKE_AGENT_MODE}"
        FAKE_AGENT_ARGV: "${FAKE_AGENT_ARGV}"
        FAKE_AGENT_RELEASE: "${FAKE_AGENT_RELEASE}"
        FAKE_AGENT_VERSION: "${FAKE_AGENT_VERSION}"
      # Only the cases that read the argv, hold the run or run an older
      # release set these three. Declared optional, a case that sets none runs
      # without them; undeclared, every such run starts without what it never
      # needed and logs the gap.
      optional_keys: [FAKE_AGENT_ARGV, FAKE_AGENT_RELEASE, FAKE_AGENT_VERSION]
  - name: Founder
    kind: human
    contact:
      slack_user_id: U0FOUNDER
turn_engine:
  max_iterations: 1
`

// sandboxCompany is the coding company's document: its phases on this host on model's
// scripted harness, and its executor's boxes under stateDir, each running the
// stand-in that asks model.
func sandboxCompany(t *testing.T, model *scriptedModel, stateDir string) string {
	t.Helper()
	return fmt.Sprintf(sandboxCompanyDoc, codingSandbox(stateDir, model.shim),
		model.entry("scripted"), model.coder("coder"))
}

// codingNode is a running node whose seat can run code.
type codingNode struct {
	*node
	stateDir string
	argvPath string
}

// agentArgv is what the stand-in harness was actually invoked with.
func (n *codingNode) agentArgv(t *testing.T) []string {
	t.Helper()
	return n.invocation(t).Argv
}

// agentBrief is the brief the stand-in harness read on its stdin — the
// prompt it ran.
func (n *codingNode) agentBrief(t *testing.T) string {
	t.Helper()
	return n.invocation(t).Stdin
}

// invocation is how the stand-in harness last recorded it was started.
func (n *codingNode) invocation(t *testing.T) codingInvocation {
	t.Helper()
	raw, err := os.ReadFile(n.argvPath)
	if err != nil {
		t.Fatalf("the harness recorded no invocation: %v", err)
	}
	var out codingInvocation
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the harness's invocation does not read: %v\n%s", err, raw)
	}
	return out
}

// startCoding stands up a node with a local sandbox whose runs are in mode.
func startCoding(t *testing.T, mode string) *codingNode {
	t.Helper()
	stateDir := t.TempDir()
	// Outside the box directory, so it survives the teardown that ends the
	// turn under test.
	argv := filepath.Join(stateDir, "argv.txt")
	// Both reach the box through role.sandbox.env, which is the ONLY way an
	// external value gets there: the engine names no tool-specific variable
	// of its own. That the run honours its mode is therefore also the proof
	// that ${VAR} references in that block resolve.
	env := map[string]string{codingModeEnv: mode, codingArgvEnv: argv}

	model := newScriptedModel(t)
	doc := sandboxCompany(t, model, stateDir)
	n := bootCompany(t, doc, model, env)
	return &codingNode{node: n, stateDir: stateDir, argvPath: argv}
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

// bootCompanyIn is [bootCompany] over the store and stream directories given.
//
// host is what the node keeps of one it replaces on the same host — see
// [sameHost] — and nil for a node with none: a port of the system's choosing
// and a keyring of its own.
func bootCompanyIn(t *testing.T, doc string, model *scriptedModel, dbPath, streamDir string,
	env map[string]string, host *sameHost) *node {
	t.Helper()
	logs.attribute(t)
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	boot := config.DefaultBootstrap()
	withServingTierA(t, &boot)
	boot.Store.Path = dbPath
	brokertest.StoreIn(&boot.Stream, streamDir)
	addr := ""
	if host != nil {
		host.ring.hold(&boot)
		addr = host.addr
	}
	seedStore(t, &boot)

	ln := bridgeListener(t, addr)
	// THE API IS SERVED ON ln, and api.port names it, as on a node that
	// cmd/crewlet starts: an ingress node whose api.port is 0 serves
	// nothing and joins no relay (internal/engine's joinBridgeFleet), so its
	// bridge is reachable only once the route below is mounted — and a node
	// that recovers a running run's seat before then has no bridge to serve
	// the run's box from again.
	boot.API.Port = ln.Addr().(*net.TCPAddr).Port
	boot.API.ExternalURL = listenerURL(ln)
	e, chatLive := bootEngine(t, engine.Options{
		Bootstrap: &boot, Company: cfg, ActivatedAt: harnessActivation,
		Environment: nodeEnvironment(env), SandboxPollInterval: runPollInterval,
		BoxHostURL: listenerURL(ln),
	})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}

	app, srv := serveAPI(t, e, &boot, chatLive.Hub(), ln, func(opts *api.Options) {
		// The DURABLE record, which is what the board must read: a run
		// parked on a question waits days, and the live projection sweeps
		// long before that.
		opts.Sources.Sandbox = sandbox.NewCoordStore(e.Backends().Fleet)
	})
	return &node{engine: e, app: app, server: srv, model: model, chat: chatLive}
}

// sameHost is what a node restarted on its host keeps of the one before it:
// the address its API listened on — the URL a run's box was handed names it —
// and its Tier A keyring, which signed that URL's token. Both are the
// operator's file, and a restart changes neither; a harness that changed
// either would be testing a redeploy, in which a box still running cannot
// reach its tools by design.
type sameHost struct {
	addr string
	ring keyring
}

// ---------------------------------------------------------------------
// the gate
// ---------------------------------------------------------------------

// A coding turn end to end: decide, launch, suspend, poll, collect, resume, and
// the same turn finishing with the agent's findings in hand.
func TestAGoldenCodingTurnSuspendsAndResumes(t *testing.T) {
	t.Parallel()
	n := startCoding(t, codingModeWork)
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
	// The RUN does the executor's work in its box: its round reaches the
	// scripted model from inside the box, and the submission it makes lands
	// over the seat's bridge.
	waitFor(t, "the run to do its work", func() bool {
		return slices.Contains(n.model.seen(), "execute")
	}, n.diagnose)

	// The RESUME: the waiter detects the completion and the engine re-enters
	// the same turn, whose reviewer judges the run on the record it left.
	waitFor(t, "the suspended turn to be resumed", func() bool {
		return slices.Contains(n.model.seen(), "review")
	}, n.diagnose)
	waitFor(t, "the run to settle", func() bool {
		return len(n.activeRuns(t)) == 0
	})

	// ONE turn, not two, and one run of it: a second executor round would
	// mean the engine launched again — a fresh turn re-deriving work already
	// done, or a resume that relaunched instead of collecting.
	seen := n.model.seen()
	if got := countOf(seen, "execute"); got != 1 {
		t.Fatalf("the executor's run asked the model %d times; one run, one round. "+
			"phases = %v", got, seen)
	}
	if got := countOf(seen, "review"); got != 1 {
		t.Fatalf("the turn was reviewed %d times; a resume re-enters the turn it "+
			"parked. phases = %v", got, seen)
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
			if rec["phase"] == "execute" && collectedARun(rec) {
				launch, _ = rec["launch_id"].(string)
				return true
			}
		}
		return false
	})
	if launch == "" {
		t.Error("the resumed execute phase names no launch_id")
	}
	// THE RUN'S STREAM REACHED ITS RECORD: the transcript the runner built
	// from the stand-in's stream-json messages — its bridged submission
	// among them — and the spend its result line, the stream's last, copied
	// out by the wrapper, reported: Claude Code's input is its uncached
	// remainder, and the record's is the whole prompt, both cache figures
	// folded in. End to end because the halves are the harness's flags, the
	// box's shell and the record's publisher, and each compiles without the
	// others. Read through the phase list, which answers a launch's record
	// beside the phase records of the round that launched it.
	var record map[string]any
	waitFor(t, "the launch's own record", func() bool {
		rows, _, err := n.engine.Backends().Store.Events().Phases(t.Context(), "", 60, nil, time.Now())
		if err != nil {
			t.Fatalf("phases: %v", err)
		}
		for _, row := range rows {
			var rec map[string]any
			if row.Type == "executor_collected" && json.Unmarshal(row.Payload, &rec) == nil {
				record = rec
				return true
			}
		}
		return false
	})
	submitted := "[tool] mcp__" + config.ToolServerName + "__" + runner.SubmitWorkTool
	if transcript, _ := record["activity_transcript"].(string); !strings.Contains(transcript, submitted) {
		t.Errorf("the run's transcript is %q; want the bridged call its stream made, %q",
			transcript, submitted)
	}
	wantInput := toolUseUsage.prompt()
	if input, _ := record["input_tokens"].(float64); input != float64(wantInput) {
		t.Errorf("the run's input tokens are %v; want the %d its result line reported",
			record["input_tokens"], wantInput)
	}
	// The box is gone: the turn settled, and a settled turn's box is
	// reclaimed with it.
	if boxes := n.liveBoxes(t); len(boxes) != 0 {
		t.Fatalf("%d box directories survived the turn: %v", len(boxes), boxes)
	}
	// THE SEAT'S ROUND CAP REACHED THE HARNESS'S ARGV. This is the
	// whole of "wired": role.sandbox.max_turns, through the config, the
	// spec overlay, the launch and the runner, to the flag the harness reads.
	// The field existed and rendered long before anything filled it in, so
	// only an end-to-end assertion tells a wired knob from a decorative one.
	argv := n.agentArgv(t)
	i := slices.Index(argv, "--max-turns")
	if i < 0 || i == len(argv)-1 {
		t.Fatalf("the harness was invoked uncapped: %v", argv)
	}
	if argv[i+1] != "7" {
		t.Fatalf("--max-turns %s, want the seat's own 7", argv[i+1])
	}
	// AND THE MODEL OF THE ENTRY THE LAUNCH PICKED, which is the half of a
	// run's pairing the seat's phases on this host cannot show: those run on the
	// chain's head, a different entry altogether, so a run handed their
	// model — or none, on the harness's own default — would look the same from
	// everywhere else.
	m := slices.Index(argv, "--model")
	if m < 0 || m == len(argv)-1 || argv[m+1] != codingModel {
		t.Fatalf("the harness was not handed the coder entry's model %s: %v", codingModel, argv)
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
	n := startCoding(t, codingModeAsk)
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
	if parked.Question != codingQuestion {
		t.Fatalf("question = %q", parked.Question)
	}
	if parked.Audience != "requester" || parked.Branch != "wip/flake" {
		t.Fatalf("audience = %q, branch = %q", parked.Audience, parked.Branch)
	}
	// THE QUESTION IS ASKED AS A DECISION: its options as options, the one
	// recommended, and its context apart from the title, which is what the
	// Inbox draws as a choice a person presses rather than prose.
	if d := parked.Decision; d == nil || len(d.Options) != len(codingOptions) ||
		d.Recommended != "main" || d.Question != codingQuestion ||
		!strings.Contains(parked.QuestionContext, "**api**") {
		t.Fatalf("the parked question is %+v with context %q, want the run's decision",
			parked.Decision, parked.QuestionContext)
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
		return slices.Contains(n.model.seen(), "review")
	}, n.diagnose)
	waitFor(t, "the answered run to settle", func() bool {
		return len(n.activeRuns(t)) == 0
	}, n.diagnose)

	// ONE TURN, not two. This is the assertion the whole path turns on: the
	// answer re-entered the parked turn rather than starting a fresh one,
	// which is what a seat that treated the reply as an ordinary message
	// would have done — launching the work again with the parked run still
	// waiting beside it. A fresh turn's run would ask again and park again,
	// so the run would never settle; and the parked turn is reviewed once.
	if got := countOf(n.model.seen(), "review"); got != 1 {
		t.Fatalf("the turn was reviewed %d times; the answer started a new turn "+
			"instead of resuming the one that asked. phases = %v",
			got, n.model.seen())
	}
	// THE ANSWER CARRIED THE RUN ON: the run that finished the work is the
	// asking run's own conversation, resumed in its box with the person's
	// words as its next prompt — the last argv the box recorded.
	argv := n.agentArgv(t)
	if !slices.Contains(argv, "--resume") || !slices.Contains(argv, codingSession) {
		t.Fatalf("the answered run was not resumed in its own session: argv = %v", argv)
	}
	if brief := n.agentBrief(t); !strings.Contains(brief, "target main, please") {
		t.Fatalf("the resumed run was not given the answer: its prompt = %q", brief)
	}
	// AND IT SUBMITTED: the turn settled on the carried-on run's own
	// submission, never on the answer filed as a report and rescued.
	if outcome := n.settledOutcome(t, parked.TurnID); outcome != "delivered" {
		t.Fatalf("the answered turn settled with outcome %q, want the run's own "+
			"`delivered` submission", outcome)
	}
}

// A RUN'S STRUCTURED QUESTION IS ANSWERED WITH A PRESS AND CARRIES THE RUN ON
// (ADR-0068). The run asks through `ask_and_wait` over its bridge, with its
// options as options; the board the Inbox reads carries the question as a
// decision; a person answers by choosing one, through the route the Inbox's
// option button presses (`answer_executor` with `choice`); and the run resumes in
// its own session told the option it was given, in its own words and by its
// id, then submits its work.
//
// Mutation: deliver an answer's words without its choice, and the resumed run
// is told nothing it can act on.
func TestARunsStructuredQuestionIsAnsweredWithAChoiceAndResumes(t *testing.T) {
	t.Parallel()
	n := startCoding(t, codingModeAsk)
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	n.wakeInConversation(t, "swe", "the api test is flaking, please fix it", codingConversation)
	waitFor(t, "the run to park on its question", func() bool {
		for _, run := range n.activeRuns(t) {
			if run.Status == sandbox.StatusAwaiting && run.Decision != nil {
				return true
			}
		}
		return false
	}, n.diagnose)
	parked := n.runFor(t, sandbox.StatusAwaiting)

	// THE INBOX'S SOURCE says the question as a decision.
	var row map[string]any
	for _, r := range n.board(t) {
		if r["turn_id"] == parked.TurnID {
			row = r
		}
	}
	d, _ := row["decision"].(map[string]any)
	if row["question"] != codingQuestion || d == nil || d["recommended"] != "main" ||
		len(d["options"].([]any)) != len(codingOptions) {
		t.Fatalf("the board carries %v, want the question with its decision", row)
	}

	status, raw := bearer(t, n.node, http.MethodPost, "/operator/act/"+builtin.AnswerExecutorTool,
		map[string]any{"args": map[string]any{"turn_id": parked.TurnID,
			"launch_id": parked.LaunchID, "choice": "main"}},
		map[string]string{opkey.Header: uuid.Must(uuid.NewV7()).String()})
	if status != http.StatusOK {
		t.Fatalf("POST /operator/act/%s answered %d: %s", builtin.AnswerExecutorTool, status, raw)
	}
	waitFor(t, "the answered run to settle", func() bool {
		return len(n.activeRuns(t)) == 0
	}, n.diagnose)
	if argv := n.agentArgv(t); !slices.Contains(argv, "--resume") {
		t.Fatalf("the answered run was not resumed in its own session: argv = %v", argv)
	}
	if brief := n.agentBrief(t); !strings.Contains(brief, "Chosen option: Target main (`main`).") {
		t.Fatalf("the resumed run was not told the option chosen: its prompt = %q", brief)
	}
	if outcome := n.settledOutcome(t, parked.TurnID); outcome != "delivered" {
		t.Fatalf("the answered turn settled with outcome %q, want the run's own "+
			"`delivered` submission", outcome)
	}
}

// settledOutcome is the outcome the record that ENDED turnID states — not one a
// segment published as it parked — waited for.
func (n *codingNode) settledOutcome(t *testing.T, turnID string) string {
	t.Helper()
	var outcome string
	waitFor(t, "the settled turn to state its outcome", func() bool {
		evs, err := n.engine.Backends().Store.Events().Turn(t.Context(), turnID, time.Now())
		if err != nil {
			t.Fatalf("the turn's events: %v", err)
		}
		for _, ev := range evs {
			var rec struct {
				Outcome   string `json:"outcome"`
				Suspended bool   `json:"suspended"`
			}
			if ev.Type == "turn_completed" && json.Unmarshal(ev.Payload, &rec) == nil &&
				!rec.Suspended {
				outcome = rec.Outcome
				return true
			}
		}
		return false
	})
	return outcome
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
	for _, record := range records {
		var run struct {
			Status       string         `json:"status"`
			SandboxID    string         `json:"sandbox_id"`
			CommandID    string         `json:"command_id"`
			Question     string         `json:"question"`
			PausedAt     time.Time      `json:"paused_at"`
			ExecuteState map[string]any `json:"execute_state"`
		}
		if err := json.Unmarshal(record.Value, &run); err != nil {
			fmt.Fprintf(&b, "\n  turn=%s <undecodable: %v>", record.Key, err)
			continue
		}
		fmt.Fprintf(&b, "\n  turn=%s status=%s sandbox=%q command=%q "+
			"execute_state_keys=%d question=%q paused_at=%s",
			record.Key, run.Status, run.SandboxID, run.CommandID,
			len(run.ExecuteState), run.Question, run.PausedAt)
	}
	return b.String()
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

// collectedARun reports whether a phase record names the run it collected —
// the harness that did the work or the box it ran in — which only the executor's
// record that collected one does.
func collectedARun(rec map[string]any) bool {
	agent, _ := rec["coding_agent"].(string)
	box, _ := rec["sandbox_id"].(string)
	return agent != "" || box != ""
}

// The board an operator looks at when a run needs somebody.
//
// The DURABLE record, over the same query channel every other read uses: a run
// parked on a question waits days, and the live projection sweeps long before
// that — so the states that most need a person were the ones least likely to
// be on screen.
func TestAParkedRunReachesTheBoardAnOperatorReads(t *testing.T) {
	t.Parallel()
	n := startCoding(t, codingModeAsk)
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
	if parked["question"] != codingQuestion {
		t.Fatalf("question = %v", parked["question"])
	}
	if parked["agent_handle"] != "swe" || parked["coding_agent"] != "claude-code" {
		t.Fatalf("row = %v", parked)
	}
	if parked["box_exists"] != true || parked["paused_at"] == "" {
		t.Fatalf("the held snapshot is invisible, so nobody can see what is being paid for: %v", parked)
	}
	// The parked turn's state is by far the largest column in the row — the
	// context the turn froze at its start among it — and every prompt built
	// from it is already reachable through the event store.
	if _, leaked := parked["execute_state"]; leaked {
		t.Fatal("execute_state reached a board that renders one line per run")
	}
}

// board reads the executors answer over the query surface, exactly as the
// dashboard does.
func (n *codingNode) board(t *testing.T) []map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		n.server.URL+"/query/executors", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	res, err := n.server.Client().Do(present(req))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /query/executors = %d", res.StatusCode)
	}
	var payload struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return payload.Runs
}

// THE RESTART LEG of the gate. An executor outlives the engine that started
// it, so the state that matters is the row: a fresh process picks the run up
// on the seat's next claim, drives it to completion, and re-enters the SAME
// turn the dead process parked.
//
// AND THE RUN KEEPS ITS TOOLS. The box goes on dialing the URL it was handed,
// on the node's own address, and the process now listening there never opened
// its session: it rebuilds it from the run's park under the token the box
// holds, and the run's submission — made after the restart, as a real harness's
// last call is — lands on the row and decides the turn's outcome. Refused, the
// run that did its work was rescued as `incomplete`.
func TestAnEngineRestartMidRunStillFinishesTheSameTurn(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	// THE JOB RUNS UNTIL THIS FILE APPEARS. The restart has to happen while
	// it is still running, and how long a second engine takes to boot is a
	// property of the machine rather than of the design.
	release := filepath.Join(t.TempDir(), "release")
	env := map[string]string{codingModeEnv: codingModeHeld, codingReleaseEnv: release}

	// ONE store and ONE stream directory across both processes, which is
	// what makes this a restart rather than a fresh company.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crewlet.db")
	streamDir := filepath.Join(dir, "stream")

	model := newScriptedModel(t)
	doc := sandboxCompany(t, model, stateDir)

	host := &sameHost{ring: fleetRing(t)}
	first := bootCompanyIn(t, doc, model, dbPath, streamDir, env, host)
	firstNode := &codingNode{node: first, stateDir: stateDir}
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
	// whole reason the row exists. ITS CHAT HUB GOES FIRST, as the process
	// stops it: the engine's teardown closes the broker the hub answers on,
	// and a hub left running would be answering over a stopped engine for
	// the rest of the case.
	first.chat.Stop(context.Background())
	first.engine.Stop(context.Background())
	// AND ITS LISTENER WITH IT, so the next process binds the same address:
	// the run's box was handed a URL on it, as a node restarted on its own
	// api.port is dialled at the address it had.
	host.addr = first.server.Listener.Addr().String()
	first.server.Close()

	second := bootCompanyIn(t, doc, model, dbPath, streamDir, env, host)
	secondNode := &codingNode{node: second, stateDir: stateDir}
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

	// And it drives the same run to completion, re-entering the turn the
	// dead process parked: the run it collects is the one that process
	// launched, and the turn is reviewed once.
	waitFor(t, "the recovered run to resume its turn", func() bool {
		return slices.Contains(model.seen(), "review")
	}, secondNode.diagnose)
	waitFor(t, "the recovered run to settle", func() bool {
		return len(secondNode.activeRuns(t)) == 0
	})
	if got := countOf(model.seen(), "review"); got != 1 {
		t.Fatalf("the turn was reviewed %d times across the restart; the resume must "+
			"re-enter the one turn. phases = %v", got, model.seen())
	}
	var collected string
	waitFor(t, "the resumed phase's record to name the launch it collected", func() bool {
		rows, _, err := second.engine.Backends().Store.Events().Phases(t.Context(), "", 60, nil, time.Now())
		if err != nil {
			t.Fatalf("phases: %v", err)
		}
		for _, row := range rows {
			var rec map[string]any
			if json.Unmarshal(row.Payload, &rec) == nil && rec["phase"] == "execute" &&
				collectedARun(rec) {
				collected, _ = rec["launch_id"].(string)
				return true
			}
		}
		return false
	})
	if collected != launched.LaunchID {
		t.Errorf("the resumed turn collected launch %q, want %q — the one the dead "+
			"process started", collected, launched.LaunchID)
	}
	// THE SUBMISSION THE RUN MADE AFTER THE RESTART decided the turn: its
	// outcome, not the engine's `incomplete` rescue of a run that said
	// nothing.
	if outcome := secondNode.settledOutcome(t, launched.TurnID); outcome != codingHeldOutcome {
		t.Errorf("the turn settled with outcome %q, want the %q the run submitted over "+
			"its bridge after the restart", outcome, codingHeldOutcome)
	}
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
	runner := executor.NewClaudeCode()
	if err := runner.Install(t.Context(), box); err != nil {
		t.Fatalf("Install: %v", err)
	}
	p := executor.PathsFor(box)

	// A file written from the host side of the mount is visible inside.
	if err := box.WriteFile(t.Context(), p.WorkDir()+"/probe", []byte("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	res, err := box.Exec(t.Context(), "cat "+p.WorkDir()+"/probe", sandbox.ExecOptions{})
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("Exec = %+v, %v", res, err)
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
	n := startCodingWithPause(t, codingModeAsk, 1)
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	// ON A CONVERSATION, which is what a person's answer is matched on.
	n.wakeInConversation(t, "swe", "the api test is flaking, please fix it", codingConversation)

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

	// THE ANSWER STILL CARRIES THE RUN ON — fresh, since the session it
	// asked from went with its box: a new conversation, in a new box, told
	// the question it asked and the person's answer, which then does the
	// work and submits it.
	n.wakeInConversation(t, "swe", "target main, please", codingConversation)
	waitFor(t, "the answered run to settle", func() bool {
		return len(n.activeRuns(t)) == 0
	}, n.diagnose)
	argv := strings.Join(n.agentArgv(t), "\n")
	if strings.Contains(argv, "--resume") {
		t.Fatalf("a run whose box was reclaimed resumed a session that went with it: %s", argv)
	}
	brief := n.agentBrief(t)
	for _, want := range []string{codingQuestion, "target main, please"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("the fresh run was not told %q: its prompt = %q", want, brief)
		}
	}
	if outcome := n.settledOutcome(t, reseeded.TurnID); outcome != "delivered" {
		t.Fatalf("the answered turn settled with outcome %q, want the fresh run's own "+
			"`delivered` submission", outcome)
	}
}

// startCodingWithPause stands a node up whose paused boxes expire after
// pauseTTL seconds, so a test can watch the reaper without waiting half an
// hour for the production value.
func startCodingWithPause(t *testing.T, mode string, pauseTTL int) *codingNode {
	t.Helper()
	stateDir := t.TempDir()

	model := newScriptedModel(t)
	doc := strings.Replace(
		sandboxCompany(t, model, stateDir),
		"default_pause_ttl_seconds: 1800",
		fmt.Sprintf("default_pause_ttl_seconds: %d", pauseTTL), 1)
	argv := filepath.Join(stateDir, "argv.txt")
	n := bootCompany(t, doc, model, map[string]string{codingModeEnv: mode, codingArgvEnv: argv})
	return &codingNode{node: n, stateDir: stateDir, argvPath: argv}
}
