package runner_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// AN EXECUTOR RUN TAKEN OVER WHILE IT WORKS IS SERVED THE SURFACE IT LEFT. Its
// box goes on calling the seat's tools after the process that launched it is
// gone, and the node that takes it over rebuilds that surface from the run's
// own state: every tool its launch recorded it holding — which is every tool
// it can reach, so nothing the run did over the bridge adds one — and its place
// in the run: every call it made counts toward a later call's repeat count, a
// submission may cite them, and its meter starts from what its row says its
// bridged calls have cost.
//
// Mutation: build the surface without the launch's record, and jira_create is
// refused as one it does not hold; seed nothing, and a call made after the
// recovery counts as the run's first of its kind; leave the run's calls out of
// the submission's record, and a delivery the run made before the recovery
// cannot be cited; start the meter from zero, and the row's total stays the
// larger.
func TestARunTakenOverIsServedTheSurfaceItLeft(t *testing.T) {
	t.Parallel()
	calls := turnctx.NewCallLog()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{calls: calls})
	state := execstate.State{
		Version: execstate.Version, Round: 1,
		ActiveTools: []string{"jira_create", "lookup_colleague", "reflect", "slack_history", "slack_post"},
	}
	spent := runner.Bridged{Aux: auxspend.Spent{Input: 900, Output: 90}, Workers: 1,
		WorkerInput: 4000, WorkerOutput: 400}
	surface, meter, err := r.ReopenAgentRun(context.Background(), state, []runner.BridgedCall{
		// AS THE ROW HOLDS THEM: in the order they finished.
		{Call: ledger.Call{Name: "jira_create", Args: map[string]any{"summary": "first"},
			Result: "created"}, Seq: 2},
		{Call: ledger.Call{Name: "slack_post", Args: map[string]any{"channel": "C1"},
			Result: "posted"}, Seq: 1},
	}, spent)
	if err != nil {
		t.Fatalf("ReopenAgentRun: %v", err)
	}

	active := surface.Active()
	for _, held := range state.ActiveTools {
		if !slices.Contains(active, held) {
			t.Errorf("active = %v, want every tool the launch recorded, %s among them", active, held)
		}
	}
	if n := calls.Ordinal("jira_create", map[string]any{"summary": "second"}); n != 1 {
		t.Errorf("a create after the recovery counts %d different creates before it, want "+
			"the one the run made before it", n)
	}
	if got := meter.Total(); got != spent {
		t.Errorf("the meter starts at %+v, want the row's %+v", got, spent)
	}

	res, err := surface.Execute(context.Background(), llm.ToolCall{
		Name: runner.SubmitWorkTool, Arguments: map[string]any{
			"outcome": "delivered", "summary": "posted the weekly summary",
			"deliveries": []any{"slack_post"},
		},
	})
	if err != nil {
		t.Fatalf("submit_work: %v", err)
	}
	if res.Failed {
		t.Errorf("a submission citing a delivery the run made before the recovery was "+
			"refused: %s", res.Output)
	}
}

// AND A SKILL THE RUN LOADED STAYS LOADED. A required skill's tools are refused
// until the skill is loaded, and a box that loaded it before its node went away
// has its body in its own transcript: a rebuilt guard that did not know would
// refuse the tools the run had unlocked, and tell its model to load what it can
// already see.
//
// Mutation: drop the replay of the run's calls into the guard, and slack_post
// is refused for a skill the run loaded.
func TestARunTakenOverKeepsTheSkillsItLoaded(t *testing.T) {
	t.Parallel()
	r, _ := guardedRunner(t, newCapture(), tools.OriginBuiltin)
	surface, _, err := r.ReopenAgentRun(context.Background(), execstate.State{
		Version: execstate.Version, Round: 1,
		ActiveTools: []string{"slack_post", toolname.LoadToolSkill},
	}, []runner.BridgedCall{
		{Call: ledger.Call{Name: toolname.LoadToolSkill, Args: map[string]any{"key": "chat-conventions"},
			Result: "Always thread your reply."}, Seq: 1},
	}, runner.Bridged{})
	if err != nil {
		t.Fatalf("ReopenAgentRun: %v", err)
	}
	res, err := surface.Execute(context.Background(), llm.ToolCall{
		Name: "slack_post", Arguments: map[string]any{"channel": "C1"},
	})
	if err != nil {
		t.Fatalf("slack_post: %v", err)
	}
	if res.Failed {
		t.Errorf("a tool the run's loaded skill unlocked was refused: %s", res.Output)
	}
}
