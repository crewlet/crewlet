package runner_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// rewritingTool is a seat-scoped tool that asks the auxiliary model for a
// rewrite, as refresh_memory's filter and query_episodes' compaction do, and
// adds its cost to the tally of the turn it is called as.
type rewritingTool struct{ cost auxspend.Spent }

func (rewritingTool) Name() string               { return "recall_notes" }
func (rewritingTool) Description() string        { return "Recall this seat's notes." }
func (rewritingTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (rewritingTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "no turn", Failed: true}, nil
}

func (r rewritingTool) CallForTurn(_ context.Context, turn *turnctx.Turn, _ map[string]any) (tools.Result, error) {
	turn.Aux().Tally.Add(r.cost)
	return tools.Result{Output: "three notes"}, nil
}

// AN EXECUTOR RUN'S BRIDGED CALLS ARE COUNTED ON THEIR OWN METER.
//
// The harness calls the seat's tools over the bridge after the launching
// segment has parked and charged its spend, so anything counted on that
// segment's tallies is never read and the work item never pays it. A tool's
// rewrite and a delegated worker land on the request's own meter instead, which
// the engine writes to the run's row for the resuming segment to pay.
//
// Mutations: build the bridged surface on the runner's own meter, bind its
// tools to the segment's turn, or close its workers on the segment's tally, and
// the meter comes up short while the segment's figures move.
func TestAnAgentRunsBridgedCallsAreCountedOnTheirOwnMeter(t *testing.T) {
	t.Parallel()
	launcher := &playLauncher{}
	cost := auxspend.Spent{Calls: 1, Input: 420, Output: 35, CacheRead: 100}
	reg := tools.NewRegistry()
	if err := reg.Register(rewritingTool{cost: cost}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: workerProvider{}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	segment := auxspend.NewTally()
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Registry: reg, Models: models,
		Task:     "fan this out",
		Launcher: launcher,
		Subagent: &runner.SubagentConfig{Limits: shipped()},
		Turn: runner.Turn{RunID: "t-bridge", AgentID: "agent-1",
			Context: &turnctx.Turn{RunID: "t-bridge", WorkKey: "wk-bridge", AuxSpend: segment}},
		Sessions: sessions,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil || !w.Suspended {
		t.Fatalf("Execute = %+v, %v; want a parked run", w, err)
	}
	req := launcher.last(t)
	if req.Spend == nil {
		t.Fatal("the run was launched with no meter for its bridged calls")
	}

	// THE BRIDGE'S CALLS, made after the segment parked.
	if !req.Surface.Activate("recall_notes") {
		t.Fatal("the bridged surface does not resolve the seat's tool")
	}
	ctx := context.Background()
	for _, call := range []llm.ToolCall{
		{Name: "recall_notes", Arguments: map[string]any{}},
		{Name: subagent.ToolName, Arguments: map[string]any{"tasks": []any{map[string]any{
			"id": "research", "prompt": "look", "system_prompt": "you research things"}}}},
	} {
		res, err := req.Surface.Execute(ctx, call)
		if err != nil || res.Failed {
			t.Fatalf("bridged %s = %+v, %v", call.Name, res, err)
		}
	}

	want := runner.Bridged{Aux: cost, Workers: 1, WorkerInput: 30, WorkerOutput: 5}
	if got := req.Spend.Total(); got != want {
		t.Fatalf("the run's meter holds %+v, want the bridged rewrite and worker %+v", got, want)
	}
	if got := segment.Total(); got != (auxspend.Spent{}) {
		t.Errorf("the launching segment's tally moved to %+v after it had charged", got)
	}
	if got := r.Spend(); got.Workers != 0 || got.WorkerTokens() != 0 {
		t.Errorf("the launching segment's tally counts %d workers / %d tokens it never paid for",
			got.Workers, got.WorkerTokens())
	}
	// STILL THIS TURN'S CALLS: only the tally they are counted on differs.
	if turn := req.Surface.Turn(); turn == nil || turn.RunID != "t-bridge" || turn.WorkKey != "wk-bridge" {
		t.Errorf("the bridged surface acts as %+v, want the launching turn", turn)
	}
}
