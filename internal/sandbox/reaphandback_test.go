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

// A CLAIM THAT DIED BEFORE ITS TURN TOOK THE ANSWER HANDS THE REPLY BACK, ONCE.
//
// A person's reply is recorded on the run and its delivery spent; a resume then
// claims the row — and the process stops before the resumed turn takes the
// answer. The seat's next holder finds a claim nobody will ever finish and
// reaps it as an abandoned tail. It used to read the reply as spent with it,
// so the person's answer was silently lost. The row now says whether a turn
// took the answer ([RecordedAnswer.TakenAt]): one nobody took goes back to the
// seat as the ordinary message it is, through the same outbox a decline uses,
// and one a turn took is spent. Each case below stops a node at one step of
// claim → resume → hand-back, by leaving the row where that node left it or by
// refusing the step, and hands the seat to a successor over the same store.

// answeredAndClaimed records R1 on t1 — its inline resume failing, so the
// answer is owed — and then claims the row for the retry's resume, as the node
// holding the seat does the instant before it stops. took says whether that
// node's turn got as far as taking the answer before it stopped.
func answeredAndClaimed(t *testing.T, rig *coordRig, took bool) *events.Event {
	t.Helper()
	parkOnAQuestion(t, rig)
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	rig.resumer.failWith(errors.New("transient"))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	launch := rig.get("t1").LaunchID
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1", RecordedAnswerTail(launch)); err != nil || !won {
		t.Fatalf("the dying node's claim = %v, %v", won, err)
	}
	if took {
		if ok, err := rig.pending.TakeAnswer(t.Context(), "t1", launch, Fence{}); err != nil || !ok {
			t.Fatalf("the dying node's turn taking the answer = %v, %v", ok, err)
		}
	}
	// THE NODE STOPS: none of its retries run again, and nothing it held
	// in memory survives.
	rig.coordinator.Stop()
	return r1
}

// reaper is the seat's next holder under lease epoch, over store: a fresh
// coordinator on the same broker, readied by prepare before it recovers the
// seat — the moment it reaps.
func reaper(ctx context.Context, t *testing.T, rig *coordRig, store PendingStore, epoch int64,
	prepare func(next *coordRig),
) *coordRig {
	t.Helper()
	next := newCoordRig(t)
	next.waiterRig = rig.waiterRig
	next.coordinator.pending = store
	next.coordinator.queue = rig.queue
	if prepare != nil {
		prepare(next)
	}
	if err := next.coordinator.RecoverSeat(ctx, "swe", "node-"+string(rune('a'+epoch)), epoch); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	return next
}

// abandonedOnce asserts the run was announced lost exactly once, as an
// abandoned tail, saying whether its answer went back to the seat.
func abandonedOnce(t *testing.T, rig *coordRig, handedBack bool) {
	t.Helper()
	got := rig.failures()
	if len(got) != 1 || got[0].Reason != types.SandboxFailureAbandoned {
		t.Fatalf("announced %+v, want the run lost once, as an abandoned tail", got)
	}
	if says := strings.Contains(got[0].Detail, "goes back to the seat"); says != handedBack {
		t.Errorf("the announcement says %q; it should say whether the answer went back (%v)",
			got[0].Detail, handedBack)
	}
}

// STOPPED BETWEEN THE CLAIM AND THE TURN: the next holder hands the reply back,
// once — spent first, so the original delivery coming round after it is not a
// second message — ends the run and announces it.
func TestAClaimThatDiedBeforeItsTurnHandsTheReplyBackOnce(t *testing.T) {
	rig := newCoordRig(t)
	r1 := answeredAndClaimed(t, rig, false)
	var holds *holdSpy
	next := reaper(t.Context(), t, rig, rig.pending, 2, func(next *coordRig) { holds = next.withHold() })

	copyID := declinedCopyID(r1.ID).String()
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v, want R1's copy exactly once: the claim died before any turn "+
			"took the reply, so nothing else will ever bring it to the seat", got)
	}
	rig.finished("t1")
	abandonedOnce(t, rig, true)
	spent := next.spentDeliveries()
	if len(spent) != 1 || spent[0].id != r1.ID.String() || spent[0].publishedBefore != 0 {
		t.Fatalf("spent %+v, want R1's own delivery recorded as worked BEFORE its copy was "+
			"handed back: a delivery the dead node never acknowledged comes round again", spent)
	}
	if calls := next.resumer.calls(); len(calls) != 0 {
		t.Fatalf("the next holder resumed a reaped run: %+v", calls)
	}
	if holds.holding("swe") {
		t.Fatalf("the seat's inbox is held after the reply was handed back and the run ended: %v",
			holds.log)
	}
}

