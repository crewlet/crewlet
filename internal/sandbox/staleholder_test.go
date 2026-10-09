package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A HOLDER THAT LOST THE SEAT ACTS ON NOTHING IT DOES NOT STILL HOLD.
//
// Every case below has two holders of one seat over one store: the old one,
// slow or stalled, still acting on what it read before its lease lapsed, and
// the seat's next holder, which fenced the run to its own newer lease. What the
// old holder does after that must land nowhere — not on the box the next holder
// is about to resume into, not on the answer it is driving.

// readThenRecover hands the caller the claim it reads, and then — before the
// caller can act on what it read — lets the seat's next holder take the seat:
// the read a settle makes to learn which box to reclaim, overtaken by a
// successor that fences the row and revives the claim.
type readThenRecover struct {
	PendingStore
	once    sync.Once
	recover func()
}

func (s *readThenRecover) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	run, found, err := s.PendingStore.Get(ctx, turnID)
	if err == nil && found && run.Status == StatusResumed {
		s.once.Do(s.recover)
	}
	return run, found, err
}

// A SLOW HOLDER'S SETTLE NEVER KILLS THE BOX ITS SUCCESSOR REVIVED. The old
// holder claims an answered run and its resume breaks before the turn began, so
// it settles the run; it reads its claim to learn the box, and before it acts
// the seat moves: the next holder fences the claim to its own lease and revives
// it, the paused box intact for the resume it now owes. The old holder's ending
// is refused by the store under the newer lease — and so is the kill, which
// used to be made first on the old holder's snapshot, destroying the box the
// revived run was then resumed into. The next holder resumes into it once, and
// the old holder announces nothing about an answer that is not its own.
func TestASlowHoldersSettleNeverKillsTheBoxItsSuccessorRevived(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.coordinator.lease = leased(rigLease)
	box := rig.get("t1").SandboxID
	if box == "" {
		t.Fatal("the premise: the parked run holds its paused box")
	}
	var next *coordRig
	rig.coordinator.pending = &readThenRecover{PendingStore: rig.pending, recover: func() {
		next = reaper(t.Context(), t, rig, rig.pending, 2, nil)
	}}
	rig.resumer.failWith(ErrResumeAbandoned)
	rig.resumer.beforeTurn = true

	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	if next == nil {
		t.Fatal("the premise: the old holder's settle read its claim, and the seat moved")
	}
	revivedUnder(t, rig, 2, 1)
	if killed := rig.provider.KilledIDs(); slices.Contains(killed, box) {
		t.Fatalf("the old holder killed box %s under the run its successor revived (killed %v)",
			box, killed)
	}
	if got := rig.answeredRecords(); len(got) != 0 {
		t.Fatalf("the old holder announced %+v for an answer the seat's next holder now drives", got)
	}

	next.fireRetries()
	resumedOnceWith(t, rig, next, "use main")
	if run := next.resumer.calls()[0].Run; run.SandboxID != box {
		t.Fatalf("the revived run was resumed naming box %q, want its paused box %q", run.SandboxID, box)
	}
	if rig.provider.Box(box) == nil {
		t.Fatalf("box %s is gone from under the resumed run", box)
	}
	if got := rig.answeredRecords(); len(got) != 1 || got[0].Outcome != types.AnswerResumed {
		t.Fatalf("announced %+v, want the answer announced once, as resumed by the next holder", got)
	}
}

