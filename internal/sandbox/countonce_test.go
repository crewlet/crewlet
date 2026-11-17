package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
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

// countRun counts a run into the set status names on handle's seat, as the
// write that put it there would have: for a case whose rows the rig writes
// straight onto the store, which no write of the coordinator's counts.
func (c *Coordinator) countRun(handle, status string) {
	holding, awaiting := setOf(status)
	c.mu.Lock()
	was, runs := c.adjustLocked(handle, holding, awaiting)
	c.mu.Unlock()
	c.seatFreed(handle, was, runs)
	c.reconcileHold(c.life, handle)
}

// seatCounts is what the rig's coordinator counts of the seat's runs.
func seatCounts(rig *coordRig) seatRuns {
	rig.coordinator.mu.Lock()
	defer rig.coordinator.mu.Unlock()
	return rig.coordinator.runs["swe"]
}

// A WRITE THAT MOVES A RUN OFF ITS CLAIM MOVES ITS SEAT'S COUNT ONCE: a claim
// handed back, or parked on its job's question, is counted as the claim until
// the write's answer is taken, which moves the count where the write put the
// run. Here a second run of the seat works throughout, and a recount lands
// between each write and its answer; counted twice, an answer's release frees a
// seat whose other run works, and a park counts its question twice. See the
// package doc, "Every write that moves a run between its seat's sets moves the
// count once".
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
// ([TestAWriteThatMovesARunOffItsClaimMovesItsSeatsCountOnce]): the record that
// takes the run out of the question set, the claim that resumes it, and a
// decline that puts it back on its question. Counted twice, the record takes a
// seat holding a second question out of the question set, so that answer runs
// as an unrelated turn ([seatRuns.awaiting]: erring low is the harmful way).
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
// LEAVE IT (countwrite.go), an earlier write's move included. A park's question
// and an answer by turn recorded to it both land, their answers in order: the
// park's moves the run onto its question while the record's is out, so a
// recount counts it there and the record moves it to the answer. Counted where
// it stood before the park's answer, the seat is held for a run holding
// nothing.
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

// recountOnDelete is a run store that, once an ending's delete has removed a
// run's record, has the seat recounted before the delete's answer is returned:
// a recount whose listing was read after the delete landed, and written before
// the node took the delete's answer.
type recountOnDelete struct {
	PendingStore
	recount func(ctx context.Context)
}

func (s *recountOnDelete) Finish(ctx context.Context, turnID, ending string) (PendingRun, bool, error) {
	settled, deleted, err := s.PendingStore.Finish(ctx, turnID, ending)
	if err == nil && deleted {
		s.recount(ctx)
	}
	return settled, deleted, err
}

// deleteRefused is a run store that refuses every ending's delete: the record
// stays, and the ending is kept for the node's retry.
type deleteRefused struct{ PendingStore }

func (deleteRefused) Finish(context.Context, string, string) (PendingRun, bool, error) {
	return PendingRun{}, false, errRefusedCall
}

// claimedBeside launches t2 and claims it for its resume, as a completion's
// collection does: a run that holds the seat throughout and that none of
// [countedEndings] ends, a person's stop included, which leaves a claimed run
// to its turn.
func claimedBeside(t *testing.T, rig *coordRig) {
	t.Helper()
	run := rig.launch("t2")
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t2", CompletionTail(run.LaunchID),
		rigLease); err != nil || !won {
		t.Fatalf("ClaimForResume t2 = %v, %v", won, err)
	}
}

// countedEnding is one of the endings whose caller counts its run out of the
// seat's sets: what puts t1 where that ending finds it, and the ending itself.
type countedEnding struct {
	name    string
	arrange func(t *testing.T, rig *coordRig)
	end     func(t *testing.T, rig *coordRig)
}

// countedEndings are the three: a run settled as lost, here a launch that never
// suspended ([Coordinator.FailRun]); a person's stop of a launch whose job
// never started, finished at once ([Coordinator.StopSeat]); and a launch
// abandoned once its row was written, here by its turn's fence
// ([Coordinator.abandon]), whose launch counts t1 in itself.
var countedEndings = []countedEnding{
	{
		name:    "a settle of a launch that never suspended",
		arrange: func(t *testing.T, rig *coordRig) { rig.launching("t1") },
		end: func(t *testing.T, rig *coordRig) {
			if err := rig.coordinator.FailRun(t.Context(), "t1", types.ExecutorFailureSuspensionUnrecorded,
				"the turn could not write its parked state onto the run"); err != nil {
				t.Fatalf("FailRun: %v", err)
			}
		},
	},
	{
		name: "a stop of a launch whose job never started",
		arrange: func(t *testing.T, rig *coordRig) {
			if _, err := rig.pending.BeginLaunch(t.Context(), PendingRun{
				TurnID: "t1", AgentHandle: "swe", AgentID: sweID, Role: "SWE", CreatedAt: rig.now,
			}, rigLease); err != nil {
				t.Fatalf("BeginLaunch: %v", err)
			}
		},
		end: func(t *testing.T, rig *coordRig) {
			if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
				t.Fatalf("StopSeat: %v", err)
			}
		},
	},
	{
		name:    "a launch abandoned once its row was written",
		arrange: func(*testing.T, *coordRig) {},
		end: func(t *testing.T, rig *coordRig) {
			req := launchReq("t1")
			refused := errors.New("the turn was stopped as its run opened")
			req.Fence = func() error { return refused }
			if _, err := rig.coordinator.Launch(t.Context(), rig.manager, req); !errors.Is(err, refused) {
				t.Fatalf("Launch = %v, want the turn's fence refusing it", err)
			}
		},
	},
}

