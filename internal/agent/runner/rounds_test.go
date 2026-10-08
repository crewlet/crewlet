package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
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
//
// Takes the PROVIDER INTERFACE rather than the scripted fixture: a phase's own
// wall clock is only observable against a provider that takes time, and a
// second copy of this constructor for that one case is how the two come to
// disagree about the caps that make a phase extendable at all.
func extendableRunner(
	t *testing.T, prov llm.Provider, pub queue.Publisher, judge extension.Judge,
	meter ...toolloop.BudgetMeter,
) *runner.Runner {
	t.Helper()
	return cappedRunner(t, prov, pub, judge, runner.Caps{
		ExecutorRounds: 2, ExecutorCeiling: 8,
		ExtensionOn: true, ExtensionStep: 4,
	}, meter...)
}

// cappedRunner is extendableRunner under caps a case states itself — the
// extension switch off, or a ceiling equal to the budget.
func cappedRunner(
	t *testing.T, prov llm.Provider, pub queue.Publisher, judge extension.Judge,
	caps runner.Caps, meter ...toolloop.BudgetMeter,
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
		Caps:      caps,
		Task:      "read the files",
		Publisher: pub,
		Judge:     judge,
		Budget:    budgetOf(meter),
		Turn:      runner.Turn{RunID: "t-rounds", WorkKey: "wk-rounds", AgentID: "agent-1"},
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

// WHY A PARKED ROUND STOPPED SURVIVES THE RESUME — AND THE NEXT SUSPEND.
//
// The parked row carries each round's stop reason, and the resume rebuilds the
// loop's rounds from it field by field. A field left out of that rebuild is
// blank on the resumed record (which reads as "the backend reported none"),
// and because a second suspend re-encodes the rounds from the rebuilt ones,
// it is gone from every later record too. Both ends are asserted: the record
// the resume publishes, and the row a second suspension would persist.
func TestAParkedRoundsStopReasonSurvivesTheResume(t *testing.T) {
	t.Parallel()
	parked := func() execstate.State {
		state := suspendedAfterTwoRounds()
		state.Rounds = []types.PhaseRound{
			{Round: 1, Model: "earlier", StopReason: string(llm.StopToolUse)},
			{Round: 2, Model: "earlier", StopReason: string(llm.StopToolUse)},
		}
		// Through JSON, as the pending-run row hands it back.
		blob, err := execstate.Encode(state)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		out, _, err := execstate.Decode(blob)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		return out
	}
	wantParked := func(t *testing.T, rounds []types.PhaseRound) {
		t.Helper()
		if len(rounds) < 2 {
			t.Fatalf("rounds = %+v, want the two parked rounds first", rounds)
		}
		for _, r := range rounds[:2] {
			if r.StopReason != string(llm.StopToolUse) {
				t.Errorf("parked round %d stop_reason = %q, want %q",
					r.Round, r.StopReason, llm.StopToolUse)
			}
		}
	}

	t.Run("the resumed record", func(t *testing.T) {
		t.Parallel()
		prov := &scriptedProvider{execute: []llm.Completion{
			submitCall(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"shipped it","evidence":"the box did the work"}`),
		}}
		pub := newCapture()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
			pub:    pub,
			resume: &runner.Resume{State: parked(), Answer: "the run succeeded"},
		})
		if _, _, err := r.Resume(context.Background(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		wantParked(t, completedPhase(t, pub, "execute").Rounds)
	})

	t.Run("a second suspension", func(t *testing.T) {
		t.Parallel()
		state := parked()
		state.ActiveTools = append(state.ActiveTools, "run_sandbox")
		prov := &scriptedProvider{execute: []llm.Completion{
			submitCall(t, "run_sandbox", `{"task":"and the follow-up"}`),
		}}
		r, reg := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
			resume: &runner.Resume{State: state, Answer: "the run succeeded"},
		})
		if err := reg.RegisterWith(suspendingTool{}, tools.Origin("sandbox"), tools.Annotations{}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		w, _, err := r.Resume(context.Background(), nil)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if !w.Suspended {
			t.Fatal("the resumed phase did not suspend again, so there is no second row to read")
		}
		again, ok := r.Suspended()
		if !ok {
			t.Fatal("no second suspension was recorded")
		}
		wantParked(t, again.State.Rounds)
	})
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
	if got.TotalTokens != 30 || got.Model != "judge-model" || got.ProviderKey != "judge-entry" {
		t.Errorf("the judge's own spend is unreported: %d tokens on %q under entry %q",
			got.TotalTokens, got.Model, got.ProviderKey)
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
	if spend.Judged != 1 || spend.JudgeTokens() != 30 {
		t.Errorf("judge tally = %d calls / %d tokens, want 1/30", spend.Judged, spend.JudgeTokens())
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
		Asked: true, Model: "judge-model", ProviderKey: "judge-entry",
		InputTokens: 20, OutputTokens: 10,
	}, nil
}

// A JUDGEMENT THE BUDGET REFUSES ENDS THE EXTENSION, AND IS CHARGED ALL THE
// SAME.
//
// The judge runs once a phase is out of rounds and has answered by the time it
// is charged. A refusal there declines the extension — the outcome of the judge
// saying no, because a seat at its cap should stop extending rather than die —
// and the meter has still been handed the judgement, which the fleet's counter
// records like any round it refuses.
func TestARefusedJudgementDeclinesTheExtensionAndIsStillCharged(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		thinkAndCall(t, "read_file", `{"path":"/a"}`, "one"),
		thinkAndCall(t, "read_file", `{"path":"/b"}`, "two"),
		thinkAndCall(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"read both"}`, "enough"),
		text("done"),
	}}
	pub := newCapture()
	// Two 100-token rounds fit; the 30-token judgement after them does not.
	meter := &ceilingMeter{ceiling: 220}
	r := extendableRunner(t, prov, pub, spendingJudge{}, meter)

	// NOT A FAILED TURN: an over-budget judgement is a declined extension.
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	judged := phasesOfKind(t, pub, "judge")
	if len(judged) != 1 {
		t.Fatalf("%d judge phases published, want the one call that ran", len(judged))
	}
	if judged[0].Decision != "rescue" {
		t.Errorf("decision = %q, want rescue: the budget refused the judgement, so "+
			"the phase was not extended", judged[0].Decision)
	}
	if got := prov.n["execute"]; got != 2 {
		t.Errorf("the executor made %d model calls, want its 2 rounds and no extension", got)
	}
	if got := meter.charges(); !slices.Equal(got, []int{100, 100, 30}) {
		t.Errorf("charges = %v, want both rounds and the refused judgement's 30", got)
	}
}

// THE PHASE AFTER A REFUSED JUDGEMENT MAKES NO CALL.
//
// The judgement's refusal declines the extension and the phase ends rescued,
// but the refused judgement is counted, so the window reads past its ceiling
// and every later charge of the turn is refused. The reviewer that follows
// used to send its whole first round — the evidence log, the prompt, its
// tools — which the vendor billed and the meter then refused. The meter holds
// the refusal now, and the reviewer is refused before it calls.
func TestTheReviewAfterARefusedJudgementMakesNoCall(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		execute: []llm.Completion{
			thinkAndCall(t, "read_file", `{"path":"/a"}`, "one"),
			thinkAndCall(t, "read_file", `{"path":"/b"}`, "two"),
		},
		review: []llm.Completion{
			thinkAndCall(t, runner.SubmitReviewTool, `{"decision":"done","notes":"ok"}`, "judged"),
		},
	}
	pub := newCapture()
	meter := &ceilingMeter{ceiling: 220} // two 100-token rounds fit; the 30-token judgement does not
	r := extendableRunner(t, prov, pub, spendingJudge{}, meter)

	work, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	calls := len(prov.seen)
	_, err = r.Review(context.Background(), 1, work, nil)
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("Review = %v, want the refusal the judgement left standing", err)
	}
	if got := len(prov.seen) - calls; got != 0 {
		t.Errorf("the reviewer made %d model calls after the turn was refused, want none", got)
	}
	if got := meter.charges(); !slices.Equal(got, []int{100, 100, 30}) {
		t.Errorf("charges = %v, want both rounds and the refused judgement, and nothing after", got)
	}
}

