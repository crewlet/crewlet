package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/ledger"
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
	// ONE NAME FOR EACH: the runner's submission is the run record's, which
	// the row's bound and this nudge read it by.
	if runner.SubmitWorkTool != sandbox.SubmitWorkTool {
		t.Fatalf("the runner submits through %q and the run record names %q",
			runner.SubmitWorkTool, sandbox.SubmitWorkTool)
	}
	for _, tool := range []string{runner.SubmitWorkTool, builtin.AskAndWaitTool} {
		if !sandbox.EndsRun(tool) {
			t.Errorf("a successful %s does not end the run, so its waiter is not nudged", tool)
		}
	}
	if sandbox.EndsRun(builtin.CommentOnWorkTool) {
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

// A RESUMED ROUND IS CUT WHERE ITS ROW SAYS THE ANSWER CUT IT. The collection
// reports only a submission made after a person's answer carried the round on,
// and the place it cuts at is the row's own (sandbox.PendingRun.AnsweredThrough),
// handed to the runner with the calls it cuts.
//
// Mutation: leave Answered unset, and the runner replays a submission the
// asking job made before it knew the answer.
func TestAResumeIsCutWhereTheAnswerCutTheRound(t *testing.T) {
	t.Parallel()
	got := runnerResume(resumeInput{Run: sandbox.PendingRun{
		AnsweredThrough: 2,
		BridgeCalls: []sandbox.BridgeCall{
			{Name: runner.SubmitWorkTool, Seq: 1},
			{Name: builtin.AskAndWaitTool, Seq: 2},
			{Name: runner.SubmitWorkTool, Seq: 3},
		},
	}}, nil, nil)
	if got.Answered != 2 || len(got.Bridged) != 3 {
		t.Fatalf("the resume is cut at %d over %d calls, want 2 over the row's 3",
			got.Answered, len(got.Bridged))
	}
}

// A RESUME IS SEEDED WITH WHAT ITS ROW COUNTS WITHOUT LISTING. A long run's row
// keeps its calls' head and tail and tallies, under the round it kept them
// for, the middle it dropped and the calls its calls' own work made; the
// resume is handed both as the round's, which the turn's call log is seeded
// with beside the listed calls and its delivery checks read — and a count the
// tally does not account for, an older build's drop, as calls of no known
// tool.
//
// Mutation: hand the resume the row's listed calls alone, and the resumed
// round's seed is short of every call the row dropped and every worker's call.
func TestAResumeIsSeededWithWhatItsRowCountsWithoutListing(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	got := runnerResume(resumeInput{Run: sandbox.PendingRun{
		BridgeCalls:       []sandbox.BridgeCall{{Name: "read_file", Seq: 1, At: first}},
		BridgeCallsElided: 63,
		ElidedCalls:       []sandbox.ElidedCall{{Name: "post_message", Digest: "d", Calls: 60, Failed: 1}},
		AbsorbedCalls:     []sandbox.ElidedCall{{Name: "leave_channel", Digest: "w", Calls: 2}},
		TallyRound:        first,
	}}, nil, nil)
	wantElided := []ledger.Elided{{Calls: 3}, {Name: "post_message", Digest: "d", Calls: 60, Failed: 1}}
	wantAbsorbed := []ledger.Elided{{Name: "leave_channel", Digest: "w", Calls: 2}}
	if !slices.Equal(got.Elided, wantElided) || !slices.Equal(got.Absorbed, wantAbsorbed) {
		t.Errorf("the resume is handed %+v dropped and %+v absorbed, want %+v and %+v", got.Elided,
			got.Absorbed, wantElided, wantAbsorbed)
	}
}
