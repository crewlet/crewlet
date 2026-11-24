package sandbox

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// THE PARKED CALLS REACH THE RESUMED RUN, WHATEVER CARRIES IT ON. A job parked
// on its own calls alone is carried on by the engine from its row, told those
// calls returned, never as a person's words; and a person's answer to a job
// that parked calls beside its question, recorded, sealed and opened, carries
// every one of those calls on, as the person's answer.
//
// Mutation: drop the row's calls from askedRun, and the calls parked beside a
// question are never told; build the engine's carrying on from the question's
// path (CallsReturned false), and the run reads a person's answer.
func TestTheParkedCallsReachTheResumedRunWhateverCarriesItOn(t *testing.T) {
	t.Parallel()
	seal := rigAnswers()
	calls := []CallWait{{Tool: "delegate", Seq: 3}, {Tool: "post_report", Seq: 5}}
	want := []types.CallReturned{{Tool: "delegate", Seq: 3}, {Tool: "post_report", Seq: 5}}

	marked := slices.Clone(calls)
	marked[0].Ask = true
	own := PendingRun{TurnID: "t1", LaunchID: "launch-1", Question: CallWaitQuestion("delegate"),
		ParkedCalls: marked}
	asked, ok := (&Coordinator{}).carriedOn(t.Context(), own)
	if !ok || !asked.CallsReturned || asked.Answer != "" || asked.By != "" || !reflect.DeepEqual(asked.Called, want) {
		t.Errorf("the engine carries the run on with %+v (%v); want its calls %+v returned, no answer", asked,
			ok, want)
	}

	beside := PendingRun{TurnID: "t1", LaunchID: "launch-1", Question: "Which branch?", ParkedCalls: calls}
	given := types.ExecutorAnswerGiven{TurnID: "t1", LaunchID: "launch-1", Answer: "use main",
		AnsweredBy: "founder", AnsweredByKind: "human"}
	words, answer, err := answerGivenOf(beside, given, events.New(given, events.TraceContext{}), time.Now().UTC())
	if err != nil {
		t.Fatalf("answerGivenOf: %v", err)
	}
	sealed, err := seal.Seal(beside, words, answer)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	beside.Answer = &sealed
	opened, err := seal.Open(beside)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	asked = askedRun(beside, opened)
	if asked.CallsReturned || !reflect.DeepEqual(asked.Called, want) || asked.Answer != "use main" {
		t.Errorf("the person's answer resumes the run with %q, calls %+v, returned %v; want their words "+
			"and every parked call", asked.Answer, asked.Called, asked.CallsReturned)
	}
}