// AND THE SAME ON A RUN WITH NOTHING TO RESUME INTO. A claimed run the launch
// path did not write is failed as no_execute_state, from the snapshot the claim
// read; a successor that fenced the row first keeps it — box and all — and the
// old holder says nothing.
func TestASlowHoldersFailureNeverEndsARunItsSuccessorFenced(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.coordinator.lease = leased(rigLease)
	box := rig.get("t1").SandboxID
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	// The successor fences the row the instant the old holder has claimed it,
	// before that holder reads the claim's conversation.
	rig.coordinator.pending = &claimThen{PendingStore: statelessStore{PendingStore: rig.pending},
		then: func() {
			if won, err := rig.pending.ClaimOwnership(t.Context(), "t1", "node-c", 2); err != nil || !won {
				t.Errorf("the successor's fence = %v, %v", won, err)
			}
		}}
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	got := rig.get("t1")
	if got.Ending != nil || got.OwnerEpoch != 2 {
		t.Fatalf("run %q ending %+v at epoch %d, want it left to the successor that fenced it",
			got.Status, got.Ending, got.OwnerEpoch)
	}
	if killed := rig.provider.KilledIDs(); slices.Contains(killed, box) {
		t.Fatalf("the old holder killed box %s under a run its successor holds", box)
	}
	if failed, answered := rig.failures(), rig.answeredRecords(); len(failed) != 0 || len(answered) != 0 {
		t.Fatalf("the old holder announced %+v and %+v about a run that is not its own", failed, answered)
	}
}

// claimThen lets a claim of a recorded answer land, and runs then before the
// claimant hears it did.
type claimThen struct {
	PendingStore
	once sync.Once
	then func()
}

func (s *claimThen) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	claimed, won, err := s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
	if err == nil && won && slices.Equal(tail.From, []string{StatusAnswered}) {
		s.once.Do(s.then)
	}
	return claimed, won, err
}

// fencedBySuccessor parks t1 on a question and lets the seat's next holder
// fence it — node-y at epoch 6, which recovered the seat and found the run
// waiting — while this rig's node still has a reply and an answer by turn on
// their way to it.
func fencedBySuccessor(t *testing.T, rig *coordRig) (Reply, types.SandboxAnswerGiven, *events.Event) {
	t.Helper()
	parkOnAQuestion(t, rig)
	given, ev := givenAgainst(t, rig, "t1")
	if won, err := rig.pending.ClaimOwnership(t.Context(), "t1", "node-y", 6); err != nil || !won {
		t.Fatalf("the successor's fence = %v, %v", won, err)
	}
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	return chatReply(answerOnTheDM, "use main", r1), given, ev
}

// leftToTheSuccessor asserts the old holder recorded, claimed, resumed, spent
// and announced nothing, counted no attempt against the delivery, and left the
// run waiting on its question under the successor's fence.
func leftToTheSuccessor(t *testing.T, rig *coordRig) {
	t.Helper()
	got := rig.get("t1")
	if got.Status != StatusAwaiting || got.Answer != nil || got.Owner != "node-y" || got.OwnerEpoch != 6 {
		t.Fatalf("run %q with answer %+v owned by %q at %d, want it waiting under node-y at 6",
			got.Status, got.Answer, got.Owner, got.OwnerEpoch)
	}
	if calls := rig.resumer.calls(); len(calls) != 0 {
		t.Fatalf("resumed %+v on a seat this node does not hold", calls)
	}
	if spent := rig.spentDeliveries(); len(spent) != 0 {
		t.Fatalf("spent %+v on a seat this node does not hold", spent)
	}
	if answered := rig.answeredRecords(); len(answered) != 0 {
		t.Fatalf("announced %+v about an answer this node may not record", answered)
	}
	if n := rig.coordinator.answerBudgetsFor("swe", "t1"); n != 0 {
		t.Fatalf("counted %d delivery budgets on a seat this node does not hold", n)
	}
}

// A NODE THAT HAS NOTICED IT LOST THE SEAT WRITES NOTHING ON IT. Its lease seam
// says the seat is not held here, and every write it would make on the seat's
// behalf is refused before anything reaches the store: the chat reply and the
// answer by turn are deferred to reach the seat's holder, and the run stays
// waiting under the holder's fence. It used to answer that as the ZERO fence,
// which constrains nothing: the answer was recorded and claimed past the
// successor's fence, taken, and its turn run on a seat this node did not hold.
func TestANodeThatLostTheSeatRecordsNoAnswerOnIt(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	reply, given, ev := fencedBySuccessor(t, rig)
	rig.coordinator.lease = notLeased

	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
	if d != AnswerDeferred || !errors.Is(err, ErrSeatNotHeld) {
		t.Fatalf("the reply = %q, %v, want it deferred to the seat's holder", d, err)
	}
	d, err = rig.coordinator.AnswerByTurn(t.Context(), given, ev)
	if d != AnswerDeferred || !errors.Is(err, ErrSeatNotHeld) {
		t.Fatalf("the answer by turn = %q, %v, want it deferred to the seat's holder", d, err)
	}
	leftToTheSuccessor(t, rig)
}