// AN ENDING'S DELETE MOVES ITS SEAT'S COUNT ONCE (countwrite.go): recorded
// before it is sent, the run counted where it was by a recount meanwhile,
// whether or not its listing has the row, and taken out by the delete's answer.
// The seat's other run is a claim that holds it throughout. Each ending is made
// twice, with no recount beside it and with one between its delete and the
// delete's answer.
//
// Mutation: count an ending's run out as it ends, after its delete, or have a
// recount count only the runs its listing has, and the seat reads free beside
// its claim; or leave the ending's run counted, and the seat stays held for a
// run that is gone.
func TestAnEndingsDeleteMovesItsSeatsCountOnce(t *testing.T) {
	t.Parallel()
	for _, e := range countedEndings {
		for _, between := range []bool{false, true} {
			name := e.name + ", nothing recounting beside it"
			if between {
				name = e.name + ", a recount between its delete and the answer"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				rig := newCoordRig(t)
				e.arrange(t, rig)
				claimedBeside(t, rig)
				rig.coordinator.syncSeat(t.Context(), "swe")
				if between {
					rig.coordinator.pending = &recountOnDelete{PendingStore: rig.coordinator.pending,
						recount: func(ctx context.Context) { rig.coordinator.syncSeat(ctx, "swe") }}
				}
				e.end(t, rig)
				if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
					t.Fatalf("the premise: t1's record found %v (%v); want the ending to have deleted it",
						found, err)
				}
				wantSeatCounts(t, rig, "once t1's ending deleted it,", "t2's claim holding the seat", 1, 0)
				if held, _ := rig.coordinator.SeatRuns("swe"); !held {
					t.Fatal("the seat reads free beside t2's claim")
				}
			})
		}
	}
}

// AN ENDING SOMEBODY ELSE FINISHED FIRST LEAVES ITS RUN COUNTED BY NOBODY
// (countwrite.go). Each of the three endings finds its run's ending decided and
// its record deleted by another party a moment before its own decision, so it
// has no delete of its own to move the run out of the seat's sets: the seat is
// recounted from the store, which no longer has the row. A settle and an
// abandon used to count the run out whatever became of their ending, and a
// stop counted nothing for a run that moved on; left to a delete that never
// came, the run stayed counted, and the seat was held for a run that was over.
//
// Mutation: drop the recount where the ending is not this node's, in any of
// the three.
func TestAnEndingSomebodyElseFinishedFirstLeavesItsRunCountedByNobody(t *testing.T) {
	t.Parallel()
	for _, e := range countedEndings {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			e.arrange(t, rig)
			claimedBeside(t, rig)
			rig.coordinator.syncSeat(t.Context(), "swe")
			rig.coordinator.pending = endedFirst{rig.coordinator.pending}
			e.end(t, rig)
			if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
				t.Fatalf("the premise: t1's record found %v (%v); want another party to have ended it",
					found, err)
			}
			wantSeatCounts(t, rig, "once another party had ended t1,", "t2's claim alone holding the seat", 1, 0)
		})
	}
}

// AN ENDING WHOSE DELETE DID NOT LAND MOVES NO COUNT (countwrite.go): its run
// is still on the store, where a recount reads it, and stays counted where it
// is until a delete lands; the ending, kept, holds the seat's mail behind it
// besides. Counted out as the ending was decided, the seat read a run still on
// its row as gone, and a recount then counted it back: the count followed
// whichever came last.
//
// Mutation: count an ending's run out whether or not its delete landed.
func TestAnEndingWhoseDeleteDidNotLandMovesNoCount(t *testing.T) {
	t.Parallel()
	for _, e := range countedEndings {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			e.arrange(t, rig)
			claimedBeside(t, rig)
			rig.coordinator.syncSeat(t.Context(), "swe")
			rig.coordinator.pending = deleteRefused{rig.coordinator.pending}
			e.end(t, rig)
			if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || !found {
				t.Fatalf("the premise: t1's record found %v (%v); want it kept, its delete refused", found, err)
			}
			wantSeatCounts(t, rig, "with t1's delete refused,", "t1, still on its row, and t2's claim holding",
				2, 0)
			rig.coordinator.syncSeat(t.Context(), "swe")
			wantSeatCounts(t, rig, "recounted,", "t1, still on its row, and t2's claim holding", 2, 0)
		})
	}
}

// recountOnOpen is a run store that, once a launch has opened its run's row,
// has the seat recounted before the open's answer is returned: a recount whose
// listing was read after the row was written, and written before the launch
// took the write's answer.
type recountOnOpen struct {
	PendingStore
	recount func(ctx context.Context)
}

func (s *recountOnOpen) BeginLaunch(ctx context.Context, run PendingRun, fence Fence) (PendingRun, error) {
	opened, err := s.PendingStore.BeginLaunch(ctx, run, fence)
	if err == nil {
		s.recount(ctx)
	}
	return opened, err
}

// besides names the two ways a case makes a write that moves a run's count:
// with nothing recounting the seat beside it, its own move the only one, and
// with a recount between the write's landing and its answer.
var besides = []struct {
	name    string
	between bool
}{
	{"nothing recounting beside it", false},
	{"a recount between the write and its answer", true},
}

// A LAUNCH MOVES ITS SEAT'S COUNT ONCE (countwrite.go): the write that opens a
// run's row counts it holding as its answer is taken, recorded before it is
// sent, so a recount between counts the run where it was: no row for a launch,
// and the claim for a relaunch its resumed turn makes, which holds already.
// Counted twice, the seat stays held until a recount after the run settled.
//
// Mutation: record the open after it lands, or record it against another run,
// or record a relaunch as opening a row nothing counted; the run is counted
// twice.
func TestALaunchMovesItsSeatsCountOnce(t *testing.T) {
	t.Parallel()
	for _, b := range besides {
		t.Run("a launch, "+b.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			claimedBeside(t, rig)
			rig.coordinator.syncSeat(t.Context(), "swe")
			if b.between {
				rig.coordinator.pending = &recountOnOpen{PendingStore: rig.coordinator.pending,
					recount: func(ctx context.Context) { rig.coordinator.syncSeat(ctx, "swe") }}
			}
			if _, err := rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t1")); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			wantSeatCounts(t, rig, "once t1 launched,", "t1's job and t2's claim holding the seat", 2, 0)
		})
		t.Run("a relaunch in its resumed turn, "+b.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			rig.launch("t1")
			rig.coordinator.syncSeat(t.Context(), "swe")
			if b.between {
				rig.coordinator.pending = &recountOnOpen{PendingStore: rig.coordinator.pending,
					recount: func(ctx context.Context) { rig.coordinator.syncSeat(ctx, "swe") }}
			}
			rig.runner.Finish(Result{Success: true})
			var during seatRuns
			rig.resumer.during = func(ctx context.Context, run PendingRun) {
				if _, err := rig.coordinator.Launch(ctx, rig.manager, launchReq(run.TurnID)); err != nil {
					t.Errorf("relaunch: %v", err)
				}
				during = seatCounts(rig)
			}
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
			if during.holding != 1 || during.awaiting != 0 {
				t.Fatalf("once the resumed turn relaunched its job the seat counted %d holding and %d "+
					"awaiting; want the one run, its claim relaunched, holding it once", during.holding,
					during.awaiting)
			}
		})
	}
}

