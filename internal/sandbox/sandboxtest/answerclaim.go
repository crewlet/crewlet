package sandboxtest

import (
	"bytes"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A TURN TAKES ITS ANSWER ONCE, on the claim that drives it, and a release
// gives it back to the run for the retry: what the seat's next holder reads to
// tell a reply a turn used from one nobody ever got to.
//
// IN A BUBBLE, so the store's clock is the case's: the take is dated the
// instant it is made, and the repeat comes a second later, where a repeat
// that dated it again read equal to the first inside one tick of the
// machine's clock.
func testATurnTakesTheAnswerItsClaimDrives(t *testing.T, s sandbox.PendingStore) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		mustLaunched(t, s, run("t1"))
		park(t, s, "t1")
		launch := mustGet(t, s, "t1").LaunchID
		if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
			t.Fatalf("RecordAnswer = %v, %v", ok, err)
		}
		// NOT BEFORE THE CLAIM: an answer still owed its resume is nobody's.
		if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}); err != nil || ok {
			t.Fatalf("TakeAnswer before the claim = %v, %v, want refused", ok, err)
		}
		if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
			t.Fatalf("ClaimForResume = %v, %v", ok, err)
		}
		if got := mustGet(t, s, "t1"); got.Answer == nil || got.Answer.Taken() {
			t.Fatalf("answer %+v, want it claimed and NOT taken: a claim is not a turn", got.Answer)
		}
		if ok, err := s.TakeAnswer(ctx, "t1", "another-launch", sandbox.Fence{}); err != nil || ok {
			t.Fatalf("TakeAnswer for another launch = %v, %v, want refused", ok, err)
		}
		took := time.Now().UTC()
		if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}); err != nil || !ok {
			t.Fatalf("TakeAnswer = %v, %v", ok, err)
		}
		taken := mustGet(t, s, "t1").Answer
		if taken == nil || !taken.Taken() || !taken.TakenAt.Equal(took) {
			t.Fatalf("answer %+v, want it taken at %s", taken, took)
		}
		// IDEMPOTENT, and the instant is the first one's.
		time.Sleep(time.Second)
		if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}); err != nil || !ok {
			t.Fatalf("a repeated TakeAnswer = %v, %v, want true", ok, err)
		}
		if got := mustGet(t, s, "t1").Answer; !got.TakenAt.Equal(took) {
			t.Errorf("a repeated take moved the instant from %v to %v", took, got.TakenAt)
		}
		// A RELEASE GIVES IT BACK: the retry's turn takes it again.
		if released, err := s.ReleaseClaim(ctx, "t1", sandbox.Release{
			Launch: launch, To: sandbox.StatusAnswered,
		}); err != nil || !released {
			t.Fatalf("ReleaseClaim = %v, %v", released, err)
		}
		if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAnswered || got.Answer == nil ||
			got.Answer.Taken() {
			t.Fatalf("run = %q answer %+v, want it owed again and not taken", got.Status, got.Answer)
		}
		// A NEWER LEASE FENCES A TAKE OUT: the seat's next holder decides on a
		// row nobody who lost the seat can still write.
		if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
			t.Fatalf("ClaimForResume = %v, %v", ok, err)
		}
		if ok, err := s.ClaimOwnership(ctx, "t1", "next", 5); err != nil || !ok {
			t.Fatalf("ClaimOwnership = %v, %v", ok, err)
		}
		if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{Owner: "old", Epoch: 1}); err != nil || ok {
			t.Fatalf("TakeAnswer under an outranked fence = %v, %v, want refused", ok, err)
		}
		// NOR UNDER NO LEASE AT ALL, which every other write here lets
		// through: a stalled claimant that held none took the answer on a row
		// its successor had fenced, and the person was answered twice.
		if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}); err != nil || ok {
			t.Fatalf("TakeAnswer under no lease on a fenced row = %v, %v, want refused", ok, err)
		}
		if got := mustGet(t, s, "t1").Answer; got == nil || got.Taken() {
			t.Fatalf("answer %+v, want it untaken after the outranked take", got)
		}
		if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{Owner: "next", Epoch: 5}); err != nil || !ok {
			t.Fatalf("TakeAnswer under the row's own lease = %v, %v", ok, err)
		}
		if ok, err := s.TakeAnswer(ctx, "gone", launch, sandbox.Fence{}); err != nil || ok {
			t.Errorf("TakeAnswer on a run that does not exist = %v, %v", ok, err)
		}
	})
}

