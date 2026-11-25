package sandboxtest

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// letGoOf is the let-go, by the decided ending named, of the answer made of
// ids, owing one copy of each.
func letGoOf(ending string, ids ...string) sandbox.LetGo {
	var copies []sandbox.HandedBack
	for _, id := range ids {
		copies = append(copies, copyOf(id))
	}
	return sandbox.LetGo{Ending: ending, Answer: ids, HandBack: copies}
}

// claimOnly is the license a claim's own ending takes.
var claimOnly = []string{sandbox.StatusResumed}

// A DECIDED ENDING LETS THE REPLY GO, in one write: only as a step of that
// ending, of the answer it holds, and only one decline's worth. The answer leaves
// the row in the same write, so a let-go repeated after a crash finds nothing
// left to let go of; and no turn can take it afterwards, nor any release give
// it to a resume — the run's ending is decided.
func testAnEndingRecordsTheReplyItLetsGoOnItsClaim(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	lease := sandbox.Fence{Owner: "node-a:1", Epoch: 3}
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), lease); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	owe := func(letGo sandbox.LetGo) (sandbox.PendingRun, bool, error) {
		return s.OweHandBack(ctx, "t1", letGo)
	}
	// NOT BEFORE AN ENDING IS DECIDED: a let-go is a step of one.
	if _, ok, err := owe(letGoOf("no-ending", "r1")); err != nil || ok {
		t.Fatalf("OweHandBack on a run with no ending decided = %v, %v, want refused", ok, err)
	}
	ending := decide(t, s, "t1", sandbox.License{Fence: lease, WhileIn: claimOnly, Launch: launch}).Ending.ID
	if _, ok, err := owe(letGoOf("another-ending", "r1")); err != nil || ok {
		t.Fatalf("OweHandBack for another ending = %v, %v, want refused", ok, err)
	}
	if _, ok, err := owe(letGoOf(ending, "other")); err != nil || ok {
		t.Fatalf("OweHandBack of an answer the run does not hold = %v, %v, want refused", ok, err)
	}
	if _, _, err := owe(letGoOf(ending)); err == nil {
		t.Error("a let-go naming no delivery was accepted")
	}
	written, ok, err := owe(letGoOf(ending, "r1"))
	if err != nil || !ok {
		t.Fatalf("OweHandBack = %v, %v", ok, err)
	}
	for name, got := range map[string]sandbox.PendingRun{"returned": written, "stored": mustGet(t, s, "t1")} {
		if got.Status != sandbox.StatusResumed || got.Answer != nil || got.Ending == nil ||
			len(got.HandBack) != 1 || got.HandBack[0].ID != "r1-copy" {
			t.Fatalf("%s row = %q answer %+v hand-back %+v ending %+v, want the ending kept, the "+
				"answer gone and r1's copy owed, from the one write", name, got.Status, got.Answer,
				got.HandBack, got.Ending)
		}
	}
	// LET GO ONCE: a repeat finds no answer left to let go of.
	if _, ok, err := owe(letGoOf(ending, "r1")); err != nil || ok {
		t.Fatalf("a second let-go of the same answer = %v, %v, want refused", ok, err)
	}
	// AND NEVER TAKEN, NOR GIVEN BACK TO A RESUME.
	if ok, err := s.TakeAnswer(ctx, "t1", launch, lease); err != nil || ok {
		t.Fatalf("TakeAnswer after the answer was let go = %v, %v, want refused", ok, err)
	}
	if released, err := s.ReleaseClaim(ctx, "t1", sandbox.Release{
		Launch: launch, To: sandbox.StatusAnswered, Fence: lease,
	}); err != nil || released {
		t.Fatalf("a claim whose answer was let go of was released to answered: %v, %v", released, err)
	}
	if _, ok, err := s.OweHandBack(ctx, "gone", letGoOf(ending, "r1")); err != nil || ok {
		t.Errorf("OweHandBack on a run that does not exist = %v, %v", ok, err)
	}
}

