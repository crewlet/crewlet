package sandbox

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/crewlet/crewlet/internal/events/types"
)

// launchCapped is [waiterRig.launch] of a run its launch handed turnCap.
func (r *waiterRig) launchCapped(turnID string, turnCap int) PendingRun {
	r.t.Helper()
	return r.launchWith(turnID, func(run *PendingRun) { run.Launch.TurnCap = turnCap })
}

// A RUN ITS STREAM SHOWS PAST ITS TURN CAP IS ENDED WITHIN ONE TICK OF THE
// WATCH, while it still works: its record says so before anything else, and
// its completion goes out in the same tick, so its collection ends the job.
// Past its cap by no more than its runner's requests in flight it is left to
// its harness's own stop, which an end would erase; a cap the count cannot
// judge (lines that may each hide a request) ends it as the collection would
// fail it.
//
// Mutation: drop the watch, count at collection only, or end at more requests
// than the cap whatever is in flight, and a case goes red.
func TestARunPastItsTurnCapIsEndedWithinOneTickOfTheWatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		shown RequestCount
		ended bool
	}{
		{name: "past its cap and what is in flight", shown: RequestCount{Requests: 5, InFlight: 1}, ended: true},
		{name: "past its cap by what is in flight", shown: RequestCount{Requests: 4, InFlight: 1}},
		{name: "at its cap", shown: RequestCount{Requests: 3}},
		{name: "with lines that may hide a request past it", shown: RequestCount{Requests: 3, Undecided: 1},
			ended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRig(t)
			rig.launchCapped("t1", 3)
			if fired := rig.tick(); fired != 0 {
				t.Fatalf("the premise: the poll fired %d completions of a job still working", fired)
			}
			rig.runner.Show(tc.shown)
			published := rig.queue.count()

			fired := rig.waiter.WatchTick(t.Context())
			if len(rig.runner.Counted()) != 1 {
				t.Fatalf("the watch read %d counts, want the capped run's", len(rig.runner.Counted()))
			}
			row := rig.get("t1")
			if got := fired == 1 && row.Launch.EndedAtTurnCap && rig.queue.count() > published; got != tc.ended {
				t.Fatalf("a count of %+v under a cap of 3: fired %d, recorded %v, published %d; want ended: %v",
					tc.shown, fired, row.Launch.EndedAtTurnCap, rig.queue.count()-published, tc.ended)
			}
		})
	}
}

// THE WATCH READS ONLY WHAT IS ITS OWN TO END: a capped running run this node
// launched. An uncapped run and a run another node drives are never read,
// whatever their streams show.
//
// Mutation: watch every running run, or every capped one whichever node drives
// it, and a count is read.
func TestTheWatchReadsOnlyCappedRunsItsNodeDrives(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("uncapped")
	rig.launchWith("elsewhere", func(run *PendingRun) {
		run.Launch.TurnCap, run.LaunchNode = 3, "node-b"
	})
	rig.tick()
	rig.runner.Show(RequestCount{Requests: 50})
	if fired := rig.waiter.WatchTick(t.Context()); fired != 0 || len(rig.runner.Counted()) != 0 {
		t.Fatalf("the watch read %d counts and ended %d runs; want none of an uncapped run's or "+
			"another node's", len(rig.runner.Counted()), fired)
	}
}

// A RUN ITS LAUNCHING NODE JUST OPENED TO ITS POLL IS WATCHED AT ONCE
// ([Waiter.Watch]), not from the waiter's next pass, an interval away.
//
// Mutation: make Watch read nothing, or watch the run only once a pass has
// listed it, and the tick reads no count.
func TestARunJustOpenedToItsPollIsWatchedAtOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := newWaiterRig(t)
		rig.launchCapped("t1", 3)
		rig.waiter.Start(t.Context())
		defer rig.waiter.Stop()
		rig.waiter.Watch("t1")
		synctest.Wait()
		rig.runner.Show(RequestCount{Requests: 4})
		if fired := rig.waiter.WatchTick(t.Context()); fired != 1 || len(rig.runner.Counted()) != 1 {
			t.Fatalf("the watch read %d counts and ended %d runs; want the run just opened ended",
				len(rig.runner.Counted()), fired)
		}
	})
}

// A RUN WHOSE RECORD SAYS ITS WATCH ENDED IT IS ANNOUNCED BY THE POLL, as one
// past its deadline is: by the node that recorded it, should its own
// announcement not have gone out, and after a restart that forgot it.
//
// Mutation: poll a run its record says was ended at its turn cap as one still
// working, and no completion goes out.
func TestARunItsWatchEndedIsAnnouncedByThePoll(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launchCapped("t1", 3)
	if ok, err := rig.pending.MarkEndedAtTurnCap(t.Context(), "t1", run.LaunchID); err != nil || !ok {
		t.Fatalf("MarkEndedAtTurnCap = %v, %v", ok, err)
	}
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("the poll fired %d completions of a run its watch ended, want its one", fired)
	}
}

// A RUN ITS WATCH ENDED IS COLLECTED AS PAST ITS TURN CAP: its job is asked to
// end before its box is read, it fails as `turn_cap_exceeded` whatever it
// reported, says the engine ended it at its turn cap and never that a
// wall-clock cap did, and is parked on no question it asked on its way out,
// the question said in its note.
//
// Mutation: read the box before ending the job, leave the record's word out of
// the collection, word it as the deadline, or park it, and a check goes red.
func TestARunItsWatchEndedIsCollectedAsPastItsTurnCap(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	run := rig.launchCapped("t1", 3)
	rig.coordinator.countRun("swe", StatusRunning)
	rig.ask("t1", Ask{Question: "which branch?", To: AskToRequester, Decision: choosing("which branch?"),
		Branch: "wip/t1"})
	if ok, err := rig.pending.MarkEndedAtTurnCap(t.Context(), "t1", run.LaunchID); err != nil || !ok {
		t.Fatalf("MarkEndedAtTurnCap = %v, %v", ok, err)
	}
	rig.runner.EndsAs(Result{Text: "halfway through", JobEnd: JobEndAsked, Error: "the run was ended on " +
		"request before it finished: its harness was told to stop and exited 143"})
	askedFirst := false
	rig.runner.CollectFunc = func(context.Context) error {
		askedFirst = rig.runner.Asks() == 1
		return nil
	}

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if !askedFirst {
		t.Fatalf("the box was read with %d requests to end its job made, want the one before", rig.runner.Asks())
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want the turn back with what its run did", len(calls))
	}
	if calls[0].Success || !strings.Contains(calls[0].Answer, "turn cap of 3") ||
		strings.Contains(calls[0].Answer, "wall-clock") || !strings.Contains(calls[0].Answer, "It had asked: which branch?") {
		t.Errorf("the resume reads success %v and %q; want the turn cap's ending, the question it had asked "+
			"said, and no wall-clock cap", calls[0].Success, calls[0].Answer)
	}
	recs := rig.collected()
	if len(recs) != 1 || !recs[0].Failed || recs[0].ErrorKind != types.ExecutorErrorTurnCapExceeded {
		t.Fatalf("the run's record is %+v; want it failed as %s", recs, types.ExecutorErrorTurnCapExceeded)
	}
	if asked := rig.questions(); len(asked) != 0 || calls[0].Run.Question != "" {
		t.Errorf("announced %+v and resumed carrying %q; a run its turn cap ended asks nobody", asked,
			calls[0].Run.Question)
	}
}
