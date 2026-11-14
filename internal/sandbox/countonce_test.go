package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

// recountOnLanding is a run store that, once a claim's release or a park's
// question has landed on it, has the seat recounted before the write's answer
// is returned: a recount whose listing was read after the write landed, and
// written before the node took the write's answer.
type recountOnLanding struct {
	PendingStore
	recount func(ctx context.Context)
}

func (s *recountOnLanding) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	released, err := s.PendingStore.ReleaseClaim(ctx, turnID, release)
	if err == nil && released {
		s.recount(ctx)
	}
	return released, err
}

func (s *recountOnLanding) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	err := s.PendingStore.MarkAwaiting(ctx, turnID, q)
	if err == nil {
		s.recount(ctx)
	}
	return err
}

// seatCounts is what the rig's coordinator counts of the seat's runs.
func seatCounts(rig *coordRig) seatRuns {
	rig.coordinator.mu.Lock()
	defer rig.coordinator.mu.Unlock()
	return rig.coordinator.runs["swe"]
}

// A WRITE THAT MOVES A RUN OFF ITS CLAIM MOVES ITS SEAT'S COUNT ONCE. A claim
// handed back, or parked on its job's question, is counted as the claim until
// the write's answer is taken, and that answer moves the count where the write
// put the run: a release the store did not answer keeps the claim, and a park
// frees the seat only with its question on the row. A recount whose listing was
// read after the write landed, and written before its answer was taken, counted
// the run where the write had put it, and the answer's move then counted it
// there again. An answer's release, whose claim goes back to the answer and
// holds nothing, took the seat's holding count down twice, freeing a seat whose
// other run still worked; a park counted its one question twice and its claim's
// hold off twice, the seat offering its mail to an answer match beside a job
// it no longer counted. Here a second run of the seat works throughout, and the
// recount lands between each write and its answer.
//
// Mutation: have a recount count a run whose release is out, or whose park is
// being written, where the store's row says it is, or leave the park unrecorded
// while it is written; each counts the run twice.
func TestAWriteThatMovesARunOffItsClaimMovesItsSeatsCountOnce(t *testing.T) {
	t.Parallel()

	t.Run("an answer's release", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		// THE SEAT'S OTHER RUN, working throughout: in the store, for the
		// recount to find.
		rig.launch("t2")
		rig.coordinator.pending = &recountOnLanding{PendingStore: rig.coordinator.pending,
			recount: func(ctx context.Context) { rig.coordinator.syncSeat(ctx, "swe") }}
		rig.resumer.failWith(errors.New("the model provider did not answer"))
		asked := rig.get("t1").AskedAt
		reply := chatReply(answerOnTheDM, "use main", replyAt("use main", asked.Add(time.Minute)))
		if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil ||
			d != AnswerConsumed {
			t.Fatalf("the reply = %q, %v; want it recorded as the answer", d, err)
		}
		if got := rig.get("t1"); got.Status != StatusAnswered {
			t.Fatalf("the premise: the answer's failed resume left the run %s; want its claim given back "+
				"to the answer", got.Status)
		}
		if got := seatCounts(rig); got.holding != 1 || got.awaiting != 0 {
			t.Fatalf("the seat counts %d holding and %d awaiting; want the run that works counted, and the "+
				"answered run in neither set", got.holding, got.awaiting)
		}
		if held, _ := rig.coordinator.SeatRuns("swe"); !held {
			t.Fatal("the seat reads free beside the run that works")
		}
	})

	t.Run("a park", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.launch("t2")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.coordinator.countRun("swe", StatusRunning)
		rig.coordinator.pending = &recountOnLanding{PendingStore: rig.coordinator.pending,
			recount: func(ctx context.Context) { rig.coordinator.syncSeat(ctx, "swe") }}
		rig.ask("t1", Ask{Question: "which branch?", To: AskToRequester, Decision: choosing("which branch?"),
			Branch: "wip/t1"})
		rig.runner.Finish(Result{})
		if err := collectAgain(t, rig); err != nil {
			t.Fatalf("the collection that parks the run: %v", err)
		}
		if got := rig.get("t1"); got.Status != StatusAwaiting {
			t.Fatalf("the premise: the collection left the run %s; want it parked on its question", got.Status)
		}
		if got := seatCounts(rig); got.holding != 1 || got.awaiting != 1 {
			t.Fatalf("the seat counts %d holding and %d awaiting; want the run that works holding it, and the "+
				"one question awaiting", got.holding, got.awaiting)
		}
	})
}

// recountOnAnswerWrite is a run store that, once one of a recorded answer's
// writes has landed on it, has the seat recounted before the write's answer is
// returned, as [recountOnLanding] does for a release and a park: the answer's
// record, the claim that resumes it, or a decline that lets it go, as which
// names.
type recountOnAnswerWrite struct {
	PendingStore
	which   answerWrite
	recount func(ctx context.Context)
}