// AND ONE THAT HAS NOT NOTICED IS REFUSED BY THE STORE. Its lease is the one it
// held, node-x at epoch 5, which the successor's fence outranks: the record of
// the answer is refused rather than landing on a run its successor recovered as
// waiting — where nothing would ever drive it, the successor having nothing
// owed and this node about to let the seat go.
func TestAStaleHoldersAnswerIsRefusedByTheSuccessorsFence(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	reply, given, ev := fencedBySuccessor(t, rig)
	rig.coordinator.lease = leased(Fence{Owner: "node-x", Epoch: 5})

	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
	if d != AnswerDeferred || !errors.Is(err, ErrSeatNotHeld) {
		t.Fatalf("the reply = %q, %v, want it deferred to the seat's holder", d, err)
	}
	d, err = rig.coordinator.AnswerByTurn(t.Context(), given, ev)
	if d != AnswerDeferred || !errors.Is(err, ErrSeatNotHeld) {
		t.Fatalf("the answer by turn = %q, %v, want it deferred to the seat's holder", d, err)
	}
	leftToTheSuccessor(t, rig)
}

// A FENCE THAT LANDS BETWEEN THE RECORD AND THE CLAIM LEAVES THE ANSWER TO THE
// SUCCESSOR, ONCE. The old holder recorded the answer under its lease, and
// before its claim the seat moved: the successor recovered the run answered,
// fenced it and owes it the resume. The old holder's claim is refused under the
// newer fence and drops the answer; the successor resumes the run with it once.
func TestAFenceBetweenTheRecordAndTheClaimLeavesTheAnswerToTheSuccessor(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	var next *coordRig
	rig.coordinator.pending = &claimFirst{PendingStore: rig.pending, first: func() {
		next = reaper(t.Context(), t, rig, rig.pending, 2, nil)
	}}
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	if next == nil {
		t.Fatal("the premise: the seat moved between the record and the claim")
	}
	if calls := rig.resumer.calls(); len(calls) != 0 {
		t.Fatalf("the old holder resumed %+v under a claim its successor's fence refuses", calls)
	}
	if rig.coordinator.owes("t1") {
		t.Fatal("the old holder still drives an answer its successor owes")
	}
	if got := rig.get("t1"); got.Status != StatusAnswered || got.OwnerEpoch != 2 {
		t.Fatalf("run %q at epoch %d, want it answered under the successor's fence", got.Status, got.OwnerEpoch)
	}
	next.fireRetries()
	resumedOnceWith(t, rig, next, "use main")
	if got := rig.answeredRecords(); len(got) != 1 || got[0].Outcome != types.AnswerResumed {
		t.Fatalf("announced %+v, want the answer resumed once, by the successor", got)
	}
}

// claimFirst runs first before the first claim of a recorded answer reaches the
// store: the moment between a record and its claim.
type claimFirst struct {
	PendingStore
	once  sync.Once
	first func()
}

func (s *claimFirst) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	if slices.Equal(tail.From, []string{StatusAnswered}) {
		s.once.Do(s.first)
	}
	return s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
}

