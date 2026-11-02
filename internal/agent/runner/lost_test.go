package runner_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// lostRound launches round 1, plays before against the run's surface, and
// resumes the parked turn on a fresh runner told its job was lost with lost —
// as the engine's resume does once the coordinator claims a lost job.
func lostRound(t *testing.T, before run, lost runner.Lost, opts buildOpts,
) (*playLauncher, *runner.Runner, error) {
	t.Helper()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	bridged := play(t, req, before)
	parked, ok := launched.Suspended()
	if !ok {
		t.Fatal("the launch recorded no state")
	}
	parked.State.ActiveTools = req.Surface.Active()

	relauncher := &playLauncher{}
	opts.calls, opts.launcher = turnctx.NewCallLog(), relauncher
	opts.resume = &runner.Resume{State: parked.State, Bridged: bridged, Lost: &lost,
		Answer: "The executor's run did NOT fully succeed.\n\nError: the run was cut off"}
	collector, _ := buildWith(t, entries, opts)
	work, _, err := collector.Resume(context.Background(), nil)
	if err == nil && !work.Suspended && relauncher.launched() > 0 {
		t.Fatalf("work = %+v: a relaunch was made but the turn did not park on it", work)
	}
	return relauncher, collector, err
}

// A LOST JOB'S ROUND IS CARRIED ON IN A FRESH SESSION, never collected: the job
// was cut off mid-step, so what it left is no account of the round, and judged
// as one the round "did not succeed" for a reason the run never had. The same
// round is relaunched with what it did so far — its tools, its calls counted —
// and the fresh run is told what happened and where to look for the work.
//
// Mutation: collect a lost job as resumeAgentRun does, and the turn files the
// truncated run as the round's result; continue its session, and the harness is
// resumed from a conversation cut off mid-call.
func TestALostJobsRoundIsCarriedOnInAFreshSession(t *testing.T) {
	t.Parallel()
	before := run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"starting on it"}`)}}
	lost := runner.Lost{Why: "the engine it ran under restarted, which killed it", BoxKept: true,
		Losses: 1, Limit: 2}
	relauncher, _, err := lostRound(t, before, lost, buildOpts{})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if again.Continue == nil || again.Continue.SessionID != "" {
		t.Fatalf("the relaunch continues %+v; want the round carried on in a session of its own",
			again.Continue)
	}
	for _, want := range []string{"was cut off", lost.Why, "git status"} {
		if !strings.Contains(again.Brief, want) {
			t.Errorf("the fresh run's brief does not say %q:\n%s", want, again.Brief)
		}
	}
	if again.Round != 1 {
		t.Errorf("the relaunch is round %d, want the round it carries on, 1", again.Round)
	}
	if !slices.Contains(again.Surface.Active(), "slack_post") {
		t.Errorf("the carried-on run is offered %v, want the tool the lost job had active",
			again.Surface.Active())
	}
	if got := again.Surface.Turn().CallLog().Ordinal("slack_post",
		map[string]any{"text": "done"}); got != 1 {
		t.Errorf("a post after the loss counts %d earlier posts, want the lost job's one", got)
	}
}

// A LOST JOB WHOSE ROUND WAS ALREADY SUBMITTED IS COLLECTED: its submit_work is
// on the round's calls, so it had finished its work and was lost on its way
// out — the round is over, and starting it again would do it twice.
//
// Mutation: carry every lost round on, and a run that delivered is relaunched
// to deliver again.
func TestALostJobThatHadSubmittedIsCollected(t *testing.T) {
	t.Parallel()
	before := run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"fixed the flake"}`),
		call(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"fixed the flake","deliveries":["slack_post"]}`)}}
	relauncher, collector, err := lostRound(t, before,
		runner.Lost{Why: "its box was lost from the host it ran on", Losses: 1, Limit: 2}, buildOpts{})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n := relauncher.launched(); n != 0 {
		t.Fatalf("a round its job had submitted was launched again %d times", n)
	}
	if _, parked := collector.Suspended(); parked {
		t.Error("the collected round parked again")
	}
}

// A ROUND LOST MORE OFTEN THAN IT IS CARRIED ON AFTER IS NOT STARTED AGAIN: a
// run that keeps taking its host down would take it — and every other seat's
// work on it — down again with each relaunch. The phase the round opened is
// closed failed, saying why, and the turn ends with it.
//
// Mutation: drop the refusal, and a run that kills its host is relaunched for
// ever.
func TestARoundLostTooOftenIsNotStartedAgain(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	relauncher, _, err := lostRound(t, run{},
		runner.Lost{Why: "the engine it ran under restarted, which killed it", Losses: 3, Limit: 2},
		buildOpts{pub: pub})
	if !errors.Is(err, runner.ErrRoundLost) {
		t.Fatalf("Resume = %v; want the round refused as lost too often", err)
	}
	if n := relauncher.launched(); n != 0 {
		t.Fatalf("a round lost too often was launched again %d times", n)
	}
	done := completedPhase(t, pub, "execute")
	if !done.Failed || !strings.Contains(done.Error, "lost 3 times") {
		t.Errorf("failed = %v error = %q; want the round's phase closed failed, saying why",
			done.Failed, done.Error)
	}
}

// A FRESH RUN IS TOLD WHERE TO LOOK, NOT PROMISED WHAT IS THERE. Whether it is
// in the lost job's box is the launch's to decide, so a kept box is described
// as a place to look first, and a box that went with its host as a fresh
// machine — where anything unpushed is gone — so the run neither hunts for a
// working tree that is not there nor starts over beside one that is.
func TestARunCarryingALostRoundIsToldWhereToLook(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		lost      runner.Lost
		want, not []string
	}{
		"its box was kept": {
			runner.Lost{Why: "the engine it ran under restarted, which killed it", BoxKept: true},
			[]string{"Its box was kept", "git status", "fresh checkout instead",
				"the engine it ran under restarted"},
			[]string{"FRESH machine"},
		},
		"its box went with its host": {
			runner.Lost{Why: "its box was lost from the host it ran on"},
			[]string{"FRESH machine", "work-in-progress branch", "its box was lost"},
			[]string{"Its box was kept"},
		},
		"no reason was given": {
			runner.Lost{},
			[]string{"it was lost with the host it ran on"}, nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			text := tc.lost.Reseed()
			for _, want := range append(tc.want, "did happen") {
				if !strings.Contains(text, want) {
					t.Errorf("the brief does not say %q:\n%s", want, text)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(text, not) {
					t.Errorf("the brief says %q:\n%s", not, text)
				}
			}
		})
	}
}
