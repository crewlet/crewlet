package runner_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/tools"
)

// ONE ROUND NUMBER PER PHASE, ON EVERY EVENT THE PHASE PUBLISHES.
//
// A round's narration and the calls that round asked for are joined on the
// number they share — that shared number is the whole contract a consumer
// interleaves them on, and the dashboard's ledger keys one block per number.
// The tool loop, though, counts from 1 each time it is ENTERED, and an
// extended phase enters it again. Every publisher therefore has to fold the
// invocation onto the rounds behind it, and the three that do it used to
// disagree:
//
//   - the live frame carried the invocation alone, so an extension's first
//     frame collapsed a twenty-round ledger to a single round;
//   - the round in flight was never renumbered at all, so its streaming text
//     was written into the block of a committed round twenty rounds earlier —
//     that round's thinking replaced, its tool calls still underneath;
//   - the completed record took the invocation's token counters, so every
//     round before the extension was billed and dropped from the report.
//
// The assertions below are over the PUBLISHED events rather than the runner's
// return value, because the wire is where every one of those defects lived and
// no suite looked at it.
func TestEveryRoundAPhasePublishesIsOnThePhasesOwnScale(t *testing.T) {
	t.Parallel()
	// Three rounds of work against a two-round cap, so the phase exhausts
	// its budget once, is granted an extension, and re-enters the loop.
	prov := &scriptedProvider{execute: []llm.Completion{
		thinkAndCall(t, "read_file", `{"path":"/a"}`, "start with the file"),
		thinkAndCall(t, "read_file", `{"path":"/b"}`, "and the other one"),
		thinkAndCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"read both"}`, "that is enough"),
		text("done"),
	}}
	pub := newCapture()
	r := extendableRunner(t, prov, pub, alwaysExtend{})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	done := completedPhase(t, pub, "execute")
	if done.RoundsUsed != 3 {
		t.Fatalf("the record claims %d rounds for a phase that ran 3", done.RoundsUsed)
	}
	// Contiguous, 1..RoundsUsed, no repeats — across the extension boundary.
	seen := map[int]bool{}
	for _, n := range done.RoundNarration {
		round, _ := n["round"].(int)
		if round < 1 || round > done.RoundsUsed {
			t.Errorf("narration numbered round %d on a phase that ran %d",
				round, done.RoundsUsed)
		}
		if seen[round] {
			t.Errorf("round %d narrated twice — the extension restarted the count", round)
		}
		seen[round] = true
	}
	if len(seen) != done.RoundsUsed {
		t.Errorf("rounds narrated = %v, want one entry per round of %d",
			seen, done.RoundsUsed)
	}
	// Every call belongs to a round the narration knows. A round that called
	// a tool and said nothing narrates nothing, but this script has the model
	// thinking in every round, so a call landing outside `seen` means the two
	// lists were folded on different scales.
	for _, ex := range done.ToolExecutions {
		round, _ := ex["round"].(int)
		if !seen[round] {
			t.Errorf("%v ran in round %d, which no narration entry names", ex["name"], round)
		}
	}
	// The tokens are the PHASE's. The loop counts from zero each time it is
	// entered, so a record built from the last invocation reports only the
	// rounds after the extension — understating exactly the long phases an
	// operator opens the page to investigate.
	if done.TotalTokens != 3*(60+40) {
		t.Errorf("total_tokens = %d, want every round the provider served (300)",
			done.TotalTokens)
	}
}

// A LIVE FRAME DESCRIBES THE PHASE, NOT THE INVOCATION.
//
// The live view rebuilds a call from each frame rather than merging, so a
// frame carrying only the extension's own rounds erases everything above it —
// and "nothing above an insertion point moves" is the one property the round
// ledger exists to guarantee.
func TestALiveFrameNeverDropsTheRoundsBehindIt(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		thinkAndCall(t, "read_file", `{"path":"/a"}`, "start with the file"),
		thinkAndCall(t, "read_file", `{"path":"/b"}`, "and the other one"),
		thinkAndCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"read both"}`, "that is enough"),
		text("done"),
	}}
	pub := newCapture()
	r := extendableRunner(t, prov, pub, alwaysExtend{})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The highest round any frame reported, and the frame count at that
	// point. Rounds only append, so neither may ever go backwards.
	highest, narrated := 0, 0
	for _, frame := range progressFrames(t, pub, "execute") {
		for _, n := range frame.RoundNarration {
			if round, _ := n["round"].(int); round > highest {
				highest = round
			}
		}
		if got := len(frame.RoundNarration); got < narrated {
			t.Fatalf("a frame reported %d narrated rounds after one reported %d — "+
				"the extension's frames dropped the rounds before it", got, narrated)
		} else if got > narrated {
			narrated = got
		}
		// The round being written is never a round already committed: one
		// block cannot be both a finished round and an arriving one.
		if frame.PartialRound == nil {
			continue
		}
		round, _ := frame.PartialRound["round"].(int)
		for _, n := range frame.RoundNarration {
			if committed, _ := n["round"].(int); committed == round {
				t.Fatalf("the in-flight round %d is also published as committed", round)
			}
		}
	}
	if highest != 3 {
		t.Errorf("the live frames topped out at round %d, want the phase's 3", highest)
	}
}

