package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// aroundRuns is runs in flight around a run, as the engine would read them.
type aroundRuns struct{ around []prompts.RunAround }

func (a aroundRuns) Around(context.Context, string) ([]prompts.RunAround, error) {
	return a.around, nil
}

func (aroundRuns) Tools() []tools.Callable {
	return []tools.Callable{stubTool{name: toolname.ListRuns}, stubTool{name: toolname.MessageRun},
		stubTool{name: toolname.WaitForRun}}
}

// A LAUNCH LISTS THE RUNS AROUND IT, AND OFFERS THEIR TOOLS, ONLY WHEN THERE ARE
// SOME. A run launched beside its seat's other runs reads them in its brief,
// one line each, and is offered list_runs, message_run and wait_for_run on its
// surface; a run alone is told nothing of other runs and offered none of the
// three, though its surface holds them for a rebuild to find.
//
// Mutation: offer the three whatever the read found (always-offered), or list
// an empty section, and this goes red.
func TestALaunchListsTheRunsAroundItAndOffersTheirToolsOnlyWhenThereAreSome(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	names := []string{toolname.ListRuns, toolname.MessageRun, toolname.WaitForRun}
	beside := []prompts.RunAround{{Turn: "t-2", Seat: "cto", OwnSeat: true, Stage: "running", SameRequester: true,
		Title: "add the logout button", Label: provenance.ExecutorSession("cto"), TakesMessages: true}}
	for name, tc := range map[string]struct {
		around  []prompts.RunAround
		offered bool
	}{
		"alone":          {offered: false},
		"beside another": {around: beside, offered: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, req := launch(t, entries, buildOpts{runs: aroundRuns{around: tc.around}}, 1, "", nil)
			listed := strings.Contains(req.Brief, "## Other runs in flight around this one")
			if listed != tc.offered || (tc.offered && !strings.Contains(req.Brief, "add the logout button")) {
				t.Errorf("the brief lists the runs around it %v, want %v:\n%s", listed, tc.offered, req.Brief)
			}
			active := req.Surface.Active()
			for _, tool := range names {
				if slices.Contains(active, tool) != tc.offered {
					t.Errorf("%s is offered %v, want %v: %v", tool, slices.Contains(active, tool), tc.offered, active)
				}
			}
		})
	}
}

// A WAITED RUN'S ACCOUNT IS TOLD IN ITS FENCE. A run the engine answers once
// the run it waited for left is told, in its own session and on a fresh
// machine alike, what it stopped to wait for, how that run left and what it
// pushed in the engine's words, and its reviewer's account only inside the
// fence of what a kept session holds, never as a person's answer.
//
// Mutation: hand the reviewer's account unfenced (bare-report), or tell the
// wait as an answered question, and this goes red.
func TestAWaitedRunsAccountIsToldInItsFence(t *testing.T) {
	t.Parallel()
	b := provenance.NewBoundary()
	asked := runner.Asked{Question: "Waiting for another run: it pushes the branch first", Branch: "login",
		BoxKept: true, Answer: "The run you waited for left its seat: ended.",
		Waited: &types.RunWaited{Turn: "run-t", Seat: "swe", Left: types.RunLeftEnded, Outcome: "completed",
			Report: "landed the logout button", Pushed: types.RunPushedSome, Refs: []string{"acme/app refs/heads/logout"}}}
	for name, said := range map[string]string{"continued": asked.Continuation(b), "reseeded": asked.Reseed(b)} {
		for _, want := range []string{"it pushes the branch first", "Run `run-t` of swe ended: completed.",
			"It pushed acme/app refs/heads/logout."} {
			if !strings.Contains(said, want) {
				t.Errorf("%s: the account does not say %q:\n%s", name, want, said)
			}
		}
		if strings.Contains(said, "answered") {
			t.Errorf("%s: the wait is told as an answered question:\n%s", name, said)
		}
		fences := provenance.Fences(said, b)
		if len(fences) != 1 || !strings.Contains(fences[0], "landed the logout button") ||
			!strings.Contains(fences[0], "source=executor_session") {
			t.Errorf("%s: the reviewer's account is not one fence of a kept session's: %q", name, fences)
		}
	}
}
