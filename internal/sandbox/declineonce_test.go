package sandbox

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// LETTING AN ANSWER GO IS EXACTLY ONCE, ACROSS A CRASH AT EACH STEP.
//
// A decline is a write to the run's row and a publish to the seat's inbox. It
// used to publish first, so a crash between the two left the answer recorded
// AND its copy on the inbox: the next holder resumed the run with the reply
// that had also been worked as an ordinary message. Each case below stops the
// node at one step — by refusing that step and then handing the seat to a
// successor over the same store — and counts what the seat was given.

// owedAnAnswerItCannotResume records R1 on t1 and fails every resume of it
// but the last, so the next retry a case fires spends the attempts and lets it
// go.
func owedAnAnswerItCannotResume(t *testing.T, rig *coordRig) *events.Event {
	t.Helper()
	parkOnAQuestion(t, rig)
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	rig.resumer.failWith(errors.New("this build cannot read the suspended conversation"))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	for range MaxAnswerAttempts - 2 {
		if rig.fireRetries() != 1 {
			t.Fatal("a failed resume scheduled no retry")
		}
	}
	return r1
}

// handedBack is every message published to the seat's inbox, by id.
func (r *coordRig) handedBack() []string {
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	var out []string
	for _, p := range r.queue.published {
		if p.topic == topics.AgentInbox("swe") {
			out = append(out, p.event.ID.String())
		}
	}
	return out
}

// successorOf is the seat's next holder: a fresh coordinator over the same
// store and the same broker, whose resumes succeed, recovering the seat.
func successorOf(t *testing.T, rig *coordRig, store PendingStore) *coordRig {
	t.Helper()
	next := newCoordRig(t)
	next.waiterRig = rig.waiterRig
	next.coordinator.pending = store
	next.coordinator.queue = rig.queue
	if err := next.coordinator.RecoverSeat(t.Context(), "swe", "node-b", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	next.fireRetries()
	return next
}

// A DECLINE SPENDS THE REPLY'S OWN DELIVERY BEFORE IT LETS THE REPLY GO: the
// copy is the reply from then on, and the original coming round afterwards — a
// node that recorded the answer and stopped before acknowledging it — must be
// dropped by the completion ledger rather than worked beside the copy.
func TestADeclineSpendsTheReplyBeforeLettingItGo(t *testing.T) {
	rig := newCoordRig(t)
	r1 := owedAnAnswerItCannotResume(t, rig)
	rig.fireRetries() // the last attempt fails, and the answer is let go of
	spent := rig.spentDeliveries()
	if len(spent) != 1 || spent[0].id != r1.ID.String() || spent[0].publishedBefore != 0 {
		t.Fatalf("spent %+v, want R1's own delivery recorded as worked before its copy went out", spent)
	}
	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy once", got)
	}
}

// STOPPED BEFORE THE WRITE: nothing is handed back, the answer is still the
// run's, and the next holder resumes the run with it — the reply reaches the
// seat once, as the answer, and never as a message as well.
func TestADeclineStoppedBeforeItsWriteHandsNothingBack(t *testing.T) {
	rig := newCoordRig(t)
	store := &refusingStore{inner: rig.pending}
	rig.coordinator.pending = store
	owedAnAnswerItCannotResume(t, rig)
	store.refuse = []string{"DeclineAnswer"}
	rig.fireRetries() // the last attempt fails, and the decline's write is refused

	if got := rig.handedBack(); len(got) != 0 {
		t.Fatalf("handed back %v before the decline was written: a crash here delivers the "+
			"reply twice — as this copy and as the answer the next holder resumes with", got)
	}
	if got := rig.get("t1"); got.Status != StatusAnswered || got.Answer == nil {
		t.Fatalf("run = %q answer %+v, want the answer still recorded", got.Status, got.Answer)
	}

	rig.coordinator.Stop()
	next := successorOf(t, rig, rig.pending)
	if calls := next.resumer.calls(); len(calls) != 1 || !strings.Contains(calls[0].Answer, "use main") {
		t.Fatalf("the next holder resumed %+v, want the recorded answer once", calls)
	}
	if got := rig.handedBack(); len(got) != 0 {
		t.Fatalf("the reply was handed back as well as resumed: %v", got)
	}
}

