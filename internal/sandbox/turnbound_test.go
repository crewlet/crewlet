package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
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
	_, ok := r.ending(turnID, reason)
	return ok
}

// ending is the run's announced ending under reason.
func (r *coordRig) ending(turnID, reason string) (types.ExecutorFailed, bool) {
	for _, f := range r.failures() {
		if f.TurnID == turnID && f.Reason == reason {
			return f, true
		}
	}
	return types.ExecutorFailed{}, false
}

// keptWithTheTurn fails unless the run's ending names what its job delivered
// and the job's own record (its account of the work) was published for the
// turn: a run the deadline ends is not thrown away with the wait.
func keptWithTheTurn(t *testing.T, rig *coordRig, turnID, account string, delivered ...string) {
	t.Helper()
	ending, ok := rig.ending(turnID, types.ExecutorFailureDeadline)
	if !ok {
		t.Fatalf("the run was not ended at its deadline: failures %+v", rig.failures())
	}
	for _, ref := range delivered {
		if !strings.Contains(ending.Detail, ref) {
			t.Errorf("the ending does not name the delivery %q: %q", ref, ending.Detail)
		}
	}
	var found bool
	for _, c := range rig.collected() {
		found = found || (c.TurnID == turnID && c.Response == account)
	}
	if !found {
		t.Errorf("no collected record carries the job's account %q: %+v", account, rig.collected())
	}
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
	rig.runner.Finish(Result{Success: true, Text: "checked the monitors",
		DeliveredRefs: []string{"GP-126#comment-9"}})
	rig.now = rig.now.Add(16 * time.Minute)
	rig.resumer.failWith(errors.New("llm chain exhausted: can't reach the API server (ENOTFOUND)"))

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("a resume that failed past the deadline asked for a retry: %v", err)
	}
	if !rig.gone("t1") {
		t.Fatal("the run was handed back rather than ended")
	}
	keptWithTheTurn(t, rig, "t1", "checked the monitors", "GP-126#comment-9")
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
	rig.runner.Finish(Result{Success: true, Text: "checked the monitors", InputTokens: 900,
		DeliveredRefs: []string{"branch:health/2026-10-30", "pr:77"}})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("a refused completion past its deadline was handed back: %v", err)
	}
	if !rig.gone("t1") {
		t.Fatal("the run was handed back rather than ended")
	}
	keptWithTheTurn(t, rig, "t1", "checked the monitors", "branch:health/2026-10-30", "pr:77")
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
	// THE ANNOUNCEMENT SAYS WHAT THE RESUME KEPT MEETING, whichever attempt makes it:
	// a reader tells the engine's own defect from an outage by this alone.
	ended, _ := rig.ending("t1", types.ExecutorFailureResumeFailing)
	if want := "kind of: the turn could not be re-entered"; ended.ErrorKind != want {
		t.Errorf("ErrorKind = %q, want %q", ended.ErrorKind, want)
	}
}

