package runner_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
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
		Turn:      runner.Turn{ID: "t-rounds", AgentID: "agent-1"},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
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