// recountOnFence is a run store that, once a recovery's fence has landed on a
// run, has the seat recounted before the fence's answer is returned: a recount
// from the seat's control topic, which is attached before the recovery runs.
type recountOnFence struct {
	PendingStore
	recount func(ctx context.Context)
}

func (s *recountOnFence) ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error) {
	won, err := s.PendingStore.ClaimOwnership(ctx, turnID, owner, epoch)
	if err == nil && won {
		s.recount(ctx)
	}
	return won, err
}

// A RECOVERY COUNTS EACH RUN IT TAKES OVER ONCE (countwrite.go), where the
// recovery leaves it: a running job holding, a question awaiting, a reaped
// launching tail nowhere. The control topic is attached before the recovery
// runs, so a recount can land before it or between a fence and a count; the
// recovery starts the counts afresh as it records every takeover.
//
// Mutation: count a run after its fence without recording it first, or leave
// the counts a recount made before the recovery standing; the seat counts a run
// twice, or a reaped tail.
func TestARecoveryCountsEachRunItTakesOverOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		before func(t *testing.T, rig *coordRig)
	}{
		{"nothing recounting beside it", func(*testing.T, *coordRig) {}},
		{"a recount between each fence and its count", func(_ *testing.T, rig *coordRig) {
			rig.coordinator.pending = &recountOnFence{PendingStore: rig.coordinator.pending,
				recount: func(ctx context.Context) { rig.coordinator.syncSeat(ctx, "swe") }}
		}},
		{"a recount before it, off the seat's control topic", func(t *testing.T, rig *coordRig) {
			if err := rig.coordinator.OnStarted(t.Context(), types.ExecutorLaunched{
				AgentHandle: "swe", TurnID: "t1",
			}); err != nil {
				t.Fatalf("OnStarted: %v", err)
			}
			if got := seatCounts(rig); got.holding != 2 || got.awaiting != 1 {
				t.Fatalf("the premise: the recount counted %d holding and %d awaiting; want t1, the tail "+
					"and t3's question", got.holding, got.awaiting)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			rig.launch("t1")
			rig.launching("t2")
			rig.launch("t3")
			rig.park("t3")
			tc.before(t, rig)
			if err := rig.recoverSeat(t.Context(), "node-b:1", 2); err != nil {
				t.Fatalf("RecoverSeat: %v", err)
			}
			if _, found, err := rig.pending.Get(t.Context(), "t2"); err != nil || found {
				t.Fatalf("the premise: the tail found %v (%v); want it reaped", found, err)
			}
			wantSeatCounts(t, rig, "once the recovery took the seat over,",
				"t1's job holding it and t3's question awaiting", 1, 1)
			rig.coordinator.syncSeat(t.Context(), "swe")
			wantSeatCounts(t, rig, "recounted,", "t1's job holding it and t3's question awaiting", 1, 1)
		})
	}
}

// listedBeforeRecovery is a run store that runs meanwhile once, after it has
// listed a seat's runs and before it returns that listing: a recount whose
// listing was read before what meanwhile does, and whose count is written
// after it.
type listedBeforeRecovery struct {
	PendingStore
	mu        sync.Mutex
	meanwhile func(ctx context.Context)
}

func (s *listedBeforeRecovery) ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error) {
	runs, err := s.PendingStore.ListActiveForSeat(ctx, handle)
	s.mu.Lock()
	meanwhile := s.meanwhile
	s.meanwhile = nil
	s.mu.Unlock()
	if meanwhile != nil {
		meanwhile(ctx)
	}
	return runs, err
}

// A RECOUNT WHOSE LISTING WAS READ BEFORE A RECOVERY READS AGAIN
// (countwrite.go). The seat's control topic is attached before its recovery
// runs, so a recount off it can list the seat's runs before the recovery and
// write its count after. The recovery starts the seat's counts afresh as it
// records its takeovers, which moves the seat for such a recount, and the
// recount reads again. Here the seat's last holder ends its one run between
// the recount's listing and the recovery's, so the recovery takes nothing over
// and moves no count of its own: the fresh start is the only move. Written
// over it instead, the recount's listing counted the ended run, and the seat
// was held for a run that was gone until a later recount.
//
// Mutation: start the seat's counts afresh without moving the seat.
func TestARecountListedBeforeARecoveryReadsAgain(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	store := &listedBeforeRecovery{PendingStore: rig.coordinator.pending}
	store.meanwhile = func(ctx context.Context) {
		// THE SEAT'S LAST HOLDER ENDS ITS RUN, and this node takes the seat.
		decided, ok, err := rig.pending.DecideEnding(ctx, "t1", Decision{
			License: License{WhileIn: Active, Launch: EveryLaunch},
			Reason:  types.ExecutorFailureSeatRemoved, Detail: "the seat left the company",
		})
		if err != nil || !ok {
			t.Errorf("the premise: t1's ending by the seat's last holder = %v, %v", ok, err)
			return
		}
		if _, deleted, err := rig.pending.Finish(ctx, "t1", decided.Ending.ID); err != nil || !deleted {
			t.Errorf("the premise: t1's delete by the seat's last holder = %v, %v", deleted, err)
			return
		}
		if err := rig.recoverSeat(ctx, "node-b:1", 2); err != nil {
			t.Errorf("RecoverSeat: %v", err)
		}
	}
	rig.coordinator.pending = store
	if err := rig.coordinator.OnStarted(t.Context(), types.ExecutorLaunched{
		AgentHandle: "swe", TurnID: "t1",
	}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	wantSeatCounts(t, rig, "once the recount listed before the recovery was written,", "nothing, t1 being gone",
		0, 0)
}

// parkLandsUnanswered is a run store whose record of a park's question lands
// and is then reported unanswered, and which leaves a claim's release
// unanswered, while it is silent: a park's write that landed with its answer
// lost, and the release it hands the claim back with owed (R2-1).
type parkLandsUnanswered struct {
	PendingStore
	mu     sync.Mutex
	silent bool
}

func (s *parkLandsUnanswered) quiet() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.silent
}

