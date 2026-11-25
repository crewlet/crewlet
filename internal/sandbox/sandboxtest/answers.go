package sandboxtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// answerOf is a recorded answer made of one delivery, carrying text's stand-in
// seal ([sealOf]).
func answerOf(id, text string) sandbox.RecordedAnswer {
	return sandbox.RecordedAnswer{Sealed: sealOf(text), Via: types.AnswerViaChat, EventIDs: []string{id},
		RecordedAt: base}
}

// sealOf stands in for the seal a coordinator puts on an answer: the store keeps
// a seal as the bytes it was handed and opens nothing, so any bytes that name
// the text tell one recorded answer from another here.
func sealOf(text string) []byte { return []byte("sealed: " + text) }

// copyOf is the hand-back a decline of delivery id owes the seat: one copy,
// under id + "-copy".
func copyOf(id string) sandbox.HandedBack {
	return sandbox.HandedBack{ID: id + "-copy", Original: id,
		Event: json.RawMessage(`{"id":"` + id + `-copy"}`)}
}

// decline lets go of the one-delivery answer id on t1, owing its copy.
func decline(t *testing.T, s sandbox.PendingStore, launch, id string) (bool, error) {
	t.Helper()
	_, ok, err := s.DeclineAnswer(t.Context(), "t1", launch, []string{id},
		[]sandbox.HandedBack{copyOf(id)}, sandbox.Fence{})
	return ok, err
}

// THE FIRST REPLY IS THE ANSWER, and the store is where that is decided: two
// replies racing for one question — on two nodes across a handoff — both read
// it waiting, and exactly one record lands.
func testTheFirstReplyRecordedIsTheAnswer(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := range racers {
		wg.Go(func() {
			_, ok, err := s.RecordAnswer(ctx, "t1", launch,
				answerOf(fmt.Sprintf("reply-%d", i), fmt.Sprintf("answer %d", i)), sandbox.Fence{})
			if err != nil {
				t.Errorf("RecordAnswer: %v", err)
				return
			}
			if ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d replies were recorded as the answer to one question, want 1", won)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAnswered || got.Answer == nil {
		t.Fatalf("run = %q with answer %+v, want it answered", got.Status, got.Answer)
	}
	if got.Answer.From != sandbox.StatusAwaiting {
		t.Errorf("the record took the run out of %q, want %q", got.Answer.From, sandbox.StatusAwaiting)
	}
	// AND A LATER ONE FINDS IT ANSWERED, whoever asks.
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("late", "late"), sandbox.Fence{}); err != nil || ok {
		t.Errorf("a reply after the answer = %v, %v, want refused", ok, err)
	}
	// AN ANSWERED QUESTION IS NOT WAITING, so no listing offers it a reply.
	if _, found, _ := findAwaiting(ctx, s, "swe", answerOnTheDM); found {
		t.Error("a question that has its answer was matched to another reply")
	}
}

// AN ANSWER IS RECORDED WITH THE ROUTE IT CAME BY, on either route, and one that
// names none is refused: the route is what tells the resumed turn who answered,
// what the answer's record says, and what a copy of it becomes when it is let
// go of — an ordinary message, or an answer by turn with no ordinary form.
func testAnAnswerIsRecordedWithItsRoute(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	routeless := answerOf("r1", "use main")
	routeless.Via = ""
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, routeless, sandbox.Fence{}); err == nil || ok {
		t.Fatalf("an answer naming no route = %v, %v, want refused", ok, err)
	}
	byTurn := answerOf("r1", "use main")
	byTurn.Via = types.AnswerViaOperator
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, byTurn, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("an answer by turn = %v, %v", ok, err)
	}
	got := mustGet(t, s, "t1").Answer
	if got == nil || got.Via != types.AnswerViaOperator || !bytes.Equal(got.Sealed, sealOf("use main")) {
		t.Fatalf("recorded %+v, want the answer by turn with its seal, kept as it was handed", got)
	}
}

// AN ANSWER IS RECORDED SEALED, and one that carries no seal is refused: its
// words live on the row only inside the seal ([sandbox.RecordedAnswer.Sealed]),
// so a record without one would be an answer no reader could resume with.
func testAnAnswerIsRecordedOnlySealed(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	unsealed := answerOf("r1", "use main")
	unsealed.Sealed = nil
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, unsealed, sandbox.Fence{}); err == nil || ok {
		t.Fatalf("an answer carrying no seal = %v, %v, want refused", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Answer != nil || got.Status != sandbox.StatusAwaiting {
		t.Fatalf("after a refused record the run is %q with answer %+v, want it still waiting "+
			"with none", got.Status, got.Answer)
	}
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("the sealed answer = %v, %v, want it recorded", ok, err)
	}
}

