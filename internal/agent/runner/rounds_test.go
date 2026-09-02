package runner_test

import (
	"context"
	"testing"

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