// A TURN THAT TOOK THE ANSWER HAS USED IT: the next holder reaps the claim
// without handing the reply back — a copy now would answer the person twice.
func TestAClaimWhoseTurnTookTheAnswerHandsNothingBack(t *testing.T) {
	rig := newCoordRig(t)
	answeredAndClaimed(t, rig, true)
	reaper(t.Context(), t, rig, rig.pending, 2, nil)

	if got := rig.handedBack(); len(got) != 0 {
		t.Fatalf("handed back %v for a reply a turn had already taken", got)
	}
	rig.finished("t1")
	abandonedOnce(t, rig, false)
}

// STOPPED AFTER THE FENCE, BEFORE THE LET-GO LANDED: the reaping holder keeps
// the row — the answer still on it — and announces nothing yet; the holder
// after it hands the reply back once and announces the loss once.
func TestAReapStoppedBeforeItsLetGoIsFinishedByTheNextHolder(t *testing.T) {
	rig := newCoordRig(t)
	r1 := answeredAndClaimed(t, rig, false)
	first := reaper(t.Context(), t, rig, &refusingStore{inner: rig.pending, refuse: []string{"OweHandBack"}}, 2, nil)
	if got := rig.handedBack(); len(got) != 0 {
		t.Fatalf("handed back %v before the reply was recorded as owed", got)
	}
	if got := rig.get("t1"); got.Answer == nil || got.Answer.Taken() || got.OwnerEpoch != 2 {
		t.Fatalf("run answer %+v epoch %d, want it kept, fenced to the reaper's lease, with the "+
			"reply untaken", got.Answer, got.OwnerEpoch)
	}
	if n := len(rig.failures()); n != 0 {
		t.Fatalf("announced %d failures for a reap that has not landed", n)
	}

	first.coordinator.Stop()
	reaper(t.Context(), t, rig, rig.pending, 3, nil)
	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy exactly once", got)
	}
	rig.finished("t1")
	abandonedOnce(t, rig, true)
}

// STOPPED AFTER THE LET-GO, BEFORE THE PUBLISH: the reply is owed on the row and
// the answer is off it, so the next holder publishes the copy once and lets go
// of nothing a second time.
func TestAReapStoppedBeforeItsPublishIsFinishedByTheNextHolder(t *testing.T) {
	rig := newCoordRig(t)
	r1 := answeredAndClaimed(t, rig, false)
	rig.failPublishes(errors.New("the broker is unreachable"))
	var holds *holdSpy
	first := reaper(t.Context(), t, rig, rig.pending, 2, func(next *coordRig) { holds = next.withHold() })
	copyID := declinedCopyID(r1.ID).String()
	got := rig.get("t1")
	if got.Answer != nil || len(got.HandBack) != 1 || got.HandBack[0].ID != copyID {
		t.Fatalf("run answer %+v hand-back %+v, want the answer let go of and R1's copy owed",
			got.Answer, got.HandBack)
	}
	if !holds.holding("swe") {
		t.Fatal("the seat's inbox is open while the reply the reaped run owes is unpublished")
	}

	first.coordinator.Stop()
	rig.failPublishes(nil)
	reaper(t.Context(), t, rig, rig.pending, 3, nil)
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v, want R1's copy exactly once", got)
	}
	rig.finished("t1")
	abandonedOnce(t, rig, true)
}

// AND THE REAPING HOLDER FINISHES IT ITSELF when it keeps the seat: the kept
// ending publishes the copy once the broker answers, ends the run and makes the
// announcement it held back.
func TestAReapWhosePublishFailedFinishesOnItsRetry(t *testing.T) {
	rig := newCoordRig(t)
	r1 := answeredAndClaimed(t, rig, false)
	rig.failPublishes(errors.New("the broker is unreachable"))
	first := reaper(t.Context(), t, rig, rig.pending, 2, nil)
	rig.failPublishes(nil)
	if first.fireRetries() != 1 {
		t.Fatal("nothing was scheduled to finish the reap the hand-back held up")
	}
	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy once", got)
	}
	rig.finished("t1")
	abandonedOnce(t, rig, true)
}