// A DEAD CLAIM'S ANSWER IS REVIVED ONCE, FENCED AND COUNTED. The seat's next
// holder gives a claim whose node stopped before its turn took the answer back
// to that answer: the run answered again, owed its resume, stamped with the
// holder's lease and the loss counted on the answer, all in one write, because
// a revival without its fence is one the stalled claimant could still take, and
// one without its count is a resume that kills its node revived for ever. Only
// the claim it names is revived (that job, that answer, no newer lease) and
// only once: a revived run is no longer the claim, so a take under the dead
// claim's lease finds nothing. IN A BUBBLE, the next loss is dated a second
// later; one tick of the machine's clock would read a re-dated loss equal.
func testADeadClaimsAnswerIsRevivedOnceFencedAndCounted(t *testing.T, s sandbox.PendingStore) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		launch := claimedAnswerOn(t, s, "t1", answerOf("r1", "use main"), false)
		next := sandbox.Fence{Owner: "next", Epoch: 2}
		for name, revival := range map[string]sandbox.Revival{
			"another launch": {Launch: "another-launch", Answer: []string{"r1"}, Fence: next, Lost: true},
			"another answer": {Launch: launch, Answer: []string{"r2"}, Fence: next, Lost: true},
		} {
			if _, ok, err := s.ReviveAnswer(ctx, "t1", revival); err != nil || ok {
				t.Fatalf("a revival naming %s = %v, %v, want refused", name, ok, err)
			}
		}
		if _, ok, err := s.ReviveAnswer(ctx, "t1", sandbox.Revival{Launch: launch, Fence: next, Lost: true}); err == nil || ok {
			t.Fatalf("a revival naming no answer = %v, %v, want an error", ok, err)
		}
		revival := sandbox.Revival{Launch: launch, Answer: []string{"r1"}, Fence: next, Lost: true}
		lost := time.Now().UTC()
		revived, ok, err := s.ReviveAnswer(ctx, "t1", revival)
		if err != nil || !ok {
			t.Fatalf("ReviveAnswer = %v, %v", ok, err)
		}
		got := mustGet(t, s, "t1")
		if got.Status != sandbox.StatusAnswered || got.Owner != "next" || got.OwnerEpoch != 2 {
			t.Fatalf("run %q owned by %q at %d, want it answered again under the reviving lease",
				got.Status, got.Owner, got.OwnerEpoch)
		}
		if a := got.Answer; a == nil || !bytes.Equal(a.Sealed, sealOf("use main")) || a.Taken() || a.LostClaims != 1 ||
			!a.FirstLostAt.Equal(lost) {
			t.Fatalf("answer %+v, want the same answer, untaken, its lost claim counted at %s", a, lost)
		}
		if revived.Status != got.Status || revived.OwnerEpoch != got.OwnerEpoch ||
			revived.Answer == nil || revived.Answer.LostClaims != 1 {
			t.Fatalf("ReviveAnswer reported %q at %d with %+v, want the row as written",
				revived.Status, revived.OwnerEpoch, revived.Answer)
		}
		if _, ok, err := s.ReviveAnswer(ctx, "t1", revival); err != nil || ok {
			t.Fatalf("a second revival of the same claim = %v, %v, want refused: it is no longer a claim", ok, err)
		}
		if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}); err != nil || ok {
			t.Fatalf("a take under the dead claim = %v, %v, want refused: the answer is the run's again", ok, err)
		}

		// THE NEXT CLAIM DYING TOO is counted on the same answer, from the same
		// first instant: what the count bounds is a series no one node sees.
		time.Sleep(time.Second)
		later := sandbox.Fence{Owner: "later", Epoch: 3}
		if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), next); err != nil || !ok {
			t.Fatalf("the revived run's claim = %v, %v", ok, err)
		}
		if _, ok, err := s.ReviveAnswer(ctx, "t1", sandbox.Revival{Launch: launch, Answer: []string{"r1"},
			Fence: later, Lost: true}); err != nil || !ok {
			t.Fatalf("the second revival = %v, %v", ok, err)
		}
		if a := mustGet(t, s, "t1").Answer; a.LostClaims != 2 || !a.FirstLostAt.Equal(lost) {
			t.Fatalf("answer lost %d claims since %v, want 2 since %v", a.LostClaims, a.FirstLostAt, lost)
		}

		// NOT UNDER AN OUTRANKED LEASE: a holder that lost the seat revives
		// nothing its successor holds.
		if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), later); err != nil || !ok {
			t.Fatalf("the third claim = %v, %v", ok, err)
		}
		if _, ok, err := s.ReviveAnswer(ctx, "t1", revival); err != nil || ok {
			t.Fatalf("a revival under an outranked lease = %v, %v, want refused", ok, err)
		}
		if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed || got.Answer.LostClaims != 2 {
			t.Fatalf("run %q with %d lost claims, want the claim left as it was", got.Status, got.Answer.LostClaims)
		}
	})
}