// AN ANSWER NONE OF WHOSE DELIVERIES COULD BE CARRIED IS LET GO OF WITH NOTHING
// TO HAND BACK, as its decline is: refused, the ending could never finish, and
// the store would never delete the row that holds it.
func testAnAnswerThatCarriesNoCopyIsStillLetGo(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	ending := decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)).Ending.ID
	letGo := letGoOf(ending, "r1")
	letGo.HandBack = nil
	if _, ok, err := s.OweHandBack(ctx, "t1", letGo); err != nil || !ok {
		t.Fatalf("OweHandBack with no copy to owe = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Answer != nil || len(got.HandBack) != 0 {
		t.Fatalf("answer %+v hand-back %+v, want the answer gone and nothing owed", got.Answer, got.HandBack)
	}
	if _, ended, err := s.Finish(ctx, "t1", ending); err != nil || !ended {
		t.Fatalf("Finish after the let-go = %v, %v", ended, err)
	}
}

// AN ANSWERED RUN IS ENDED ONLY BY AN ENDING LICENSED FOR IT. A release that
// landed although it reported a failure, or an old holder's release under a reap
// whose fence did not land, puts a run an ending decided on as a claim back to
// answered — owed its resume, not an ending. A claim's own ending is licensed
// for the claim alone, so it decides nothing about that run; an ending licensed
// for it does, and from then on no resume can claim it.
func testAnAnsweredRunsReplyIsLetGoOnlyUnderItsLicense(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: sandbox.License{WhileIn: claimOnly, Launch: launch},
	}); err != nil || ok {
		t.Fatalf("a claim's own ending of an answered run = %v, %v, want refused", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAnswered || got.Answer == nil || got.Ending != nil {
		t.Fatalf("run = %q answer %+v ending %+v, want it still answered and owed its resume",
			got.Status, got.Answer, got.Ending)
	}
	ending := decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)).Ending.ID
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || ok {
		t.Fatalf("a resume claimed a run whose ending is decided: %v, %v", ok, err)
	}
	if _, ok, err := s.OweHandBack(ctx, "t1", letGoOf(ending, "r1")); err != nil || !ok {
		t.Fatalf("the ending's let-go of an answered run = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAnswered || got.Answer != nil ||
		len(got.HandBack) != 1 || got.HandBack[0].ID != "r1-copy" {
		t.Fatalf("row = %q answer %+v hand-back %+v, want the answer gone and r1's copy owed",
			got.Status, got.Answer, got.HandBack)
	}
}

// THE TAKE AND THE LET-GO ARE EXCLUSIVE IN THE STORE, whichever lands first and
// whatever any fence says: a take that lands before the ending is decided is
// seen by the decision, and the ending's let-go of the answer is refused as
// taken — the reply was used, and goes with the run — unless the ending says it
// went unused; and a take that comes after the decision is refused.
func testATakenAnswerIsNotLetGo(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	claimedAnswerOn(t, s, "t1", answerOf("r1", "use main"), true)
	ending := decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)).Ending.ID
	row, ok, err := s.OweHandBack(ctx, "t1", letGoOf(ending, "r1"))
	if !errors.Is(err, sandbox.ErrAnswerTaken) || ok || row.Answer == nil || !row.Answer.Taken() {
		t.Fatalf("a let-go of a taken answer = %v, %+v, %v, want refused as taken with the row",
			ok, row.Answer, err)
	}
	if got := mustGet(t, s, "t1"); got.Answer == nil || !got.Answer.Taken() || len(got.HandBack) != 0 {
		t.Fatalf("answer %+v hand-back %+v, want the taken answer left alone", got.Answer, got.HandBack)
	}
	if _, ended, err := s.Finish(ctx, "t1", ending); err != nil || !ended {
		t.Fatalf("Finish of a run whose turn took the reply = %v, %v, want it ended with it", ended, err)
	}

	claimedAnswerOn(t, s, "t2", answerOf("r1", "use main"), true)
	unused := everyJob(sandbox.Fence{}, sandbox.Active)
	unused.Unused = true
	ending = decide(t, s, "t2", unused).Ending.ID
	if _, ended, err := s.Finish(ctx, "t2", ending); !errors.Is(err, sandbox.ErrAnswerOwed) || ended {
		t.Fatalf("Finish of a run whose taken reply went unused = %v, %v, want refused with ErrAnswerOwed",
			ended, err)
	}
	if _, ok, err := s.OweHandBack(ctx, "t2", letGoOf(ending, "r1")); err != nil || !ok {
		t.Fatalf("the let-go of an answer its turn gave back = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t2"); got.Answer != nil || len(got.HandBack) != 1 {
		t.Fatalf("answer %+v hand-back %+v, want the reply let go and its copy owed", got.Answer, got.HandBack)
	}
}

