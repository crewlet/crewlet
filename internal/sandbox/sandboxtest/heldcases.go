package sandboxtest

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A call held for a person's approval on its run's record: the cases [Run]
// drives for it (ADR-0077).
//
// EVERY ONE IS A PROPERTY OF A WRITE OF THE ROW: the held call recorded with
// the job's question and pinned to that job, gone when the next job opens; and
// the call marked as being made exactly once per answer, by the claim that
// carries the run on, under its fence, the mark outliving every claim given
// back, so a held call is never made twice.

// heldCases are the held-call cases, in the shape [Run] drives.
var heldCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"AHeldCallIsRecordedWithItsQuestionAndPinnedToItsJob", testAHeldCallIsRecordedWithItsQuestionAndPinnedToItsJob},
	{"AHeldCallIsMarkedOnceAndTheMarkOutlivesAClaimGivenBack", testAHeldCallIsMarkedOnceAndTheMarkOutlivesAClaimGivenBack},
	{"AHeldCallIsMarkedOnlyByTheClaimOfItsAnswer", testAHeldCallIsMarkedOnlyByTheClaimOfItsAnswer},
}

// heldSeal seals a held call's arguments for the cases, under a keyring of
// their own: the store keeps the envelope as it was handed and opens nothing.
func heldSeal(t *testing.T) *sandbox.CallSeal {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.NewCallSeal(cipher)
}

// heldAsk is the question a held call to close_issue is asked as, its
// arguments sealed for turnID's job launch.
func heldAsk(t *testing.T, turnID, launch string) sandbox.Ask {
	t.Helper()
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-000000000001", LaunchID: launch,
		Server: "tracker", Tool: "tr_close_issue", Raw: "close_issue", Digest: "sha256:d1", HeldAt: base}
	sealed, err := heldSeal(t).SealArguments(turnID, held, map[string]any{"id": "7"})
	if err != nil {
		t.Fatal(err)
	}
	held.Arguments = sealed
	const question = "May swe call tr_close_issue on the tracker MCP server?"
	return sandbox.Ask{Question: question, Decision: sandbox.HeldDecision(question, nil),
		To: sandbox.AskToManager, Held: &held}
}

// heldParked launches turnID, holds a call for it and parks it on the
// question, answering the job's launch.
func heldParked(t *testing.T, s sandbox.PendingStore, turnID string) string {
	t.Helper()
	ctx := t.Context()
	launch := mustLaunched(t, s, run(turnID))
	if got, err := s.RecordAsk(ctx, turnID, launch, heldAsk(t, turnID, launch)); err != nil ||
		got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk of a held call = %v, %v", got, err)
	}
	if err := s.MarkAwaiting(ctx, turnID, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	return launch
}

// A HELD CALL IS RECORDED WITH ITS QUESTION, PINNED TO ITS JOB: the row carries
// it under the job that made it, its arguments sealed as they were handed, and
// leaves the branch an earlier question named for the job that carries the
// work on, since a held call names none. The next job opens with no held call.
//
// Mutation: leave HeldCall out of resetLaunch, and the next job inherits a call
// a person could approve on its behalf.
func testAHeldCallIsRecordedWithItsQuestionAndPinnedToItsJob(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t1"))
	ask := heldAsk(t, "t1", launch)
	if got, err := s.RecordAsk(ctx, "t1", launch, ask); err != nil || got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk = %v, %v", got, err)
	}
	got := mustGet(t, s, "t1")
	if got.HeldCall == nil || got.HeldCall.LaunchID != launch || got.HeldCall.ID != ask.Held.ID ||
		got.HeldCall.Arguments != ask.Held.Arguments || got.Question != ask.Question {
		t.Fatalf("the row holds %+v with question %q, want the held call pinned to %s", got.HeldCall,
			got.Question, launch)
	}
	if !got.AskedBy(launch) {
		t.Fatal("a job that held a call does not read as one that asked, so its submission would pass")
	}
	mustBeginLaunch(t, s, run("t1"))
	if next := mustGet(t, s, "t1"); next.HeldCall != nil || next.Question != "" {
		t.Fatalf("the next job opened holding %+v, %q", next.HeldCall, next.Question)
	}
}