// answerWrite names one of the writes a recorded answer moves its run's count
// by.
type answerWrite string

const (
	answerRecorded answerWrite = "record"
	answerClaimed  answerWrite = "claim"
	answerDeclined answerWrite = "decline"
)

func (s *recountOnAnswerWrite) RecordAnswer(ctx context.Context, turnID, launch string, answer RecordedAnswer,
	fence Fence,
) (PendingRun, bool, error) {
	run, won, err := s.PendingStore.RecordAnswer(ctx, turnID, launch, answer, fence)
	if err == nil && won && s.which == answerRecorded {
		s.recount(ctx)
	}
	return run, won, err
}

func (s *recountOnAnswerWrite) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	run, won, err := s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
	if err == nil && won && run.ClaimedFrom == StatusAnswered && s.which == answerClaimed {
		s.recount(ctx)
	}
	return run, won, err
}

func (s *recountOnAnswerWrite) DeclineAnswer(ctx context.Context, turnID, launch string, answer []string,
	handBack []HandedBack, fence Fence,
) (PendingRun, bool, error) {
	run, let, err := s.PendingStore.DeclineAnswer(ctx, turnID, launch, answer, handBack, fence)
	if err == nil && let && s.which == answerDeclined {
		s.recount(ctx)
	}
	return run, let, err
}

// recountsOn has the rig's seat recounted once which has landed, before the
// coordinator takes its answer.
func recountsOn(rig *coordRig, which answerWrite) {
	rig.coordinator.pending = &recountOnAnswerWrite{PendingStore: rig.coordinator.pending, which: which,
		recount: func(ctx context.Context) { rig.coordinator.syncSeat(ctx, "swe") }}
}

// parkElsewhere parks turnID on a question of its own, asked in a conversation
// that [answerOnTheDM] does not reply in.
func parkElsewhere(t *testing.T, rig *coordRig, turnID string) {
	t.Helper()
	rig.launchWith(turnID, func(run *PendingRun) {
		run.ConversationKey, run.PartitionKey = "chat:D3", "chat:D3:root-1"
	})
	rig.ask(turnID, whichBranch())
	rig.runner.Finish(Result{})
	payload, ev := rig.completion(turnID)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted %s: %v", turnID, err)
	}
	if got := rig.get(turnID); got.Status != StatusAwaiting {
		t.Fatalf("status of %s = %q, want it parked on its question", turnID, got.Status)
	}
}