// A ROW HOLDING A PERSON'S REPLY NO TURN TOOK IS NOT DELETED, whatever ending it
// is decided on: its delivery was spent when it was recorded, so the row is the
// only thing that still carries it. The ending is told so, with the row, and
// lets the reply go first — and no turn can take it from a row whose ending is
// decided. One a turn took before the decision has been used, and goes with
// the run.
func testARowHoldingAnUntakenReplyIsNotDeleted(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	claimed, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{})
	if err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	ending := decide(t, s, "t1", sandbox.License{WhileIn: claimOnly, Launch: claimed.LaunchID}).Ending.ID
	row, ended, err := s.Finish(ctx, "t1", ending)
	if !errors.Is(err, sandbox.ErrAnswerOwed) || ended || row.Answer == nil {
		t.Fatalf("Finish of a claim holding an untaken reply = %v, %+v, %v, want refused with the row",
			ended, row.Answer, err)
	}
	if ok, err := s.TakeAnswer(ctx, "t1", launch, sandbox.Fence{}); err != nil || ok {
		t.Fatalf("TakeAnswer after the ending was decided = %v, %v, want refused", ok, err)
	}
	if _, found, err := s.Get(ctx, "t1"); err != nil || !found {
		t.Fatalf("the row holding the reply is gone (found %v, %v)", found, err)
	}

	mustLaunched(t, s, run("t2"))
	park(t, s, "t2")
	launch = mustGet(t, s, "t2").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t2", launch, answerOf("r2", "use dev"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer t2 = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, "t2", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume t2 = %v, %v", ok, err)
	}
	if ok, err := s.TakeAnswer(ctx, "t2", launch, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("TakeAnswer t2 = %v, %v", ok, err)
	}
	if _, ended, err := end(ctx, s, "t2", sandbox.License{WhileIn: claimOnly, Launch: launch}); err != nil || !ended {
		t.Fatalf("the ending of a claim whose turn took the reply = %v, %v", ended, err)
	}
}

// A CLAIM'S OWN ENDING NAMES ITS JOB, and a run that holds another is not its
// to end: the resumed turn's relaunch is claimed by its own completion in the
// very status the first claim held, and an ending licensed on the status alone
// ended it, box and all.
func testAnEndingLicensedForAJobLeavesAnother(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	first := mustClaim(t, s, "t1")
	mustLaunched(t, s, run("t1"))
	mustClaim(t, s, "t1")
	if _, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: sandbox.License{WhileIn: claimOnly, Launch: first.LaunchID},
	}); err != nil || ok {
		t.Fatalf("an ending licensed for the first job ended the second: %v, %v", ok, err)
	}
	if _, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: sandbox.License{WhileIn: claimOnly},
	}); err != nil || ok {
		t.Fatalf("an ending naming no job ended a run that holds one: %v, %v", ok, err)
	}
	if _, ended, err := end(ctx, s, "t1", everyJob(sandbox.Fence{}, claimOnly)); err != nil || !ended {
		t.Fatalf("an ending licensed for every job = %v, %v", ended, err)
	}
}

