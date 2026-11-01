package runner

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/structured"
)

// A SUBMISSION MADE BEFORE A PERSON'S ANSWER IS NOT THE CARRIED-ON ROUND'S. A
// run carried on by a person's answer keeps the calls it made before its
// question (ADR-0068), and a `submit_work` among them was made without the
// answer: replayed, the turn would report it for the work the run did after,
// or for a run that never submitted again. The cut is the place the row
// recorded when the continuing job opened ([Resume.Answered]), so it holds
// where the ask itself is gone from the bounded log, and where the asking job
// submitted after its own question as well as before it. A round no answer
// carried on keeps every submission, after a refused ask too.
//
// Mutation: replay every submission whatever the cut, or cut after the round's
// last ask instead, and the cases naming the asking job's submissions report
// them.
func TestASubmissionBeforeTheAnswerIsNotReplayed(t *testing.T) {
	t.Parallel()
	const asked = "ask_and_wait"
	at := func(seq int, call ledger.Call) BridgedCall { return BridgedCall{Call: call, Seq: seq} }
	submit := func(summary string) ledger.Call {
		return ledger.Call{Name: SubmitWorkTool, Args: map[string]any{"summary": summary}}
	}
	ask := ledger.Call{Name: asked, Args: map[string]any{"question": "Which branch?"}}
	refusedAsk := ledger.Call{Name: asked, Failed: true}
	read := ledger.Call{Name: "read_file", Args: map[string]any{"path": "go.mod"}}
	for _, tc := range []struct {
		name     string
		calls    []BridgedCall
		answered int
		want     string
	}{
		{"submitted, then asked, then carried on with no submission",
			[]BridgedCall{at(1, submit("before")), at(2, ask), at(3, read)}, 2, ""},
		{"the asking job submitted after its question too",
			[]BridgedCall{at(1, submit("before")), at(2, ask), at(3, submit("past the question"))}, 3, ""},
		{"the asking job submitted after its question, then read on",
			[]BridgedCall{at(1, ask), at(2, submit("pre-answer")), at(3, read)}, 3, ""},
		{"the carried-on job submitted",
			[]BridgedCall{at(1, submit("before")), at(2, ask), at(3, submit("past the question")),
				at(4, read), at(5, submit("carried on"))}, 3, "carried on"},
		{"the ask was dropped from the middle of the log",
			[]BridgedCall{at(1, submit("before")), at(7, read)}, 5, ""},
		{"no answer carried the round on, and a refused ask ends nothing",
			[]BridgedCall{at(1, submit("before")), at(2, refusedAsk)}, 0, "before"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tool := structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
				func(args map[string]any) (workPayload, error) {
					summary, _ := args["summary"].(string)
					return workPayload{Summary: summary}, nil
				})
			since := bridgedLedger(sinceAnswer(tc.calls, tc.answered))
			replaySubmission(t.Context(), tool, since)
			got, replayed := tool.Value()
			if replayed != (tc.want != "") || got.Summary != tc.want {
				t.Fatalf("replayed %q (submitted %v), want %q", got.Summary, replayed, tc.want)
			}
			// AND A LOST JOB'S ROUND READS AS SUBMITTED BY THE SAME CALLS
			// ([submitted]), or a loss after the answer would close the
			// round on a submission the replay then drops.
			if submitted(since) != (tc.want != "") {
				t.Fatalf("submitted(%v) = %v, want %v", since, submitted(since), tc.want != "")
			}
		})
	}
}