// answer has the store answer every call again.
func (s *parkLandsUnanswered) answer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent = false
}

func (s *parkLandsUnanswered) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	if err := s.PendingStore.MarkAwaiting(ctx, turnID, q); err != nil {
		return err
	}
	if s.quiet() {
		return errStoreUnavailable("write the sandbox run", turnID)
	}
	return nil
}

func (s *parkLandsUnanswered) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	if s.quiet() {
		return false, errStoreUnavailable("write the sandbox run", turnID)
	}
	return s.PendingStore.ReleaseClaim(ctx, turnID, release)
}

// A RECORD MADE WHILE A RELEASE IS OWED MOVES ITS RUN AFTER THE RELEASE
// (countwrite.go). A park's question lands with its answer lost, the park hands
// its claim back, and the store leaves that release unanswered, so it is owed.
// A reply's record of the question then lands; recorded behind the release in
// the run's entry, its move waits for the release's answer. Read apart, the
// seat's other question reads as closed.
//
// Mutation: keep a claim's release apart from its run's entry, or move each
// write by the sets it was recorded from and to rather than to where it put the
// run; t3's question reads as closed.
func TestARecordMadeWhileAReleaseIsOwedMovesItsRunAfterTheRelease(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkElsewhere(t, rig, "t3")
	rig.launch("t1")
	rig.coordinator.syncSeat(t.Context(), "swe")
	store := &parkLandsUnanswered{PendingStore: rig.coordinator.pending, silent: true}
	rig.coordinator.pending = store
	rig.ask("t1", whichBranch())
	rig.runner.Finish(Result{})
	payload, ev := rig.completion("t1")
	_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
	parked := rig.get("t1")
	if parked.Status != StatusAwaiting || !rig.coordinator.releasing("t1", parked.LaunchID) {
		t.Fatalf("the premise: t1 is %s, its release out %v; want its question on the row and the claim's "+
			"release owed", parked.Status, rig.coordinator.releasing("t1", parked.LaunchID))
	}
	wantSeatCounts(t, rig, "with the park's release owed,", "t1 as the claim it still is and t3's question", 1, 1)

	reply := chatReply(answerOnTheDM, "use main", replyAt("use main", parked.AskedAt.Add(time.Minute)))
	_, _ = rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
	if got := rig.get("t1"); got.Status != StatusAnswered {
		t.Fatalf("the premise: the reply left t1 %s; want it recorded as its answer", got.Status)
	}
	wantSeatCounts(t, rig, "with the reply recorded while the release is owed,",
		"t1 still the claim and t3's question still open", 1, 1)
	if _, awaits := rig.coordinator.SeatRuns("swe"); !awaits {
		t.Fatal("t3's question reads as closed: its answer would run as an unrelated turn")
	}

	store.answer()
	rig.fireRetries()
	wantSeatCounts(t, rig, "once the release, made again, found t1 moved on to its answer,",
		"t3's question alone, t1's answer resumed and its run over", 0, 1)
}

// stepOnLogLine is a log destination that runs step once, on the goroutine
// that logs it, when the first line carrying the message msg is written: a
// case's way to act between two steps of the coordinator's that log between
// them and call out to nothing a fake could stand in. The process's handler
// holds its lock while it writes, so step logs nothing at the level the case
// installs, and waits on nothing that would.
type stepOnLogLine struct {
	msg  []byte
	once sync.Once
	step func()
}

func (w *stepOnLogLine) Write(p []byte) (int, error) {
	if bytes.Contains(p, w.msg) {
		w.once.Do(w.step)
	}
	return len(p), nil
}

// onLogLine has step run once, where the coordinator writes its first line
// with the message msg ([stepOnLogLine]), with the process's logging at level
// until the case ends. A case that calls it is not parallel: the logger is the
// process's.
func onLogLine(t *testing.T, level slog.Level, msg string, step func()) {
	t.Helper()
	logging.Configure(level, logging.FormatText, &stepOnLogLine{msg: []byte(" msg=" + msg + " "), step: step})
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
}

// A PARK'S HAND-BACK IS RECORDED IN THE STEP THAT TAKES THE PARK'S ANSWER
// (countwrite.go), where the park's write did not land, so no moment falls with
// no write out on the run, in which a recount would count the question off its
// row while the release counts the claim. The recount is made where the park
// logs that its write did not land, after the one step.
//
// Mutation: take the park's answer and record its hand-back in two steps of the
// lock; the recount between them counts t1's question.
//
// Not parallel: it hooks the process's logger ([onLogLine]).
func TestAParksHandBackIsRecordedInTheStepThatTakesItsAnswer(t *testing.T) {
	rig := newCoordRig(t)
	parkElsewhere(t, rig, "t3")
	rig.launch("t2")
	rig.launch("t1")
	rig.coordinator.syncSeat(t.Context(), "swe")
	wantSeatCounts(t, rig, "before t1 parks,", "t1 and t2 working and t3's question", 2, 1)
	store := &parkLandsUnanswered{PendingStore: rig.coordinator.pending, silent: true}
	rig.coordinator.pending = store
	recounts := 0
	onLogLine(t, slog.LevelError, "sandbox_park_write_failed", func() {
		recounts++
		rig.coordinator.syncSeat(t.Context(), "swe")
	})
	rig.ask("t1", whichBranch())
	rig.runner.Finish(Result{})
	payload, ev := rig.completion("t1")
	_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
	parked := rig.get("t1")
	if parked.Status != StatusAwaiting || !rig.coordinator.releasing("t1", parked.LaunchID) || recounts != 1 {
		t.Fatalf("the premise: t1 is %s, its release out %v, recounts %d; want its question on the row, the "+
			"claim's release owed, and one recount where the park's write failed", parked.Status,
			rig.coordinator.releasing("t1", parked.LaunchID), recounts)
	}
	wantSeatCounts(t, rig, "with the park's release owed and the seat recounted as the park handed back,",
		"t1 as the claim it still is, t2 working and t3's question", 2, 1)

	store.answer()
	rig.fireRetries()
	wantSeatCounts(t, rig, "once the release, made again, found t1 moved on to its question,",
		"t2 working, and t1's and t3's questions", 1, 2)
}