// A CLAIM IS TAKEN UNDER THE CLAIMANT'S LEASE: refused where a newer lease owns
// the run, and stamped on the row where it lands, so the take, the release and
// the ending that follow it carry the CLAIMANT's lease rather than whatever the
// row was stamped with before — which, for a run nobody re-stamped, fenced out
// nobody. And an UNFENCED claim is refused on a row any lease owns: a node that
// noticed it lost the seat, holding no lease, claimed past its successor's
// fence. One claims only a row no lease has owned, and leaves its owner as it
// stands.
func testAClaimIsTakenUnderTheClaimantsLease(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	if _, err := s.ClaimOwnership(ctx, "t1", "node-b:1", 4); err != nil {
		t.Fatalf("own: %v", err)
	}
	if _, won, err := s.ClaimForResume(ctx, "t1", completionOf(t, s, "t1"),
		sandbox.Fence{Owner: "node-a:1", Epoch: 3}); err != nil || won {
		t.Fatalf("a claim under a lease the run's outranks = %v, %v, want refused", won, err)
	}
	claimed, won, err := s.ClaimForResume(ctx, "t1", completionOf(t, s, "t1"),
		sandbox.Fence{Owner: "node-c:1", Epoch: 6})
	if err != nil || !won {
		t.Fatalf("a claim under the newest lease = %v, %v", won, err)
	}
	for name, got := range map[string]sandbox.PendingRun{"returned": claimed, "stored": mustGet(t, s, "t1")} {
		if got.Owner != "node-c:1" || got.OwnerEpoch != 6 {
			t.Fatalf("%s row owned by %q at %d, want the claimant's lease", name, got.Owner, got.OwnerEpoch)
		}
	}
	mustRelease(t, s, claimed)
	if _, won, err := s.ClaimForResume(ctx, "t1", completionOf(t, s, "t1"), sandbox.Fence{}); err != nil || won {
		t.Fatalf("an unfenced claim of a row a lease owns = %v, %v, want refused", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning || got.Owner != "node-c:1" ||
		got.OwnerEpoch != 6 {
		t.Fatalf("run %q owned by %q at %d, want it left running under its owner",
			got.Status, got.Owner, got.OwnerEpoch)
	}

	mustLaunched(t, s, run("t2"))
	if _, won, err := s.ClaimForResume(ctx, "t2", completionOf(t, s, "t2"), sandbox.Fence{}); err != nil || !won {
		t.Fatalf("an unfenced claim of a row no lease owns = %v, %v", won, err)
	}
	if got := mustGet(t, s, "t2"); got.Owner != "" || got.OwnerEpoch != 0 {
		t.Fatalf("an unfenced claim moved the owner to %q at %d", got.Owner, got.OwnerEpoch)
	}
}

// A LET-GO WAITS FOR THE COPIES ALREADY OWED, and says what they are: an ending
// lets a claimed answer go only once an earlier decline's copies are out, so the
// row never owes more than one decline's worth.
func testALetGoWaitsForTheCopiesAlreadyOwed(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer r1 = %v, %v", ok, err)
	}
	if ok, err := decline(t, s, launch, "r1"); err != nil || !ok {
		t.Fatalf("DeclineAnswer r1 = %v, %v", ok, err)
	}
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r2", "use dev"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer r2 = %v, %v", ok, err)
	}
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	ending := decide(t, s, "t1", sandbox.License{WhileIn: claimOnly, Launch: launch}).Ending.ID
	owing, ok, err := s.OweHandBack(ctx, "t1", letGoOf(ending, "r2"))
	if !errors.Is(err, sandbox.ErrHandBackOwed) || ok ||
		len(owing.HandBack) != 1 || owing.HandBack[0].ID != "r1-copy" {
		t.Fatalf("a let-go over owed copies = %v, %+v, %v, want refused with r1's copy", ok,
			owing.HandBack, err)
	}
	if got := mustGet(t, s, "t1"); got.Answer == nil || len(got.HandBack) != 1 {
		t.Fatalf("answer %+v hand-back %+v, want r2 still on the claim and only r1's copy owed",
			got.Answer, got.HandBack)
	}
	published(t, s, "r1-copy")
	if _, ok, err := s.OweHandBack(ctx, "t1", letGoOf(ending, "r2")); err != nil || !ok {
		t.Fatalf("a let-go once r1's copy was out = %v, %v", ok, err)
	}
}
