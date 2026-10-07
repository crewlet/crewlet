package sandbox

import (
	"context"
	"errors"
	"slices"
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
	rig := newCoordRig(t)
	r1, store := owedAnAnswerWhoseRunEnds(t, rig)
	rig.coordinator.pending = &finishOnce{refusingStore: store}
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

// finishOnce lets the first Finish through — the one the owed copy refuses —
// and refuses every later one: a process stopped after the clear, before the
// delete.
type finishOnce struct {
	*refusingStore
	finishes int
}

func (s *finishOnce) Finish(ctx context.Context, turnID string, fence Fence, whileIn []string,
) (PendingRun, bool, error) {
	s.finishes++
	if s.finishes > 1 {
		return PendingRun{}, false, errRefusedCall
	}
	return s.refusingStore.Finish(ctx, turnID, fence, whileIn)
}
