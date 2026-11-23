package sandbox

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// AN ANSWER SAYS ONLY THE CALL ITS JOB PARKED ON RETURNED. An answer by turn
// saying a bridged call returned is recorded only on a run whose job is parked
// on that very call, by its tool and its place; a run waiting on another run,
// or on another call, takes no such answer, so no run is told a call of its
// returned that it never parked on.
//
// Mutation: take the call whatever the row waits on (any-call), and a run
// waiting on another run is told its call returned.
func TestAnAnswerSaysOnlyTheCallItsJobParkedOnReturned(t *testing.T) {
	t.Parallel()
	parked := PendingRun{TurnID: "t1", WaitsFor: &RunWait{Call: &CallWait{Tool: "delegate", Seq: 3}}}
	returned := &types.CallReturned{Tool: "delegate", Seq: 3}
	if got, err := calledOf(parked, returned); err != nil || got == nil || *got != *returned {
		t.Errorf("the parked call's own answer is taken as %+v, %v", got, err)
	}
	for name, run := range map[string]PendingRun{
		"a run waiting on nothing":     {TurnID: "t1"},
		"a run waiting on another run": {TurnID: "t1", WaitsFor: &RunWait{Turn: "t2", Seat: "swe"}},
		"another call's place":         {TurnID: "t1", WaitsFor: &RunWait{Call: &CallWait{Tool: "delegate", Seq: 2}}},
		"another tool":                 {TurnID: "t1", WaitsFor: &RunWait{Call: &CallWait{Tool: "read", Seq: 3}}},
	} {
		if _, err := calledOf(run, returned); err == nil {
			t.Errorf("%s: the call was taken as returned", name)
		}
	}
	if got, err := calledOf(parked, nil); got != nil || err != nil {
		t.Errorf("an answer saying nothing of a call is taken as %+v, %v", got, err)
	}
}

// A CALL WAIT NAMES ITS CALL AND NO RUN. Recorded as the run's ask, it is put
// to nobody and answered by the engine, so it names the tool and the call's
// place, and never a run, a seat or a reason a wait on a run carries.
//
// Mutation: drop the call's own checks (validate answering nothing), and a
// wait naming no call is recorded.
func TestACallWaitNamesItsCallAndNoRun(t *testing.T) {
	t.Parallel()
	ask := func(w *RunWait) Ask { return Ask{Question: CallWaitQuestion("delegate"), Wait: w} }
	if err := ask(&RunWait{Call: &CallWait{Tool: "delegate", Seq: 1}}).Validate(); err != nil {
		t.Errorf("a call wait was refused: %v", err)
	}
	for name, w := range map[string]*RunWait{
		"no tool":      {Call: &CallWait{Seq: 1}},
		"no place":     {Call: &CallWait{Tool: "delegate"}},
		"a run beside": {Turn: "t2", Seat: "swe", Call: &CallWait{Tool: "delegate", Seq: 1}},
	} {
		if err := ask(w).Validate(); err == nil {
			t.Errorf("%s: the call wait was recorded", name)
		}
	}
}