// STOPPED AFTER THE PUBLISH, BEFORE THE CLEAR: the next holder publishes the copy
// again under THE SAME ID — one message to the inbox's same-id dedupe and the
// completion ledger — and the loss is announced once.
func TestAReapStoppedBeforeItsClearRepublishesTheSameMessage(t *testing.T) {
	rig := newCoordRig(t)
	r1 := answeredAndClaimed(t, rig, false)
	first := reaper(t.Context(), t, rig, &refusingStore{inner: rig.pending, refuse: []string{"ClearHandBack"}}, 2, nil)
	copyID := declinedCopyID(r1.ID).String()
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v, want R1's copy published once while its clear is refused", got)
	}

	first.coordinator.Stop()
	reaper(t.Context(), t, rig, rig.pending, 3, nil)
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID, copyID}) {
		t.Fatalf("handed back %v, want R1's copy republished under the SAME id", got)
	}
	rig.finished("t1")
	abandonedOnce(t, rig, true)
}

// STOPPED AFTER THE CLEAR, BEFORE THE DELETE: the row owes nothing and carries
// no answer, so the next holder ends it without handing anything back again.
func TestAReapStoppedBeforeItsDeleteHandsNothingBackTwice(t *testing.T) {
	rig := newCoordRig(t)
	r1 := answeredAndClaimed(t, rig, false)
	reaper(t.Context(), t, rig, &finishOnce{refusingStore: &refusingStore{inner: rig.pending}}, 2, nil)
	copyID := declinedCopyID(r1.ID).String()
	if got := rig.get("t1"); got.Answer != nil || len(got.HandBack) != 0 {
		t.Fatalf("answer %+v hand-back %+v, want both gone before the refused delete",
			got.Answer, got.HandBack)
	}

	reaper(t.Context(), t, rig, rig.pending, 3, nil)
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v, want R1's copy only once", got)
	}
	rig.finished("t1")
}

// A ROW THE REAP CANNOT FENCE IS STILL REAPED AS OWING ITS REPLY: whether a
// turn took the answer is then unknowable, and a reply handed back twice is a
// duplicate where one read as spent is lost.
func TestAReapThatCannotFenceTheClaimStillHandsTheReplyBack(t *testing.T) {
	rig := newCoordRig(t)
	r1 := answeredAndClaimed(t, rig, false)
	reaper(t.Context(), t, rig, &refusingStore{inner: rig.pending, refuse: []string{"ClaimOwnership"}}, 2, nil)
	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy once", got)
	}
	rig.finished("t1")
}

// A NODE THAT LOST THE SEAT CANNOT TAKE THE ANSWER THE NEXT HOLDER IS RETURNING.
// The old holder's retry has claimed the row and stalls on the way to its turn;
// the next holder fences the row and decides to hand the reply back. When the
// old holder's turn finally tries to begin, the take is refused — its turn
// never runs — so the person gets the reply once, as the ordinary message, and
// not also as the answer of a turn on a node that no longer holds the seat.
func TestANodeThatLostTheSeatCannotTakeTheAnswerItsSuccessorReturns(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	rig.resumer.failWith(errors.New("transient"))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	// THE SUCCESSOR'S LET-GO IS REFUSED, so the row keeps the answer and
	// only the fence stands between it and the stalled node's take.
	successorStore := &refusingStore{inner: rig.pending, refuse: []string{"OweHandBack"}}
	var next *coordRig
	rig.resumer.failWith(nil)
	rig.resumer.beforeBegin = func(ctx context.Context) {
		next = reaper(ctx, t, rig, successorStore, 2, nil)
	}
	rig.fireRetries()
	if next == nil {
		t.Fatal("the stalled retry never reached its turn")
	}
	if n := rig.resumer.turnsBegun(); n != 0 {
		t.Fatalf("the node that lost the seat began %d turns with an answer its successor had "+
			"fenced: the reply reaches the person as that turn and as the copy", n)
	}
	if got := rig.get("t1"); got.Answer == nil || got.Answer.Taken() {
		t.Fatalf("answer %+v, want it untaken: the stalled take must not land", got.Answer)
	}
	rig.coordinator.Stop()

	successorStore.refuse = nil
	if next.fireRetries() != 1 {
		t.Fatal("the successor kept no ending to finish")
	}
	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy once", got)
	}
	rig.finished("t1")
}