// A JUDGE IS NOT CALLED ON A REFUSAL ALREADY CERTAIN. The judge is a model
// call like any other — billed, then charged — and once nothing more fits a
// window every charge after is refused. Here the phase's two rounds fill the
// ceiling exactly: both are admitted, nothing refused them, and the meter
// knows no later charge can fit. Asking the judge would have paid for a call
// whose only possible verdict was the refusal.
func TestTheJudgeIsNotCalledOnARefusalAlreadyCertain(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		thinkAndCall(t, "read_file", `{"path":"/a"}`, "one"),
		thinkAndCall(t, "read_file", `{"path":"/b"}`, "two"),
	}}
	pub := newCapture()
	meter := &ceilingMeter{ceiling: 200}
	r := extendableRunner(t, prov, pub, spendingJudge{}, meter)

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := phasesOfKind(t, pub, "judge"); len(got) != 0 {
		t.Errorf("%d judge phases published on a refusal the meter already held", len(got))
	}
	if got := meter.charges(); !slices.Equal(got, []int{100, 100}) {
		t.Errorf("charges = %v, want the two rounds and no judgement", got)
	}
}

// ceilingMeter records every charge it is handed and refuses one that does
// not fit its ceiling — recording it all the same, as the fleet's counter
// does, because the call it charges has already been made.
type ceilingMeter struct {
	countingMeter
	ceiling int
}