// A RECORD NAMES ITS QUESTION: the launch that asked, while it waits. One for
// a job that replaced the asker, or for a run that is not waiting, is refused.
func testAnAnswerIsRecordedOnlyOnTheQuestionThatAsked(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || ok {
		t.Fatalf("an answer to a RUNNING run = %v, %v, want refused", ok, err)
	}
	park(t, s, "t1")
	if _, ok, err := s.RecordAnswer(ctx, "t1", "another-launch", answerOf("r1", "use main"), sandbox.Fence{}); err != nil || ok {
		t.Fatalf("an answer naming another launch = %v, %v, want refused", ok, err)
	}
	if _, ok, err := s.RecordAnswer(ctx, "gone", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || ok {
		t.Fatalf("an answer to a missing run = %v, %v, want false and no error", ok, err)
	}
	if _, _, err := s.RecordAnswer(ctx, "t1", launch, sandbox.RecordedAnswer{Sealed: sealOf("x")}, sandbox.Fence{}); err == nil {
		t.Fatal("an answer naming no delivery was recorded")
	}
	// A RESEED IS STILL WAITING: the box was reaped, the question was not.
	if err := s.SetStatus(ctx, "t1", sandbox.StatusReseed, sandbox.Fence{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if got, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok ||
		got.Answer.From != sandbox.StatusReseed {
		t.Fatalf("an answer to a reseeded run = %+v, %v, %v, want it recorded from reseed",
			got.Answer, ok, err)
	}
}

// THE RESUME CLAIMS THE RECORD, not the question: an answered run is claimed
// out of answered, and a claim that is handed back puts it there again — the
// answer still on it, owed the same resume.
func testAnAnswerIsResumedFromItsRecord(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	// A CLAIM OUT OF THE OPEN QUESTION finds it answered, and takes nothing.
	if _, ok, err := s.ClaimForResume(ctx, "t1", sandbox.Tail{Launch: launch, From: sandbox.Awaiting}, sandbox.Fence{}); err != nil || ok {
		t.Fatalf("a claim out of the open question took one that already has its answer: %v, %v", ok, err)
	}
	claimed, ok, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{})
	if err != nil || !ok || claimed.ClaimedFrom != sandbox.StatusAnswered {
		t.Fatalf("claim = %v from %q, %v, want it claimed from answered", ok, claimed.ClaimedFrom, err)
	}
	if released, err := s.ReleaseClaim(ctx, "t1", sandbox.Release{
		Launch: launch, To: sandbox.StatusAnswered,
	}); err != nil || !released {
		t.Fatalf("ReleaseClaim = %v, %v", released, err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAnswered || got.Answer == nil || !bytes.Equal(got.Answer.Sealed, sealOf("use main")) {
		t.Fatalf("run = %q with answer %+v, want the answer still owed", got.Status, got.Answer)
	}
}

// A DECLINED ANSWER REOPENS THE QUESTION, and the reply it was is declined for
// good — with its copy — so it can never be recorded against it again.
func testADeclinedAnswerReopensTheQuestionAndIsNeverRecordedAgain(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	// Only the answer it holds is let go of.
	if ok, err := decline(t, s, launch, "other"); err != nil || ok {
		t.Fatalf("declining an answer the run does not hold = %v, %v, want refused", ok, err)
	}
	if got := mustGet(t, s, "t1"); len(got.HandBack) != 0 {
		t.Fatalf("a refused decline still recorded copies as owed: %+v", got.HandBack)
	}
	if ok, err := decline(t, s, launch, "r1"); err != nil || !ok {
		t.Fatalf("DeclineAnswer = %v, %v", ok, err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAwaiting || got.Answer != nil ||
		!slices.Equal(got.DeclinedAnswers, []string{"r1", "r1-copy"}) {
		t.Fatalf("run = %q answer %+v declined %v, want the question open again with r1 "+
			"and its copy declined", got.Status, got.Answer, got.DeclinedAnswers)
	}
	for _, id := range []string{"r1", "r1-copy"} {
		if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf(id, "use main"), sandbox.Fence{}); err != nil || ok {
			t.Errorf("a declined reply %s was recorded again: %v, %v", id, ok, err)
		}
	}
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r2", "use dev"), sandbox.Fence{}); err != nil || !ok {
		t.Errorf("a new reply to the reopened question = %v, %v, want it recorded", ok, err)
	}
	published(t, s, "r1-copy")
	if ok, err := decline(t, s, launch, "r2"); err != nil || !ok {
		t.Fatalf("DeclineAnswer: %v, %v", ok, err)
	}
	// BOUNDED, newest kept.
	for i := range 20 {
		id := fmt.Sprintf("r%d", i+3)
		if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf(id, "x"), sandbox.Fence{}); err != nil || !ok {
			t.Fatalf("RecordAnswer %s = %v, %v", id, ok, err)
		}
		published(t, s, fmt.Sprintf("r%d-copy", i+2))
		if ok, err := decline(t, s, launch, id); err != nil || !ok {
			t.Fatalf("DeclineAnswer %s = %v, %v", id, ok, err)
		}
	}
	got = mustGet(t, s, "t1")
	if len(got.DeclinedAnswers) != 16 || got.DeclinedAnswers[15] != "r22-copy" {
		t.Errorf("declined = %v, want the newest 16", got.DeclinedAnswers)
	}

	// AND THE NEWEST DECLINE WHOLE, however many deliveries its answer was:
	// a copy of it whose id fell off would answer the question that let it
	// go, and circle the run it already failed to reach.
	published(t, s, "r22-copy")
	var batch []string
	var copies []sandbox.HandedBack
	for i := range 12 {
		id := fmt.Sprintf("b%d", i)
		batch = append(batch, id)
		copies = append(copies, copyOf(id))
	}
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, sandbox.RecordedAnswer{
		Sealed: sealOf("a long batch"), Via: types.AnswerViaChat, EventIDs: batch, RecordedAt: base}, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer of a batch = %v, %v", ok, err)
	}
	if _, ok, err := s.DeclineAnswer(ctx, "t1", launch, batch, copies, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("DeclineAnswer of a batch = %v, %v", ok, err)
	}
	got = mustGet(t, s, "t1")
	for _, copied := range copies {
		if !slices.Contains(got.DeclinedAnswers, copied.ID) || !slices.Contains(got.DeclinedAnswers, copied.Original) {
			t.Fatalf("declined = %v: a decline of %d deliveries lost %s or its copy",
				got.DeclinedAnswers, len(batch), copied.Original)
		}
	}
}

// published clears one copy a decline owed t1, the way a publisher does once
// the copy is out.
func published(t *testing.T, s sandbox.PendingStore, id string) {
	t.Helper()
	if ok, err := s.ClearHandBack(t.Context(), "t1", []string{id}); err != nil || !ok {
		t.Fatalf("ClearHandBack(%s) = %v, %v", id, ok, err)
	}
}

// WHAT A ROW OWES THE SEAT IS BOUNDED BY ONE DECLINE, and outlives nothing it
// has not handed back. A second decline, and an ending's let-go, are refused
// while earlier copies are owed — the answer stays recorded and owed, and nothing
// grows — and a run that owes copies is not deleted until they are cleared,
// because nothing reads a deleted row again.
func testWhatARowOwesTheSeatIsBoundedAndOutlivesNothing(t *testing.T, s sandbox.PendingStore) {
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

	// A SECOND DECLINE WAITS for the first one's copies.
	if ok, err := decline(t, s, launch, "r2"); !errors.Is(err, sandbox.ErrHandBackOwed) || ok {
		t.Fatalf("a decline over owed copies = %v, %v, want refused with ErrHandBackOwed", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAnswered || got.Answer == nil ||
		!slices.Equal(got.Answer.EventIDs, []string{"r2"}) || len(got.HandBack) != 1 {
		t.Fatalf("run = %q answer %+v hand-back %+v, want r2 still recorded and only r1's copy "+
			"owed", got.Status, got.Answer, got.HandBack)
	}

	// AN ENDING OUTSIDE ITS LICENSE is refused as one, not as a debt.
	if _, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{License: everyJob(sandbox.Fence{}, nil)}); err != nil || ok {
		t.Fatalf("an unlicensed ending = %v, %v, want a plain refusal", ok, err)
	}
	decided := decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active))
	ending := decided.Ending.ID

	// NOR IS THE RUN DELETED while it owes them, and it says what it owes.
	owing, ended, err := s.Finish(ctx, "t1", ending)
	if !errors.Is(err, sandbox.ErrHandBackOwed) || ended ||
		len(owing.HandBack) != 1 || owing.HandBack[0].ID != "r1-copy" {
		t.Fatalf("Finish over owed copies = %v, %+v, %v, want refused with the copies it owes",
			ended, owing.HandBack, err)
	}
	// NOR LET GO OF ANOTHER until they are out.
	if _, ok, err := s.OweHandBack(ctx, "t1", letGoOf(ending, "r2")); !errors.Is(err, sandbox.ErrHandBackOwed) || ok {
		t.Fatalf("a let-go over owed copies = %v, %v, want refused with ErrHandBackOwed", ok, err)
	}
	if _, found, err := s.Get(ctx, "t1"); err != nil || !found {
		t.Fatalf("the run owing its seat a reply was deleted: %v, %v", found, err)
	}

	// ONCE HANDED BACK, the reply r2 is still owed, and then let go of.
	published(t, s, "r1-copy")
	if _, ended, err := s.Finish(ctx, "t1", ending); !errors.Is(err, sandbox.ErrAnswerOwed) || ended {
		t.Fatalf("Finish over the reply the run still holds = %v, %v, want refused with ErrAnswerOwed", ended, err)
	}
	if _, ok, err := s.OweHandBack(ctx, "t1", letGoOf(ending, "r2")); err != nil || !ok {
		t.Fatalf("the let-go of r2 once r1's copy was out = %v, %v", ok, err)
	}
	published(t, s, "r2-copy")
	if _, ended, err := s.Finish(ctx, "t1", ending); err != nil || !ended {
		t.Fatalf("Finish once nothing is owed = %v, %v", ended, err)
	}
}
