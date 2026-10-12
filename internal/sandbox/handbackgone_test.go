package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A RETRIED RESUME THAT FINDS ITS RUN GONE HANDS THE REPLY BACK EXACTLY ONCE,
// ACROSS A CRASH AT EACH STEP.
//
// The answer was recorded and its delivery acknowledged; the inline resume
// failed, and the retry's resume fails too — and so does handing the claim
// back, which ENDS the run. Nothing else will ever bring the person's reply to
// the seat, so it goes back to the seat's inbox as the ordinary message it is.
// It used to be published straight off the retry AFTER the ending had deleted
// the row, the only record of it, so a process that stopped between the two
// lost it. Now it is recorded on the claimed row as owed before the ending,
// and the ending publishes what the row owes before it deletes the row. Each
// case stops the node at one step — by refusing that step and then handing
// the seat to a successor over the same store — and counts what the seat was
// given.

// owedAnAnswerWhoseRunEnds records R1 on t1, fails its inline resume, and arms
// the retry so that its resume fails too and handing the claim back is refused
// — the retry ends the run. The case refuses whatever else it is about.
func owedAnAnswerWhoseRunEnds(t *testing.T, rig *coordRig, alsoRefuse ...string) (*events.Event, *refusingStore) {
	t.Helper()
	store := &refusingStore{inner: rig.pending}
	rig.coordinator.pending = store
	parkOnAQuestion(t, rig)
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	rig.resumer.failWith(errors.New("transient"))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	store.refuse = append([]string{"ReleaseClaim"}, alsoRefuse...)
	return r1, store
}

// THE ORDINARY CASE: the reply is handed back once, and the run is ended.
func TestARetryWhoseRunEndsHandsTheReplyBackOnce(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	r1, _ := owedAnAnswerWhoseRunEnds(t, rig)
	if rig.fireRetries() != 1 {
		t.Fatal("the failed resume scheduled no retry")
	}
	copyID := declinedCopyID(r1.ID).String()
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v, want R1's copy exactly once", got)
	}
	rig.finished("t1")
	rig.fireRetries()
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v after the retries settled, want R1's copy once", got)
	}
}

// STOPPED AFTER THE OWED WRITE, BEFORE THE PUBLISH: the run's ending is kept
// for the reply it owes — the row survives the ending with the copy on it —
// and the seat's next holder publishes it, once, as it reaps the row.
func TestARetryWhoseRunEndsStoppedBeforeThePublishIsFinishedByTheNextHolder(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	r1, _ := owedAnAnswerWhoseRunEnds(t, rig)
	rig.failPublishes(errors.New("the broker is unreachable"))
	rig.fireRetries()

	copyID := declinedCopyID(r1.ID).String()
	got, found, err := rig.pending.Get(t.Context(), "t1")
	if err != nil || !found || len(got.HandBack) != 1 || got.HandBack[0].ID != copyID {
		t.Fatalf("run %v (found %v, %v), want it kept with R1's copy owed: the ending deleted "+
			"the only record of the reply before the reply was out", got.HandBack, found, err)
	}
	if !holds.holding("swe") {
		t.Fatal("the seat's inbox was let go while the reply its ended run owes is unpublished")
	}

	rig.coordinator.Stop()
	rig.failPublishes(nil)
	successorOf(t, rig, rig.pending)
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v, want R1's copy exactly once", got)
	}
	rig.finished("t1")
}

// AND THIS NODE FINISHES IT ITSELF when it keeps the seat: the ending it kept
// is retried, the copy published, and the row ended.
func TestARetryWhoseRunEndsFinishesItsEndingOnceThePublishLands(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	r1, _ := owedAnAnswerWhoseRunEnds(t, rig)
	rig.failPublishes(errors.New("the broker is unreachable"))
	rig.fireRetries()
	rig.failPublishes(nil)

	if rig.fireRetries() != 1 {
		t.Fatal("nothing was scheduled to finish the ending the hand-back held up")
	}
	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy once", got)
	}
	rig.finished("t1")
	if holds.holding("swe") {
		t.Fatalf("the seat's inbox stays held after the reply was handed back and the run ended: %v %+v", holds.log, rig.coordinator.runs)
	}
}

// STOPPED AFTER THE PUBLISH, BEFORE THE CLEAR: the copy is published once here,
// not over and over, and the next holder publishes it again under THE SAME ID
// — one message to the inbox's same-id dedupe and the completion ledger.
func TestARetryWhoseRunEndsStoppedBeforeTheClearRepublishesTheSameMessage(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	r1, _ := owedAnAnswerWhoseRunEnds(t, rig, "ClearHandBack")
	rig.fireRetries()
	copyID := declinedCopyID(r1.ID).String()
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v, want R1's copy published once while its clear is refused", got)
	}

	rig.coordinator.Stop()
	successorOf(t, rig, rig.pending)
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID, copyID}) {
		t.Fatalf("handed back %v, want R1's copy republished under the SAME id", got)
	}
	rig.finished("t1")
}