// STOPPED AFTER THE WRITE, BEFORE THE PUBLISH: the write recorded what it
// owes, so the next holder publishes the copy — once — before it opens the
// seat's mailbox.
func TestADeclineStoppedBeforeItsPublishIsFinishedByTheNextHolder(t *testing.T) {
	rig := newCoordRig(t)
	holds := rig.withHold()
	r1 := owedAnAnswerItCannotResume(t, rig)
	rig.failPublishes(errors.New("the broker is unreachable"))
	rig.fireRetries() // the last attempt fails, the decline is written, its publish is not

	got := rig.get("t1")
	if got.Status != StatusAwaiting || got.Answer != nil || len(got.HandBack) != 1 ||
		got.HandBack[0].ID != declinedCopyID(r1.ID).String() {
		t.Fatalf("run = %q answer %+v hand-back %+v, want the question reopened and R1's copy "+
			"recorded as owed", got.Status, got.Answer, got.HandBack)
	}
	if !holds.holding("swe") {
		t.Fatal("the seat's inbox was let go while the reply it owes is unpublished")
	}

	rig.coordinator.Stop()
	rig.failPublishes(nil)
	next := successorOf(t, rig, rig.pending)
	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy exactly once", got)
	}
	if got := rig.get("t1"); len(got.HandBack) != 0 {
		t.Fatalf("hand-back %+v still owed after it was published", got.HandBack)
	}
	if calls := next.resumer.calls(); len(calls) != 0 {
		t.Fatalf("the next holder resumed the run with a reply that was let go: %+v", calls)
	}
}

// STOPPED AFTER THE PUBLISH, BEFORE THE CLEAR: the next holder publishes the
// copy again, and it is THE SAME MESSAGE — the same id — which the inbox's
// same-id dedupe and the completion ledger collapse into one.
func TestADeclineStoppedBeforeItsClearRepublishesTheSameMessage(t *testing.T) {
	rig := newCoordRig(t)
	store := &refusingStore{inner: rig.pending}
	rig.coordinator.pending = store
	r1 := owedAnAnswerItCannotResume(t, rig)
	store.refuse = []string{"ClearHandBack"}
	rig.fireRetries()
	copyID := declinedCopyID(r1.ID).String()
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v, want R1's copy", got)
	}

	rig.coordinator.Stop()
	successorOf(t, rig, rig.pending)
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID, copyID}) {
		t.Fatalf("handed back %v, want R1's copy republished under the SAME id", got)
	}
	if got := rig.get("t1"); len(got.HandBack) != 0 {
		t.Fatalf("hand-back %+v still owed after the next holder published it", got.HandBack)
	}
}

