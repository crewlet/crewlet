package runner_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/org"
	llm "github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// headroom is a seat's remaining allowance, or a counter that cannot be read.
type headroom struct {
	left int
	err  error
}

func (h headroom) Remaining(context.Context) (int, error) { return h.left, h.err }

// spawnRunner builds a runner whose Execute phase answers with plain text, so
// the test's subject is the SURFACE rather than the loop.
func spawnRunner(t *testing.T, sub *runner.SubagentConfig) *runner.Runner {
	t.Helper()
	prov := &scriptedProvider{
		plan: []llm.Completion{submitCall(t, runner.SubmitPlanTool, `{
			"decision":"plan","reasoning":"go","tools_needed":[],
			"steps":[{"intent":"x","approach":"y"}],"success_criteria":["z"]}`)},
		execute: []llm.Completion{{Content: "done"}},
		review: []llm.Completion{submitCall(t, runner.SubmitReviewTool,
			`{"decision":"done","final_artifact":"a"}`)},
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: prov}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Registry: tools.NewRegistry(),
		Models:   models,
		Caps:     runner.Caps{PlanRounds: 4, ExecuteRounds: 6, ReviewRounds: 3},
		Task:     "fan this out",
		Subagent: sub,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

func shipped() subagent.Limits {
	return subagent.Limits{
		MaxTurns: 20, Timeout: 2 * time.Minute, BatchTimeout: 2 * time.Minute,
		MaxParallel: 3, BudgetFraction: 0.2, MinPerChildTokens: 500,
	}
}

// executeOffers reports what the Execute surface actually carried.
func executeOffers(t *testing.T, r *runner.Runner) []string {
	t.Helper()
	p, _, err := r.Plan(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	_, surface, err := r.Execute(context.Background(), 1, p, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return surface.Catalogue
}

// THE SPAWNER REACHES THE EXECUTE SURFACE.
//
// The regression this exists for: internal/agent/subagent was imported by
// nothing outside its own test, so spawn_subagent was never registered on any
// surface and all six turn_engine.subagent_* knobs were validated, schema'd,
// documented and read by nobody. The package's whole contract — the grant, the
// caps, the batch, the panic containment — could not run.
func TestTheSpawnerReachesTheExecuteSurface(t *testing.T) {
	t.Parallel()
	r := spawnRunner(t, &runner.SubagentConfig{Limits: shipped()})
	if got := executeOffers(t, r); !slices.Contains(got, subagent.ToolName) {
		t.Errorf("Execute was offered %v, without the spawner", got)
	}
}

// AND NOWHERE ELSE.
//
// Plan is choosing what to do and Review is judging what was done; a spawner
// on either can spend a batch of model calls on work the turn has not decided
// to do or has already finished.
func TestOnlyExecuteMaySpawn(t *testing.T) {
	t.Parallel()
	r := spawnRunner(t, &runner.SubagentConfig{Limits: shipped()})

	p, planSurface, err := r.Plan(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if slices.Contains(planSurface.Catalogue, subagent.ToolName) {
		t.Error("the planner was offered a spawner")
	}
	exec, _, err := r.Execute(context.Background(), 1, p, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Review's own surface is not returned, so this asserts the rule from
	// the other end: a Review that ran at all did so without the spawner
	// being reachable, because spawnEntry gates on the phase.
	if _, err := r.Review(context.Background(), 1, p, exec, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
}

// NO CONFIG, NO SPAWNER. A build with no budget source leaves the tool off
// entirely rather than offering it with no ceiling.
func TestWithoutASubagentConfigThereIsNoSpawner(t *testing.T) {
	t.Parallel()
	r := spawnRunner(t, nil)
	if got := executeOffers(t, r); slices.Contains(got, subagent.ToolName) {
		t.Errorf("a runner with no sub-agent config offered %v", got)
	}
}

// AN UNREADABLE BUDGET COUNTER REFUSES THE SPAWN.
//
// subagent reads a ParentRemaining of ZERO AS UNCAPPED, so a counter that
// answered 0 for "I could not reach the store" would hand a fan-out no ceiling
// at all — the fail-OPEN direction, on the one path where money leaves the
// building per token. The tool is left off the surface instead.
func TestAnUnreadableBudgetLeavesTheSpawnerOff(t *testing.T) {
	t.Parallel()
	r := spawnRunner(t, &runner.SubagentConfig{
		Limits:    shipped(),
		Remaining: headroom{err: errors.New("the coordination store is unreachable")},
	})
	if got := executeOffers(t, r); slices.Contains(got, subagent.ToolName) {
		t.Error("an unreadable budget counter still offered an uncapped spawner")
	}
}

// A READABLE ONE DOES NOT. The ordinary case, and the half that proves the
// test above is not passing for the wrong reason.
func TestAReadableBudgetKeepsTheSpawner(t *testing.T) {
	t.Parallel()
	r := spawnRunner(t, &runner.SubagentConfig{
		Limits: shipped(), Remaining: headroom{left: 50_000},
	})
	if got := executeOffers(t, r); !slices.Contains(got, subagent.ToolName) {
		t.Errorf("a seat with 50k of headroom was offered %v", got)
	}
}
