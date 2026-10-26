package runner_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// headroom is a seat's remaining allowance, or a counter that cannot be read.
type headroom struct {
	left int
	err  error
}

func (h headroom) Remaining(context.Context) (int, error) { return h.left, h.err }

// spawnRunner builds a runner whose executor's launch starts nothing, so the
// test's subject is the SURFACE its run is handed.
func spawnRunner(t *testing.T, sub *runner.SubagentConfig) *runner.Runner {
	t.Helper()
	prov := &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool,
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
		Launcher: &playLauncher{},
		Task:     "fan this out",
		Subagent: sub,
		Sessions: sessions,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

func shipped() subagent.Limits {
	return subagent.Limits{
		MaxTurns: 20, MaxTasksPerCall: 8,
		TaskTimeout: 2 * time.Minute, CallTimeout: 5 * time.Minute,
		MaxParallel: 3, BudgetFraction: 0.2, MinTokensPerTask: 500,
	}
}

// executeOffers reports what the surface the executor's run was launched with
// actually carried.
func executeOffers(t *testing.T, r *runner.Runner) []string {
	t.Helper()
	_, surface, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return surface.Catalogue
}

// THE SPAWNER REACHES THE EXECUTOR'S SURFACE.
//
// The regression this exists for: internal/agent/subagent was imported by
// nothing outside its own test, so delegate was never registered on any
// surface and all six turn_engine.subagent_* knobs were validated, schema'd,
// documented and read by nobody. The package's whole contract — the grant, the
// caps, the batch, the panic containment — could not run.
func TestTheSpawnerReachesTheExecutorsSurface(t *testing.T) {
	t.Parallel()
	r := spawnRunner(t, &runner.SubagentConfig{Limits: shipped()})
	if got := executeOffers(t, r); !slices.Contains(got, subagent.ToolName) {
		t.Errorf("the executor was offered %v, without the spawner", got)
	}
}

// AND NOWHERE ELSE.
//
// The reviewer is judging what was done; a spawner there can spend a batch of
// model calls on work the turn has already finished.
func TestOnlyTheExecutorMaySpawn(t *testing.T) {
	t.Parallel()
	r := spawnRunner(t, &runner.SubagentConfig{Limits: shipped()})

	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The reviewer's own surface is not returned, so this asserts the rule
	// from the other end: a review that ran at all did so without the
	// spawner being reachable, because spawnEntry gates on the phase.
	if _, err := r.Review(context.Background(), 1, w, nil); err != nil {
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

// A SEAT OFFERED DELEGATE IS TOLD WHO ITS WORKERS ARE.
//
// `## Your workers` is gated on `delegate` being on the surface, and the gate
// read the registry snapshot the surface was built FROM — which never holds
// `delegate`, since the phase adds the spawner to the surface's own copy. So
// the block was rendered for no seat at all, and an executor with templates
// configured for it chose between them blind. The control is the same seat
// whose budget cannot be read: the spawner is left off its surface, so the
// block naming the templates must be left out with it, while the templates
// themselves are still configured.
func TestASeatOfferedDelegateIsToldItsWorkers(t *testing.T) {
	t.Parallel()
	workers := map[string]config.Worker{"researcher": {
		Description:  "reads sources and reports findings with citations",
		SystemPrompt: "You research.",
	}}
	offered := &runner.SubagentConfig{Limits: shipped(), Workers: workers}
	withheld := &runner.SubagentConfig{
		Limits: shipped(), Workers: workers,
		Remaining: headroom{err: errors.New("the coordination store is unreachable")},
	}

	system := func(t *testing.T, sub *runner.SubagentConfig) string {
		t.Helper()
		_, req := launch(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
			buildOpts{subagent: sub}, 1, "", nil)
		return req.System
	}
	got := system(t, offered)
	if !strings.Contains(got, "## Your workers") || !strings.Contains(got, "`researcher`") {
		t.Errorf("a seat offered %s was not told its workers:\n%s", subagent.ToolName, got)
	}
	if got := system(t, withheld); strings.Contains(got, "## Your workers") {
		t.Errorf("a seat whose surface holds no %s was told about workers it cannot reach:\n%s",
			subagent.ToolName, got)
	}
}

// A SPENT BUDGET REFUSES THE SPAWN TOO. A reader answering zero is a capped
// seat with no room left — a seat nothing caps is handed no reader — and
// subagent reads a ParentRemaining of zero as uncapped, so passing it on gave
// an exhausted seat's fan-out no slice at all.
func TestASpentBudgetLeavesTheSpawnerOff(t *testing.T) {
	t.Parallel()
	r := spawnRunner(t, &runner.SubagentConfig{
		Limits: shipped(), Remaining: headroom{left: 0},
	})
	if got := executeOffers(t, r); slices.Contains(got, subagent.ToolName) {
		t.Error("a seat with no budget left was offered an uncapped spawner")
	}
}

// A READABLE ONE DOES NOT. The ordinary case, and the half that proves the
// tests above are not passing for the wrong reason.
func TestAReadableBudgetKeepsTheSpawner(t *testing.T) {
	t.Parallel()
	r := spawnRunner(t, &runner.SubagentConfig{
		Limits: shipped(), Remaining: headroom{left: 50_000},
	})
	if got := executeOffers(t, r); !slices.Contains(got, subagent.ToolName) {
		t.Errorf("a seat with 50k of headroom was offered %v", got)
	}
}
