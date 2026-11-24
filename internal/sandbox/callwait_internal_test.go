package sandbox

import (
	"testing"
)

// A PARK ON A CALL NAMES ITS CALL AND ASKS NOTHING ELSE. Recorded beside
// whatever its job asked, it names the tool and the call's place, and never a
// question, an audience or a run of its own; its mark as the job's ask is
// written by the park alone, never asked for.
//
// Mutation: drop the call's own checks (validate answering nothing), and a
// park naming no call is recorded; accept the mark from an ask, and a run may
// ask the engine to answer it as its calls'.
func TestAParkOnACallNamesItsCallAndAsksNothingElse(t *testing.T) {
	t.Parallel()
	if err := (Ask{Call: &CallWait{Tool: "delegate", Seq: 1}}).Validate(); err != nil {
		t.Errorf("a park on a call was refused: %v", err)
	}
	for name, ask := range map[string]Ask{
		"no tool":        {Call: &CallWait{Seq: 1}},
		"no place":       {Call: &CallWait{Tool: "delegate"}},
		"a run beside":   {Call: &CallWait{Tool: "delegate", Seq: 1}, Wait: &RunWait{Turn: "t2", Seat: "swe"}},
		"a question":     {Call: &CallWait{Tool: "delegate", Seq: 1}, Question: "Which?", To: "requester"},
		"marked its ask": {Call: &CallWait{Tool: "delegate", Seq: 1, Ask: true}},
	} {
		if err := ask.Validate(); err == nil {
			t.Errorf("%s: the ask was recorded", name)
		}
	}
}