// WHAT A RUN OWES THE SEAT IS BOUNDED BY ONE DECLINE. R1 is let go of and its
// copy published, but clearing it from the row keeps failing; R2 is recorded,
// fails every resume and is let go of in turn. Its decline WAITS for R1's copy
// to be cleared — the answer stays recorded and owed, the row carries R1's copy
// and nothing more — and lands the moment it is, handing R2 back once. Without
// the bound, every answer let go of while the broker or the store kept failing
// grew the row by one more batch of copies.
func TestADeclineWaitsForTheCopiesAlreadyOwed(t *testing.T) {
	rig := newCoordRig(t)
	store := &refusingStore{inner: rig.pending}
	rig.coordinator.pending = store
	r1 := owedAnAnswerItCannotResume(t, rig)
	store.refuse = []string{"ClearHandBack"}
	rig.fireRetries() // R1's last attempt fails; it is let go of and published, not cleared
	r1Copy := declinedCopyID(r1.ID).String()
	if got := rig.get("t1"); got.Status != StatusAwaiting || len(got.HandBack) != 1 {
		t.Fatalf("run = %q hand-back %+v, want the question reopened with R1's copy still owed",
			got.Status, got.HandBack)
	}

	r2 := replyAt("use dev", r1.Timestamp.Add(time.Minute))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use dev", r2)); d != AnswerConsumed {
		t.Fatalf("R2 = %q, want it recorded", d)
	}
	for range MaxAnswerAttempts - 1 {
		if rig.fireRetries() != 1 {
			t.Fatal("R2's failed resume scheduled no retry")
		}
	}
	got := rig.get("t1")
	if got.Status != StatusAnswered || got.Answer == nil || !slices.Equal(got.Answer.EventIDs, []string{r2.ID.String()}) {
		t.Fatalf("run = %q answer %+v, want R2 still recorded and owed while R1's copy is "+
			"uncleared", got.Status, got.Answer)
	}
	if len(got.HandBack) != 1 || got.HandBack[0].ID != r1Copy {
		t.Fatalf("hand-back = %+v, want R1's copy and nothing more: what a row owes is bounded "+
			"by one decline", got.HandBack)
	}

	store.refuse = nil
	rig.fireRetries()
	r2Copy := declinedCopyID(r2.ID).String()
	handed := rig.handedBack()
	r2s := 0
	for _, id := range handed {
		if id == r2Copy {
			r2s++
		}
	}
	if slices.ContainsFunc(handed, func(id string) bool { return id != r1Copy && id != r2Copy }) ||
		!slices.Contains(handed, r1Copy) || r2s != 1 || handed[len(handed)-1] != r2Copy {
		t.Fatalf("handed back %v, want R1's copy (repeated under its one id while it could not "+
			"be cleared) and then R2's once", handed)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || len(got.HandBack) != 0 {
		t.Fatalf("run = %q hand-back %+v, want the question reopened owing nothing", got.Status,
			got.HandBack)
	}
}

// A SECOND ANSWER GETS ITS OWN ATTEMPTS. R1 was let go of and its copy is still
// owed, so the run's retry series is alive — with R1's attempts all spent. R2
// is recorded and its first resume fails: it is retried on its own count, not
// let go of on the strength of R1's.
func TestASecondAnswerGetsItsOwnAttempts(t *testing.T) {
	rig := newCoordRig(t)
	r1 := owedAnAnswerItCannotResume(t, rig)
	rig.failPublishes(errors.New("the broker is unreachable"))
	rig.fireRetries() // R1 is let go of; its copy is owed and the series kept for it

	r2 := replyAt("use dev", r1.Timestamp.Add(time.Minute))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use dev", r2)); d != AnswerConsumed {
		t.Fatalf("R2 = %q, want it recorded", d)
	}
	if got := rig.get("t1"); got.Status != StatusAnswered || got.Answer == nil {
		t.Fatalf("run = %q answer %+v after R2's first failed resume, want R2 still owed its "+
			"attempts", got.Status, got.Answer)
	}
	if got := rig.retries.delays(); !slices.Equal(got, []time.Duration{answerRetryDelay(1)}) {
		t.Fatalf("next attempt in %v, want R2's first retry at %v", got, answerRetryDelay(1))
	}
}

// AND A RUN THAT ENDS WITH A COPY STILL OWED hands it back as its record goes:
// what a decline owes is the seat's, and nothing reads a deleted row again.
// Here the next reply is recorded and resumes the run to its end before the
// retry that would have published R1's copy comes round.
func TestARunThatEndsHandsBackWhatItStillOwes(t *testing.T) {
	rig := newCoordRig(t)
	r1 := owedAnAnswerItCannotResume(t, rig)
	rig.failPublishes(errors.New("the broker is unreachable"))
	rig.fireRetries()
	rig.failPublishes(nil)

	rig.resumer.failWith(nil)
	r2 := replyAt("use dev", r1.Timestamp.Add(time.Minute))
	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use dev", r2)); err != nil || d != AnswerConsumed {
		t.Fatalf("R2 = %q, %v, want it to answer the reopened question", d, err)
	}
	rig.finished("t1")
	copyID := declinedCopyID(r1.ID).String()
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v, want R1's copy as the run ended", got)
	}
	rig.fireRetries()
	if got := rig.handedBack(); !slices.Equal(got, []string{copyID}) {
		t.Fatalf("handed back %v after the retry, want R1's copy once", got)
	}
}