// A CLAIM GIVEN BACK BEFORE THE FENCE LANDED IS NOT ABANDONED: the old holder's
// release reached the row first, so it is owed its resume again, and the next
// holder resumes it with the answer rather than reaping it.
func TestAClaimGivenBackUnderTheReapIsResumedNotReaped(t *testing.T) {
	rig := newCoordRig(t)
	answeredAndClaimed(t, rig, false)
	run := rig.get("t1")
	store := &releasesFirst{PendingStore: rig.pending, release: func(ctx context.Context) {
		if ok, err := rig.pending.ReleaseClaim(ctx, "t1", Release{
			Launch: run.LaunchID, To: StatusAnswered, Fence: fenceOf(run),
		}); err != nil || !ok {
			t.Errorf("the old holder's release = %v, %v", ok, err)
		}
	}}
	next := reaper(t.Context(), t, rig, store, 2, nil)
	next.fireRetries()
	if calls := next.resumer.calls(); len(calls) != 1 || !strings.Contains(calls[0].Answer, "use main") {
		t.Fatalf("the next holder resumed %+v, want the run resumed once with its answer", calls)
	}
	if got := rig.handedBack(); len(got) != 0 {
		t.Fatalf("handed back %v for an answer its run was resumed with", got)
	}
	if n := len(rig.failures()); n != 0 {
		t.Fatalf("announced %d failures for a run that was resumed", n)
	}
}

// releasesFirst runs the old holder's release at the instant the reap fences
// the row: the one write a node that lost the seat can still land first.
type releasesFirst struct {
	PendingStore
	release func(ctx context.Context)
	done    bool
}

func (s *releasesFirst) ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error) {
	if !s.done {
		s.done = true
		s.release(ctx)
	}
	return s.PendingStore.ClaimOwnership(ctx, turnID, owner, epoch)
}

// A RESUME THAT BROKE BEFORE ITS TURN BEGAN USED NOTHING. A retried resume
// abandoned that way — a panic re-entering the conversation — settles the run
// as every abandoned resume does, but the reply it was driving goes back to
// the seat rather than being read as spent; the inline attempt, which still
// holds the delivery, hands that on instead; and an answer by turn is reported
// as reaching a run that is gone, not as having resumed it.
func TestAResumeAbandonedBeforeItsTurnHandsTheAnswerOn(t *testing.T) {
	t.Run("a retry hands the reply back through the row", func(t *testing.T) {
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
		rig.resumer.failWith(errors.New("transient"))
		if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
			chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
			t.Fatalf("R1 = %q, want it recorded", d)
		}
		rig.resumer.failWith(ErrResumeAbandoned)
		rig.resumer.beforeTurn = true
		rig.fireRetries()
		if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
			t.Fatalf("handed back %v, want R1's copy once: no turn ever took it", got)
		}
		rig.finished("t1")
	})
	t.Run("the inline attempt hands its own delivery on", func(t *testing.T) {
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
		rig.resumer.failWith(ErrResumeAbandoned)
		rig.resumer.beforeTurn = true
		d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
			chatReply(answerOnTheDM, "use main", r1))
		if d != AnswerNotMine {
			t.Fatalf("disposition = %q, want the delivery handed on as the ordinary message", d)
		}
		if got := rig.handedBack(); len(got) != 0 {
			t.Fatalf("handed back %v as well as handing the delivery on", got)
		}
		rig.finished("t1")
	})
	t.Run("an answer by turn reached a run that is gone", func(t *testing.T) {
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		rig.resumer.failWith(ErrResumeAbandoned)
		rig.resumer.beforeTurn = true
		given := types.SandboxAnswerGiven{TurnID: "t1", AgentHandle: "swe", Answer: "use main"}
		if d, _ := rig.coordinator.AnswerByTurn(t.Context(), given,
			events.New(given, events.TraceContext{})); d != AnswerNotMine {
			t.Fatalf("disposition = %q, want the answer spent on a run that is gone", d)
		}
		if got := rig.answeredRecords(); len(got) != 1 || got[0].Outcome != types.AnswerGone {
			t.Fatalf("announced %+v, want one answer that reached a run that is gone", got)
		}
		rig.finished("t1")
	})
}

