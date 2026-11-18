package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// AN EXECUTOR RUN TAKEN OVER WHILE IT WORKS IS SERVED THE SURFACE IT LEFT. The
// taking node rebuilds it from the run's state: every tool the launch recorded
// (all it can reach, so no bridged call adds one), its calls counted toward a
// later repeat and citable by a submission, and its meter from its row's
// bridged cost.
//
// Mutation: drop the launch's record (jira_create refused), seed nothing (a
// later call counts as the first of its kind), leave the run's calls out of
// the submission's record (an earlier delivery cannot be cited), or start the
// meter at zero (the row's total stays larger).
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
	reopened, err := r.ReopenAgentRun(context.Background(), state, []runner.BridgedCall{
		// In the order they finished, as the log holds them.
		{Call: ledger.Call{Name: "jira_create", Args: map[string]any{"summary": "first"},
			Result: "created"}, Seq: 2},
		{Call: ledger.Call{Name: "slack_post", Args: map[string]any{"channel": "C1"},
			Result: "posted"}, Seq: 1},
	}, nil, nil, nil, spent, false)
	if err != nil {
		t.Fatalf("ReopenAgentRun: %v", err)
	}
	surface, meter := reopened.Surface, reopened.Meter

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

// A RUN TAKEN OVER IS REFUSED A NO_ACTION ITS RUN ACTED BEFORE. The taking
// node's submission tool reads the calls made before the takeover, so a run
// that asked a colleague is refused "nobody was asking" there as on its
// launching node.
//
// Mutation: the reopen's submission back to decodeWork, and the no_action is
// accepted.
func TestARunTakenOverIsRefusedANoActionItsRunActedBefore(t *testing.T) {
	t.Parallel()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{reply: turn.NoReply(), register: asksColleagues, calls: turnctx.NewCallLog()})
	state := execstate.State{
		Version: execstate.Version, Round: 1,
		ActiveTools: []string{toolname.A2AAsk, "lookup_colleague", "reflect", "slack_history", "slack_post"},
	}
	reopened, err := r.ReopenAgentRun(context.Background(), state, []runner.BridgedCall{
		{Call: ledger.Call{Name: toolname.A2AAsk, Args: map[string]any{"to": "cto"}, Result: "asked"}, Seq: 1},
	}, nil, nil, nil, runner.Bridged{}, false)
	if err != nil {
		t.Fatalf("ReopenAgentRun: %v", err)
	}
	res, err := reopened.Surface.Execute(context.Background(), llm.ToolCall{
		Name: runner.SubmitWorkTool, Arguments: map[string]any{"outcome": "no_action", "summary": "not mine"},
	})
	if err != nil {
		t.Fatalf("submit_work: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Output, `called "a2a_ask"`) {
		t.Errorf("failed = %v, output = %q; want the no_action refused, naming the ask the run "+
			"made before the takeover", res.Failed, res.Output)
	}
}

// AND A SKILL THE RUN LOADED STAYS LOADED: its body is in the box's transcript,
// and a rebuilt guard that forgot would refuse the tools it unlocked.
//
// Mutation: drop the replay of the run's calls into the guard, and slack_post
// is refused for a skill the run loaded.
func TestARunTakenOverKeepsTheSkillsItLoaded(t *testing.T) {
	t.Parallel()
	r, _ := guardedRunner(t, newCapture(), tools.OriginBuiltin)
	reopened, err := r.ReopenAgentRun(context.Background(), execstate.State{
		Version: execstate.Version, Round: 1,
		ActiveTools: []string{"slack_post", toolname.LoadToolSkill},
	}, []runner.BridgedCall{
		{Call: ledger.Call{Name: toolname.LoadToolSkill, Args: map[string]any{"key": "chat-conventions"},
			Result: "Always thread your reply."}, Seq: 1},
	}, nil, nil, nil, runner.Bridged{}, false)
	if err != nil {
		t.Fatalf("ReopenAgentRun: %v", err)
	}
	res, err := reopened.Surface.Execute(context.Background(), llm.ToolCall{
		Name: "slack_post", Arguments: map[string]any{"channel": "C1"},
	})
	if err != nil {
		t.Fatalf("slack_post: %v", err)
	}
	if res.Failed {
		t.Errorf("a tool the run's loaded skill unlocked was refused: %s", res.Output)
	}
}
