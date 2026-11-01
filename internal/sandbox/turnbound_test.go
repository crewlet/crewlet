package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// weeklyLimit is the refusal a resumed turn answers with when its review met
// a model's usage limit: a wait, lifted at until.
func weeklyLimit(until time.Time) error {
	return &TurnRefused{
		Cause: errors.New("rate_limit: you have hit your weekly limit"),
		Refusal: Refusal{Condition: "provider", Reason: "every model of the seat's chain is at its limit",
			Until: until},
	}
}

// gone reports whether the run's row is gone: settled, not handed back.
func (r *coordRig) gone(turnID string) bool {
	r.t.Helper()
	_, found, err := r.pending.Get(r.t.Context(), turnID)
	if err != nil {
		r.t.Fatalf("Get: %v", err)
	}
	return !found
}

// endedWith reports whether the run was announced ended under reason.
func (r *coordRig) endedWith(turnID, reason string) bool {
	for _, f := range r.failures() {
		if f.TurnID == turnID && f.Reason == reason {
			return true
		}
	}
	return false
}

// A RESUME THAT FAILS PAST ITS TURN'S DEADLINE ENDS THE RUN. A scheduled fire's
// job finished, and its review could not run (a network outage): handed back,
// the waiter announced the job on every poll and the turn was resumed again
// for as long as the outage lasted, its seat held throughout. Past the cap the
// attempt is the turn's last, and the run is ended, announced and its seat
// freed.
//
// Mutation: drop the deadline check from the failure branch and the run is
// handed back running, and OnCompleted asks for a retry.
func TestAResumeThatFailsPastItsDeadlineEndsTheRun(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launchUntil("t1", rig.now.Add(10*time.Minute))
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "checked the monitors"})
	rig.now = rig.now.Add(16 * time.Minute)
	rig.resumer.failWith(errors.New("llm chain exhausted: can't reach the API server (ENOTFOUND)"))

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("a resume that failed past the deadline asked for a retry: %v", err)
	}
	if !rig.gone("t1") || !rig.endedWith("t1", types.ExecutorFailureDeadline) {
		t.Fatalf("the run was not ended at its deadline: failures %+v", rig.failures())
	}
	if held, _ := rig.coordinator.SeatRuns("swe"); held {
		t.Fatal("the seat is still held by a run its deadline ended")
	}
}

// A RESUME HELD ON A LIMIT BEFORE THE DEADLINE ENDS ONCE THE DEADLINE PASSES.
// Before the cap, a review that met the model's limit waits for it: the turn
// still has time. The next announcement past the cap does not wait out the
// rest of the limit (a weekly one held a ten-minute health check for
// twenty-seven hours): the run is collected, charged and ended.
//
// Mutation: drop the deadline check from the held-here path and the
// announcement is refused until the limit's instant.
func TestAResumeHeldOnALimitEndsWhenItsDeadlinePasses(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launchUntil("t1", rig.now.Add(10*time.Minute))
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "checked the monitors"})
	rig.resumer.failWith(weeklyLimit(rig.now.Add(7 * 24 * time.Hour)))

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("the premise: a resume refused before the deadline is handed back to wait")
	}
	if rig.gone("t1") || rig.get("t1").Status != StatusRunning {
		t.Fatal("the premise: before the deadline the run waits, running")
	}

	// The limit has not lifted, but a resume let through now would run: the
	// announcement past the deadline must not try it.
	rig.resumer.failWith(nil)
	rig.now = rig.now.Add(11 * time.Minute)
	payload, ev = rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("an announcement past the deadline was refused: %v", err)
	}
	if !rig.gone("t1") || !rig.endedWith("t1", types.ExecutorFailureDeadline) {
		t.Fatalf("the held run was not ended at its deadline: failures %+v", rig.failures())
	}
	if n := len(rig.resumer.calls()); n != 0 {
		t.Fatalf("the turn was resumed %d times past its deadline, want none", n)
	}
}

