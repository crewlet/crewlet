package sandbox

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// A PLAYED JOB DOES ITS WORK WHEN IT IS POLLED, NOT WHEN IT IS STARTED, and
// finishes with what its play answered.
//
// The double is what a `providers.sandbox.fake: true` company runs every
// executor on, and what every engine test drives a turn's run through, so its
// shape is the shape those runs have: the work happens after the launch has
// returned — a play inside Start would land the run's bridged calls while its
// launch is still in flight — and it happens once, however often the job is
// polled and collected.
//
// Mutation: play at Start and the first assertion fails; drop the once and the
// count does.
func TestAPlayedJobRunsOnceWhenPolledAndFinishesWithItsPlay(t *testing.T) {
	t.Parallel()
	runner := NewFakeRunner("claude-code")
	var played atomic.Int32
	runner.Player = func(_ context.Context, req RunRequest) Result {
		played.Add(1)
		return Result{Success: true, Text: "played: " + req.Brief}
	}
	box := NewFakeSandbox("box-1")
	handle, err := runner.Start(t.Context(), box, RunRequest{Brief: "fix the test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := played.Load(); n != 0 {
		t.Fatalf("the job was played %d times by its start, want none until it is polled", n)
	}
	state, err := runner.Poll(t.Context(), box, handle)
	if err != nil || state != JobEnded {
		t.Fatalf("Poll = %v, %v; want the played job ended", state, err)
	}
	for range 2 {
		result, err := runner.Collect(t.Context(), box, handle)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if !result.Success || result.Text != "played: fix the test" {
			t.Fatalf("collected %+v, want what the play answered for its own request", result)
		}
	}
	if n := played.Load(); n != 1 {
		t.Fatalf("the job was played %d times, want once", n)
	}
}

// EACH JOB IS PLAYED FROM ITS OWN REQUEST: two jobs on one runner are two
// plays, so a turn's second run is not handed its first run's result.
func TestEachPlayedJobIsPlayedFromItsOwnRequest(t *testing.T) {
	t.Parallel()
	runner := NewFakeRunner("claude-code")
	runner.Player = func(_ context.Context, req RunRequest) Result {
		return Result{Success: true, Text: req.Brief}
	}
	box := NewFakeSandbox("box-1")
	first, err := runner.Start(t.Context(), box, RunRequest{Brief: "first"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	second, err := runner.Start(t.Context(), box, RunRequest{Brief: "second"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for handle, want := range map[RunHandle]string{first: "first", second: "second"} {
		result, err := runner.Collect(t.Context(), box, handle)
		if err != nil || result.Text != want {
			t.Errorf("job %s collected %q (%v), want %q", handle.CommandID, result.Text, err, want)
		}
	}
}

// A JOB THIS RUNNER NEVER STARTED IS OVER, AND SAYS WHY: under a player, a job
// a previous process launched has no play here to finish it, and a job left
// running for ever would park its turn for ever.
func TestAPlayedRunnerEndsAJobItDidNotStart(t *testing.T) {
	t.Parallel()
	runner := NewFakeRunner("claude-code")
	runner.Player = func(context.Context, RunRequest) Result {
		t.Error("a job this runner never started was played")
		return Result{Success: true}
	}
	box := NewFakeSandbox("box-1")
	stranger := RunHandle{CommandID: "cmd-from-another-process"}
	state, err := runner.Poll(t.Context(), box, stranger)
	if err != nil || state != JobEnded {
		t.Fatalf("Poll = %v, %v; want the stranger's job reported ended", state, err)
	}
	result, err := runner.Collect(t.Context(), box, stranger)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Success || !strings.Contains(result.Error, "did not start this job") {
		t.Fatalf("collected %+v, want a failure that says the job is not this runner's", result)
	}
}