// STOPPED AFTER THE CLEAR, BEFORE THE DELETE: the row owes nothing, and the next
// holder ends it without handing anything back again.
func TestARetryWhoseRunEndsStoppedBeforeTheDeleteHandsNothingBackTwice(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	r1, store := owedAnAnswerWhoseRunEnds(t, rig)
	// THE FIRST FINISH IS THE DELETE: the ending lets the reply go and
	// publishes and clears its copy before it asks for one.
	rig.coordinator.pending = &finishUntil{refusingStore: store}
	rig.fireRetries()
	copyID := declinedCopyID(r1.ID).String()
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v, want R1's copy", got)
	}
	if got := rig.get("t1"); len(got.HandBack) != 0 {
		t.Fatalf("hand-back %+v, want it cleared before the refused delete", got.HandBack)
	}

	rig.coordinator.Stop()
	successorOf(t, rig, rig.pending)
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v, want R1's copy only once", got)
	}
	rig.finished("t1")
}

// A ROW THAT CANNOT RECORD THE REPLY IS NOT ENDED until it can: the ending is
// kept, with the answer still on the row and the seat's inbox held behind it,
// and nothing is announced yet. Once the store takes the let-go, the reply is
// handed back once, the run ended, and the loss announced once. The reply used
// to be published straight off the refusal instead — and lost, with the row
// deleted under it, when the broker refused that publish too.
func TestARetryWhoseRunEndsKeepsItsEndingUntilTheReplyIsRecorded(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	r1, store := owedAnAnswerWhoseRunEnds(t, rig, "OweHandBack")
	rig.fireRetries()
	if got := rig.handedBack(); len(got) != 0 {
		t.Fatalf("handed back %v before the reply was recorded as owed", got)
	}
	got, found, err := rig.pending.Get(t.Context(), "t1")
	if err != nil || !found || got.Answer == nil {
		t.Fatalf("run %+v (found %v, %v), want it kept with its answer: an ending that could not "+
			"record the reply deleted the only record of it", got.Answer, found, err)
	}
	if !holds.holding("swe") {
		t.Fatal("the seat's inbox was let go while the reply its ended run owes is unrecorded")
	}
	if n := len(rig.failures()); n != 0 {
		t.Fatalf("announced %d failures for an ending that has not landed", n)
	}

	store.refuse = []string{"ReleaseClaim"}
	if rig.fireRetries() != 1 {
		t.Fatal("nothing was scheduled to finish the kept ending")
	}
	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy once", got)
	}
	rig.finished("t1")
	if got := rig.failures(); len(got) != 1 || got[0].Reason != types.SandboxFailureClaimStranded {
		t.Fatalf("announced %+v, want the run's loss once, under the reason it was decided on", got)
	}
	if holds.holding("swe") {
		t.Fatal("the seat's inbox stays held after the reply was handed back and the run ended")
	}
}

// finishUntil lets the first allowed deletes through and refuses every later
// one: a process stopped after the clear, before the delete.
type finishUntil struct {
	*refusingStore
	allowed  int
	finishes int
}

func (s *finishUntil) Finish(ctx context.Context, turnID, ending string) (PendingRun, bool, error) {
	s.finishes++
	if s.finishes > s.allowed {
		return PendingRun{}, false, errRefusedCall
	}
	return s.refusingStore.Finish(ctx, turnID, ending)
}

// releaseLandsThenFails gives every claim back and then reports that it could
// not: the write landed, and the store's answer was lost — a timeout after the
// commit.
type releaseLandsThenFails struct{ PendingStore }

func (s releaseLandsThenFails) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	if _, err := s.PendingStore.ReleaseClaim(ctx, turnID, release); err != nil {
		return false, err
	}
	return false, errRefusedCall
}