// AN ANSWER'S WRITE MOVES ITS SEAT'S COUNT ONCE, as a release and a park do
// ([TestAWriteThatMovesARunOffItsClaimMovesItsSeatsCountOnce]). The writes a
// person's answer moves its run by are the record that takes the run out of the
// question set, the claim that resumes it, and a decline that puts it back on
// its question. Each moves its seat's count where the write put the run as its
// answer is taken, and a recount whose listing was read after the write landed,
// and written before that, counts the run where it was. Moved after the write
// and outside the lock, the move counted the run where the write put it a
// second time: the record took a seat holding a second question out of the
// question set altogether, so that question's answer was run as an unrelated
// turn ([seatRuns.awaiting]: erring low is the harmful way); the claim counted
// its run holding twice; and each decline counted the question it put the run
// back on twice.
//
// Mutation: drop any one write's record before it is sent, or have a recount
// count a run with a write out where the store's row says it is; each counts a
// run twice.
func TestAnAnswersWriteMovesItsSeatsCountOnce(t *testing.T) {
	t.Parallel()

	t.Run("the record of an answer to one of two questions", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		parkElsewhere(t, rig, "t3")
		rig.coordinator.syncSeat(t.Context(), "swe")
		if got := seatCounts(rig); got.holding != 0 || got.awaiting != 2 {
			t.Fatalf("the premise: the seat counts %d holding and %d awaiting; want t1's question and "+
				"t3's awaiting", got.holding, got.awaiting)
		}
		recountsOn(rig, answerRecorded)
		var during seatRuns
		var awaits bool
		rig.resumer.during = func(context.Context, PendingRun) {
			during = seatCounts(rig)
			_, awaits = rig.coordinator.SeatRuns("swe")
		}
		asked := rig.get("t1").AskedAt
		reply := chatReply(answerOnTheDM, "use main", replyAt("use main", asked.Add(time.Minute)))
		if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil ||
			d != AnswerConsumed {
			t.Fatalf("the reply = %q, %v; want it recorded as t1's answer", d, err)
		}
		if calls := rig.resumer.calls(); len(calls) != 1 || calls[0].Run.TurnID != "t1" {
			t.Fatalf("the premise: the answer resumed %d turns; want t1 once", len(calls))
		}
		if during.holding != 1 || during.awaiting != 1 || !awaits {
			t.Fatalf("while t1's answer ran the seat counted %d holding and %d awaiting (a question open: "+
				"%v); want t1's claim holding and t3's question still open, which t3's answer is "+
				"offered to", during.holding, during.awaiting, awaits)
		}
	})

	t.Run("the claim of an answer", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		rig.launch("t2")
		rig.coordinator.syncSeat(t.Context(), "swe")
		if got := seatCounts(rig); got.holding != 1 || got.awaiting != 1 {
			t.Fatalf("the premise: the seat counts %d holding and %d awaiting; want t2 holding and t1 "+
				"awaiting", got.holding, got.awaiting)
		}
		recountsOn(rig, answerClaimed)
		var during seatRuns
		rig.resumer.during = func(context.Context, PendingRun) { during = seatCounts(rig) }
		asked := rig.get("t1").AskedAt
		reply := chatReply(answerOnTheDM, "use main", replyAt("use main", asked.Add(time.Minute)))
		if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil ||
			d != AnswerConsumed {
			t.Fatalf("the reply = %q, %v; want it recorded as t1's answer", d, err)
		}
		if len(rig.resumer.calls()) != 1 {
			t.Fatalf("the premise: the answer resumed %d times; want once", len(rig.resumer.calls()))
		}
		if during.holding != 2 || during.awaiting != 0 {
			t.Fatalf("while t1's answer ran the seat counted %d holding and %d awaiting; want 2 holding "+
				"(t1's claim and t2) and none awaiting", during.holding, during.awaiting)
		}
	})

	t.Run("the decline of an answer that does not open", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		run := rig.get("t1")
		words, answer := chatReply(answerOnTheDM, "use main",
			replyAt("use main", run.AskedAt.Add(time.Minute))).answerOf(rig.now)
		// SEALED FOR ANOTHER RUN, so it does not open as t1's answer.
		other := run
		other.TurnID = "t2"
		if _, won, err := rig.pending.RecordAnswer(t.Context(), "t1", run.LaunchID,
			sealedOnto(t, rigAnswers(), other, words, answer), rigLease); err != nil || !won {
			t.Fatalf("RecordAnswer = %v, %v", won, err)
		}
		rig.coordinator.syncSeat(t.Context(), "swe")
		if got := seatCounts(rig); got.holding != 0 || got.awaiting != 0 {
			t.Fatalf("the premise: the seat counts %d holding and %d awaiting; want t1's answered run in "+
				"neither set", got.holding, got.awaiting)
		}
		recountsOn(rig, answerDeclined)
		rig.coordinator.queueOwed(t.Context(), rig.get("t1"))
		rig.fireRetries()
		if got := rig.get("t1"); got.Status != StatusAwaiting || len(rig.resumer.calls()) != 0 {
			t.Fatalf("the premise: the answer left t1 %s after %d resumes; want it let go of unresumed",
				got.Status, len(rig.resumer.calls()))
		}
		if got := seatCounts(rig); got.holding != 0 || got.awaiting != 1 {
			t.Fatalf("the seat counts %d holding and %d awaiting; want t1's one question awaiting",
				got.holding, got.awaiting)
		}
	})

	t.Run("the decline of an answer whose attempts are spent", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		owedAnAnswerItCannotResume(t, rig)
		recountsOn(rig, answerDeclined)
		rig.fireRetries() // the last attempt fails, and the answer is let go of
		if got := rig.get("t1"); got.Status != StatusAwaiting {
			t.Fatalf("the premise: the spent answer left t1 %s; want it let go of", got.Status)
		}
		if got := seatCounts(rig); got.holding != 0 || got.awaiting != 1 {
			t.Fatalf("the seat counts %d holding and %d awaiting; want t1's one question awaiting",
				got.holding, got.awaiting)
		}
	})
}

// parkAndRecordOut has t1's park and the record of an answer to its question
// both land on the row, each still out on this node, recorded in the order an
// answer by turn to a question whose park has not heard back yet makes them:
// the park's (the claim to the question), then the record's (the question to
// the answer).
func parkAndRecordOut(t *testing.T, rig *coordRig) (park, record *countWrite) {
	t.Helper()
	parkOnAQuestion(t, rig)
	run := rig.get("t1")
	words, answer := chatReply(answerOnTheDM, "use main",
		replyAt("use main", run.AskedAt.Add(time.Minute))).answerOf(rig.now)
	if _, won, err := rig.pending.RecordAnswer(t.Context(), "t1", run.LaunchID,
		sealedOnto(t, rigAnswers(), run, words, answer), rigLease); err != nil || !won {
		t.Fatalf("RecordAnswer = %v, %v", won, err)
	}
	return rig.coordinator.writingCount(run, StatusResumed, StatusAwaiting),
		rig.coordinator.writingCount(run, StatusAwaiting, StatusAnswered)
}

