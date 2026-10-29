package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN'S QUESTION IS AS MUCH ITS END AS ITS SUBMISSION. The waiter that drives
// a run is nudged once a call that ends it is on its row, so a run that asked
// is collected and parked at once rather than at the waiter's next pass; a
// call the gate refused ends nothing.
//
// Mutation: nudge on `submit_work` alone, and the asking run is not nudged.
func TestARunsAskEndsItLikeItsSubmission(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{runner.SubmitWorkTool, builtin.AskAndWaitTool} {
		if !endsRun(tool) {
			t.Errorf("a successful %s does not end the run, so its waiter is not nudged", tool)
		}
	}
	if endsRun(builtin.CommentOnWorkTool) {
		t.Errorf("%s ends the run, and the run goes on after it", builtin.CommentOnWorkTool)
	}
}

// A NODE WITHOUT A SANDBOX RUNTIME RECORDS NO QUESTION, and says why: there is
// no row for it to land on, and the tool answers the run that the question was
// not asked rather than that it was.
func TestAnAskOnANodeWithNoRunStoreIsAnError(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	got, err := runAsks{engine: e}.RecordAsk(t.Context(), "t1", "l1",
		sandbox.Ask{Question: "Which branch?", To: sandbox.AskToRequester})
	if err == nil || got != "" {
		t.Fatalf("RecordAsk = %q, %v; want the missing store said", got, err)
	}
}