// A RELEASE THAT LANDED AND REPORTED A FAILURE LEAVES THE RUN TO ITS RETRY,
// REPLY AND ALL. The retried resume fails and gives its claim back; the write
// lands — the run is answered again, R1 still on it — and the store reports a
// failure. The claim's own ending is licensed for the claim alone, so it finds
// the run no longer the claim and leaves it: the next retry resumes it with R1,
// nothing is handed back, and nothing is announced lost. It used to end the run
// on every status — its box reclaimed and R1's delivery recorded as worked —
// and, refusing to let R1 go from a run that no longer read as the claim,
// delete the run with R1 on it.
func TestARetryWhoseReleaseLandedAndFailedIsResumedWithItsReply(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	parkOnAQuestion(t, rig)
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	rig.resumer.failWith(errors.New("transient"))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	box := rig.get("t1").SandboxID
	rig.coordinator.pending = releaseLandsThenFails{PendingStore: rig.pending}
	if rig.fireRetries() != 1 {
		t.Fatal("the failed resume scheduled no retry")
	}

	got := rig.get("t1")
	if got.Status != StatusAnswered || got.Answer == nil || got.Answer.Taken() ||
		!slices.Equal(got.Answer.EventIDs, []string{r1.ID.String()}) {
		t.Fatalf("run = %q answer %+v, want it answered with R1 still on it, owed its retry",
			got.Status, got.Answer)
	}
	if handed := rig.handedBack(); len(handed) != 0 {
		t.Fatalf("handed back %v for a run its retry still owes R1 to", handed)
	}
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("announced %+v for a run that was handed back for its retry", failed)
	}
	if killed := rig.provider.KilledIDs(); slices.Contains(killed, box) {
		t.Fatalf("killed %v: the box of a run owed its retry", killed)
	}
	if !holds.holding("swe") {
		t.Fatal("the seat's inbox is open while its run is still owed R1's resume")
	}

	rig.coordinator.pending = rig.pending
	rig.resumer.failWith(nil)
	if rig.fireRetries() != 1 {
		t.Fatal("nothing retried the resume the run is owed")
	}
	if calls := rig.resumer.calls(); len(calls) != 1 || !strings.Contains(calls[0].Answer, "use main") {
		t.Fatalf("resumed %+v, want the run resumed once with R1", calls)
	}
	if handed := rig.handedBack(); len(handed) != 0 {
		t.Fatalf("handed back %v as well as resuming the run with it", handed)
	}
	rig.finished("t1")
}

// A KEPT ENDING THAT FINDS ITS CLAIM HANDED BACK AFTER ALL RESUMES THE RUN.
// The retried resume fails and gives its claim back; the write lands, the
// store reports a failure, and the ending decided for the claim cannot reach
// the store either, so it is kept — undecided whether the hand-back landed.
// The retry that finishes it finds the run answered again on the same launch,
// R1 still on it: no longer the claim, so the ending declines, and the run is
// owed exactly the resume this series is for. It is resumed with R1, nothing
// is handed back and nothing is announced lost. Settled as an ending that
// merely had nothing left to do, as it was, the series stopped there and the
// answered run waited — its seat's inbox held behind it — for a resume
// nothing would ever start.
func TestAKeptEndingWhoseClaimWasHandedBackResumesTheRun(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	parkOnAQuestion(t, rig)
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	rig.resumer.failWith(errors.New("transient"))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	box := rig.get("t1").SandboxID
	store := &refusingStore{
		inner:  releaseLandsThenFails{PendingStore: rig.pending},
		refuse: []string{"Finish"},
	}
	rig.coordinator.pending = store
	if rig.fireRetries() != 1 {
		t.Fatal("the failed resume scheduled no retry")
	}
	if got := rig.get("t1"); got.Status != StatusAnswered || got.Answer == nil || got.Answer.Taken() {
		t.Fatalf("run = %q answer %+v; the premise is a hand-back that landed with R1 on the row",
			got.Status, got.Answer)
	}
	if killed := rig.provider.KilledIDs(); slices.Contains(killed, box) {
		t.Fatalf("killed %v: the box of a run its kept ending has not decided yet", killed)
	}

	rig.coordinator.pending = rig.pending
	rig.resumer.failWith(nil)
	if rig.fireRetries() == 0 {
		t.Fatal("nothing was scheduled to finish the kept ending")
	}
	if calls := rig.resumer.calls(); len(calls) != 1 || !strings.Contains(calls[0].Answer, "use main") {
		t.Fatalf("resumed %+v, want the run resumed once with R1", calls)
	}
	if handed := rig.handedBack(); len(handed) != 0 {
		t.Fatalf("handed back %v as well as resuming the run with it", handed)
	}
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("announced %+v for a run that was resumed", failed)
	}
	// ONE ACCOUNT OF THE ANSWER, and the true one: the attempt whose
	// ending was kept undecided did not know the run was gone, so it says
	// nothing about the answer until the ending decides.
	var outcomes []types.AnswerOutcome
	for _, a := range rig.answeredRecords() {
		outcomes = append(outcomes, a.Outcome)
	}
	if !slices.Equal(outcomes, []types.AnswerOutcome{types.AnswerResumed}) {
		t.Fatalf("answer announced as %v, want it resumed, once", outcomes)
	}
	rig.finished("t1")
	if holds.holding("swe") {
		t.Fatal("the seat's inbox stays held after its run was resumed and finished")
	}
}

