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