// A RESUME THE NODE FAILED IS NOT THE RUN'S FAILURE, and counts nothing toward
// the give-up. Half an hour of a coordination store that did not answer the
// read of where the turn's writes are minted failed every attempt, and
// counted, it ended a run whose job had finished, though the first attempt
// after the outage would have resumed it. The failure is handed back as any
// failure is, and the run waits on; the run's own failure after the outage is
// still counted, from one.
//
// Mutation: count every failure that is not a wait, whatever
// CoordinatorOptions.NodesFailure says, and the third attempt, half an hour
// in, ends the run.
func TestAResumeTheNodeFailedIsNotCountedTowardItsGiveUp(t *testing.T) {
	t.Parallel()
	outage := errors.New("coordination store unavailable: nats: timeout")
	rig := newCoordRigWith(t, func(opts *CoordinatorOptions) {
		opts.NodesFailure = func(_ context.Context, err error) bool { return errors.Is(err, outage) }
	})
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done"})
	rig.resumer.failWith(fmt.Errorf("engine: read the rebase: %w", outage))

	attempt := func() error {
		payload, ev := rig.completion("t1")
		return rig.coordinator.OnCompleted(t.Context(), payload, ev)
	}
	for i, after := range []time.Duration{0, 20 * time.Minute, 11 * time.Minute, 5 * time.Minute} {
		rig.now = rig.now.Add(after)
		if err := attempt(); err == nil {
			t.Fatalf("failure %d was not handed back for a retry", i+1)
		}
		if got := rig.get("t1"); got.Status != StatusRunning || got.Launch.ResumeFailures != 0 {
			t.Fatalf("after the node's failure %d, thirty-six minutes at most into the outage, the row "+
				"is %s with %d failures counted, want it running with none", i+1, got.Status,
				got.Launch.ResumeFailures)
		}
	}
	if rig.gone("t1") || len(rig.failures()) != 0 {
		t.Fatalf("a run whose resume only the node failed was ended: failures %+v", rig.failures())
	}
	rig.resumer.failWith(errors.New("the turn could not be re-entered"))
	if err := attempt(); err == nil {
		t.Fatal("the run's own failure was not handed back for a retry")
	}
	if got := rig.get("t1"); got.Launch.ResumeFailures != 1 {
		t.Fatalf("the run's own failure after the outage counted %d, want 1", got.Launch.ResumeFailures)
	}
}

// A RESUME ANOTHER BUILD CAN MAKE COUNTS TOWARD THE GIVE-UP ONLY ONCE NO LIVE
// NODE CAN. A parked state a newer build wrote, met by an older node still
// holding the seat through a roll, failed every attempt as a routing failure,
// and counted, it ended at half an hour a run a newer node had parked, filed
// as the engine's own defect. While a node that reads the state is live, the
// failure is handed back and counts nothing; once none is, as after a
// rollback, it counts from one, and the give-up ends the run as it ends any
// other, so the seat is not held for ever.
//
// Mutation: count it whatever the fleet says, and the newer node's half hour
// ends the run; skip it whatever the fleet says, and the rollback holds the
// seat for ever.
func TestAResumeAnotherBuildCanMakeIsCountedOnlyOnceNoLiveNodeCan(t *testing.T) {
	t.Parallel()
	var newerLive atomic.Bool
	newerLive.Store(true)
	var asked atomic.Value
	rig := newCoordRigWith(t, func(opts *CoordinatorOptions) {
		opts.ResumableElsewhere = func(_ context.Context, run PendingRun) (bool, error) {
			asked.Store(run.TurnID)
			return newerLive.Load(), nil
		}
	})
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done"})
	rig.resumer.failWith(fmt.Errorf("%w: execstate: unknown version: 9 (this build reads 8)",
		ErrResumeElsewhere))

	attempt := func() error {
		payload, ev := rig.completion("t1")
		return rig.coordinator.OnCompleted(t.Context(), payload, ev)
	}
	for i, after := range []time.Duration{0, 20 * time.Minute, 11 * time.Minute, 5 * time.Minute} {
		rig.now = rig.now.Add(after)
		if err := attempt(); !errors.Is(err, ErrResumeElsewhere) {
			t.Fatalf("failure %d = %v, want it handed back for a node that can resume it", i+1, err)
		}
		if got := rig.get("t1"); got.Status != StatusRunning || got.Launch.ResumeFailures != 0 {
			t.Fatalf("after failure %d with a node of the newer build live, the row is %s with %d "+
				"failures counted, want it running with none", i+1, got.Status, got.Launch.ResumeFailures)
		}
	}
	if asked.Load() != "t1" {
		t.Fatalf("the fleet was asked about %v, want the run that failed, t1", asked.Load())
	}
	// THE ROLLBACK: no live node reads the state any more.
	newerLive.Store(false)
	for i, after := range []time.Duration{0, 20 * time.Minute} {
		rig.now = rig.now.Add(after)
		if err := attempt(); err == nil {
			t.Fatalf("failure %d after the rollback was not handed back", i+1)
		}
		if got := rig.get("t1"); got.Launch.ResumeFailures != i+1 {
			t.Fatalf("failure %d after the rollback counted %d, want %d", i+1,
				got.Launch.ResumeFailures, i+1)
		}
	}
	rig.now = rig.now.Add(11 * time.Minute)
	if err := attempt(); err != nil {
		t.Fatalf("the failure that spans the give-up window after the rollback asked for a retry: %v", err)
	}
	if !rig.gone("t1") || !rig.endedWith("t1", types.ExecutorFailureResumeFailing) {
		t.Fatalf("the run no live node can resume was not given up: failures %+v", rig.failures())
	}
}

