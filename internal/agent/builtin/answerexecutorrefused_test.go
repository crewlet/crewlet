package builtin_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// AN ANSWER AFTER A REFUSED REPLY IS TOLD THE REPLY WAS NOT TAKEN (ADR-0114). A
// reply in the conversation too large for the executor to take reached its
// question first, and its refusal is the question's outcome: the question no
// longer waits, so a later answer is refused as one to an answered question
// is, naming whose reply it was and when — but never as an answer the
// executor carries on with, which it was given none of. The model is told the
// reply's figures and that the executor is resumed told so and asks again if it
// still needs an answer; the person is told the same in their own words.
//
// Mutation: word it as any answered question is, and the person and their
// assistant are told the executor carries on with an answer it never got.
func TestAnAnswerAfterARefusedReplyIsToldTheReplyWasNotTaken(t *testing.T) {
	t.Parallel()
	seal := answerSeal(t, "the fleet's key material")
	run := parked(t, "t1", sandbox.StatusResumed)
	refusal := sandbox.AnswerRefusal{EventIDs: []string{"reply-1"}, Bytes: 512<<10 + 1, MaxBytes: 512 << 10}
	answer, err := seal.Seal(run, sandbox.AnswerWords{Via: types.AnswerViaChat, By: "miles", ByKind: "human",
		Refusal: &refusal}, sandbox.RecordedAnswer{EventIDs: refusal.EventIDs,
		RecordedAt: time.Date(2026, 10, 28, 0, 35, 6, 0, time.UTC)})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	run.Answer, run.AnswerRefusal = &answer, &refusal
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": run}, seal: seal}
	result, err := answerExecutorTool(t, desk).Call(t.Context(),
		map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "answer": "use the release branch"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Failed || tools.RefusalOf(result) != tools.RefusalAlreadyAnswered {
		t.Fatalf("answer_executor = %+v, want an already_answered refusal", result)
	}
	for _, want := range []string{"A reply from miles", "at 2026-10-28T00:35:06Z", "513 KiB", "512 KiB",
		"was not taken", "asks again"} {
		if !strings.Contains(result.Output, want) {
			t.Errorf("the model is not told %q: %s", want, result.Output)
		}
	}
	if strings.Contains(result.Output, "already answered by") {
		t.Errorf("the model is told the question was answered: %s", result.Output)
	}
	person := result.PersonRefusal.Person()
	if !strings.Contains(person, "@miles") || !strings.Contains(person, "too long") ||
		strings.Contains(person, "answered") {
		t.Errorf("a person is told %q, want whose reply was too long and no answer", person)
	}
	if len(desk.delivered) != 0 {
		t.Fatalf("delivered %+v to a question a refusal closed", desk.delivered)
	}
}