// AN ENDING KEPT BEFORE ITS DELETE STILL RECLAIMS ITS BOX. A retried resume
// breaks before its turn begins, and the store degrades the instant it does:
// the row cannot be read, so the run is ended while it is still the claim —
// record first, box second — and the reply on it cannot be let go of, so that
// ending is kept. The retry that finishes it hands the reply back, deletes the
// record, and reclaims the box the record named. Finished without the reclaim,
// as it was, the kept ending deleted the only record of a paused box and left
// it billed, named by nothing, until its provider's TTL.
func TestAKeptClaimEndingReclaimsItsBoxWhenItIsFinished(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	store := &refusingStore{inner: rig.pending}
	rig.coordinator.pending = store
	parkOnAQuestion(t, rig)
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	rig.resumer.failWith(errors.New("transient"))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	box := rig.get("t1").SandboxID
	rig.resumer.failWith(ErrResumeAbandoned)
	rig.resumer.beforeTurn = true
	rig.resumer.during = func(context.Context, PendingRun) {
		store.refuse = []string{"Get", "OweHandBack"}
	}
	rig.fireRetries()

	if got := rig.get("t1"); got.Answer == nil {
		t.Fatalf("answer %+v, want the ending kept with R1 still on the row", got.Answer)
	}
	if killed := rig.provider.KilledIDs(); slices.Contains(killed, box) {
		t.Fatalf("killed %v before the record that names the box was deleted", killed)
	}
	if got := rig.handedBack(); len(got) != 0 {
		t.Fatalf("handed back %v before the reply was recorded as owed", got)
	}

	store.refuse = nil
	rig.resumer.during = nil
	if rig.fireRetries() == 0 {
		t.Fatal("nothing was scheduled to finish the kept ending")
	}
	rig.finished("t1")
	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy exactly once", got)
	}
	if killed := rig.provider.KilledIDs(); !slices.Contains(killed, box) {
		t.Fatalf("killed %v, want the box %q the deleted record named", killed, box)
	}
}

// A DELETE THE STORE DID NOT TAKE IS FINISHED BY THIS NODE. The run's turn came
// back and its ending could not delete the record, which was a claim — and a
// claim holds its seat on every recount. It used to be left for the seat's next
// recovery pass, which comes only when the seat moves, so the seat took no mail
// until it did. The ending is kept and retried instead, and the seat is free
// the moment the delete lands.
func TestAnEndingWhoseDeleteTheStoreRefusedIsFinishedByThisNode(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	store := &refusingStore{inner: rig.pending}
	rig.coordinator.pending = store
	rig.runner.Finish(Result{Success: true, Text: "done"})
	rig.resumer.during = func(context.Context, PendingRun) {
		store.refuse = []string{"Finish"}
	}
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || !found {
		t.Fatalf("Get = %v, %v; the premise is a delete that did not land", found, err)
	}

	store.refuse = nil
	rig.resumer.during = nil
	if rig.fireRetries() != 1 {
		t.Fatal("nothing was scheduled to finish the ending whose delete the store did not take")
	}
	rig.finished("t1")
	if rig.coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the seat is still held by a run whose record is gone")
	}
}

// A TURN THAT TOOK THE REPLY AND GAVE ITS CLAIM BACK USED NOTHING, so when the
// claim cannot go back, the reply still goes back to the seat — once. The
// retry's turn takes the answer and then fails as a retry: a resume that gives
// its claim back has said nothing it did reached anybody, and the retry would
// hand the reply to a turn of its own. But the claim cannot be given back, so no
// retry will ever come, and the run is ended. Read as "a turn took it", the
// reply went with the run; the claim's own ending lets it go back however it
// was taken.
func TestARetryWhoseTurnGaveItsClaimBackHandsTheReplyBackWhenTheClaimCannotGoBack(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	r1, _ := owedAnAnswerWhoseRunEnds(t, rig)
	rig.resumer.failsInTurn = true
	if rig.fireRetries() != 1 {
		t.Fatal("the failed resume scheduled no retry")
	}
	if n := rig.resumer.turnsBegun(); n != 1 {
		t.Fatalf("%d turns began, want the retry's turn to have taken the answer", n)
	}
	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy exactly once: the turn that took it gave it back", got)
	}
	rig.finished("t1")
	failed := rig.failures()
	if len(failed) != 1 || failed[0].Reason != types.SandboxFailureClaimStranded ||
		!strings.Contains(failed[0].Detail, "goes back to the seat") {
		t.Fatalf("announced %+v, want the run's loss once, saying its reply went back", failed)
	}
}