// A RESUME FAILING ON A CONDITION ONLY AN OPERATOR CLEARS IS RAISED FOR THE
// OPERATOR while the run waits. The give-up never ends such a run (the failure
// is the node's), so its seat waits for as long as the condition lasts, and
// each attempt's error line, repeated on every announcement, was the only
// sign of it. The coordinator counts the run with its seat, the condition's
// kind and when the first such failure in a row was met, an attempt that
// meets the condition again keeping that instant; and takes it out once a
// resume gets past the condition, once one fails for anything else, once the
// run settles, and once the seat is let go of. COUNTED ONCE THE ATTEMPT'S
// HAND-BACK LANDS: an attempt whose own hand-back failed ends the run, and one
// whose run had its ending decided by another path while the resume ran (a
// person's stop of the run the turn relaunched) finds it moved on, and counted
// as the failure was met, or as the attempt returned, after that ending had
// taken it out, a run that was over held the alarm up until its seat moved. A
// resume whose turn relaunched before it failed is out, its hand-back finding
// the turn's next job on the row. A hand-back the store answers late counts the
// run once it lands, and an ending
// that lands beside it, before its answer is taken, leaves it uncounted. A
// failure of the node's that a wait clears (a coordination store that did not
// answer) is never counted.
//
// Mutation: record no failure, keep the latest instant rather than the first,
// drop the record on any one of the ways out, count the failure as it is met
// or as the attempt returns rather than once its hand-back lands, count it in
// the step that takes the landing's answer whatever settled before it, or
// keep it where the hand-back did not land, and a row goes red.
func TestAResumeOnlyAnOperatorClearsIsRaisedWhileTheRunWaits(t *testing.T) {
	t.Parallel()
	full := errors.New("write: no space left on device")
	outage := errors.New("nats: timeout")
	rigFor := func(t *testing.T) *coordRig {
		rig := newCoordRigWith(t, func(opts *CoordinatorOptions) {
			opts.NodesFailure = func(_ context.Context, err error) bool {
				return errors.Is(err, full) || errors.Is(err, outage)
			}
			opts.OperatorsCondition = func(err error) (string, bool) {
				return "disk_full", errors.Is(err, full)
			}
		})
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "done"})
		return rig
	}
	attempt := func(t *testing.T, rig *coordRig) error {
		t.Helper()
		payload, ev := rig.completion("t1")
		return rig.coordinator.OnCompleted(t.Context(), payload, ev)
	}
	blockedFor := func(t *testing.T, rig *coordRig, want ResumesBlocked) {
		t.Helper()
		got := rig.coordinator.ResumesBlocked()
		if got.Runs != want.Runs || got.Seat != want.Seat || got.Kind != want.Kind || !got.Since.Equal(want.Since) {
			t.Fatalf("resumes waiting for an operator = %+v, want %+v", got, want)
		}
	}
	waiting := func(t *testing.T, rig *coordRig) {
		t.Helper()
		rig.resumer.failWith(fmt.Errorf("resuming t1: %w", full))
		first := rig.now
		for i, after := range []time.Duration{0, 20 * time.Minute, 20 * time.Minute} {
			rig.now = rig.now.Add(after)
			if err := attempt(t, rig); err == nil {
				t.Fatalf("the premise: attempt %d on a full disk is handed back", i+1)
			}
		}
		if rig.gone("t1") || rig.get("t1").Launch.ResumeFailures != 0 {
			t.Fatal("the premise: a resume only the operator can unblock is never counted or ended")
		}
		blockedFor(t, rig, ResumesBlocked{Runs: 1, Seat: "swe", Kind: "disk_full", Since: first})
	}
	t.Run("a resume that gets past it", func(t *testing.T) {
		t.Parallel()
		rig := rigFor(t)
		waiting(t, rig)
		rig.resumer.failWith(nil)
		if err := attempt(t, rig); err != nil {
			t.Fatalf("the resume after the operator's fix: %v", err)
		}
		blockedFor(t, rig, ResumesBlocked{})
	})
	t.Run("a resume that fails for anything else", func(t *testing.T) {
		t.Parallel()
		rig := rigFor(t)
		waiting(t, rig)
		rig.resumer.failWith(errors.New("the turn could not be re-entered"))
		if err := attempt(t, rig); err == nil {
			t.Fatal("the premise: the run's own failure is handed back")
		}
		blockedFor(t, rig, ResumesBlocked{})
	})
	t.Run("the seat let go of", func(t *testing.T) {
		t.Parallel()
		rig := rigFor(t)
		waiting(t, rig)
		rig.coordinator.ReleaseSeat("swe")
		blockedFor(t, rig, ResumesBlocked{})
	})
	t.Run("the run settled", func(t *testing.T) {
		t.Parallel()
		rig := rigFor(t)
		waiting(t, rig)
		rig.coordinator.runSettled("t1")
		blockedFor(t, rig, ResumesBlocked{})
	})
	t.Run("a hand-back that failed, which ends the run", func(t *testing.T) {
		t.Parallel()
		rig := rigFor(t)
		waiting(t, rig)
		rig.coordinator.pending = &refusingStore{inner: rig.coordinator.pending, refuse: []string{"ReleaseClaim"}}
		if err := attempt(t, rig); err != nil {
			t.Fatalf("the premise: an attempt whose claim cannot be handed back ends the run: %v", err)
		}
		if !rig.gone("t1") {
			t.Fatal("the premise: the run whose claim could not be handed back is ended")
		}
		blockedFor(t, rig, ResumesBlocked{})
	})
	t.Run("an ending decided while its resume runs", func(t *testing.T) {
		t.Parallel()
		rig := rigFor(t)
		waiting(t, rig)
		// THE TURN RELAUNCHES ITS EXECUTOR, and a person stops the seat's
		// running work before the turn meets the full disk: the stop ends
		// the relaunched run while the resume's frame still runs.
		rig.resumer.failsInTurn = true
		rig.resumer.during = func(ctx context.Context, r PendingRun) {
			if _, err := rig.launchVia(ctx, rig.manager, launchReq(r.TurnID)); err != nil {
				t.Errorf("the turn's relaunch: %v", err)
			}
			if err := rig.coordinator.StopSeat(ctx, "swe", stopper); err != nil {
				t.Errorf("the person's stop: %v", err)
			}
		}
		_ = attempt(t, rig)
		if run, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || (found && run.Ending == nil) {
			t.Fatalf("the premise: the stop decided the run's ending (%+v, %v, %v)", run.Ending, found, err)
		}
		blockedFor(t, rig, ResumesBlocked{})
	})
	t.Run("a resume whose turn relaunched before it failed", func(t *testing.T) {
		t.Parallel()
		rig := rigFor(t)
		waiting(t, rig)
		// THE TURN'S NEXT JOB RUNS, so its run waits on nothing: the claim's
		// hand-back finds the row holding that job and does not land.
		rig.resumer.failsInTurn = true
		rig.resumer.during = func(ctx context.Context, r PendingRun) {
			if _, err := rig.launchVia(ctx, rig.manager, launchReq(r.TurnID)); err != nil {
				t.Errorf("the turn's relaunch: %v", err)
			}
		}
		_ = attempt(t, rig)
		if got := rig.get("t1"); got.Ending != nil || !slices.Contains(Active, got.Status) {
			t.Fatalf("the premise: the relaunched run is live (%s, ending %+v)", got.Status, got.Ending)
		}
		blockedFor(t, rig, ResumesBlocked{})
	})
	t.Run("a hand-back the store answers late", func(t *testing.T) {
		t.Parallel()
		rig := rigFor(t)
		store := &unansweredRelease{silentStore: &silentStore{PendingStore: rig.coordinator.pending,
			silent: []string{"ReleaseClaim"}}}
		rig.coordinator.pending = store
		rig.resumer.failWith(fmt.Errorf("resuming t1: %w", full))
		met := rig.now
		if err := attempt(t, rig); err == nil {
			t.Fatal("the premise: the attempt on a full disk is not settled")
		}
		blockedFor(t, rig, ResumesBlocked{})
		store.answer()
		if fired := rig.fireRetries(); fired == 0 {
			t.Fatal("the premise: the release the store did not answer is owed and made again")
		}
		if got := rig.get("t1"); got.Status != StatusRunning {
			t.Fatalf("the premise: the owed hand-back landed, but the run is %s", got.Status)
		}
		blockedFor(t, rig, ResumesBlocked{Runs: 1, Seat: "swe", Kind: "disk_full", Since: met})
	})
	t.Run("an ending that lands beside the hand-back", func(t *testing.T) {
		t.Parallel()
		rig := rigFor(t)
		waiting(t, rig)
		rig.coordinator.pending = &settledAsReleased{PendingStore: rig.coordinator.pending,
			settle: func() { rig.coordinator.runSettled("t1") }}
		if err := attempt(t, rig); err == nil {
			t.Fatal("the premise: the attempt on a full disk is handed back")
		}
		blockedFor(t, rig, ResumesBlocked{})
	})
	t.Run("a failure of the node's a wait clears", func(t *testing.T) {
		t.Parallel()
		rig := rigFor(t)
		rig.resumer.failWith(fmt.Errorf("resuming t1: %w", outage))
		if err := attempt(t, rig); err == nil {
			t.Fatal("the premise: the outage's failure is handed back")
		}
		blockedFor(t, rig, ResumesBlocked{})
	})
}

