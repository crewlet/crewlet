package builtin_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// receiptOutcome is the `outcome` a tool's answer states, which the act route
// reads as the write's outcome.
func receiptOutcome(t *testing.T, result tools.Result) statelog.Outcome {
	t.Helper()
	var receipt struct {
		Outcome statelog.Outcome `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(result.Output), &receipt); err != nil {
		t.Fatalf("the answer %s is not a receipt: %v", result.Output, err)
	}
	return receipt.Outcome
}

// A FOLD-IN'S ANSWER IS `applied`, AND ITS OWN RETRY RECORDS NOTHING TWICE: a
// sentence as its `outcome` read as `unknown` on the act route. Another choice,
// or another person's, is still refused naming the first.
func TestAFoldInAnswerIsAppliedAndItsRetryNeverRecordsTwice(t *testing.T) {
	t.Parallel()
	approve := map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "fold_in": "f-1", "choice": "approve"}
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": running(t, "t1")}}
	founder := newAnswerExecutorRig(t, desk, boundTo("founder"), seatLeads)

	first, err := founder.Call(t.Context(), approve)
	if err != nil || first.Failed {
		t.Fatalf("the answer came to %+v, %v", first, err)
	}
	if got := receiptOutcome(t, first); got != statelog.OutcomeApplied {
		t.Fatalf("the answer's outcome is %q, want %q: %s", got, statelog.OutcomeApplied, first.Output)
	}
	retry, err := founder.Call(t.Context(), approve)
	if err != nil || retry.Failed || receiptOutcome(t, retry) != statelog.OutcomeApplied {
		t.Fatalf("the same answer sent again came to %+v, %v; want it applied", retry, err)
	}
	if desk.foldInWrites != 1 {
		t.Fatalf("the answer and its retry recorded %d answers, want 1", desk.foldInWrites)
	}

	changed, err := founder.Call(t.Context(), map[string]any{"turn_id": "t1", "launch_id": "launch-t1",
		"fold_in": "f-1", "choice": "refuse"})
	if err != nil || tools.RefusalOf(changed) != tools.RefusalAlreadyAnswered {
		t.Errorf("another choice after the first came to %+v, %v; want already_answered", changed, err)
	}
	other, err := newAnswerExecutorRig(t, desk, boundTo("jane"), seatLeads).Call(t.Context(), approve)
	if err != nil || tools.RefusalOf(other) != tools.RefusalAlreadyAnswered {
		t.Errorf("a lead's same choice after the founder's came to %+v, %v; want already_answered", other, err)
	}
	if desk.foldInWrites != 1 || desk.runs["t1"].FoldIns[0].Answer != sandbox.FoldInApprove {
		t.Errorf("the refused answers recorded: %d writes, %+v", desk.foldInWrites, desk.runs["t1"].FoldIns[0])
	}
}

// A PERSON'S OWN ANSWER, SENT AGAIN AFTER IT WAS RECORDED, IS `applied` and
// delivered once; other words are still refused naming whose came first.
// Mutation: deliver the retry, or match on the answerer alone, and this fails.
func TestAnExecutorAnswersRetryAfterItWasRecordedIsAppliedNotRefused(t *testing.T) {
	t.Parallel()
	seal := answerSeal(t, "the fleet's key material")
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked(t, "t1", sandbox.StatusAwaiting)},
		seal: seal}
	tool := answerExecutorTool(t, desk)
	args := map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "answer": "use main"}
	if result, err := tool.Call(t.Context(), args); err != nil || result.Failed || len(desk.delivered) != 1 {
		t.Fatalf("the answer came to %+v, %v, delivering %d", result, err, len(desk.delivered))
	}
	// THE SEAT RECORDS IT as its coordinator does, from what was delivered.
	given := desk.delivered[0]
	run := parked(t, "t1", sandbox.StatusAnswered)
	answer, err := seal.Seal(run, sandbox.AnswerWords{Text: given.Answer, Choice: given.Choice,
		Via: types.AnswerViaOperator, By: given.AnsweredBy, ByKind: given.AnsweredByKind,
		OperatorID: given.OperatorID}, sandbox.RecordedAnswer{EventIDs: []string{"answer-1"}})
	if err != nil {
		t.Fatal(err)
	}
	run.Answer = &answer
	desk.runs["t1"] = run

	retry, err := tool.Call(t.Context(), args)
	if err != nil || retry.Failed || receiptOutcome(t, retry) != statelog.OutcomeApplied {
		t.Fatalf("the same answer sent again came to %+v, %v; want it applied", retry, err)
	}
	if len(desk.delivered) != 1 {
		t.Fatalf("the retry delivered again: %+v", desk.delivered)
	}
	other, err := tool.Call(t.Context(), map[string]any{"turn_id": "t1", "launch_id": "launch-t1",
		"answer": "use the release branch"})
	if err != nil || tools.RefusalOf(other) != tools.RefusalAlreadyAnswered || len(desk.delivered) != 1 {
		t.Errorf("other words after the first came to %+v, %v; want already_answered, nothing delivered",
			other, err)
	}
}
