package sandbox

import (
	"testing"
	"time"
)

// A DELIVERY THAT SAYS NOTHING NEVER ANSWERS A PARKED QUESTION, even from the
// seat it was put to: a field change, an alarm or a pickup on the question's
// conversation runs as the ordinary event it is, and the run keeps waiting.
func TestADeliveryThatSaysNothingIsNeverTakenAsTheAnswer(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	asked := rig.get("t1").AskedAt

	change := chatReply(answerOnTheDM, "status: done", replyAt("status: done", asked.Add(time.Minute)))
	change.Says = false
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", change); d != AnswerNotMine {
		t.Fatalf("a delivery that says nothing = %q, want it left to the seat", d)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.Answer != nil {
		t.Fatalf("run %q with answer %+v, want it still waiting", got.Status, got.Answer)
	}
	reply := chatReply(answerOnTheDM, "use main", replyAt("use main", asked.Add(2*time.Minute)))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); d != AnswerConsumed {
		t.Fatalf("the asked seat's message = %q, want it recorded as the answer", d)
	}
}
