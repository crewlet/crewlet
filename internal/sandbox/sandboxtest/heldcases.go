package sandboxtest

import (
	"errors"
	"slices"
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
	{"TheCallAPersonApprovedIsACallOfItsRound", testTheCallAPersonApprovedIsACallOfItsRound},
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
// AND THE CLAIM TAKEN AGAIN READS THEM: the row it returns holds the mark, and
// the result once one is recorded. The resume it drives answers a call an
// earlier attempt marked from that read, before it asks whether the call may
// be made at all (internal/agent/runner's makeHeld).
//
// Mutation: let ReleaseClaim clear the mark, or the mark write a call already
// marked, and the call is made twice; return the claimed row without the held
// call's mark or result, and a later attempt reads a call it made as one
// nobody made.
func testAHeldCallIsMarkedOnceAndTheMarkOutlivesAClaimGivenBack(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := heldParked(t, s, "t1")
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "once"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	claim := func() sandbox.HeldCall {
		t.Helper()
		claimed, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{})
		if err != nil || !ok || claimed.HeldCall == nil {
			t.Fatalf("ClaimForResume = %v, %v, holding %+v, want the claim with its held call", ok, err,
				claimed.HeldCall)
		}
		return *claimed.HeldCall
	}
	release := func() {
		t.Helper()
		if ok, err := s.ReleaseClaim(ctx, "t1", sandbox.Release{Launch: launch, To: sandbox.StatusAnswered}); err != nil || !ok {
			t.Fatalf("ReleaseClaim = %v, %v", ok, err)
		}
	}
	if read := claim(); read.Calling() || read.Made() {
		t.Fatalf("the first claim reads %+v, want the call unmarked", read)
	}
	held, first, err := s.MarkHeldCall(ctx, "t1", launch, sandbox.Fence{})
	if err != nil || !first || !held.Calling() || held.Made() {
		t.Fatalf("the first mark = %+v, %v, %v, want this mark the first", held, first, err)
	}
	again, first, err := s.MarkHeldCall(ctx, "t1", launch, sandbox.Fence{})
	if err != nil || first || !again.CallingAt.Equal(held.CallingAt) {
		t.Fatalf("a second mark = %+v, %v, %v, want it found marked", again, first, err)
	}
	release()
	if read := claim(); !read.Calling() || !read.CallingAt.Equal(held.CallingAt) || read.Made() {
		t.Fatalf("the claim taken again reads %+v, want the call marked as the first mark left it", read)
	}
	again, first, err = s.MarkHeldCall(ctx, "t1", launch, sandbox.Fence{})
	if err != nil || first {
		t.Fatalf("a mark after the claim came back = %+v, %v, %v, want it found marked", again, first, err)
	}
	ok, err := s.RecordHeldResult(ctx, "t1", launch, heldMade("sealed-result"), sandbox.Fence{})
	if err != nil || !ok {
		t.Fatalf("RecordHeldResult = %v, %v", ok, err)
	}
	if ok, err = s.RecordHeldResult(ctx, "t1", launch, heldMade("another"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("a second RecordHeldResult = %v, %v, want it answered as recorded", ok, err)
	}
	made, first, err := s.MarkHeldCall(ctx, "t1", launch, sandbox.Fence{})
	if err != nil || first || !made.Made() || made.Result != "sealed-result" {
		t.Fatalf("a mark after the result = %+v, %v, %v, want the first result", made, first, err)
	}
	release()
	if read := claim(); !read.Calling() || !read.Made() || read.Result != "sealed-result" {
		t.Fatalf("the claim taken again after the result reads %+v, want the call marked and the first "+
			"result recorded", read)
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

// THE CALL A PERSON APPROVED IS A CALL OF ITS ROUND, IN ITS LOG: recording what
// it returned on the row appends the call to the run's log beside every call
// the job made over its bridge — answered as the tool answered it, its
// arguments and output left sealed on the held call, named by the held call's
// id — so the job that carries the work on keeps it in its round's log, a later
// round finds it among the turn's earlier calls, and every reader of whether
// the turn replied reads it. It was made on the engine host and recorded
// nowhere a reader looked: a reply that went out read as one that did not. And
// ONCE, though answered twice: the retry that finds the result recorded
// appends only a call the log lacks. AND WITH NO ID OF A HARNESS'S
// ([sandbox.BridgeCall.CallID]), whatever its resume handed: the resume made
// the call on the engine host, where no harness named it, and the box's
// session holds the held call's answer under the held call's own id.
//
// Mutation: record the result alone in RecordHeldResult, and the round's log
// holds nothing the approved call came to; append without the held call's id,
// and the second answer puts it in the log twice; keep the id the resume
// handed (approved-call-keeps-its-id), and the approved call carries an id no
// harness gave it.
func testTheCallAPersonApprovedIsACallOfItsRound(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := heldParked(t, s, "t1")
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "once"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	if _, first, err := s.MarkHeldCall(ctx, "t1", launch, sandbox.Fence{}); err != nil || !first {
		t.Fatalf("MarkHeldCall = %v, %v", first, err)
	}
	made := heldMade("sealed-result")
	// WHAT THE CALL'S SURFACE SAID, which a write must never put in the
	// clear: the arguments and output ride the row sealed.
	made.Call.Name, made.Call.Args, made.Call.Output = "another_tool", `{"id":"7"}`, "closed #7"
	made.Call.CallID = "toolu_01nosuchharness"
	if ok, err := s.RecordHeldResult(ctx, "t1", launch, made, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordHeldResult = %v, %v", ok, err)
	}
	if ok, err := s.RecordHeldResult(ctx, "t1", launch, heldMade("again"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("a second RecordHeldResult = %v, %v, want it answered as recorded", ok, err)
	}
	approved := func(r loggedRun) []sandbox.BridgeCall {
		var out []sandbox.BridgeCall
		for _, call := range r.Log.Calls {
			if call.Name == "tr_close_issue" && !call.Failed {
				out = append(out, call)
			}
		}
		return out
	}
	row := mustLogged(t, s, "t1")
	got := approved(row)
	if len(got) != 1 || !got[0].Approved || got[0].Args != "" || got[0].Output != "" ||
		got[0].Origin != "mcp:tracker" || got[0].Server != "tracker" || got[0].At.IsZero() ||
		got[0].Seq != row.Log.LastSeq() || got[0].HeldID != row.HeldCall.ID || got[0].CallID != "" {
		t.Fatalf("the round's log holds %+v of the approved call, want it once, approved, last, named "+
			"the held call's tool and id, answered by its server, its arguments and output left sealed, and "+
			"with no id of a harness's", got)
	}
	carried := run("t1")
	carried.Continues = true
	mustBeginLaunch(t, s, carried)
	if again := approved(mustLogged(t, s, "t1")); len(again) != 1 {
		t.Fatalf("the job carrying the work on keeps %+v of the approved call, want it in its round", again)
	}
	mustBeginLaunch(t, s, run("t1"))
	if prior := mustLogged(t, s, "t1").Log.PriorCalls; !slices.Contains(prior, "tr_close_issue") {
		t.Fatalf("a later round finds %v of the turn's earlier calls, want the approved call among them", prior)
	}
}

// heldMade is what a held call to close_issue came to, its result sealed as
// result: made, as the tracker's server answered it.
func heldMade(result string) sandbox.HeldMade {
	return sandbox.HeldMade{Result: result, Call: sandbox.BridgeCall{
		Name: "tr_close_issue", Origin: "mcp:tracker", Server: "tracker", Source: "mcp:tracker",
	}}
}