func (m *ceilingMeter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	m.mu.Lock()
	before := 0
	for _, c := range m.seen {
		before += c
	}
	m.mu.Unlock()
	if _, err := m.countingMeter.Spend(ctx, tokens); err != nil {
		return toolloop.SpendOutcome{}, err
	}
	if before+tokens > m.ceiling {
		return toolloop.SpendOutcome{Scope: "agent", Used: before + tokens, Limit: m.ceiling}, nil
	}
	return toolloop.SpendOutcome{OK: true}, nil
}

// Refused holds what the meter has seen, as the engine's meter does: once
// nothing more fits under the ceiling, every later charge is refused, and the
// meter says so before the next call is made.
func (m *ceilingMeter) Refused(context.Context) (toolloop.SpendOutcome, bool) {
	used := 0
	for _, c := range m.charges() {
		used += c
	}
	if used < m.ceiling {
		return toolloop.SpendOutcome{}, false
	}
	return toolloop.SpendOutcome{Scope: "agent", Used: used, Limit: m.ceiling}, true
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

// Refused never holds a refusal: nothing caps this meter.
func (m *countingMeter) Refused(context.Context) (toolloop.SpendOutcome, bool) {
	return toolloop.SpendOutcome{}, false
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
// The tool loop's corrective re-prompt was once gated on the caller requiring
// a tool call, and no caller did — so it was unreachable and the package doc's
// claim that a forced tool call is ENFORCED held for no phase the engine ran. A
// reviewer that answered with prose fell straight through to the rescue, which
// sends the whole turn back for another executor round: a whole extra turn
// spent on the one failure a model reliably fixes when it is simply asked
// again. The reviewer's loop finishes by `submit_review`, so the corrective it
// gets now is the FINISHING one, naming that tool.
func TestAReviewerThatAnswersWithProseIsRePromptedRatherThanRescued(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{review: []llm.Completion{
		// Round 1: thinks, calls nothing. Nothing forces a call and
		// some models think-then-stop; this is that round.
		text("The work looks fine to me."),
		submitCall(t, runner.SubmitReviewTool,
			`{"decision":"done","notes":"the delivery matches the ask"}`),
	}}
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}},
		buildOpts{reply: turn.NoReply(), pub: pub})

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
		Turn:      runner.Turn{RunID: "t-frames", WorkKey: "wk-t-frames", AgentID: "agent-1"},
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