// AN ANSWER IS RECORDED ONLY UNDER THE SEAT'S LEASE. A row a newer lease owns
// refuses a record under an older lease, and under none, with
// [sandbox.ErrSeatNotHeld] — the answer is the seat holder's to record and
// drive, and one recorded by a node that lost the seat landed on a run its
// successor had recovered as waiting, where nothing drove it. Under the row's
// own lease or a newer one it records, and stamps nothing: a record is not a
// claim of the run.
func testAnAnswerIsRecordedOnlyUnderTheSeatsLease(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	if ok, err := s.ClaimOwnership(ctx, "t1", "node-y", 6); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}
	launch := mustGet(t, s, "t1").LaunchID
	for name, fence := range map[string]sandbox.Fence{
		"an outranked lease": {Owner: "node-x", Epoch: 5},
		"no lease":           {},
	} {
		if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), fence); ok ||
			!errors.Is(err, sandbox.ErrSeatNotHeld) {
			t.Fatalf("a record under %s = %v, %v, want it refused as a seat not held", name, ok, err)
		}
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAwaiting || got.Answer != nil {
		t.Fatalf("run %q with answer %+v, want it still waiting", got.Status, got.Answer)
	}
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"),
		sandbox.Fence{Owner: "node-y", Epoch: 6}); err != nil || !ok {
		t.Fatalf("a record under the row's own lease = %v, %v", ok, err)
	}

	mustLaunched(t, s, run("t2"))
	park(t, s, "t2")
	if ok, err := s.ClaimOwnership(ctx, "t2", "node-y", 6); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}
	if _, ok, err := s.RecordAnswer(ctx, "t2", mustGet(t, s, "t2").LaunchID, answerOf("r2", "use dev"),
		sandbox.Fence{Owner: "node-z", Epoch: 7}); err != nil || !ok {
		t.Fatalf("a record under a newer lease = %v, %v", ok, err)
	}
	for _, turn := range []string{"t1", "t2"} {
		if got := mustGet(t, s, turn); got.Status != sandbox.StatusAnswered || got.Owner != "node-y" ||
			got.OwnerEpoch != 6 {
			t.Fatalf("%s %q owned by %q at %d, want it answered and its owner left as it stood",
				turn, got.Status, got.Owner, got.OwnerEpoch)
		}
	}
}

