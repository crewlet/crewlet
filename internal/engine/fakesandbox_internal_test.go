package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A DEMONSTRATION COMPANY'S RUNS FINISH, ON EVERY HARNESS. `providers.sandbox.fake`
// replaces the box, not the harness choice, so every harness must run the
// double: a real runner in a fake box never ends, and every turn parks on its
// first launch for good. Both plays: the double's own and a scripted one.
func TestAFakeCatalogueCompletesEveryHarnessRuns(t *testing.T) {
	t.Parallel()
	cfg := parseCompany(t, sandboxDoc(""))
	scripted := func(_ context.Context, req sandbox.RunRequest) sandbox.Result {
		return sandbox.Result{Success: true, Text: "played: " + req.Brief}
	}
	for name, tc := range map[string]struct {
		play fakePlayer
		want string
	}{
		"the double's own play": {nil, demoRun(t.Context(), sandbox.RunRequest{}).Text},
		"a scripted play":       {scripted, "played: fix the failing test"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			manager, err := buildSandbox(cfg, nil, nil, tc.play, nil)
			if err != nil || manager == nil {
				t.Fatalf("buildSandbox = %v, %v", manager, err)
			}
			completesOnEveryHarness(t, manager, tc.want)
		})
	}
}

// A BOOTED NODE'S FAKE CATALOGUE PLAYS ITS RUNS AS ITS OPTIONS SAY, the seam a
// case drives a whole turn through, and pins it on the epoch, which every
// launch places its box by ([Company.catalogue]).
func TestABootedFakeCatalogueRunsTheConfiguredPlay(t *testing.T) {
	t.Parallel()
	played := make(chan string, len(executor.Names()))
	b := testBootstrap(t)
	dir := brokertest.TempDir(t)
	b.Store.Path = filepath.Join(dir, "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(dir, "stream"))
	SeedStore(t, &b)
	e, err := New(t.Context(), Options{
		Bootstrap: &b, Company: parseCompany(t, sandboxDoc("")),
		fakePlay: func(_ context.Context, req sandbox.RunRequest) sandbox.Result {
			played <- req.Brief
			return sandbox.Result{Success: true, Text: "played: " + req.Brief}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	c := e.Company()
	if c == nil || c.catalogue == nil {
		t.Fatal("the booted epoch carries no catalogue of its own")
	}
	completesOnEveryHarness(t, c.catalogue, "played: fix the failing test")
	if len(played) != len(executor.Names()) {
		t.Errorf("the configured play ran %d times, want once per harness", len(played))
	}
}

// completesOnEveryHarness starts one job on each executor harness's runner in manager,
// in a box of the double's, and holds it to finishing with want as its report.
func completesOnEveryHarness(t *testing.T, manager *sandbox.Manager, want string) {
	t.Helper()
	for _, cli := range executor.Names() {
		runner, err := manager.RunnerFor(cli)
		if err != nil {
			t.Fatalf("%s: no runner: %v", cli, err)
		}
		if _, double := runner.(*sandbox.FakeRunner); !double {
			t.Fatalf("%s runs on %T in a fake catalogue, want the double", cli, runner)
		}
		box := sandbox.NewFakeSandbox("box-" + cli)
		handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "fix the failing test"})
		if err != nil {
			t.Fatalf("%s: Start: %v", cli, err)
		}
		if state, err := runner.Poll(t.Context(), box, handle); err != nil || state != sandbox.JobEnded {
			t.Fatalf("%s: the run is %v (%v), want a run that finishes", cli, state, err)
		}
		res, err := runner.Collect(t.Context(), box, handle, queue.NewAnswerBudget(nil))
		if err != nil || !res.Success || res.Text != want {
			t.Fatalf("%s: collected %+v (%v), want a finished run reporting %q", cli, res, err, want)
		}
	}
}
