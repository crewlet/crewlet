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
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
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
// the run's log is seeded from the calls it made before this segment, which
// the bridge records in the model's own spelling ("Draft"), and was seeded
// with that spelling, so the same call made after the resume ("draft")
// counted as a different one, took a fresh operation id and wrote twice.
// Seeded by the canonical arguments, through the surface each segment serves,
// the repeat is the first write's operation on all three paths an executor
// run's calls are read back on: its collection, its continuation after a
// person's answer, and its reopening on a node that took it over.
//
// Mutation: seed the log with the recorded arguments as written and every
// case counts the repeat as the run's second; drop any one segment's seed and
// its case finds the run's call missing from the log.
func TestAResumedRunsRepeatSpeltAnotherWayIsTheSameWrite(t *testing.T) {
	t.Parallel()
	register := func(w *statusWriter) func(*testing.T, *tools.Registry) {
		return func(t *testing.T, reg *tools.Registry) {
			if err := reg.RegisterWith(w, tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.No}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// bridged is the run's call before this segment, as the bridge's log
	// holds it: the model's own spelling.
	bridged := func() []runner.BridgedCall {
		return []runner.BridgedCall{{Call: ledger.Call{Name: "set_status",
			Args: map[string]any{"item": "GP-12", "status": "Draft"}, Result: "applied"}, Seq: 1}}
	}
	state := func() execstate.State {
		return execstate.State{Version: execstate.Version, Round: 1, ActiveTools: []string{"set_status"}}
	}
	// seeded fails unless the run's call is in the log at all: another
	// status counts it as the one different call before it.
	seeded := func(t *testing.T, calls *turnctx.CallLog) {
		t.Helper()
		if n := calls.Ordinal("set_status", map[string]any{"item": "GP-12", "status": "published"}); n != 1 {
			t.Errorf("another status counts %d different calls before it, want 1: the run's call", n)
		}
	}

	t.Run("collected", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
			calls: calls, register: register(&statusWriter{}),
			resume: &runner.Resume{State: state(), Answer: "the run succeeded", Bridged: bridged()},
		})
		if _, _, err := r.Resume(context.Background(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if n := calls.Ordinal("set_status", draft); n != 0 {
			t.Errorf("the repeat counts %d different calls before it, want 0: it is the run's call again", n)
		}
		seeded(t, calls)
	})

	t.Run("continued", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		w := &statusWriter{}
		w.write(firstWrite())
		relauncher := &playLauncher{}
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
			calls: calls, register: register(w), launcher: relauncher,
			resume: &runner.Resume{State: state(), Bridged: bridged(), Asked: &runner.Asked{
				Question: "which item?", Answer: "GP-12", SessionID: "s-1", BoxKept: true}},
		})
		if _, _, err := r.Resume(context.Background(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		seeded(t, calls)
		// THROUGH THE BRIDGE'S OWN PATH: the relaunched run's call is
		// admitted to its surface, as every bridged call is.
		answered := play(t, relauncher.last(t), run{calls: []llm.ToolCall{
			call(t, "set_status", `{"item":"GP-12","status":"draft"}`)}})
		if answered[0].Failed {
			t.Fatalf("the repeat answered %+v", answered[0])
		}
		if len(w.writes) != 1 {
			t.Errorf("the run wrote %d operations, want the one: %v", len(w.writes), w.writes)
		}
	})

	t.Run("reopened", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		w := &statusWriter{}
		w.write(firstWrite())
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
			buildOpts{calls: calls, register: register(w)})
		surface, _, err := r.ReopenAgentRun(context.Background(), state(), bridged(), runner.Bridged{})
		if err != nil {
			t.Fatalf("ReopenAgentRun: %v", err)
		}
		seeded(t, calls)
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
		buildOpts{calls: calls})
	state := execstate.State{Version: execstate.Version, Round: 1}
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