// answeredBehindPark is a run store whose record of a park's question lands
// and is then reported unanswered, as [parkLandsUnanswered]'s is, once record
// has answered the question on the run while the park's write was still out:
// the record of an answer by turn to a question the park has written and not
// yet heard back on, its own answer taken first. Every claim's release it is
// sent has the seat recounted before it goes on, as a recount made while the
// release was out.
type answeredBehindPark struct {
	*parkLandsUnanswered
	record  func(ctx context.Context)
	recount func(ctx context.Context)
}

func (s *answeredBehindPark) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	if err := s.PendingStore.MarkAwaiting(ctx, turnID, q); err != nil {
		return err
	}
	s.record(ctx)
	return errStoreUnavailable("write the sandbox run", turnID)
}

func (s *answeredBehindPark) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	s.recount(ctx)
	return s.parkLandsUnanswered.ReleaseClaim(ctx, turnID, release)
}

// A PARK'S HAND-BACK MOVES AFTER THE WRITES RECORDED BEHIND THE PARK
// (countwrite.go). An answer by turn is recorded to a park's question while the
// park's write is out, its answer taken first; the park's lost answer then lets
// the record's move go, to the answer, and its release, recorded behind them in
// the same step, finds the run moved on. Recorded later, in an entry of its
// own, the release counts the answered run as the claim.
//
// Mutation: record the park's hand-back after taking the park's answer; the
// recount made as the release goes out counts t1 holding.
func TestAParksHandBackMovesAfterTheWritesRecordedBehindThePark(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkElsewhere(t, rig, "t3")
	rig.launch("t2")
	rig.launch("t1")
	rig.coordinator.syncSeat(t.Context(), "swe")
	wantSeatCounts(t, rig, "before t1 parks,", "t1 and t2 working and t3's question", 2, 1)
	var during *seatRuns
	store := &answeredBehindPark{
		parkLandsUnanswered: &parkLandsUnanswered{PendingStore: rig.coordinator.pending, silent: true},
		record: func(ctx context.Context) {
			run := rig.getIn(ctx, "t1")
			words, answer := chatReply(answerOnTheDM, "use main",
				replyAt("use main", run.AskedAt.Add(time.Minute))).answerOf(rig.now)
			if _, won, err := rig.coordinator.record(ctx, run, words, answer); err != nil || !won {
				t.Errorf("record = %v, %v; want t1's answer recorded while its park is out", won, err)
			}
		},
		recount: func(ctx context.Context) {
			if during != nil {
				return
			}
			rig.coordinator.syncSeat(ctx, "swe")
			counted := seatCounts(rig)
			during = &counted
		},
	}
	rig.coordinator.pending = store
	rig.ask("t1", whichBranch())
	rig.runner.Finish(Result{})
	payload, ev := rig.completion("t1")
	_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
	answered := rig.get("t1")
	if answered.Status != StatusAnswered || !rig.coordinator.releasing("t1", answered.LaunchID) || during == nil {
		t.Fatalf("the premise: t1 is %s, its release out %v, the seat recounted as it went out %v; want its "+
			"answer on the row and the claim's release owed", answered.Status,
			rig.coordinator.releasing("t1", answered.LaunchID), during != nil)
	}
	if during.holding != 1 || during.awaiting != 1 {
		t.Fatalf("recounted as the park's release went out, the seat counts %d holding and %d awaiting; want "+
			"t2 working and t3's question, t1's answer in neither set", during.holding, during.awaiting)
	}
	wantSeatCounts(t, rig, "with the park's release owed,", "t2 working and t3's question", 1, 1)

	store.answer()
	rig.fireRetries()
	wantSeatCounts(t, rig, "once the release, made again, found t1 moved on to its answer,",
		"t2 working and t3's question, t1's answer resumed and its run over", 1, 1)
}

