package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// rotatedRound launches round 1, plays before against the run's surface, and
// resumes the parked turn told its job was refused on a replaced sign-in, as
// the engine's resume does when the job's recorded sign-in is not the entry's.
func rotatedRound(t *testing.T, before run, rotated runner.Rotated) (*playLauncher, *runner.Runner, error) {
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
	opts := buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher}
	opts.resume = &runner.Resume{State: parked.State, Bridged: bridged, Rotated: &rotated,
		Answer: "The executor's run did NOT fully succeed.\n\nError: Failed to authenticate"}
	collector, _ := buildWith(t, entries, opts)
	work, _, err := collector.Resume(context.Background(), nil)
	if err == nil && !work.Suspended && relauncher.launched() > 0 {
		t.Fatalf("work = %+v: a relaunch was made but the turn did not park on it", work)
	}
	return relauncher, collector, err
}

// A ROUND A REPLACED SIGN-IN CUT OFF IS CARRIED ON, NOT COLLECTED: the job was
// refused on the credential it started with, which the entry no longer signs in
// with, so the refusal is no account of the round. The same round is relaunched
// continuing the job's own session where it runs in the job's box, told why, and
// marked as a follow-up for this reason so it is not carried on again for it.
//
// Mutation: collect the refusal as resumeAgentRun does, continue no session, or
// leave the launch unmarked, and this fails.
func TestARoundAReplacedSignInCutOffIsCarriedOnInItsSession(t *testing.T) {
	t.Parallel()
	before := run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"starting on it"}`)}}
	relauncher, _, err := rotatedRound(t, before, runner.Rotated{SessionID: "sess-1", BoxKept: true})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if again.Continue == nil || again.Continue.SessionID != "sess-1" || !again.Continue.SignInReplaced {
		t.Fatalf("the relaunch continues %+v; want the refused job's session, marked as a "+
			"follow-up of a replaced sign-in", again.Continue)
	}
	if again.Continue.Brief != runner.RotatedContinuation {
		t.Errorf("the continued session is told %q, want %q", again.Continue.Brief, runner.RotatedContinuation)
	}
	if !strings.Contains(again.Brief, "sign-in was replaced") {
		t.Errorf("a fresh run's brief does not say the sign-in was replaced:\n%s", again.Brief)
	}
	if again.Round != 1 {
		t.Errorf("the relaunch is round %d, want the round it carries on, 1", again.Round)
	}
	if got := again.Surface.Turn().CallLog().Ordinal("slack_post",
		map[string]any{"text": "done"}); got != 1 {
		t.Errorf("a post after the refusal counts %d earlier posts, want the refused job's one", got)
	}
}

// A ROUND ALREADY SUBMITTED IS COLLECTED, as a lost one is: the job had finished
// its work, and relaunching it would deliver twice.
func TestARoundSubmittedBeforeTheRefusalIsCollected(t *testing.T) {
	t.Parallel()
	before := run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"fixed the flake"}`),
		call(t, runner.SubmitWorkTool,
			`{"outcome":"delivered","summary":"fixed the flake","deliveries":["slack_post"]}`)}}
	relauncher, _, err := rotatedRound(t, before, runner.Rotated{SessionID: "sess-1", BoxKept: true})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n := relauncher.launched(); n != 0 {
		t.Fatalf("a round its job had submitted was launched again %d times", n)
	}
}