// A HELD CALL IS MARKED ONCE, AND THE MARK OUTLIVES A CLAIM GIVEN BACK: the
// first mark of an answer's claim is the one that makes the call; a second, on
// the same claim or on the claim taken again after a failed attempt gave it
// back, finds it marked and does not; the result is recorded once and read by
// every later mark.
//
// Mutation: let ReleaseClaim clear the mark, or the mark write a call already
// marked, and the call is made twice.
func testAHeldCallIsMarkedOnceAndTheMarkOutlivesAClaimGivenBack(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := heldParked(t, s, "t1")
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "once"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	claim := func() {
		t.Helper()
		if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
			t.Fatalf("ClaimForResume = %v, %v", ok, err)
		}
	}
	claim()
	held, first, err := s.MarkHeldCall(ctx, "t1", launch, sandbox.Fence{})
	if err != nil || !first || !held.Calling() || held.Made() {
		t.Fatalf("the first mark = %+v, %v, %v, want this mark the first", held, first, err)
	}
	again, first, err := s.MarkHeldCall(ctx, "t1", launch, sandbox.Fence{})
	if err != nil || first || !again.CallingAt.Equal(held.CallingAt) {
		t.Fatalf("a second mark = %+v, %v, %v, want it found marked", again, first, err)
	}
	ok, err := s.ReleaseClaim(ctx, "t1", sandbox.Release{Launch: launch, To: sandbox.StatusAnswered})
	if err != nil || !ok {
		t.Fatalf("ReleaseClaim = %v, %v", ok, err)
	}
	claim()
	again, first, err = s.MarkHeldCall(ctx, "t1", launch, sandbox.Fence{})
	if err != nil || first {
		t.Fatalf("a mark after the claim came back = %+v, %v, %v, want it found marked", again, first, err)
	}
	if ok, err = s.RecordHeldResult(ctx, "t1", launch, "sealed-result", sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordHeldResult = %v, %v", ok, err)
	}
	if ok, err = s.RecordHeldResult(ctx, "t1", launch, "another", sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("a second RecordHeldResult = %v, %v, want it answered as recorded", ok, err)
	}
	made, first, err := s.MarkHeldCall(ctx, "t1", launch, sandbox.Fence{})
	if err != nil || first || !made.Made() || made.Result != "sealed-result" {
		t.Fatalf("a mark after the result = %+v, %v, %v, want the first result", made, first, err)
	}
}

// A HELD CALL IS MARKED ONLY BY THE CLAIM OF ITS ANSWER: not while the run
// waits, not for another job, not on a run that holds no call, and not under a
// fence the row's newer lease outranks. Each is refused aloud, and nothing is
// written.
//
// Mutation: drop the status or the fence from heldClaim, and a stalled node
// marks the call its successor is carrying on.
func testAHeldCallIsMarkedOnlyByTheClaimOfItsAnswer(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := heldParked(t, s, "t1")
	if _, _, err := s.MarkHeldCall(ctx, "t1", launch, sandbox.Fence{}); !errors.Is(err, sandbox.ErrHeldCallUnclaimed) {
		t.Fatalf("a mark on a waiting run = %v, want ErrHeldCallUnclaimed", err)
	}
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "once"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	if _, _, err := s.MarkHeldCall(ctx, "t1", "not-"+launch, sandbox.Fence{}); !errors.Is(err, sandbox.ErrHeldCallUnclaimed) {
		t.Fatalf("a mark for another job = %v, want ErrHeldCallUnclaimed", err)
	}
	if ok, err := s.ClaimOwnership(ctx, "t1", "node-b:2", 7); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}
	stale := sandbox.Fence{Owner: "node-a:1", Epoch: 3}
	if _, _, err := s.MarkHeldCall(ctx, "t1", launch, stale); !errors.Is(err, sandbox.ErrHeldCallUnclaimed) {
		t.Fatalf("a mark under a stale fence = %v, want ErrHeldCallUnclaimed", err)
	}
	if got := mustGet(t, s, "t1"); got.HeldCall == nil || got.HeldCall.Calling() {
		t.Fatalf("a refused mark wrote %+v", got.HeldCall)
	}

	mustLaunched(t, s, run("t2"))
	park(t, s, "t2")
	plain := mustGet(t, s, "t2").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t2", plain, answerOf("r2", "main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer t2 = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, "t2", sandbox.RecordedAnswerTail(plain), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume t2 = %v, %v", ok, err)
	}
	if _, _, err := s.MarkHeldCall(ctx, "t2", plain, sandbox.Fence{}); !errors.Is(err, sandbox.ErrHeldCallUnclaimed) {
		t.Fatalf("a mark on a run that holds no call = %v, want ErrHeldCallUnclaimed", err)
	}
}
