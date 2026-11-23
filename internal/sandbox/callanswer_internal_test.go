package sandbox

import (
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE PARKED CALLS REACH THE RESUMED RUN THROUGH THE ANSWER'S SEAL. An answer
// by turn the engine gives a job parked on its own calls alone is recorded,
// sealed, opened and carried into the resume as those calls returning, never
// as a person's words; and a person's answer to a job that parked calls beside
// its question carries every one of those calls on, as the person's answer.
// The real path end to end: answerGivenOf, Seal, Open, askedRun.
//
// Mutation: drop Called from the seal (Seal or Open), and the engine's answer
// resumes the run as a person's; drop the row's calls from askedRun, and the
// calls parked beside a question are never told.
func TestTheParkedCallsReachTheResumedRunThroughTheAnswersSeal(t *testing.T) {
	t.Parallel()
	seal := rigAnswers()
	calls := []CallWait{{Tool: "delegate", Seq: 3}, {Tool: "post_report", Seq: 5}}
	want := []types.CallReturned{{Tool: "delegate", Seq: 3}, {Tool: "post_report", Seq: 5}}
	resumed := func(run PendingRun, given types.ExecutorAnswerGiven) *AskedRun {
		t.Helper()
		words, answer, err := answerGivenOf(run, given, events.New(given, events.TraceContext{}), time.Now().UTC())
		if err != nil {
			t.Fatalf("answerGivenOf: %v", err)
		}
		sealed, err := seal.Seal(run, words, answer)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		run.Answer = &sealed
		opened, err := seal.Open(run)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return askedRun(run, opened)
	}

	own := PendingRun{TurnID: "t1", LaunchID: "launch-1", Question: CallWaitQuestion("delegate"),
		WaitsFor: &RunWait{OwnCalls: true}, ParkedCalls: calls}
	asked := resumed(own, types.ExecutorAnswerGiven{TurnID: "t1", LaunchID: "launch-1",
		Answer: "Your calls returned.", AnsweredByKind: string(iam.ActorSystem), Called: want})
	if !asked.CallsReturned || !reflect.DeepEqual(asked.Called, want) {
		t.Errorf("the engine's answer resumes the run with calls %+v, returned %v; want %+v, returned",
			asked.Called, asked.CallsReturned, want)
	}

	beside := PendingRun{TurnID: "t1", LaunchID: "launch-1", Question: "Which branch?", ParkedCalls: calls}
	asked = resumed(beside, types.ExecutorAnswerGiven{TurnID: "t1", LaunchID: "launch-1", Answer: "use main",
		AnsweredBy: "founder", AnsweredByKind: "human"})
	if asked.CallsReturned || !reflect.DeepEqual(asked.Called, want) || asked.Answer != "use main" {
		t.Errorf("the person's answer resumes the run with %q, calls %+v, returned %v; want their words "+
			"and every parked call", asked.Answer, asked.Called, asked.CallsReturned)
	}
}