// A COMPLETION THE GATE REFUSES PAST ITS DEADLINE IS ENDED, NOT KEPT: a seat
// whose model is at its limit refuses every turn, and before the cap the job
// waits for it. Past the cap it is collected (its spend charged and its
// launch recorded) and ended.
//
// Mutation: drop the deadline check from the gate's refusal and the job is
// kept for the condition, its run running.
func TestACompletionTheGateRefusesPastItsDeadlineIsEnded(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.coordinator.gate = func(context.Context, string, ResumeRoute) (context.Context, func(), error) {
		return nil, nil, weeklyLimit(rig.now.Add(time.Hour))
	}
	rig.launchUntil("t1", rig.now.Add(-time.Minute))
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "checked the monitors", InputTokens: 900})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("a refused completion past its deadline was handed back: %v", err)
	}
	if !rig.gone("t1") || !rig.endedWith("t1", types.ExecutorFailureDeadline) {
		t.Fatalf("the run was not ended at its deadline: failures %+v", rig.failures())
	}
	if len(rig.collected()) != 1 {
		t.Fatalf("the run ended at its deadline published %d collected records, want its one",
			len(rig.collected()))
	}
	if n := len(rig.resumer.calls()); n != 0 {
		t.Fatalf("a completion the gate refused was resumed %d times", n)
	}
}

// BEFORE THE DEADLINE A REFUSED COMPLETION STILL WAITS. The deadline ends a
// wait only once it has passed; a turn with time left keeps its job for the
// condition, as it always did.
func TestACompletionRefusedBeforeItsDeadlineWaits(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.coordinator.gate = func(context.Context, string, ResumeRoute) (context.Context, func(), error) {
		return nil, nil, weeklyLimit(rig.now.Add(time.Hour))
	}
	rig.launchUntil("t1", rig.now.Add(time.Hour))
	rig.runner.Finish(Result{Success: true})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a completion refused before its deadline was acknowledged as done")
	}
	if rig.gone("t1") || len(rig.failures()) != 0 {
		t.Fatalf("a run with time left was ended: failures %+v", rig.failures())
	}
}

// A RESUME THAT KEEPS FAILING IS GIVEN UP, ON ANY TURN. Each failure hands the
// claim back and the poll announces the job again, so the broker's delivery
// budget never bit: a resume that always failed was retried on every poll for
// as long as the engine ran, the seat held behind it. Counted on the job's
// record, the run is settled once it has failed often enough for long enough,
// and not before.
//
// Mutation: stop counting the failure on the release and the run is handed
// back for ever.
func TestAResumeThatKeepsFailingIsGivenUp(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done"})
	rig.resumer.failWith(errors.New("the turn could not be re-entered"))

	attempt := func() error {
		payload, ev := rig.completion("t1")
		return rig.coordinator.OnCompleted(t.Context(), payload, ev)
	}
	for i, after := range []time.Duration{0, 20 * time.Minute} {
		rig.now = rig.now.Add(after)
		if err := attempt(); err == nil {
			t.Fatalf("failure %d was not handed back for a retry", i+1)
		}
		if got := rig.get("t1"); got.Status != StatusRunning || got.Launch.ResumeFailures != i+1 {
			t.Fatalf("after failure %d the row is %s with %d failures counted",
				i+1, got.Status, got.Launch.ResumeFailures)
		}
	}
	rig.now = rig.now.Add(11 * time.Minute)
	if err := attempt(); err != nil {
		t.Fatalf("the failure that spans the give-up window asked for a retry: %v", err)
	}
	if !rig.gone("t1") || !rig.endedWith("t1", types.ExecutorFailureResumeFailing) {
		t.Fatalf("the run was not given up: failures %+v", rig.failures())
	}
}

// A WAIT IS NOT A FAILURE: a resume held on a limit counts nothing toward the
// give-up, so a turn with no deadline waits for its condition however long it
// lasts, as it always did.
func TestAResumeThatWaitsIsNotCountedAsFailing(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true})
	rig.resumer.failWith(weeklyLimit(rig.now.Add(time.Hour)))

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("the premise: a refused resume is handed back")
	}
	if got := rig.get("t1"); got.Launch.ResumeFailures != 0 {
		t.Fatalf("a wait was counted as %d failed resumes", got.Launch.ResumeFailures)
	}
}

// A POLL THAT FAILS PAST THE DEADLINE ENDS THE JOB. The deadline was read only
// once a poll answered, so a job whose poll kept failing was kept alive and
// never ended, however far past its turn's cap.
//
// Mutation: return running on a failed poll whatever the deadline, and the
// tick past the cap fires nothing.
func TestAPollThatFailsPastTheDeadlineEndsTheJob(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launchUntil("capped", rig.now.Add(time.Minute))
	rig.runner.PollErr = errors.New("the harness's status file could not be read")

	if fired := rig.tick(); fired != 0 {
		t.Fatalf("fired %d completions on a failed poll before the deadline", fired)
	}
	rig.now = rig.now.Add(2 * time.Minute)
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("fired %d completions on a failed poll past the deadline, want the job's", fired)
	}
}