// settledAsReleased is a run store whose claim's release lands and then, before
// its answer is taken, has the run settled on this node by settle: an ending
// decided on another path between the release's landing on the store and the
// step that takes its answer.
type settledAsReleased struct {
	PendingStore
	settle func()
}

func (s *settledAsReleased) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	released, err := s.PendingStore.ReleaseClaim(ctx, turnID, release)
	s.settle()
	return released, err
}

// claimBehindRelease is a run store whose next claim of a job, on its way to the
// store, is overtaken by before: a claim already on its way when a hand-back of
// the job's last claim went out ([Coordinator.claimCompletion]).
type claimBehindRelease struct {
	PendingStore
	before func(ctx context.Context)
}

func (s *claimBehindRelease) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	if before := s.before; before != nil {
		s.before = nil
		before(ctx)
	}
	return s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
}

// A CLAIM THAT PROVES A BLOCKED RESUME'S HAND-BACK LANDED COUNTS ITS RUN. A
// hand-back whose answer the store lost has landed where a claim of its job, on
// its way when the hand-back went out, wins on the run the hand-back put back,
// and that win is the step that takes the hand-back's answer
// ([Coordinator.claimCompletion]): the run of a resume that failed on a
// condition only an operator clears was back where its claim found it, so it is
// counted there with its seat, its condition and the instant the failure was
// met, as the hand-back's own answer would have counted it.
//
// Mutation: leave the count out of the claim's proof, and the run waits for
// the operator uncounted.
func TestAClaimThatProvesABlockedHandBackLandedCountsItsRun(t *testing.T) {
	t.Parallel()
	live := t.Context()
	rig := newCoordRig(t)
	run := rig.launch("t1")
	claimed, won, err := rig.pending.ClaimForResume(live, "t1", CompletionTail(run.LaunchID), rigLease)
	if err != nil || !won {
		t.Fatalf("the premise: the first claim of t1 = %v, %v", won, err)
	}
	payload, _ := rig.completion("t1")
	met := rig.now
	store := &claimBehindRelease{PendingStore: rig.coordinator.pending}
	rig.coordinator.pending = store
	// THE FIRST CLAIM'S HAND-BACK, its resume failed on a full disk, goes out
	// while the second claim is on its way, and lands with its answer lost.
	store.before = func(ctx context.Context) {
		rig.coordinator.mu.Lock()
		cr, moved := rig.coordinator.handingBackLocked(claimed, resumeUnrevertedDetail,
			failure{blocked: "disk_full"}, nil)
		rig.coordinator.mu.Unlock()
		moved()
		if landed, landErr := store.PendingStore.ReleaseClaim(ctx, "t1", cr.release); landErr != nil || !landed {
			t.Errorf("the premise: the hand-back landed = %v, %v", landed, landErr)
		}
	}
	if _, onItsWay, claimErr := rig.coordinator.claimCompletion(live, payload, rigLease); claimErr != nil || !onItsWay {
		t.Fatalf("the claim on its way = %v, %v; want it won on the run the hand-back put back",
			onItsWay, claimErr)
	}
	got := rig.coordinator.ResumesBlocked()
	want := ResumesBlocked{Runs: 1, Seat: "swe", Kind: "disk_full", Since: met}
	if got.Runs != want.Runs || got.Seat != want.Seat || got.Kind != want.Kind || !got.Since.Equal(want.Since) {
		t.Fatalf("resumes waiting for an operator once the claim proved the hand-back landed = %+v, want %+v",
			got, want)
	}
}