// A RELEASE A LATER JOB'S TAKES THE PLACE OF LEAVES NOTHING OUT ON ITS RUN
// (claimrelease.go). A resumed turn relaunches and fails, its release owed
// unanswered; the relaunched job's claim wins and its resume fails too, so the
// later release takes the earlier one's place, which is answered as a write
// that did not land. Left out, the seat counts the run holding after it is
// over; answered as landed, the seat reads free beside the relaunched job.
//
// Mutation: put a release in the place of one still out without answering the
// earlier one's write, or answer it as one that landed.
func TestAReleaseALaterJobsTakesThePlaceOfLeavesNothingOutOnItsRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// arrange puts t1 where the claim the case gives back is taken from,
		// and claim takes it, its resume relaunching the job and failing.
		arrange func(t *testing.T, rig *coordRig)
		claim   func(t *testing.T, rig *coordRig)
	}{
		{name: "a completion's claim",
			arrange: func(_ *testing.T, rig *coordRig) {
				rig.launch("t1")
				rig.runner.Finish(Result{Success: true})
			},
			claim: func(t *testing.T, rig *coordRig) {
				payload, ev := rig.completion("t1")
				_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
			}},
		{name: "an answer's claim",
			arrange: func(t *testing.T, rig *coordRig) { parkOnAQuestion(t, rig) },
			claim: func(t *testing.T, rig *coordRig) {
				reply := chatReply(answerOnTheDM, "use main",
					replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute)))
				_, _ = rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			tc.arrange(t, rig)
			rig.coordinator.syncSeat(t.Context(), "swe")
			first := rig.get("t1")
			store := &unansweredRelease{silentStore: &silentStore{PendingStore: rig.coordinator.pending}}
			rig.coordinator.pending = store
			store.silence("ReleaseClaim")
			rig.resumer.err, rig.resumer.failsInTurn = errors.New("the model provider did not answer"), true
			relaunched := false
			rig.resumer.during = func(ctx context.Context, run PendingRun) {
				if relaunched {
					return
				}
				relaunched = true
				if _, err := rig.coordinator.Launch(ctx, rig.manager, launchReq(run.TurnID)); err != nil {
					t.Errorf("relaunch: %v", err)
				}
				rig.suspendIn(ctx, run.TurnID)
			}
			tc.claim(t, rig)
			second := rig.get("t1")
			if second.LaunchID == first.LaunchID || second.Status != StatusRunning ||
				!rig.coordinator.releasing("t1", first.LaunchID) {
				t.Fatalf("the premise: t1 is %s on job %s, the first job's release out %v; want the relaunched "+
					"job running and the first job's release owed", second.Status, second.LaunchID,
					rig.coordinator.releasing("t1", first.LaunchID))
			}
			wantSeatCounts(t, rig, "with the first job's release owed,", "the relaunched job holding the seat",
				1, 0)

			store.answer()
			payload, ev := rig.completion("t1")
			_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
			if got := rig.get("t1"); got.LaunchID != second.LaunchID || got.Status != StatusRunning ||
				rig.coordinator.releasing("t1", second.LaunchID) {
				t.Fatalf("the premise: t1 is %s on job %s; want the relaunched job's claim given back to "+
					"running", got.Status, got.LaunchID)
			}
			rig.coordinator.mu.Lock()
			out := rig.coordinator.writing["t1"]
			rig.coordinator.mu.Unlock()
			if out != nil {
				t.Errorf("%d writes are left out on t1 once both releases are answered, holding every later "+
					"move on it", len(out.out))
			}
			wantSeatCounts(t, rig, "with the relaunched job's claim given back,",
				"the relaunched job holding the seat", 1, 0)

			rig.fireRetries()
			rig.resumer.failWith(nil)
			payload, ev = rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
			if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
				t.Fatalf("the premise: t1 found %v (%v); want its run over", found, err)
			}
			wantSeatCounts(t, rig, "once t1's run is over,", "nothing", 0, 0)
		})
	}
}

// A RECOVERY'S TAKEOVER HAS THE LAST WORD ON ITS RUN (countwrite.go). A write
// of this node's earlier holding is still out when this node takes the seat
// back; the recovery reaps the run and counts it in no set, and the earlier
// write then lands. Each landed write puts the run where its status says, in
// recorded order, so the takeover, answered as landed, leaves the run in none;
// answered as not landed, the seat counts a question for a run that is gone.
//
// Mutation: answer a takeover that counts its run in no set as a write that did
// not land.
func TestARecoverysTakeoverHasTheLastWordOnItsRun(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launching("t1")
	rig.coordinator.ReleaseSeat("swe")
	stale := rig.coordinator.writingCount(rig.get("t1"), StatusResumed, StatusAwaiting)
	if err := rig.recoverSeat(t.Context(), "node-b:1", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("the premise: t1 found %v (%v); want the recovery to have reaped it", found, err)
	}
	answerLanded(rig, stale)
	wantSeatCounts(t, rig, "once the earlier write's answer was taken,", "nothing, t1 being gone", 0, 0)
}

// A WRITE THAT LANDS PUTS ITS RUN WHERE ITS OWN STATUS SAYS (countwrite.go),
// whatever the write before it on the run did. Here a park's question and the
// record of an answer to it have both landed, the record's answer is taken
// first and waits for the park's, and the park's answer is then lost, though
// it landed: the park moves nothing, and the record moves the run from the
// claim it is still counted as to its answer, which is in neither set. Moved
// by the sets it was recorded from and to, the record took the run out of a
// question set the park never counted it into, and the seat's other question
// read as closed, its answer then run as an unrelated turn.
//
// Mutation: move each landed write by the sets it was recorded from and to.
func TestAWriteThatLandsPutsItsRunWhereItsOwnStatusSays(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	park, record := parkAndRecordOut(t, rig)
	parkElsewhere(t, rig, "t3")
	rig.coordinator.syncSeat(t.Context(), "swe")
	wantSeatCounts(t, rig, "recounted with both of t1's writes out,", "t1 as the claim and t3's question", 1, 1)
	answerLanded(rig, record)
	rig.coordinator.mu.Lock()
	moved := rig.coordinator.wroteCountLocked(park, false)
	rig.coordinator.mu.Unlock()
	moved()
	wantSeatCounts(t, rig, "with the park's answer lost and the record's taken,",
		"t1's answer in neither set and t3's question still open", 0, 1)
}

// seatMovesOnLanding is a run store that, once a park's question or an
// answer's record has landed on it, has the seat let go of and taken back by
// this node, at a newer epoch, before the write's answer is returned: a seat
// that moved while the write was out, and came back.
type seatMovesOnLanding struct {
	PendingStore
	moved func(ctx context.Context)
}

func (s *seatMovesOnLanding) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	err := s.PendingStore.MarkAwaiting(ctx, turnID, q)
	if err == nil {
		s.moved(ctx)
	}
	return err
}

func (s *seatMovesOnLanding) RecordAnswer(ctx context.Context, turnID, launch string, answer RecordedAnswer,
	fence Fence,
) (PendingRun, bool, error) {
	run, won, err := s.PendingStore.RecordAnswer(ctx, turnID, launch, answer, fence)
	if err == nil && won {
		s.moved(ctx)
	}
	return run, won, err
}

