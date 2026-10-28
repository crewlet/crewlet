package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// A DEMONSTRATION COMPANY'S RUNS FINISH, ON EVERY CLI. `providers.sandbox.fake`
// stands in for the box and not for the choice of CLI, so a fake company's
// chains pick their member as a real one's do — and whichever CLI that member
// drives has to be the double, with a run that completes. A real runner in a
// fake box starts nothing that ever ends, and every turn of the company parks
// on its first launch for good.
//
// BOTH PLAYS: the double's own, whose run did nothing and says so in its
// report, and a scripted one, which is what a run DOES wherever a case scripts
// it.
func TestAFakeCatalogueCompletesEveryCLIsRuns(t *testing.T) {
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
			manager, err := buildSandbox(cfg, nil, nil, tc.play)
			if err != nil || manager == nil {
				t.Fatalf("buildSandbox = %v, %v", manager, err)
			}
			completesOnEveryCLI(t, manager, tc.want)
		})
	}
}

// A BOOTED NODE'S FAKE CATALOGUE PLAYS ITS RUNS AS ITS OPTIONS SAY — the seam
// a case uses to drive a whole turn through the engine it boots — and pins
// that catalogue on its epoch, which is what every launch of the epoch's turns
// places its box by ([Company.catalogue]).
func TestABootedFakeCatalogueRunsTheConfiguredPlay(t *testing.T) {
	t.Parallel()
	played := make(chan string, len(codingagent.Names()))
	b := testBootstrap(t)
	dir := t.TempDir()
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
	completesOnEveryCLI(t, c.catalogue, "played: fix the failing test")
	if len(played) != len(codingagent.Names()) {
		t.Errorf("the configured play ran %d times, want once per CLI", len(played))
	}
}

// completesOnEveryCLI starts one job on each executor CLI's runner in manager,
// in a box of the double's, and holds it to finishing with want as its report.
func completesOnEveryCLI(t *testing.T, manager *sandbox.Manager, want string) {
	t.Helper()
	for _, cli := range codingagent.Names() {
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
		res, err := runner.Collect(t.Context(), box, handle)
		if err != nil || !res.Success || res.Text != want {
			t.Fatalf("%s: collected %+v (%v), want a finished run reporting %q", cli, res, err, want)
		}
	}
}
