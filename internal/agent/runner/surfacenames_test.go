package runner_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname/toolnametest"
)

// EVERY TOOL A PHASE SURFACE OFFERS OF ITS OWN IS A NAME THE WALK READS.
//
// internal/toolname's walk holds every string literal in the module to the
// tool names a model is offered ([toolnametest.Names]): the authority table's
// tool-shaped verbs, which builtin's walks hold equal to the tools its
// registry and the operator catalogue register, and the names internal/toolname
// declares. A phase surface offers tools BEYOND the seat's registry, which no
// row names: the submissions, the discovery pair, `delegate`, and the tool the
// executor's run asks a person through, which the engine hands the runner from
// outside every registry. One the set lacks is a name the walk never reads,
// so a literal copy of it compiles and passes: `ask_and_wait` was one,
// declared beside its implementation alone. So every such tool is held to the
// set here, where the surfaces are built, and the set fails closed.
//
// Mutation: declare `ask_and_wait` beside its implementation alone again
// (builtin.AskAndWaitTool a literal and toolname.AskAndWait gone) and the
// executor's case is red.
func TestEveryToolAPhaseSurfaceOffersOfItsOwnIsANameTheWalkReads(t *testing.T) {
	t.Parallel()
	universe := toolnametest.Names(t)

	launcher := &playLauncher{}
	prov := &scriptedProvider{review: []llmtest.Response{
		submitCall(t, runner.SubmitReviewTool, `{"decision":"done","final_artifact":"a"}`),
	}}
	r, reg := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		launcher: launcher,
		ask:      builtin.NewAskAndWait(builtin.RunAskDeps{Desk: noAsks{}}),
		subagent: &runner.SubagentConfig{Limits: shipped()},
	})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	reviewed := prov.offeredFor("review")
	if len(reviewed) == 0 {
		t.Fatal("the reviewer made no model request, so its surface was never read")
	}

	onboarding := marking()
	onboarded := onboardingRunner(t, &markers{claimHeld: true}, runner.NewLatch(), onboarding)
	if _, err := onboarded.Onboard(context.Background()); err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	onboard := onboarding.offeredFor("onboarding")
	if len(onboard) == 0 {
		t.Fatal("the onboarding pass made no model request, so its surface was never read")
	}

	for _, surface := range []struct {
		phase    string
		offered  []string
		register []string
	}{
		{"the executor's run", toolNames(launcher.last(t).Surface.ToolDefs()), reg.Names()},
		{"the reviewer", reviewed[0], reg.Names()},
		{"the onboarding pass", onboard[0], []string{"reflect_and_persist", "mark_onboarded"}},
	} {
		var own []string
		for _, name := range surface.offered {
			if !slices.Contains(surface.register, name) {
				own = append(own, name)
			}
		}
		if len(own) == 0 {
			t.Errorf("%s offers %v, nothing beyond the seat's registry: the case reads "+
				"no tool of the surface's own, so it certifies nothing", surface.phase,
				surface.offered)
		}
		for _, name := range own {
			if !slices.Contains(universe, name) {
				t.Errorf("%s offers %q, which neither the authority table nor "+
					"internal/toolname names, so internal/toolname's walk never reads "+
					"it and a literal copy of it passes: declare it in internal/toolname",
					surface.phase, name)
			}
		}
	}
	// THE CONTROL: the executor's own tools are the run's two endings among
	// the rest, so the case read the surface the run is offered.
	executor := toolNames(launcher.last(t).Surface.ToolDefs())
	for _, ending := range []string{runner.SubmitWorkTool, builtin.AskAndWaitTool} {
		if !slices.Contains(executor, ending) {
			t.Errorf("the executor's run offers %v, without %s", executor, ending)
		}
	}
}

// noAsks records nothing: the case reads the ask tool's name, never calls it.
type noAsks struct{}

func (noAsks) RecordAsk(context.Context, string, string, sandbox.Ask) (sandbox.AskOutcome, error) {
	return sandbox.AskRecorded, nil
}

func (noAsks) Get(context.Context, string) (sandbox.PendingRun, bool, error) {
	return sandbox.PendingRun{}, false, nil
}