// A MOVE FOR A SEAT THIS NODE NO LONGER HOLDS MOVES NOTHING (countwrite.go). A
// write out on a seat this node lets go of is answered after the seat's counts
// have gone ([Coordinator.ReleaseSeat]), and a recovery taking the seat back
// counts its run where it finds it. Moved all the same, the run is counted
// twice; kept out, the recovery's count waits behind it.
//
// Mutation: keep a seat's writes out when the seat is let go of; the run is
// counted where the write was recorded from while its answer is out, and twice
// once it comes.
func TestAMoveForASeatThisNodeNoLongerHoldsMovesNothing(t *testing.T) {
	t.Parallel()
	takeBack := func(t *testing.T, rig *coordRig, during *seatRuns) func(context.Context) {
		return func(ctx context.Context) {
			rig.coordinator.ReleaseSeat("swe")
			if err := rig.recoverSeat(ctx, "node-a:2", 2); err != nil {
				t.Errorf("RecoverSeat: %v", err)
			}
			*during = seatCounts(rig)
		}
	}

	t.Run("a park's answer", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.coordinator.syncSeat(t.Context(), "swe")
		rig.ask("t1", whichBranch())
		rig.runner.Finish(Result{})
		var during seatRuns
		rig.coordinator.pending = &seatMovesOnLanding{PendingStore: rig.coordinator.pending,
			moved: takeBack(t, rig, &during)}
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("OnCompleted: %v", err)
		}
		if during.holding != 0 || during.awaiting != 1 {
			t.Fatalf("once this node took the seat back the seat counted %d holding and %d awaiting; want "+
				"t1's question awaiting, where the recovery found it", during.holding, during.awaiting)
		}
		wantSeatCounts(t, rig, "once the park's answer was taken,", "t1's question awaiting once", 0, 1)
	})

	t.Run("an answer's record", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		parkElsewhere(t, rig, "t3")
		rig.coordinator.syncSeat(t.Context(), "swe")
		run := rig.get("t1")
		words, answer := chatReply(answerOnTheDM, "use main",
			replyAt("use main", run.AskedAt.Add(time.Minute))).answerOf(rig.now)
		var during seatRuns
		rig.coordinator.pending = &seatMovesOnLanding{PendingStore: rig.coordinator.pending,
			moved: takeBack(t, rig, &during)}
		if _, won, err := rig.coordinator.record(t.Context(), run, words, answer); err != nil || !won {
			t.Fatalf("record = %v, %v; want t1's answer recorded", won, err)
		}
		if during.holding != 0 || during.awaiting != 1 {
			t.Fatalf("once this node took the seat back the seat counted %d holding and %d awaiting; want "+
				"t3's question awaiting and t1's answer in neither set", during.holding, during.awaiting)
		}
		wantSeatCounts(t, rig, "once the record's answer was taken,", "t3's question still awaiting", 0, 1)
	})
}

// panicsOn is a run store whose call named method panics, once, before it
// reaches the store: a store call that panics, which the queue's handler
// recovers and the process outlives.
type panicsOn struct {
	PendingStore
	method string

	mu       sync.Mutex
	panicked bool
}

// panics reports whether method is the call to panic, and marks it panicked.
func (s *panicsOn) panics(method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.panicked || s.method != method {
		return false
	}
	s.panicked = true
	return true
}

// raised reports whether the store's call panicked.
func (s *panicsOn) raised() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.panicked
}

func (s *panicsOn) BeginLaunch(ctx context.Context, run PendingRun, fence Fence) (PendingRun, error) {
	if s.panics("BeginLaunch") {
		panic("the store's BeginLaunch panicked")
	}
	return s.PendingStore.BeginLaunch(ctx, run, fence)
}

func (s *panicsOn) ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error) {
	if s.panics("ClaimOwnership") {
		panic("the store's ClaimOwnership panicked")
	}
	return s.PendingStore.ClaimOwnership(ctx, turnID, owner, epoch)
}

func (s *panicsOn) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	if s.panics("MarkAwaiting") {
		panic("the store's MarkAwaiting panicked")
	}
	return s.PendingStore.MarkAwaiting(ctx, turnID, q)
}

func (s *panicsOn) RecordAnswer(ctx context.Context, turnID, launch string, answer RecordedAnswer, fence Fence,
) (PendingRun, bool, error) {
	if s.panics("RecordAnswer") {
		panic("the store's RecordAnswer panicked")
	}
	return s.PendingStore.RecordAnswer(ctx, turnID, launch, answer, fence)
}

func (s *panicsOn) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	if s.panics("ClaimForResume") {
		panic("the store's ClaimForResume panicked")
	}
	return s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
}

func (s *panicsOn) DeclineAnswer(ctx context.Context, turnID, launch string, answer []string,
	handBack []HandedBack, fence Fence,
) (PendingRun, bool, error) {
	if s.panics("DeclineAnswer") {
		panic("the store's DeclineAnswer panicked")
	}
	return s.PendingStore.DeclineAnswer(ctx, turnID, launch, answer, handBack, fence)
}

func (s *panicsOn) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	if s.panics("ReleaseClaim") {
		panic("the store's ReleaseClaim panicked")
	}
	return s.PendingStore.ReleaseClaim(ctx, turnID, release)
}

func (s *panicsOn) Finish(ctx context.Context, turnID, ending string) (PendingRun, bool, error) {
	if s.panics("Finish") {
		panic("the store's Finish panicked")
	}
	return s.PendingStore.Finish(ctx, turnID, ending)
}

// recovered runs f, recovering a panic out of it as the queue's handler does.
func recovered(f func()) {
	defer func() { _ = recover() }()
	f()
}

