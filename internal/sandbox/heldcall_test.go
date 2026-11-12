package sandbox

import (
	"strings"
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

// A QUESTION ABOUT A HELD CALL IS ANNOUNCED AS ONE, with the job it pins and
// the call it holds, never the arguments: a live surface offers Allow and Deny
// from the announcement, where the run's own answer would be refused. A
// question about no call announces none.
//
// Mutation: leave the call or the launch off the announcement, or put the
// sealed arguments on it, and this fails.
func TestAHeldCallIsAnnouncedWithItsQuestion(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	id := parksOnAHeldCall(t, rig, "t1")
	asked := rig.questions()
	if len(asked) != 1 {
		t.Fatalf("announced %d questions, want 1", len(asked))
	}
	got := asked[0]
	if got.HeldCall == nil || got.HeldCall.ID != id || got.HeldCall.Server != "tracker" ||
		got.HeldCall.Tool != "tr_close_issue" || got.HeldCall.HeldAt.IsZero() {
		t.Fatalf("the announcement holds %+v, want the held call", got.HeldCall)
	}
	if run := rig.get("t1"); got.LaunchID == "" || got.LaunchID != run.LaunchID {
		t.Errorf("the announcement names job %q, want the run's %q", got.LaunchID, run.LaunchID)
	}

	plain := newCoordRig(t)
	launchWithNoConversation(t, plain, "t2")
	parksOnAQuestion(t, plain, "t2")
	if asked := plain.questions(); len(asked) != 1 || asked[0].HeldCall != nil || asked[0].LaunchID == "" {
		t.Errorf("a question about no call announced %+v, want its job and no held call", asked)
	}
}

// A HELD CALL IS OPEN FROM ITS RECORDING TO ITS ANSWER, which is what a seat's
// repeat of the same request joins (ADR-0125): open before the job has parked
// and while the run waits, and no longer once an answer is recorded. A run
// parked on its own question holds no call to join.
//
// Mutation: count only a parked run, and a second turn asking in the seconds
// before the first one parks makes a second request; count an answered one,
// and an ask after a denial waits on an answer already given.
func TestAHeldCallIsOpenFromItsRecordingToItsAnswer(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	if rig.get("t1").HeldOpen() {
		t.Fatal("a run that held no call reads as holding an open one")
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	held := HeldCall{ID: "0190f3a8-0000-7000-8000-000000000002", LaunchID: rig.get("t1").LaunchID,
		Server: "crewlet", Tool: "ask_to_merge", Raw: "ask_to_merge", Digest: "sha256:d1", Fixed: true,
		Subject: strings.Repeat("ab", 32), HeldAt: time.Now().UTC()}
	if held.Arguments, err = NewCallSeal(cipher).SealArguments("t1", held, map[string]any{"change": "web!42"}); err != nil {
		t.Fatal(err)
	}
	const question = "May swe merge web!42 at a1a1a1a1a1a1?"
	rig.ask("t1", Ask{Question: question, Decision: HeldFixedDecision(question, nil), To: AskToManager, Held: &held})
	if got := rig.get("t1"); !got.HeldOpen() || got.HeldCall.Subject != held.Subject {
		t.Fatalf("a call just recorded, its job not yet parked, is %q holding %+v, want it open with its subject",
			got.Status, got.HeldCall)
	}
	rig.runner.Finish(Result{})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || !got.HeldOpen() {
		t.Fatalf("the parked run is %q, open %v, want it waiting and open", got.Status, got.HeldOpen())
	}
	given, _ := givenAgainst(t, rig, "t1")
	given.Choice, given.Approval = HeldDeny, held.ID
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, events.New(given, events.TraceContext{})); err != nil ||
		d != AnswerConsumed {
		t.Fatalf("the denial = %q, %v, want consumed", d, err)
	}
	if got := rig.get("t1"); got.HeldOpen() {
		t.Fatalf("an answered call still reads as open: the run is %q", got.Status)
	}

	launchWithNoConversation(t, rig, "t2")
	parksOnAQuestion(t, rig, "t2")
	if rig.get("t2").HeldOpen() {
		t.Fatal("a run parked on its own question reads as holding an open call")
	}
}