// A COMPLETION REACHING A NODE THAT LOST THE SEAT IS ROUTED ON, unclaimed. The
// lease seam says the seat is held elsewhere, so the completion is handed back
// as one this node cannot resume — to reach the holder, the poll firing it again
// while the job's row is running — and nothing is claimed or collected here.
func TestACompletionOnANodeThatLostTheSeatIsRoutedOn(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})
	rig.coordinator.lease = notLeased
	payload, ev := rig.completion("t1")
	err := rig.coordinator.OnCompleted(t.Context(), payload, ev)
	if !errors.Is(err, ErrResumeUnavailable) || !errors.Is(err, ErrSeatNotHeld) {
		t.Fatalf("OnCompleted = %v, want it handed back as a seat this node does not hold", err)
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("run %q, want it left running for the seat's holder", got.Status)
	}
	if calls := rig.resumer.calls(); len(calls) != 0 {
		t.Fatalf("resumed %+v on a seat this node does not hold", calls)
	}

	// AND A STALE LEASE IS OUTRANKED by the holder's fence in the store.
	if won, err := rig.pending.ClaimOwnership(t.Context(), "t1", "node-y", 6); err != nil || !won {
		t.Fatalf("the successor's fence = %v, %v", won, err)
	}
	rig.coordinator.lease = leased(Fence{Owner: "node-x", Epoch: 5})
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted under a stale lease = %v, want it left to the holder", err)
	}
	if got := rig.get("t1"); got.Status != StatusRunning || got.OwnerEpoch != 6 {
		t.Fatalf("run %q at epoch %d, want it left running under the holder's fence",
			got.Status, got.OwnerEpoch)
	}
}

// A LAUNCH ON A SEAT THIS NODE DOES NOT HOLD IS REFUSED, and writes no row: the
// turn asking for it is one the seat's lease no longer stands behind.
func TestALaunchOnASeatNotHeldIsRefused(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.coordinator.lease = notLeased
	if _, err := rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t1")); !errors.Is(err, ErrSeatNotHeld) {
		t.Fatalf("Launch = %v, want it refused", err)
	}
	if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("Get = %v, %v, want no row", found, err)
	}
	if held, _ := rig.coordinator.SeatRuns("swe"); held {
		t.Fatal("the seat is counted held by a launch that never began")
	}
}

// A SERIES ON A SEAT THIS NODE LOST STOPS, AND TOUCHES NOTHING. The answer's
// claim landed unseen, so its series owes the row a give-back — and before the
// attempt runs, the seat moves. The attempt finds the seat not held here and
// drops the series without a write: the claim is the seat's next holder's to
// find on the row and revive, and a give-back under this node's lease would only
// race that holder's fence. The seat's mail is no longer held behind it here.
func TestASeriesOnASeatThisNodeLostStopsWithoutAWrite(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	parkOnAQuestion(t, rig)
	rig.coordinator.pending = &claimLandsThenFails{PendingStore: rig.pending, fails: 1}
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	claimed := rig.get("t1")
	if claimed.Status != StatusResumed || !holds.holding("swe") {
		t.Fatalf("run %q, inbox held %v: the premise is a landed claim the series owes",
			claimed.Status, holds.holding("swe"))
	}

	rig.coordinator.lease = notLeased
	rig.now = rig.now.Add(time.Second) // so a write would show on the row
	if rig.fireRetries() != 1 {
		t.Fatal("nothing was scheduled for the suspected claim")
	}
	got := rig.get("t1")
	if got.Status != StatusResumed || !got.UpdatedAt.Equal(claimed.UpdatedAt) || got.OwnerEpoch != claimed.OwnerEpoch {
		t.Fatalf("run %q at epoch %d, written at %v: a node that lost the seat wrote on its run",
			got.Status, got.OwnerEpoch, got.UpdatedAt)
	}
	if rig.coordinator.owes("t1") || len(rig.retries.delays()) != 0 {
		t.Fatalf("owes %v, armed %v: the series outlived the seat", rig.coordinator.owes("t1"),
			rig.retries.delays())
	}
	if holds.holding("swe") {
		t.Fatal("the seat's inbox is still held here behind a series this node dropped")
	}
	if calls := rig.resumer.calls(); len(calls) != 0 {
		t.Fatalf("resumed %+v on a seat this node does not hold", calls)
	}
}
