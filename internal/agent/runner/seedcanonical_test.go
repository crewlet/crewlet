package runner_test

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// statusWriter writes the way the builtins do: its operation id is a digest
// of the arguments it was handed and its repeat count in the run.
type statusWriter struct {
	mu     sync.Mutex
	writes map[string]int
}

func (*statusWriter) Name() string        { return "set_status" }
func (*statusWriter) Description() string { return "sets a status" }
func (*statusWriter) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"item":   map[string]any{"type": "string"},
		"status": map[string]any{"type": "string", "enum": []any{"draft", "published"}},
	}}
}

func (w *statusWriter) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return w.CallForTurn(ctx, nil, args)
}

func (w *statusWriter) CallForTurn(_ context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	w.write(turnctx.ArgsDigest(args) + "#" + strconv.Itoa(turn.CallLog().Ordinal("set_status", args)))
	return tools.Result{Output: "applied"}, nil
}

func (w *statusWriter) write(op string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writes == nil {
		w.writes = map[string]int{}
	}
	w.writes[op]++
}

// draft is the call as the canonical form spells it, which the tool reads.
var draft = map[string]any{"item": "GP-12", "status": "draft"}

// firstWrite is the operation the run's first call wrote, before it parked:
// the canonical arguments, the run's first of its kind.
func firstWrite() string { return turnctx.ArgsDigest(draft) + "#0" }

// A RESUMED RUN'S REPEAT SPELT ANOTHER WAY IS THE SAME WRITE (#423 re-verify):
// the run's log is seeded from the calls it made before it parked, which hold
// the model's own spelling ("Draft"), and was seeded with that spelling, so the
// same call made after the resume ("draft") counted as a different one, took
// a fresh operation id and wrote twice. Seeded by the canonical arguments,
// the repeat is the first write's operation, for a parked round and for a
// box-mode run's bridged calls alike.
//
// Mutation: seed the log with the recorded arguments as written and both
// cases count the repeat as the run's second.
func TestAResumedRunsRepeatSpeltAnotherWayIsTheSameWrite(t *testing.T) {
	t.Parallel()
	register := func(w *statusWriter) func(*testing.T, *tools.Registry) {
		return func(t *testing.T, reg *tools.Registry) {
			if err := reg.RegisterWith(w, tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.No}); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("parked", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		state := suspendedAfterTwoRounds()
		state.ToolExecutions = append(state.ToolExecutions, types.ToolExecution{
			"name": "set_status", "arguments": `{"item": "GP-12", "status": "Draft"}`,
			"result": "applied", "success": true, "round": 1,
		})
		prov := &scriptedProvider{execute: []llmtest.Response{submitWork(t)}}
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
			calls: calls, register: register(&statusWriter{}),
			resume: &runner.Resume{State: state, Answer: "the run succeeded"},
		})
		if _, _, err := r.Resume(context.Background(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if n := calls.Ordinal("set_status", draft); n != 0 {
			t.Errorf("the repeat counts %d different calls before it, want 0: it is the parked call again", n)
		}
	})

	t.Run("bridged", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		w := &statusWriter{}
		w.write(firstWrite())
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
			buildOpts{agentRun: &recordingLauncher{}, calls: calls, register: register(w)})
		state := execstate.State{Version: execstate.Version, AgentRun: true, Round: 1,
			ActiveTools: []string{"set_status"}}
		surface, _, err := r.ReopenAgentRun(context.Background(), state, []runner.BridgedCall{
			{Call: ledger.Call{Name: "set_status",
				Args: map[string]any{"item": "GP-12", "status": "Draft"}, Result: "applied"}, Seq: 1},
		}, runner.Bridged{})
		if err != nil {
			t.Fatalf("ReopenAgentRun: %v", err)
		}
		res, err := surface.Execute(context.Background(), llm.ToolCall{Name: "set_status",
			Arguments: map[string]any{"item": "GP-12", "status": "draft"}})
		if err != nil || res.Failed {
			t.Fatalf("the repeat answered %+v (%v)", res, err)
		}
		if len(w.writes) != 1 {
			t.Errorf("the run wrote %d operations, want the one: %v", len(w.writes), w.writes)
		}
	})
}

// A TOOL THE SURFACE ADDS IS SEEDED BY ITS CANONICAL FORM TOO (#423 review):
// submit_work is no tool of the registry; the resumed surface adds it. Seeded
// through the registry's snapshot, a bridged submission spelt "Delivered"
// fell back to the spelling sent, and the same submission after the resume
// counted it as a different call. Seeded through the surface, it is one.
//
// Mutation: seed through the registry's snapshot and the repeat counts 1.
func TestASeededSubmissionIsReadByTheSurfacesUniverse(t *testing.T) {
	t.Parallel()
	calls := turnctx.NewCallLog()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{agentRun: &recordingLauncher{}, calls: calls})
	state := execstate.State{Version: execstate.Version, AgentRun: true, Round: 1}
	submission := func(outcome string) map[string]any {
		return map[string]any{"outcome": outcome, "summary": "posted the weekly summary",
			"evidence": "the post is in the channel"}
	}
	surface, _, err := r.ReopenAgentRun(context.Background(), state, []runner.BridgedCall{
		{Call: ledger.Call{Name: runner.SubmitWorkTool, Args: submission(" Delivered "),
			Result: "recorded"}, Seq: 1},
	}, runner.Bridged{})
	if err != nil {
		t.Fatalf("ReopenAgentRun: %v", err)
	}
	again := surface.ArgsOf(runner.SubmitWorkTool, submission("delivered")).Map()
	if again["outcome"] != "delivered" {
		t.Fatalf("the surface reads the submission as %v", again)
	}
	if n := calls.Ordinal(runner.SubmitWorkTool, again); n != 0 {
		t.Errorf("the repeated submission counts %d different calls before it, want 0", n)
	}
}