// A CLAIM ITS OWN NODE GIVES BACK IS NOT COUNTED. A claim whose write reported a
// failure and landed is given back to its answer by the node that made it,
// under the lease it was taken under: the run answered again, owed its resume —
// and nothing counted, because no node stopped. Counted, a coordination store
// that answered a few of a healthy node's claims with errors spent the answer's
// revivals and ended the run as an abandoned tail. A claim that IS lost after it
// is counted as the first.
func testAClaimItsOwnNodeGivesBackIsNotCounted(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := claimedAnswerOn(t, s, "t1", answerOf("r1", "use main"), false)
	own := sandbox.Fence{Owner: "own", Epoch: 1}
	for i := range 3 {
		if i > 0 {
			if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), own); err != nil || !ok {
				t.Fatalf("claim %d = %v, %v", i+1, ok, err)
			}
		}
		given, ok, err := s.ReviveAnswer(ctx, "t1", sandbox.Revival{Launch: launch, Answer: []string{"r1"},
			Fence: own})
		if err != nil || !ok {
			t.Fatalf("give-back %d = %v, %v", i+1, ok, err)
		}
		got := mustGet(t, s, "t1")
		if got.Status != sandbox.StatusAnswered || got.Owner != "own" || got.OwnerEpoch != 1 {
			t.Fatalf("run %q owned by %q at %d, want it answered again under the claim's own lease",
				got.Status, got.Owner, got.OwnerEpoch)
		}
		if a := got.Answer; a == nil || a.Taken() || a.LostClaims != 0 || !a.FirstLostAt.IsZero() {
			t.Fatalf("answer after give-back %d: %+v, want the same answer, untaken, nothing counted",
				i+1, a)
		}
		if given.Answer == nil || given.Answer.LostClaims != 0 || given.Status != sandbox.StatusAnswered {
			t.Fatalf("ReviveAnswer reported %q with %+v, want the row as written", given.Status, given.Answer)
		}
	}

	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), own); err != nil || !ok {
		t.Fatalf("the claim that dies = %v, %v", ok, err)
	}
	next := sandbox.Fence{Owner: "next", Epoch: 2}
	if _, ok, err := s.ReviveAnswer(ctx, "t1", sandbox.Revival{Launch: launch, Answer: []string{"r1"},
		Fence: next, Lost: true}); err != nil || !ok {
		t.Fatalf("the lost claim's revival = %v, %v", ok, err)
	}
	if a := mustGet(t, s, "t1").Answer; a.LostClaims != 1 || a.FirstLostAt.IsZero() {
		t.Fatalf("answer lost %d claims since %v, want the one lost claim counted", a.LostClaims, a.FirstLostAt)
	}
}

