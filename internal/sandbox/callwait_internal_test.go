package sandbox

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// AN ANSWER SAYS ONLY THE CALLS ITS JOB PARKED ON ALONE RETURNED. An answer by
// turn saying bridged calls returned is recorded only on a run whose job asked
// nothing but to wait on its own calls, naming every one of them by its tool
// and its place; a run waiting on another run or a person, or on other calls,
// takes no such answer, so no run is told a call of its returned that it never
// parked on, nor has one left untold by the answer that carries it on.
//
// Mutation: take the calls whatever the row waits on (any-call), and a run
// waiting on another run is told its call returned; compare only the first
// call, and an answer leaving the second untold is taken.
func TestAnAnswerSaysOnlyTheCallsItsJobParkedOnAloneReturned(t *testing.T) {
	t.Parallel()
	own := &RunWait{OwnCalls: true}
	two := []CallWait{{Tool: "delegate", Seq: 3}, {Tool: "read", Seq: 5}}
	parked := PendingRun{TurnID: "t1", WaitsFor: own, ParkedCalls: two}
	returned := []types.CallReturned{{Tool: "delegate", Seq: 3}, {Tool: "read", Seq: 5}}
	if got, err := calledOf(parked, returned); err != nil || len(got) != 2 || got[0] != returned[0] ||
		got[1] != returned[1] {
		t.Errorf("the parked calls' own answer is taken as %+v, %v", got, err)
	}
	for name, run := range map[string]PendingRun{
		"a run waiting on nothing":     {TurnID: "t1"},
		"a run waiting on another run": {TurnID: "t1", WaitsFor: &RunWait{Turn: "t2", Seat: "swe"}, ParkedCalls: two},
		"a run asking a person":        {TurnID: "t1", Question: "Which?", ParkedCalls: two},
		"another call's place": {TurnID: "t1", WaitsFor: own,
			ParkedCalls: []CallWait{{Tool: "delegate", Seq: 2}, {Tool: "read", Seq: 5}}},
		"another tool": {TurnID: "t1", WaitsFor: own,
			ParkedCalls: []CallWait{{Tool: "write", Seq: 3}, {Tool: "read", Seq: 5}}},
		"a call more": {TurnID: "t1", WaitsFor: own,
			ParkedCalls: append(append([]CallWait(nil), two...), CallWait{Tool: "post", Seq: 7})},
	} {
		if _, err := calledOf(run, returned); err == nil {
			t.Errorf("%s: the calls were taken as returned", name)
		}
	}
	if _, err := calledOf(parked, returned[:1]); err == nil {
		t.Error("an answer leaving a parked call untold was taken")
	}
	if got, err := calledOf(parked, nil); got != nil || err != nil {
		t.Errorf("an answer saying nothing of a call is taken as %+v, %v", got, err)
	}
}

// A PARK ON A CALL NAMES ITS CALL AND ASKS NOTHING ELSE. Recorded beside
// whatever its job asked, it names the tool and the call's place, and never a
// question, an audience or a run of its own; a wait on the job's own calls is
// written by the park alone, never asked for.
//
// Mutation: drop the call's own checks (validate answering nothing), and a
// park naming no call is recorded; accept a wait on own calls from an ask, and
// a run may ask the engine to answer it as its calls'.
func TestAParkOnACallNamesItsCallAndAsksNothingElse(t *testing.T) {
	t.Parallel()
	if err := (Ask{Call: &CallWait{Tool: "delegate", Seq: 1}}).Validate(); err != nil {
		t.Errorf("a park on a call was refused: %v", err)
	}
	for name, ask := range map[string]Ask{
		"no tool":         {Call: &CallWait{Seq: 1}},
		"no place":        {Call: &CallWait{Tool: "delegate"}},
		"a run beside":    {Call: &CallWait{Tool: "delegate", Seq: 1}, Wait: &RunWait{Turn: "t2", Seat: "swe"}},
		"a question":      {Call: &CallWait{Tool: "delegate", Seq: 1}, Question: "Which?", To: "requester"},
		"own calls asked": {Question: CallWaitQuestion("delegate"), Wait: &RunWait{OwnCalls: true}},
	} {
		if err := ask.Validate(); err == nil {
			t.Errorf("%s: the ask was recorded", name)
		}
	}
}
