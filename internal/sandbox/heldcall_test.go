package sandbox

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/secrets"
)

// parksOnAHeldCall records a call to a tool at Needs approval as turnID's
// job's question, its arguments sealed, and parks the run on it, answering the
// held call's id.
func parksOnAHeldCall(t *testing.T, rig *coordRig, turnID string) string {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	run := rig.get(turnID)
	held := HeldCall{ID: "0190f3a8-0000-7000-8000-000000000001", LaunchID: run.LaunchID, Server: "tracker",
		Tool: "tr_close_issue", Raw: "close_issue", Digest: "sha256:d1", HeldAt: time.Now().UTC()}
	if held.Arguments, err = NewCallSeal(cipher).SealArguments(turnID, held, map[string]any{"id": "7"}); err != nil {
		t.Fatal(err)
	}
	const question = "May swe call tr_close_issue on the tracker MCP server?"
	rig.ask(turnID, Ask{Question: question, Decision: HeldDecision(question, nil), To: AskToManager, Held: &held})
	rig.runner.Finish(Result{})
	payload, ev := rig.completion(turnID)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.get(turnID); got.Status != StatusAwaiting || got.HeldCall == nil {
		t.Fatalf("the run is %q holding %+v, want it parked on its held call", got.Status, got.HeldCall)
	}
	return held.ID
}

// A CHAT REPLY NEVER ANSWERS A HELD CALL, AND AN ANSWER BY TURN ONLY BY ITS ID
// (ADR-0077). Whoever speaks on the conversation the run was launched from is
// not, for that, somebody who may approve a call to a tool, so a reply there is
// the ordinary message it looks like and the run waits on. An answer by turn
// that does not name the held call is spent unrecorded, as one naming a held
// call for a question that is not one is; the approval naming it is recorded.
//
// Mutation: drop the held-call check from ConversationRef.Answers, and the
// chat reply approves the call.
func TestAChatReplyNeverAnswersAHeldCall(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	id := parksOnAHeldCall(t, rig, "t1")

	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", chatReply(answerOnTheDM, "once", nil))
	if err != nil || d != AnswerNotMine {
		t.Fatalf("a chat reply to a held call = %q, %v, want not mine", d, err)
	}
	if got := rig.get("t1"); got.Answer != nil || got.Status != StatusAwaiting {
		t.Fatalf("a chat reply was recorded as the held call's answer: %+v", got.Answer)
	}

	given, _ := givenAgainst(t, rig, "t1")
	given.Choice = HeldOnce
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, events.New(given, events.TraceContext{})); err != nil ||
		d != AnswerNotMine {
		t.Fatalf("an answer by turn not naming the held call = %q, %v, want not mine", d, err)
	}
	if got := rig.get("t1"); got.Answer != nil {
		t.Fatal("an answer by turn not naming the held call was recorded")
	}
	records := rig.answeredRecords()
	if len(records) != 1 || records[0].Outcome != types.AnswerUnoffered {
		t.Fatalf("recorded %+v, want one unoffered", records)
	}

	given.Approval = id
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, events.New(given, events.TraceContext{})); err != nil ||
		d != AnswerConsumed {
		t.Fatalf("the approval naming the held call = %q, %v, want consumed", d, err)
	}
}

// AN APPROVAL NAMES A HELD CALL, so one naming a held call answers no question
// that is not one: spent unrecorded on a run parked on its own question.
func TestAnApprovalAnswersNoQuestionThatIsNotAHeldCall(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	launchWithNoConversation(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")
	given, _ := givenAgainst(t, rig, "t1")
	given.Answer, given.Approval = "go", "0190f3a8-0000-7000-8000-000000000001"
	ev := events.New(given, events.TraceContext{})
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); err != nil || d != AnswerNotMine {
		t.Fatalf("an approval to a run's own question = %q, %v, want not mine", d, err)
	}
	if got := rig.get("t1"); got.Answer != nil {
		t.Fatal("an approval was recorded as the answer to a question that holds no call")
	}
}