// A WRITE WHOSE STORE CALL PANICS LEAVES NOTHING OUT ON ITS RUN
// (countwrite.go): answered on every way out as one that did not land, and so
// are a claim's release record, the count of claims on their way, a claim's
// count as this node's live attempt, and the recovery gate a pass closes
// (recoverygate.go). The queue's handler recovers a panic and the process lives
// on, so anything left up blocks its run or seat until a restart.
//
// Mutation: drop any one write's settle on its way out, the release's, the
// claims' count's, the live attempt's or the recovery gate's; its case leaves
// it out.
func TestAWriteWhoseStoreCallPanicsLeavesNothingOutOnItsRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method string
		// arrange puts t1 where the write finds it; write makes it, under
		// the panic's recovery.
		arrange func(t *testing.T, rig *coordRig)
		write   func(t *testing.T, rig *coordRig)
	}{
		{name: "a launch's open", method: "BeginLaunch",
			arrange: func(*testing.T, *coordRig) {},
			write: func(t *testing.T, rig *coordRig) {
				_, _ = rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t1"))
			}},
		{name: "a recovery's takeover", method: "ClaimOwnership",
			arrange: func(_ *testing.T, rig *coordRig) { rig.launch("t1") },
			write: func(t *testing.T, rig *coordRig) {
				_ = rig.recoverSeat(t.Context(), "node-b:1", 2)
			}},
		{name: "a completion's claim", method: "ClaimForResume",
			arrange: func(_ *testing.T, rig *coordRig) {
				rig.launch("t1")
				rig.runner.Finish(Result{Success: true})
			},
			write: func(t *testing.T, rig *coordRig) {
				payload, ev := rig.completion("t1")
				_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
			}},
		{name: "a park's question", method: "MarkAwaiting",
			arrange: func(_ *testing.T, rig *coordRig) {
				rig.launch("t1")
				rig.ask("t1", whichBranch())
				rig.runner.Finish(Result{})
			},
			write: func(t *testing.T, rig *coordRig) {
				payload, ev := rig.completion("t1")
				_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
			}},
		{name: "a claim's release", method: "ReleaseClaim",
			arrange: func(_ *testing.T, rig *coordRig) {
				rig.launch("t1")
				rig.runner.Finish(Result{Success: true})
				rig.resumer.failWith(errors.New("the model provider did not answer"))
			},
			write: func(t *testing.T, rig *coordRig) {
				payload, ev := rig.completion("t1")
				_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
			}},
		{name: "an answer's record", method: "RecordAnswer",
			arrange: func(t *testing.T, rig *coordRig) { parkOnAQuestion(t, rig) },
			write: func(t *testing.T, rig *coordRig) {
				run := rig.get("t1")
				words, answer := chatReply(answerOnTheDM, "use main",
					replyAt("use main", run.AskedAt.Add(time.Minute))).answerOf(rig.now)
				_, _, _ = rig.coordinator.record(t.Context(), run, words, answer)
			}},
		{name: "the claim that resumes an answer", method: "ClaimForResume",
			arrange: func(t *testing.T, rig *coordRig) { parkOnAQuestion(t, rig) },
			write: func(t *testing.T, rig *coordRig) {
				reply := chatReply(answerOnTheDM, "use main",
					replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute)))
				_, _ = rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
			}},
		{name: "an unopened answer's decline", method: "DeclineAnswer",
			arrange: func(t *testing.T, rig *coordRig) {
				parkOnAQuestion(t, rig)
				run := rig.get("t1")
				words, answer := chatReply(answerOnTheDM, "use main",
					replyAt("use main", run.AskedAt.Add(time.Minute))).answerOf(rig.now)
				// SEALED FOR ANOTHER RUN, so it does not open as t1's answer
				// and is declined.
				other := run
				other.TurnID = "t2"
				if _, won, err := rig.pending.RecordAnswer(t.Context(), "t1", run.LaunchID,
					sealedOnto(t, rigAnswers(), other, words, answer), rigLease); err != nil || !won {
					t.Fatalf("RecordAnswer = %v, %v", won, err)
				}
				rig.coordinator.queueOwed(t.Context(), rig.get("t1"))
			},
			write: func(*testing.T, *coordRig) {}},
		{name: "a spent answer's decline", method: "DeclineAnswer",
			arrange: func(t *testing.T, rig *coordRig) { owedAnAnswerItCannotResume(t, rig) },
			write:   func(*testing.T, *coordRig) {}},
		{name: "an ending's delete", method: "Finish",
			arrange: func(_ *testing.T, rig *coordRig) { rig.launching("t1") },
			write: func(t *testing.T, rig *coordRig) {
				_ = rig.coordinator.FailRun(t.Context(), "t1", types.ExecutorFailureSuspensionUnrecorded,
					"the turn could not write its parked state onto the run")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			tc.arrange(t, rig)
			store := &panicsOn{PendingStore: rig.coordinator.pending, method: tc.method}
			rig.coordinator.pending = store
			recovered(func() {
				tc.write(t, rig)
				rig.fireRetries()
			})
			if !store.raised() {
				t.Fatalf("the premise: the store's %s never panicked", tc.method)
			}
			rig.coordinator.mu.Lock()
			out, release, claiming := rig.coordinator.writing["t1"], rig.coordinator.releases["t1"],
				rig.coordinator.claiming["t1"]
			live, gate := rig.coordinator.live["t1"], rig.coordinator.recovering["swe"]
			rig.coordinator.mu.Unlock()
			if out != nil {
				t.Errorf("a write is left out on t1 (%d of them), holding every later move on it", len(out.out))
			}
			if release != nil {
				t.Error("t1's claim's release is left recorded, refusing every claim of its job")
			}
			if claiming != 0 {
				t.Errorf("%d claims of t1's job are left counted as on their way, holding its release back",
					claiming)
			}
			if live != nil {
				t.Errorf("%d claims of t1 are left counted as this node's live attempt, keeping every later "+
					"recovery pass of its seat off it", live.n)
			}
			if gate != nil {
				t.Error("the seat's recovery gate is left closed, refusing every later claim of its runs")
			}
		})
	}
}

// A WRITE LEFT OUT BY A PANIC HOLDS NO LATER MOVE ON ITS RUN: the record of an
// answer whose store call panicked is answered as one that did not land, so the
// record that follows moves the run out of the question set. Left out, the
// panicked record held the second record's move behind it, and the seat read a
// question open that had its answer.
//
// Mutation: drop the record's settle on its way out.
func TestARecordAfterOneThatPanickedMovesItsRun(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.coordinator.syncSeat(t.Context(), "swe")
	store := &panicsOn{PendingStore: rig.coordinator.pending, method: "RecordAnswer"}
	rig.coordinator.pending = store
	run := rig.get("t1")
	words, answer := chatReply(answerOnTheDM, "use main",
		replyAt("use main", run.AskedAt.Add(time.Minute))).answerOf(rig.now)
	recovered(func() { _, _, _ = rig.coordinator.record(t.Context(), run, words, answer) })
	if !store.raised() {
		t.Fatal("the premise: the record's store call never panicked")
	}
	if _, won, err := rig.coordinator.record(t.Context(), run, words, answer); err != nil || !won {
		t.Fatalf("the record made again = %v, %v; want it recorded", won, err)
	}
	wantSeatCounts(t, rig, "once the answer was recorded,", "t1's answer in neither set", 0, 0)
}