// A RETRIED RESUME WHOSE TURN TOOK THE ANSWER SPENDS ITS DELIVERY, so a copy of
// it — left unacknowledged by a node that recorded the answer and stopped — is
// not run again as an ordinary message once the run is gone. The inline
// attempt records its own delivery, through the dispatcher, and spends nothing
// here.
func TestATurnThatTookARetriedAnswerSpendsItsDelivery(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	rig.resumer.failWith(errors.New("transient"))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	if spent := rig.spentDeliveries(); len(spent) != 0 {
		t.Fatalf("the inline attempt spent %+v, want nothing: its caller holds the delivery", spent)
	}
	rig.resumer.failWith(nil)
	// THE TURN TOOK THE ANSWER ON THE ROW before it ran: what the seat's
	// next holder reads if this process stops mid-turn.
	took := false
	rig.resumer.during = func(ctx context.Context, _ PendingRun) {
		got, found, err := rig.pending.Get(ctx, "t1")
		took = err == nil && found && got.Answer != nil && got.Answer.Taken()
	}
	rig.fireRetries()
	if calls := rig.resumer.calls(); len(calls) != 1 {
		t.Fatalf("resumed %d times, want once", len(calls))
	}
	if !took {
		t.Fatal("the resumed turn ran without its run recording that it took the answer: a " +
			"crash mid-turn would have the reply handed back as well as used")
	}
	spent := rig.spentDeliveries()
	if len(spent) != 1 || spent[0].id != r1.ID.String() {
		t.Fatalf("spent %+v, want R1's delivery once the retry's turn took it", spent)
	}
	rig.finished("t1")
}

// A REAP WAITS ON AN EARLIER COPY IT CANNOT CLEAR, RATHER THAN REPUBLISHING IT.
// R1 was let go of and its copy published, but clearing it from the row keeps
// failing; R2 is recorded and claimed by a node that stops before its turn
// takes it. The reap hands R1's copy out once more on its way to letting R2
// go — and when the clear still fails, it waits for the retry instead of going
// round publishing the same copy until it gives up. The holder after it finds
// the store answering and hands R2 back.
func TestAReapWaitsOnAnEarlierCopyItCannotClear(t *testing.T) {
	rig := newCoordRig(t)
	store := &refusingStore{inner: rig.pending}
	rig.coordinator.pending = store
	r1 := owedAnAnswerItCannotResume(t, rig)
	store.refuse = []string{"ClearHandBack"}
	rig.fireRetries() // R1's last attempt fails; it is let go of and published, not cleared
	r1Copy := declinedCopyID(r1.ID).String()
	r2 := replyAt("use dev", r1.Timestamp.Add(time.Minute))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use dev", r2)); d != AnswerConsumed {
		t.Fatalf("R2 = %q, want it recorded", d)
	}
	launch := rig.get("t1").LaunchID
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1", RecordedAnswerTail(launch)); err != nil || !won {
		t.Fatalf("the dying node's claim = %v, %v", won, err)
	}
	rig.coordinator.Stop()
	before := count(rig.handedBack(), r1Copy)

	first := reaper(t.Context(), t, rig,
		&refusingStore{inner: rig.pending, refuse: []string{"ClearHandBack"}}, 2, nil)
	if got := count(rig.handedBack(), r1Copy); got != before+1 {
		t.Fatalf("the reap published R1's copy %d more times, want once: a copy it cannot "+
			"clear is waited on, not republished", got-before)
	}
	if got := rig.get("t1"); got.Answer == nil || !slices.Equal(got.Answer.EventIDs, []string{r2.ID.String()}) {
		t.Fatalf("answer %+v, want R2 still on the row while R1's copy is uncleared", got.Answer)
	}

	first.coordinator.Stop()
	reaper(t.Context(), t, rig, rig.pending, 3, nil)
	if got := count(rig.handedBack(), declinedCopyID(r2.ID).String()); got != 1 {
		t.Fatalf("handed R2 back %d times, want once", got)
	}
	rig.finished("t1")
}

// count is how many times id appears in ids.
func count(ids []string, id string) int {
	n := 0
	for _, got := range ids {
		if got == id {
			n++
		}
	}
	return n
}