// A RESUMED RUN COUNTS THE CALLS IT MADE BEFORE IT PARKED.
//
// A resume is the same run in a fresh process with a fresh call log, and a
// write it makes derives its operation id's repeat count from that log. Left
// empty, a call made again after a different one before the suspend counted
// nothing and took its first copy's id — answered as that copy's retry, with
// the different write's value in place. So the resume seeds the log with the
// rounds that closed, the parked round's own calls and, for an agent-mode run,
// what it called over the bridge.
func TestAResumedRunCountsTheCallsItMadeBeforeItParked(t *testing.T) {
	t.Parallel()
	first := map[string]any{"summary": "first"}
	closed := []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{
		{Name: "jira_create", Args: first},
	}}}
	parked := suspendedAfterTwoRounds()
	parked.ToolExecutions = append(parked.ToolExecutions, types.ToolExecution{
		"name": "jira_create", "arguments": `{"summary":"parked"}`, "success": true, "round": 2,
	})

	t.Run("a native run", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{
			execute: []llm.Completion{thinkAndCall(t, runner.SubmitWorkTool,
				`{"outcome":"delivered","summary":"done"}`, "finish"), text("done")},
		}}}, buildOpts{
			resume: &runner.Resume{State: parked, Answer: "the run succeeded"},
			calls:  calls,
		})
		if _, _, err := r.Resume(context.Background(), closed); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if got := calls.Ordinal("jira_create", first); got != 1 {
			t.Fatalf("after the resume the first create counts %d different "+
				"creates before it, want the parked round's 1 — made again, it "+
				"would take the first copy's id", got)
		}
		if got := calls.Ordinal("jira_create", map[string]any{"summary": "new"}); got != 2 {
			t.Fatalf("a new create counts %d, want both the closed round's and "+
				"the parked round's", got)
		}
	})

	t.Run("an agent-mode run", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
			buildOpts{agentRun: &recordingLauncher{}, calls: calls, resume: &runner.Resume{
				State:  execstate.State{Version: execstate.Version, AgentRun: true, Round: 1},
				Answer: "done",
				Bridged: []ledger.Call{
					{Name: "jira_create", Args: map[string]any{"summary": "bridged"}},
				},
			}})
		if _, _, err := r.Resume(context.Background(), closed); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if got := calls.Ordinal("jira_create", first); got != 1 {
			t.Fatalf("after the resume the first create counts %d, want the "+
				"bridged call's 1", got)
		}
	})
}

// A RESUMED RUN MAY CITE A DELIVERY A CLOSED ROUND MADE, on both runtimes.
//
// The citation check reads the TURN's record ([turn.Record]): the rounds
// that closed before the suspend, then the phase's own calls. A resume that
// read only the phase refused the one citation that was true, because the
// prior-work ledger had told the round not to make that delivery again.
func TestAResumedRunMayCiteADeliveryAClosedRoundMade(t *testing.T) {
	t.Parallel()
	tracker := func(t *testing.T, reg *tools.Registry) {
		t.Helper()
		if err := reg.RegisterWith(stubTool{name: "comment_on_work_item", out: "commented"},
			tools.OriginBuiltin, tools.Annotations{}, tools.DeliversTo("work")); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	const citation = `{"outcome":"delivered","summary":"collected the run",` +
		`"deliveries":["comment_on_work_item"]}`
	closed := []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{
		{Name: "comment_on_work_item", Result: "commented"},
	}}}

	native := func(t *testing.T, history []ledger.Iteration) turn.Work {
		t.Helper()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{
			execute: []llm.Completion{submitCall(t, runner.SubmitWorkTool, citation)},
		}}}, buildOpts{
			reply: turn.ToolReply("work"), register: tracker,
			resume: &runner.Resume{State: suspendedAfterTwoRounds(), Answer: "the run succeeded"},
		})
		w, _, err := r.Resume(context.Background(), history)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		return w
	}
	agent := func(t *testing.T, history []ledger.Iteration) turn.Work {
		t.Helper()
		var args map[string]any
		if err := json.Unmarshal([]byte(citation), &args); err != nil {
			t.Fatal(err)
		}
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
			buildOpts{reply: turn.ToolReply("work"), register: tracker,
				agentRun: &recordingLauncher{}, resume: &runner.Resume{
					State:   execstate.State{Version: execstate.Version, AgentRun: true, Round: 2},
					Answer:  "done",
					Bridged: []ledger.Call{{Name: runner.SubmitWorkTool, Args: args}},
				}})
		w, _, err := r.Resume(context.Background(), history)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		return w
	}

	for name, resume := range map[string]func(*testing.T, []ledger.Iteration) turn.Work{
		"a native run": native, "an agent-mode run": agent,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if w := resume(t, closed); w.Rescued || w.Outcome != turn.OutcomeDelivered {
				t.Errorf("outcome = %s (rescued %v), want the citation of the closed "+
					"round's comment accepted", w.Outcome, w.Rescued)
			}
			// The counterfactual: with no closed round behind it the same
			// citation names a comment nobody made, and is refused.
			if w := resume(t, nil); !w.Rescued {
				t.Errorf("outcome = %s, want a citation of a comment nobody made refused",
					w.Outcome)
			}
		})
	}
}