// alwaysExtend grants every request, so the phase re-enters the tool loop.
type alwaysExtend struct{}

func (alwaysExtend) Decide(context.Context, extension.Request) (extension.Decision, error) {
	return extension.Decision{Extend: true, Reason: "still making progress"}, nil
}

// thinkAndCall is a round in which the model reasons and then asks for a tool
// — the shape whose two halves have to end up in one block.
func thinkAndCall(t *testing.T, name, argsJSON, reasoning string) llm.Completion {
	t.Helper()
	call := submitCall(t, name, argsJSON)
	call.ReasoningContent = reasoning
	call.InputTokens, call.OutputTokens = 60, 40
	return call
}

// extendableRunner is a seat whose executor exhausts a two-round cap and can
// be granted more.
func extendableRunner(
	t *testing.T, prov *scriptedProvider, pub queue.Publisher, judge extension.Judge,
	meter ...toolloop.BudgetMeter,
) *runner.Runner {
	t.Helper()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: prov}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg := tools.NewRegistry()
	if err := reg.Register(stubTool{name: "read_file", out: "contents"},
		tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Registry: reg, Models: models,
		Caps: runner.Caps{
			ExecutorRounds: 2, ExecutorCeiling: 8,
			ExtensionOn: true, ExtensionStep: 4,
		},
		Task:      "read the files",
		Publisher: pub,
		Judge:     judge,
		Budget:    budgetOf(meter),
		Turn:      runner.Turn{ID: "t-rounds", AgentID: "agent-1"},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

// budgetOf is the shared meter a fixture was given, or none.
func budgetOf(meter []toolloop.BudgetMeter) toolloop.BudgetMeter {
	if len(meter) == 0 {
		return nil
	}
	return meter[0]
}

// completedPhase is the durable record one phase published.
func completedPhase(t *testing.T, c *capture, ph string) *types.AgentPhaseCompleted {
	t.Helper()
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	for _, ev := range c.events {
		got, ok := events.DataAs[*types.AgentPhaseCompleted](ev)
		if ok && string(got.Phase) == ph {
			return got
		}
	}
	t.Fatalf("no %s phase was published", ph)
	return nil
}

// progressFrames is every live frame one phase published, in order.
func progressFrames(t *testing.T, c *capture, ph string) []*types.AgentTurnProgress {
	t.Helper()
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var out []*types.AgentTurnProgress
	for _, ev := range c.events {
		got, ok := events.DataAs[*types.AgentTurnProgress](ev)
		if ok && string(got.Phase) == ph && got.RoundNum >= 0 {
			out = append(out, got)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s frame was published", ph)
	}
	return out
}

// A RESUMED PHASE IS THE SAME PHASE, AND ITS RECORD COVERS ALL OF IT.
//
// A suspending Execute returns before `emit.completed` runs, so it publishes
// no durable record at all, and its progress frames are stream-only. The
// resumed half is therefore the only account this phase will ever have — and
// it used to start at round 1, so the pre-suspend rounds, the `run_sandbox`
// call that caused the suspension included, were gone from the store for good,
// under token counters that covered both halves.
func TestAResumedPhasePublishesTheWholePhase(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		thinkAndCall(t, "slack_post", `{"text":"the fix is up"}`, "tell the requester"),
		thinkAndCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"shipped it","deliveries":["slack_post"]}`,
			"the box did the work"),
		text("done"),
	}}
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		pub:    pub,
		resume: &runner.Resume{State: suspendedAfterTwoRounds(), Answer: "the run succeeded"},
	})

	if _, _, err := r.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	done := completedPhase(t, pub, "execute")
	if done.RoundsUsed != 4 {
		t.Errorf("RoundsUsed = %d, want the phase's own 4 (two before the suspend, two after)",
			done.RoundsUsed)
	}
	names := map[string]int{}
	for _, ex := range done.ToolExecutions {
		name, _ := ex["name"].(string)
		round, _ := ex["round"].(int)
		names[name] = round
	}
	// The call that CAUSED the suspension. If this is missing, the only
	// durable evidence that the phase started a coding run is gone.
	if names["run_sandbox"] != 2 {
		t.Errorf("run_sandbox is recorded in round %d, want 2: %v", names["run_sandbox"], names)
	}
	if names["search_knowledge"] != 1 {
		t.Errorf("the pre-suspend read is in round %d, want 1: %v",
			names["search_knowledge"], names)
	}
	// And the resumed round CONTINUES rather than restarting.
	if names["slack_post"] != 3 || names[runner.SubmitWorkTool] != 4 {
		t.Errorf("the resumed rounds are numbered %d and %d, want 3 and 4: %v",
			names["slack_post"], names[runner.SubmitWorkTool], names)
	}
	rounds := map[int]bool{}
	for _, n := range done.RoundNarration {
		round, _ := n["round"].(int)
		if rounds[round] {
			t.Errorf("round %d is narrated twice — the resume restarted the count", round)
		}
		rounds[round] = true
	}
	if len(rounds) != 4 {
		t.Errorf("narrated rounds = %v, want one per round of 4", rounds)
	}
	// The counters were already the phase's total; they must not double now
	// that the rounds they cover are on the record too.
	if done.TotalTokens != 500+2*100 {
		t.Errorf("total_tokens = %d, want 700 (500 before the suspend, 200 after)",
			done.TotalTokens)
	}
}

// suspendedAfterTwoRounds is a phase parked on run_sandbox, two rounds in.
func suspendedAfterTwoRounds() execstate.State {
	return execstate.State{
		Version: execstate.Version,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "you are the CTO"},
			{Role: llm.RoleUser, Content: "post the weekly summary"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "run_sandbox", Arguments: map[string]any{"task": "fix it"}},
			}},
		},
		PendingCallID:   "call-1",
		PendingCallName: "run_sandbox",
		// The activations the pre-suspend rounds made, replayed — which is
		// the reason the field exists.
		ActiveTools:  []string{"slack_post"},
		Round:        1,
		RoundsUsed:   2,
		InputTokens:  400,
		OutputTokens: 100,
		ToolExecutions: []types.ToolExecution{
			{"name": "search_knowledge", "arguments": "{}", "result": "2 pages", "success": true, "round": 1},
			{"name": "run_sandbox", "arguments": "{}", "result": "started", "success": true, "round": 2},
		},
		RoundNarration: []types.RoundNarration{
			{"round": 1, "reasoning": "what do we already know", "content": ""},
			{"round": 2, "reasoning": "this needs code", "content": "Starting a coding run."},
		},
	}
}

// THE JUDGE IS A MODEL CALL, AND A MODEL CALL LEAVES A RECORD.
//
// It was the only one in the engine that left none: no phase event, so no card
// under the round that fired it and no row in the token breakdown; and no
// charge, because it runs outside the tool loop where every other call is
// metered. `types.PhaseJudge` was declared, read by the dashboard's grouping,
// and produced by nobody — so a company whose judge rescues every phase looked
// exactly like one whose phases deserved no extension.
func TestTheExtensionJudgeIsPublishedAsAPhaseAndCharged(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		thinkAndCall(t, "read_file", `{"path":"/a"}`, "one"),
		thinkAndCall(t, "read_file", `{"path":"/b"}`, "two"),
		thinkAndCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"read both"}`, "enough"),
		text("done"),
	}}
	pub := newCapture()
	meter := &countingMeter{}
	r := extendableRunner(t, prov, pub, spendingJudge{}, meter)

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	judged := phasesOfKind(t, pub, "judge")
	if len(judged) != 1 {
		t.Fatalf("%d judge phases published, want the one call that ran", len(judged))
	}
	got := judged[0]
	// NESTED under the phase that asked, which is what puts the card under
	// the Execute round rather than beside the turn's own phases.
	if got.HostPhase != "execute" || got.HostIteration != 1 {
		t.Errorf("host = %q/%d, want execute/1", got.HostPhase, got.HostIteration)
	}
	if got.Decision != "extend" {
		t.Errorf("decision = %q, want the verdict", got.Decision)
	}
	if got.Notes == "" {
		t.Error("the judge's reason is missing, which is what makes a rescue readable")
	}
	if got.TotalTokens != 30 || got.Model != "judge-model" {
		t.Errorf("the judge's own spend is unreported: %d tokens on %q",
			got.TotalTokens, got.Model)
	}
	// AND CHARGED. The judge runs outside the tool loop, so nothing else
	// meters it: a seat's reported cost was below what it actually cost by
	// one model call per exhausted phase.
	if !meter.sawAtLeast(30) {
		t.Errorf("the judge's tokens never reached the shared meter: charges = %v",
			meter.charges())
	}
	// The turn's own totals must NOT double-count it: its spend went through
	// the meter, so folding it in would stop the phase events summing to the
	// turn's number.
	spend := r.Spend()
	if spend.Judged != 1 || spend.JudgeTokens != 30 {
		t.Errorf("judge tally = %d calls / %d tokens, want 1/30", spend.Judged, spend.JudgeTokens)
	}
}