// answerLanded takes the answer to w, a write that landed, as its caller does:
// under the lock, with what it returns run once the lock is let go.
func answerLanded(rig *coordRig, w *countWrite) {
	rig.coordinator.mu.Lock()
	moved := rig.coordinator.wroteCountLocked(w, true)
	rig.coordinator.mu.Unlock()
	moved()
}

// wantSeatCounts fails t unless the rig's seat counts holding and awaiting,
// saying when and what was wanted.
func wantSeatCounts(t *testing.T, rig *coordRig, when, want string, holding, awaiting int) {
	t.Helper()
	if got := seatCounts(rig); got.holding != holding || got.awaiting != awaiting {
		t.Fatalf("%s the seat counts %d holding and %d awaiting; want %s", when, got.holding,
			got.awaiting, want)
	}
}

// TWO WRITES OUT ON ONE RUN MOVE ITS SEAT'S COUNT IN THE ORDER THEY WERE
// RECORDED, whatever order their answers come back in (countwrite.go). Here a
// park's question has landed, and an answer by turn has been recorded to it,
// before the park took its answer, and the record's answer is taken first. Its
// move waits for the park's, a recount meanwhile counts the run as the claim it
// was before either write, and the park's answer then makes both moves: out of
// the claim, through the question, to the answer, which is in neither set. Made
// at once, the record's move took the run out of a question set it was not yet
// counted in, and the park's then counted the answered run as a question.
//
// Mutation: make each write's move as its own answer is taken, whatever was
// recorded before it.
func TestTwoWritesOutOnOneRunMoveItsSeatsCountInTheOrderTheyWereRecorded(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	// BOTH WRITES HAVE LANDED: the question is on the row, and its answer.
	park, record := parkAndRecordOut(t, rig)

	rig.coordinator.syncSeat(t.Context(), "swe")
	wantSeatCounts(t, rig, "recounted with both writes out,", "the run as the claim it was before either", 1, 0)
	answerLanded(rig, record)
	wantSeatCounts(t, rig, "with the record's answer taken first,",
		"the run still the claim, its move waiting for the park's", 1, 0)
	rig.coordinator.syncSeat(t.Context(), "swe")
	wantSeatCounts(t, rig, "recounted with the park still out,", "the run still the claim", 1, 0)
	answerLanded(rig, park)
	wantSeatCounts(t, rig, "with both answers taken,", "the answered run in neither set", 0, 0)
	rig.coordinator.syncSeat(t.Context(), "swe")
	wantSeatCounts(t, rig, "recounted once both are answered,", "the answered run in neither set", 0, 0)
}

// A RECOUNT COUNTS A RUN WITH A WRITE STILL OUT WHERE THE MOVES ALREADY MADE
// LEAVE IT (countwrite.go), a move made by an earlier write's answer included.
// Here a park's question and an answer by turn recorded to it have both landed,
// and their answers come back in the order the writes were made: the park's
// first, which moves the run off the claim and onto its question, while the
// record's is still out. A recount then counts the run on its question, and
// the record's answer moves it from there to the answer, which is in neither
// set. Counted where the run stood before the park's answer, the recount put
// the run back on the claim, and the record's move then took the run out of a
// question set it was no longer counted in: the seat held for a run that holds
// nothing, its mail waiting behind it until the next recount.
//
// Mutation: leave a write's move out of what a recount counts its run as while
// a later write on it is still out.
func TestARecountCountsARunWithAWriteOutWhereTheMovesAlreadyMadeLeaveIt(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	park, record := parkAndRecordOut(t, rig)

	rig.coordinator.syncSeat(t.Context(), "swe")
	wantSeatCounts(t, rig, "recounted with both writes out,", "the run as the claim it was before either", 1, 0)
	answerLanded(rig, park)
	wantSeatCounts(t, rig, "with the park's answer taken first,", "the run on its question", 0, 1)
	rig.coordinator.syncSeat(t.Context(), "swe")
	wantSeatCounts(t, rig, "recounted with the record still out,",
		"the run on its question, where the park's move left it", 0, 1)
	answerLanded(rig, record)
	wantSeatCounts(t, rig, "with both answers taken,", "the answered run in neither set", 0, 0)
	rig.coordinator.syncSeat(t.Context(), "swe")
	wantSeatCounts(t, rig, "recounted once both are answered,", "the answered run in neither set", 0, 0)
}