// WHAT THE GIVE-UP COUNTS OF A RESUME THIS NODE CANNOT MAKE, by the fleet's
// answer: one another node or build can make counts nothing while a live node
// may, nor while the fleet cannot be read, and counts once none may; one no
// node gets past (a run with no parked state) counts whatever the fleet says.
//
// Mutation: count a failure whose fleet read failed, or skip every resume
// this node cannot make, and a row goes red.
func TestWhatTheGiveUpCountsOfAResumeThisNodeCannotMake(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err     error
		live    bool
		readErr error
		counted int
	}{
		"another build's, a node of it live": {err: fmt.Errorf("%w: a newer state",
			ErrResumeElsewhere), live: true},
		"another build's, none live": {err: fmt.Errorf("%w: a newer state", ErrResumeElsewhere),
			counted: 1},
		"another build's, the fleet unread": {err: fmt.Errorf("%w: a newer state", ErrResumeElsewhere),
			readErr: errors.New("coordination store unavailable")},
		"one no node gets past, a node live": {err: fmt.Errorf("%w: run t1 has no parked turn state",
			ErrResumeUnavailable), live: true, counted: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRigWith(t, func(opts *CoordinatorOptions) {
				opts.ResumableElsewhere = func(context.Context, PendingRun) (bool, error) {
					return tc.live, tc.readErr
				}
			})
			rig.launch("t1")
			rig.runner.Finish(Result{Success: true, Text: "done"})
			rig.resumer.failWith(tc.err)
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
				t.Fatal("the premise: a resume that failed is handed back")
			}
			if got := rig.get("t1").Launch.ResumeFailures; got != tc.counted {
				t.Errorf("%v counted %d toward the give-up, want %d", tc.err, got, tc.counted)
			}
		})
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