// A POLICY THAT DECLINES TO ASK IS NOT A JUDGEMENT. Nothing was called, so
// there is nothing to report or to charge — and an event would claim a model
// call that never happened, which is the fact the judge phase exists to carry.
func TestNoJudgePhaseIsPublishedWhenNoJudgeWasAsked(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		thinkAndCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"done in one"}`, "straight in"),
		text("done"),
	}}
	pub := newCapture()
	r := extendableRunner(t, prov, pub, spendingJudge{}, &countingMeter{})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := phasesOfKind(t, pub, "judge"); len(got) != 0 {
		t.Errorf("%d judge phases on a phase that never ran out of rounds", len(got))
	}
}

// spendingJudge grants every request and reports what the call cost, which is
// the half nothing carried.
type spendingJudge struct{}

func (spendingJudge) Decide(context.Context, extension.Request) (extension.Decision, error) {
	return extension.Decision{
		Extend: true, Reason: "each call advances on the last",
		Asked: true, Model: "judge-model", InputTokens: 20, OutputTokens: 10,
	}, nil
}

// countingMeter records every charge the shared budget saw.
type countingMeter struct {
	mu   sync.Mutex
	seen []int
}

func (m *countingMeter) Spend(_ context.Context, tokens int) (toolloop.SpendOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, tokens)
	return toolloop.SpendOutcome{OK: true}, nil
}

func (m *countingMeter) charges() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.seen...)
}

func (m *countingMeter) sawAtLeast(tokens int) bool {
	for _, got := range m.charges() {
		if got == tokens {
			return true
		}
	}
	return false
}

// phasesOfKind is every phase of one kind the turn published.
func phasesOfKind(t *testing.T, c *capture, ph string) []*types.AgentPhaseCompleted {
	t.Helper()
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var out []*types.AgentPhaseCompleted
	for _, ev := range c.events {
		if got, ok := events.DataAs[*types.AgentPhaseCompleted](ev); ok && string(got.Phase) == ph {
			out = append(out, got)
		}
	}
	return out
}

// A REVIEWER THAT THINKS AND STOPS IS ASKED AGAIN, not rescued.
//
// The tool loop's corrective re-prompt is gated on the caller requiring a tool
// call, and no caller did — so `maxForcedToolRetries` and
// `forcedToolCorrective` were unreachable and the package doc's claim that a
// forced tool call is ENFORCED held for no phase the engine runs. A reviewer
// that answered with prose fell straight through to the rescue, which sends
// the whole turn back for another executor round: a whole extra turn spent on
// the one failure a model reliably fixes when it is simply asked again.
func TestAReviewerThatAnswersWithProseIsRePromptedRatherThanRescued(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{review: []llm.Completion{
		// Round 1: thinks, calls nothing. Some endpoints ignore tool_choice
		// and some models think-then-stop; this is that round.
		text("The work looks fine to me."),
		submitCall(t, runner.SubmitReviewTool,
			`{"decision":"done","notes":"the delivery matches the ask"}`),
	}}
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}},
		buildOpts{reply: turn.ReplyNone, pub: pub})

	got, err := r.Review(context.Background(), 1, turn.Work{
		Outcome: turn.OutcomeDelivered, Summary: "posted it",
	}, nil)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got.Decision != phase.Done {
		t.Fatalf("decision = %q, want the one the second round submitted", got.Decision)
	}
	done := completedPhase(t, pub, "review")
	if done.RescueFired {
		t.Error("the reviewer was rescued, so the turn goes back for another executor round")
	}
	// The corrective is a round of the REVIEW phase, not a new turn.
	if done.RoundsUsed != 2 {
		t.Errorf("rounds_used = %d, want the prose round plus the corrected one", done.RoundsUsed)
	}
	// And the model was told what to do, by name. A bare "no" sends it round
	// the same loop.
	var corrective string
	for _, msg := range prov.requestsFor("review")[1].Messages {
		if strings.Contains(msg.Content, runner.SubmitReviewTool) && msg.Role == llm.RoleUser {
			corrective = msg.Content
		}
	}
	if corrective == "" {
		t.Error("the reviewer was re-asked without being told which tool to call")
	}
}

// A LIVE FRAME IS BOUNDED; THE DURABLE RECORD IS VERBATIM.
//
// The whole frame is republished five times a second for the length of the
// phase, and only the round in flight was bounded. The system prompt rode
// every one of them unchanged, and so did every tool result — routinely the
// largest thing on the frame, and already final. Past the queue's ceiling the
// publish is refused, this publisher logs and moves on, and the live row stops
// for the rest of the phase with nothing on screen to say why.
func TestALiveFrameCarriesNeitherThePromptNorWholeToolResults(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("x", 20_000)
	prov := &scriptedProvider{execute: []llm.Completion{
		submitCall(t, "read_file", `{"path":"/big"}`),
		submitCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"read it"}`),
		text("done"),
	}}
	pub := newCapture()
	r := bigResultRunner(t, prov, pub, huge)

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, frame := range progressFrames(t, pub, "execute") {
		// The prompt is sent ONCE, on the opening frame, and carried by the
		// projection from there.
		if frame.Prompt != "" || len(frame.PromptMessages) != 0 {
			t.Errorf("round %d re-sent the prompt", frame.RoundNum)
		}
		for _, ex := range frame.ToolExecutions {
			if result, _ := ex["result"].(string); len(result) > 5_000 {
				t.Errorf("round %d shipped a %d-character tool result",
					frame.RoundNum, len(result))
			}
		}
	}
	// The opening frame still carries it — that is its whole job.
	opening := openingFrame(t, pub, "execute")
	if opening.Prompt == "" || len(opening.PromptMessages) == 0 {
		t.Error("the opening frame carries no prompt, so the live row never gets one")
	}
	// AND THE DURABLE RECORD KEEPS THE RESULT WHOLE. A reader opens the
	// finished card to read what a tool actually returned.
	done := completedPhase(t, pub, "execute")
	var stored string
	for _, ex := range done.ToolExecutions {
		if name, _ := ex["name"].(string); name == "read_file" {
			stored, _ = ex["result"].(string)
		}
	}
	if len(stored) != len(huge) {
		t.Errorf("the stored result is %d characters, want the whole %d", len(stored), len(huge))
	}
}

// bigResultRunner is a seat whose one tool returns more than a frame should
// carry.
func bigResultRunner(
	t *testing.T, prov *scriptedProvider, pub queue.Publisher, out string,
) *runner.Runner {
	t.Helper()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: prov}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg := tools.NewRegistry()
	if err := reg.Register(stubTool{name: "read_file", out: out}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Registry: reg, Models: models,
		Caps:      runner.Caps{ExecutorRounds: 4},
		Task:      "read the big file",
		Publisher: pub,
		Turn:      runner.Turn{ID: "t-frames", AgentID: "agent-1"},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

// openingFrame is the update a phase publishes before its first provider call.
func openingFrame(t *testing.T, c *capture, ph string) *types.AgentTurnProgress {
	t.Helper()
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	for _, ev := range c.events {
		got, ok := events.DataAs[*types.AgentTurnProgress](ev)
		if ok && string(got.Phase) == ph && got.RoundNum < 0 {
			return got
		}
	}
	t.Fatalf("no opening frame for %s", ph)
	return nil
}