// A REVIVAL IS REFUSED AN ANSWER A TURN TOOK, with the row — the take and the
// revival are exclusive, as the take and an ending's let-go are, so whichever
// lands first decides and the person is answered once: by the turn, or by the
// revived run's resume. Nor does it revive a run whose ending is decided —
// that ending owns what becomes of the answer — or one that is gone.
func testARevivalIsRefusedAnAnswerATurnTook(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	next := sandbox.Fence{Owner: "next", Epoch: 2}
	launch := claimedAnswerOn(t, s, "t1", answerOf("r1", "use main"), true)
	row, ok, err := s.ReviveAnswer(ctx, "t1", sandbox.Revival{Launch: launch, Answer: []string{"r1"}, Fence: next,
		Lost: true})
	if !errors.Is(err, sandbox.ErrAnswerTaken) || ok || row.Answer == nil || !row.Answer.Taken() {
		t.Fatalf("a revival of a taken answer = %v, %+v, %v, want refused as taken with the row",
			ok, row.Answer, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed || got.Answer.LostClaims != 0 {
		t.Fatalf("run %q with %d lost claims, want the turn's claim left alone", got.Status, got.Answer.LostClaims)
	}

	launch = claimedAnswerOn(t, s, "t2", answerOf("r2", "use dev"), false)
	decide(t, s, "t2", everyJob(sandbox.Fence{}, sandbox.Active))
	if _, ok, err := s.ReviveAnswer(ctx, "t2", sandbox.Revival{Launch: launch, Answer: []string{"r2"},
		Fence: next, Lost: true}); err != nil || ok {
		t.Fatalf("a revival of a run whose ending is decided = %v, %v, want refused", ok, err)
	}
	if got := mustGet(t, s, "t2"); got.Status != sandbox.StatusResumed {
		t.Fatalf("run %q, want the decided run left as its ending found it", got.Status)
	}
	if _, ok, err := s.ReviveAnswer(ctx, "gone", sandbox.Revival{Launch: launch, Answer: []string{"r2"},
		Fence: next, Lost: true}); err != nil || ok {
		t.Fatalf("a revival of a run that does not exist = %v, %v", ok, err)
	}
}

// A DECLINE OWES ITS COPIES IN THE SAME WRITE, and they are owed until a
// publisher clears them — whatever the run does in between. The write is the
// decision to hand the reply back, and the copies on the row are what make
// that decision survive the process that took it: a decline that cleared the
// answer and recorded nothing would lose the reply to a crash before its
// publish, and one that recorded the copies anywhere but this write could be
// seen half done.
func testADeclineOwesItsCopiesInTheSameWrite(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	written, ok, err := s.DeclineAnswer(ctx, "t1", launch, []string{"r1"},
		[]sandbox.HandedBack{copyOf("r1")}, sandbox.Fence{})
	if err != nil || !ok {
		t.Fatalf("DeclineAnswer = %v, %v", ok, err)
	}
	for name, got := range map[string]sandbox.PendingRun{"returned": written, "stored": mustGet(t, s, "t1")} {
		if got.Status != sandbox.StatusAwaiting || got.Answer != nil ||
			len(got.HandBack) != 1 || got.HandBack[0].ID != "r1-copy" ||
			string(got.HandBack[0].Event) != `{"id":"r1-copy"}` {
			t.Fatalf("%s row = %q answer %+v hand-back %+v, want the question open and r1's "+
				"copy owed, from the one write", name, got.Status, got.Answer, got.HandBack)
		}
	}
	// A DECLINE NEEDS THE ANSWER IT LETS GO OF.
	if _, _, err := s.DeclineAnswer(ctx, "t1", launch, nil, nil, sandbox.Fence{}); err == nil {
		t.Error("a decline naming no delivery was accepted")
	}

	// OWED THROUGH WHATEVER THE RUN DOES NEXT: a new answer, and a new job.
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r2", "use dev"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer r2 = %v, %v", ok, err)
	}
	if _, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if got := mustGet(t, s, "t1"); len(got.HandBack) != 1 {
		t.Fatalf("hand-back = %+v after a new answer and a relaunch, want r1's copy still owed",
			got.HandBack)
	}

	// CLEARED BY ID, and only what is named.
	if ok, err := s.ClearHandBack(ctx, "t1", []string{"someone-else"}); err != nil || ok {
		t.Errorf("clearing a copy the run does not owe = %v, %v, want nothing removed", ok, err)
	}
	if ok, err := s.ClearHandBack(ctx, "t1", []string{"r1-copy"}); err != nil || !ok {
		t.Fatalf("ClearHandBack = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t1"); len(got.HandBack) != 0 {
		t.Fatalf("hand-back = %+v after it was cleared", got.HandBack)
	}
	if ok, err := s.ClearHandBack(ctx, "gone", []string{"r1-copy"}); err != nil || ok {
		t.Errorf("clearing a run that does not exist = %v, %v", ok, err)
	}
}

// A NEW QUESTION IS MEASURED FROM ITS OWN ASKING, with nothing recorded or
// declined against it: a reply one question let go of may be exactly what the
// next is waiting for, and an answer belongs to the question it answered.
func testANewQuestionForgetsTheLastOnesAnswer(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	asked := base.Add(time.Hour)
	if err := s.MarkAwaiting(ctx, "t1", sandbox.Clarification{AskedAt: asked}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if got := mustGet(t, s, "t1"); !got.AskedAt.Equal(asked) {
		t.Fatalf("asked at %v, want %v", got.AskedAt, asked)
	}
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "x"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if ok, err := decline(t, s, launch, "r1"); err != nil || !ok {
		t.Fatalf("DeclineAnswer = %v, %v", ok, err)
	}
	if _, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	got := mustGet(t, s, "t1")
	if !got.AskedAt.IsZero() || got.Answer != nil || len(got.DeclinedAnswers) != 0 {
		t.Fatalf("a new launch kept asked_at %v, answer %+v, declined %v",
			got.AskedAt, got.Answer, got.DeclinedAnswers)
	}
}

// THE ENGINE'S CARRYING ON IS A CLAIM OF ITS OWN, NEVER AN ANSWER. A run parked
// on its own calls alone is claimed straight out of waiting, no answer recorded
// on it; its turn takes the claim on the claimant's lease; a claim no turn took
// is revived to the status it was claimed out of, fenced and counted; one a
// turn took is never revived; and a run waiting on a person is never claimed so.
//
// Mutation: claim a run waiting on a person (any awaiting row), or revive a
// taken claim, and this goes red.
func testTheEnginesCarryingOnIsAClaimOfItsOwnNeverAnAnswer(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("person"))
	park(t, s, "person")
	asked := mustGet(t, s, "person")
	if _, ok, err := s.ClaimWaitOver(ctx, "person", asked.LaunchID, sandbox.Fence{}); err != nil || ok {
		t.Fatalf("a run waiting on a person claimed as carried on = %v, %v, want refused", ok, err)
	}

	launch := mustLaunched(t, s, run("t1"))
	Ask(t, s, "t1", sandbox.Ask{Call: &sandbox.CallWait{Tool: "delegate", Seq: 1}})
	if err := s.MarkAwaiting(ctx, "t1", sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	first := sandbox.Fence{Owner: "first", Epoch: 1}
	claimed, ok, err := s.ClaimWaitOver(ctx, "t1", launch, first)
	if err != nil || !ok || claimed.Status != sandbox.StatusResumed || claimed.Answer != nil ||
		claimed.WaitOver == nil || claimed.WaitOver.From != sandbox.StatusAwaiting ||
		claimed.ClaimedFrom != sandbox.StatusAwaiting {
		t.Fatalf("ClaimWaitOver = %+v, %v, %v; want it claimed out of awaiting, no answer on it", claimed, ok, err)
	}
	if _, ok, err = s.ClaimWaitOver(ctx, "t1", launch, first); err != nil || ok {
		t.Fatalf("a second claim = %v, %v, want refused", ok, err)
	}
	next := sandbox.Fence{Owner: "next", Epoch: 2}
	revived, ok, err := s.ReviveWaitOver(ctx, "t1", launch, next, true)
	if err != nil || !ok || revived.Status != sandbox.StatusAwaiting || revived.OwnerEpoch != 2 ||
		revived.WaitOver.LostClaims != 1 || revived.WaitOver.Taken() {
		t.Fatalf("ReviveWaitOver = %+v, %v, %v; want it waiting again under epoch 2, the lost claim counted",
			revived, ok, err)
	}
	if ok, err = s.TakeWaitOver(ctx, "t1", launch, next); err != nil || ok {
		t.Fatalf("a take of a revived claim = %v, %v, want refused: the run waits again", ok, err)
	}
	if _, ok, err = s.ClaimWaitOver(ctx, "t1", launch, next); err != nil || !ok {
		t.Fatalf("the revived run's claim = %v, %v", ok, err)
	}
	if ok, err = s.TakeWaitOver(ctx, "t1", launch, first); err != nil || ok {
		t.Fatalf("a take under an outranked lease = %v, %v, want refused", ok, err)
	}
	if ok, err = s.TakeWaitOver(ctx, "t1", launch, next); err != nil || !ok {
		t.Fatalf("TakeWaitOver = %v, %v", ok, err)
	}
	if _, ok, err = s.ReviveWaitOver(ctx, "t1", launch, sandbox.Fence{Owner: "later", Epoch: 3}, true); ok ||
		!errors.Is(err, sandbox.ErrWaitOverTaken) {
		t.Fatalf("a revival of a taken claim = %v, %v, want ErrWaitOverTaken", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Answer != nil || got.WaitOver.LostClaims != 1 {
		t.Fatalf("the row holds answer %+v, claim %+v; want no answer and the one lost claim", got.Answer,
			got.WaitOver)
	}
}
