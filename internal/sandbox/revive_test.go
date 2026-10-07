package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A DEAD CLAIM'S ANSWER IS REVIVED, ACROSS A CRASH AT EACH STEP, AND BOUNDED.
//
// The seat's next holder gives a claim whose node stopped before its turn took
// the person's answer back to that answer ([Coordinator.reapTail]): the run is
// answered again, fenced to the holder's lease and the lost claim counted on
// the row, and the holder resumes it as it resumes any answer its last holder
// recorded and never resumed with. Each case below stops a holder at one step
// of reap → revival → resume and hands the seat to a successor over the same
// store, or refuses the step and lets the same holder retry it; and the last
// two run the series past the bounds that stop a revival.

// revivalRefused is a store that refuses every revival.
func revivalRefused(rig *coordRig) *refusingStore {
	return &refusingStore{inner: rig.pending, refuse: []string{"ReviveAnswer"}}
}

// STOPPED AFTER THE FENCE, BEFORE THE REVIVAL LANDED: the reaping holder keeps
// the claim — fenced to its lease, the reply untaken on it — announces nothing,
// hands nothing back and holds the seat's mail; the holder after it revives it
// and resumes the run once. The refused revival is not counted: no claim of the
// answer died in it.
func TestARevivalStoppedBeforeItLandedIsRevivedByTheNextHolder(t *testing.T) {
	rig := newCoordRig(t)
	answeredAndClaimed(t, rig, false)
	var holds *holdSpy
	first := reaper(t.Context(), t, rig, revivalRefused(rig), 2, func(next *coordRig) { holds = next.withHold() })
	got := rig.get("t1")
	if got.Status != StatusResumed || got.OwnerEpoch != 2 || got.Answer == nil || got.Answer.Taken() ||
		got.Answer.LostClaims != 0 {
		t.Fatalf("run %q at epoch %d answer %+v, want the claim kept, fenced to the reaper, its "+
			"reply untaken and nothing counted", got.Status, got.OwnerEpoch, got.Answer)
	}
	if !holds.holding("swe") {
		t.Fatal("the seat's inbox is open while the claim's answer is owed its revival")
	}
	if handed, failed := rig.handedBack(), rig.failures(); len(handed) != 0 || len(failed) != 0 {
		t.Fatalf("handed back %v and announced %+v over a revival the store did not take", handed, failed)
	}

	first.coordinator.Stop()
	next := reaper(t.Context(), t, rig, rig.pending, 3, nil)
	revivedUnder(t, rig, 3, 1)
	next.fireRetries()
	resumedOnceWith(t, rig, next, "use main")
	if calls := first.resumer.calls(); len(calls) != 0 {
		t.Fatalf("the holder that stopped resumed %+v as well", calls)
	}
}

// A REVIVAL THE STORE REFUSED IS RETRIED BY THE HOLDER THAT OWES IT, rather than
// read as a reason to end the run: on the hand-back's spacing it reaps the claim
// afresh, revives it and resumes the run once, the seat's mail held until then.
// It used to fall straight through to the reap — the run ended and the answer
// handed to the seat as an ordinary message — over a store that did not answer
// once.
func TestARevivalTheStoreRefusedIsRetriedByItsHolder(t *testing.T) {
	rig := newCoordRig(t)
	answeredAndClaimed(t, rig, false)
	store := revivalRefused(rig)
	var holds *holdSpy
	first := reaper(t.Context(), t, rig, store, 2, func(next *coordRig) { holds = next.withHold() })
	if got := rig.get("t1"); got.Status != StatusResumed {
		t.Fatalf("run %q, want the claim kept for the revival's retry", got.Status)
	}

	// STILL REFUSED: kept again, and nothing given up.
	if first.fireRetries() != 1 {
		t.Fatal("nothing was scheduled to retry the revival")
	}
	if got := rig.get("t1"); got.Status != StatusResumed || !holds.holding("swe") {
		t.Fatalf("run %q, inbox held %v, want the claim kept again and the seat's mail behind it",
			got.Status, holds.holding("swe"))
	}
	if handed, failed := rig.handedBack(), rig.failures(); len(handed) != 0 || len(failed) != 0 {
		t.Fatalf("handed back %v and announced %+v over a revival the store did not take", handed, failed)
	}

	store.refuse = nil
	if first.fireRetries() != 1 {
		t.Fatal("nothing was scheduled to retry the revival")
	}
	resumedOnceWith(t, rig, first, "use main")
	if holds.holding("swe") {
		t.Fatalf("the seat's inbox is held after the revived run was resumed: %v", holds.log)
	}
}

// reviveLandsThenFails gives the claim back to its answer and then reports that
// it could not: the write landed, and the store's answer was lost.
type reviveLandsThenFails struct{ PendingStore }

func (s reviveLandsThenFails) ReviveAnswer(ctx context.Context, turnID string, revival Revival,
) (PendingRun, bool, error) {
	if _, _, err := s.PendingStore.ReviveAnswer(ctx, turnID, revival); err != nil {
		return PendingRun{}, false, err
	}
	return PendingRun{}, false, errRefusedCall
}

// A REVIVAL THAT LANDED UNSEEN IS FOUND ON THE RETRY, and the run it made is
// resumed — once, and counted once: the retry reads the row, finds the run
// answered rather than a claim, and goes on as its owed resume rather than
// reviving or reaping anything.
func TestARevivalThatLandedUnseenIsResumedOnItsRetry(t *testing.T) {
	rig := newCoordRig(t)
	answeredAndClaimed(t, rig, false)
	holder := reaper(t.Context(), t, rig, reviveLandsThenFails{rig.pending}, 2, nil)
	revivedUnder(t, rig, 2, 1)
	if calls := holder.resumer.calls(); len(calls) != 0 {
		t.Fatalf("resumed %+v on a revival the store reported as failed", calls)
	}
	holder.fireRetries()
	resumedOnceWith(t, rig, holder, "use main")
}

// STOPPED AFTER THE REVIVAL, BEFORE THE RESUME CLAIMED IT: the run is an answered
// run like any other, so the holder after it resumes it once — and counts no
// second lost claim, because no claim died.
func TestARevivedRunWhoseHolderStoppedBeforeItsClaimIsResumedNotRevived(t *testing.T) {
	rig := newCoordRig(t)
	answeredAndClaimed(t, rig, false)
	first := reaper(t.Context(), t, rig, rig.pending, 2, nil)
	revivedUnder(t, rig, 2, 1)
	first.coordinator.Stop()

	next := reaper(t.Context(), t, rig, rig.pending, 3, nil)
	revivedUnder(t, rig, 3, 1)
	next.fireRetries()
	resumedOnceWith(t, rig, next, "use main")
}

// dieOnTheWayToTheTurn claims t1's answer for its resume, as the holder's retry
// does, and stops the holder before its turn takes it.
func dieOnTheWayToTheTurn(t *testing.T, rig, holder *coordRig) {
	t.Helper()
	launch := rig.get("t1").LaunchID
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1", RecordedAnswerTail(launch), Fence{}); err != nil || !won {
		t.Fatalf("the holder's claim = %v, %v", won, err)
	}
	holder.coordinator.Stop()
}

// A REVIVED RUN WHOSE CLAIM DIES AGAIN IS REVIVED AGAIN, the second lost claim
// counted on the same answer from the same first instant, and resumed once.
func TestARevivedRunWhoseClaimDiesAgainIsRevivedAgain(t *testing.T) {
	rig := newCoordRig(t)
	answeredAndClaimed(t, rig, false)
	first := reaper(t.Context(), t, rig, rig.pending, 2, nil)
	since := rig.get("t1").Answer.FirstLostAt
	dieOnTheWayToTheTurn(t, rig, first)

	next := reaper(t.Context(), t, rig, rig.pending, 3, nil)
	revivedUnder(t, rig, 3, 2)
	if got := rig.get("t1").Answer.FirstLostAt; !got.Equal(since) {
		t.Fatalf("the first lost claim moved from %v to %v", since, got)
	}
	next.fireRetries()
	resumedOnceWith(t, rig, next, "use main")
}

// THE REVIVALS OF ONE ANSWER ARE BOUNDED. A resume that takes its node down
// before its turn begins would otherwise be handed to every next holder to fall
// over the same way, the seat's mail held behind it each time. After
// [MaxAnswerRevivals] revivals, the next claim that dies is reaped: the reply
// goes back to the seat once, the run is announced lost once, saying why it was
// not resumed again, and no holder ever resumed it.
func TestTheRevivalsOfOneAnswerAreBounded(t *testing.T) {
	rig := newCoordRig(t)
	r1 := answeredAndClaimed(t, rig, false)
	var holders []*coordRig
	for i := range MaxAnswerRevivals {
		epoch := int64(i + 2)
		holder := reaper(t.Context(), t, rig, rig.pending, epoch, nil)
		revivedUnder(t, rig, epoch, i+1)
		holders = append(holders, holder)
		dieOnTheWayToTheTurn(t, rig, holder)
	}
	last := reaper(t.Context(), t, rig, rig.pending, int64(MaxAnswerRevivals+2), nil)
	last.fireRetries()

	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy exactly once, past the answer's revivals", got)
	}
	rig.finished("t1")
	abandonedOnce(t, rig, true)
	want := fmt.Sprintf("%d claims of this answer have now died", MaxAnswerRevivals+1)
	if detail := rig.failures()[0].Detail; !strings.Contains(detail, want) {
		t.Errorf("the announcement says %q, want it to say %q", detail, want)
	}
	for _, holder := range append(holders, last) {
		if calls := holder.resumer.calls(); len(calls) != 0 {
			t.Fatalf("a holder resumed %+v", calls)
		}
	}
}

// AND BY THE RUN'S OWN WINDOW. A claim that dies once its answer has been owed
// its resume for longer than the run's pause_ttl_seconds since the first claim
// of it died is reaped, the reply handed back, however few claims have died:
// past the tolerance the run itself declared for a reply, a resume that keeps
// dying with its node is not a transient worth waiting out.
func TestARevivalPastTheRunsWindowHandsTheReplyBack(t *testing.T) {
	rig := newCoordRig(t)
	r1 := answeredAndClaimed(t, rig, false)
	window := answerWindow(rig.get("t1"))
	if window <= 0 {
		t.Fatalf("the premise: the rig's run declares an awaiting window, got %v", window)
	}
	if _, ok, err := rig.pending.mutate(t.Context(), "t1", func(run *PendingRun) bool {
		answer := *run.Answer
		answer.LostClaims, answer.FirstLostAt = 1, rig.now.Add(-window)
		run.Answer = &answer
		return true
	}); err != nil || !ok {
		t.Fatalf("dating the first lost claim = %v, %v", ok, err)
	}
	next := reaper(t.Context(), t, rig, rig.pending, 2, nil)
	next.fireRetries()

	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy exactly once, past the run's window", got)
	}
	rig.finished("t1")
	abandonedOnce(t, rig, true)
	if detail := rig.failures()[0].Detail; !strings.Contains(detail, "pause_ttl_seconds") {
		t.Errorf("the announcement says %q; it should name the window it passed", detail)
	}
	if calls := next.resumer.calls(); len(calls) != 0 {
		t.Fatalf("the next holder resumed %+v past the run's window", calls)
	}
}

// AN ENDING DECIDED WHILE A REVIVAL WAITS IS FINISHED BY IT, never revived over.
// The holder's revival is refused and kept; before it is retried, a node that
// held no lease — and so is outranked by nothing — ends the claim it still
// believes it holds. The retry finds the ending on the row and finishes it on
// the terms recorded: the reply handed back once, the loss announced once.
func TestAnEndingDecidedWhileARevivalWaitsIsFinishedByIt(t *testing.T) {
	rig := newCoordRig(t)
	r1 := answeredAndClaimed(t, rig, false)
	store := revivalRefused(rig)
	holder := reaper(t.Context(), t, rig, store, 2, nil)
	launch := rig.get("t1").LaunchID
	if _, ok, err := rig.pending.DecideEnding(t.Context(), "t1", Decision{
		License: License{WhileIn: []string{StatusResumed}, Launch: launch},
		Reason:  types.SandboxFailureClaimStranded, Detail: "its claim could not be handed back",
	}); err != nil || !ok {
		t.Fatalf("the unleased node's ending = %v, %v", ok, err)
	}

	store.refuse = nil
	holder.fireRetries()
	if got := rig.handedBack(); !slices.Equal(got, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy exactly once", got)
	}
	rig.finished("t1")
	if got := rig.failures(); len(got) != 1 || got[0].Reason != types.SandboxFailureClaimStranded {
		t.Fatalf("announced %+v, want the decided ending announced once", got)
	}
	if calls := holder.resumer.calls(); len(calls) != 0 {
		t.Fatalf("the holder resumed %+v a run whose ending was decided", calls)
	}
}

// A REVIVAL TAKES ITS INSTANT FROM THE STORE THAT WRITES IT, so the window it is
// later judged against runs from one clock — the row's — whichever holder
// revived it and whichever reads it.
func TestARevivalIsDatedWhenItLanded(t *testing.T) {
	rig := newCoordRig(t)
	answeredAndClaimed(t, rig, false)
	rig.now = rig.now.Add(7 * time.Minute)
	reaper(t.Context(), t, rig, rig.pending, 2, nil)
	if got := rig.get("t1").Answer.FirstLostAt; !got.Equal(rig.now) {
		t.Fatalf("the first lost claim is dated %v, want %v", got, rig.now)
	}
}

// raceTheRevival fails the first revival it is asked for, and races the second:
// the claim is given back and taken again around it, so the revival finds no
// claim and the reap's read after it finds one — another resume of the answer,
// by a node still running under the same lease. Every later revival is the
// store's own.
type raceTheRevival struct {
	PendingStore
	calls int
}

func (s *raceTheRevival) ReviveAnswer(ctx context.Context, turnID string, revival Revival,
) (PendingRun, bool, error) {
	s.calls++
	switch s.calls {
	case 1:
		return PendingRun{}, false, errRefusedCall
	case 2:
		run, _, err := s.PendingStore.Get(ctx, turnID)
		if err != nil {
			return PendingRun{}, false, err
		}
		if _, err := s.PendingStore.ReleaseClaim(ctx, turnID, Release{
			Launch: run.LaunchID, To: StatusAnswered, Fence: fenceOf(run),
		}); err != nil {
			return PendingRun{}, false, err
		}
		written, ok, err := s.PendingStore.ReviveAnswer(ctx, turnID, revival)
		if _, _, claimErr := s.PendingStore.ClaimForResume(ctx, turnID,
			RecordedAnswerTail(run.LaunchID), Fence{}); claimErr != nil {
			return PendingRun{}, false, claimErr
		}
		return written, ok, err
	}
	return s.PendingStore.ReviveAnswer(ctx, turnID, revival)
}

// A CLAIM TAKEN AGAIN UNDER A RETRIED REVIVAL IS REVIVED ON THE NEXT RETRY. The
// holder's retry finds the row moved under its revival, reads it again and finds
// a claim once more; it keeps the revival owed rather than settling the series,
// which would leave that claim — if its node stops too — to nothing but the
// seat's next move.
func TestAClaimTakenAgainUnderARetriedRevivalIsRevivedOnTheNextRetry(t *testing.T) {
	rig := newCoordRig(t)
	answeredAndClaimed(t, rig, false)
	holder := reaper(t.Context(), t, rig, &raceTheRevival{PendingStore: rig.pending}, 2, nil)
	if holder.fireRetries() != 1 {
		t.Fatal("nothing was scheduled to retry the revival")
	}
	if got := rig.get("t1"); got.Status != StatusResumed {
		t.Fatalf("run %q, want the claim taken again under the retry", got.Status)
	}
	if holder.fireRetries() != 1 {
		t.Fatal("the retry settled its series over a claim it did not revive")
	}
	resumedOnceWith(t, rig, holder, "use main")
}

// movesUnreadably gives the claim back under the first revival it is asked for,
// so the revival finds no claim, and then fails the one read the reap makes
// after it: the old holder's release landing, and the store not answering.
type movesUnreadably struct {
	PendingStore
	moved, unread bool
}

func (s *movesUnreadably) ReviveAnswer(ctx context.Context, turnID string, revival Revival,
) (PendingRun, bool, error) {
	if !s.moved {
		s.moved = true
		run, _, err := s.PendingStore.Get(ctx, turnID)
		if err != nil {
			return PendingRun{}, false, err
		}
		if _, err := s.PendingStore.ReleaseClaim(ctx, turnID, Release{
			Launch: run.LaunchID, To: StatusAnswered, Fence: fenceOf(run),
		}); err != nil {
			return PendingRun{}, false, err
		}
	}
	return s.PendingStore.ReviveAnswer(ctx, turnID, revival)
}

func (s *movesUnreadably) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	if s.moved && !s.unread {
		s.unread = true
		return PendingRun{}, false, errRefusedCall
	}
	return s.PendingStore.Get(ctx, turnID)
}

// A CLAIM THAT MOVED UNDER ITS REVIVAL AND COULD NOT BE READ AGAIN IS RETRIED,
// never left: what it moved to is unknown, and the retry reads it — an answered
// run, given back by its holder — and resumes it once, the seat's mail held
// until then.
func TestAClaimThatMovedUnreadablyUnderItsRevivalIsRetried(t *testing.T) {
	rig := newCoordRig(t)
	answeredAndClaimed(t, rig, false)
	var holds *holdSpy
	holder := reaper(t.Context(), t, rig, &movesUnreadably{PendingStore: rig.pending}, 2,
		func(next *coordRig) { holds = next.withHold() })
	if !holds.holding("swe") {
		t.Fatal("the seat's inbox is open while the moved claim's answer is unaccounted for")
	}
	if holder.fireRetries() != 1 {
		t.Fatal("nothing was scheduled to read the moved claim again")
	}
	resumedOnceWith(t, rig, holder, "use main")
}

// claimLandsThenFails lets the next fails claims of a recorded answer land and
// then reports that each could not: the write committed, and the store's answer
// was lost. slow, where set, runs between the two — a claim whose reply takes
// that long to be lost.
type claimLandsThenFails struct {
	PendingStore
	fails int
	slow  func()
}

func (s *claimLandsThenFails) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	claimed, won, err := s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
	if err != nil || !won || s.fails == 0 || !slices.Equal(tail.From, []string{StatusAnswered}) {
		return claimed, won, err
	}
	s.fails--
	if s.slow != nil {
		s.slow()
	}
	return PendingRun{}, false, errRefusedCall
}

// A CLAIM THAT LANDED UNSEEN IS REVIVED BY THE SERIES THAT MADE IT. The answer's
// inline attempt claims the run and the store's reply is lost, so the attempt
// counts a failure — but the claim landed, and no resume holds it. The next
// attempt finds it, revives it under this node's lease and resumes the run
// once. It used to read the claim as the run having moved on and settle: the
// run sat in a claim nothing drives, the person's answer with it, until the
// seat changed hands.
func TestAClaimThatLandedUnseenIsRevivedByItsOwnSeries(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.coordinator.lease = func(string) Fence { return rigLease }
	rig.coordinator.pending = &claimLandsThenFails{PendingStore: rig.pending, fails: 1}
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	if got := rig.get("t1"); got.Status != StatusResumed || got.Answer == nil || got.Answer.Taken() {
		t.Fatalf("run %q answer %+v, want the premise: the claim landed and no turn took the answer",
			got.Status, got.Answer)
	}
	if calls := rig.resumer.calls(); len(calls) != 0 {
		t.Fatalf("resumed %+v under a claim the store reported as failed", calls)
	}
	if rig.fireRetries() != 1 {
		t.Fatal("nothing was scheduled after the claim reported a failure")
	}
	resumedOnceWith(t, rig, rig, "use main")
}

// A HEALTHY NODE'S OWN UNCONFIRMED CLAIMS ARE NO LOST CLAIMS. Every claim of the
// answer lands and reports a failure, more times than the revivals of a dead
// claim are bounded by: no node stopped, so none of them is counted on the row,
// nothing ends the run, and the series resumes it once the store answers — with
// the person's answer, nothing handed back and nothing announced lost. Counted as
// lost claims, the fourth ended the run as an abandoned tail, announced as a node
// that had stopped, on a node that never did.
func TestAHealthyNodesUnconfirmedClaimsAreNotLostClaims(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.coordinator.lease = func(string) Fence { return rigLease }
	rig.coordinator.pending = &claimLandsThenFails{PendingStore: rig.pending, fails: MaxAnswerRevivals + 1}
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	for i := range MaxAnswerRevivals + 1 {
		if got := rig.get("t1"); got.Answer == nil || got.Answer.LostClaims != 0 ||
			!got.Answer.FirstLostAt.IsZero() {
			t.Fatalf("after %d unconfirmed claims the answer is %+v, want nothing counted as lost",
				i+1, got.Answer)
		}
		if rig.fireRetries() != 1 {
			t.Fatalf("after %d unconfirmed claims nothing was scheduled", i+1)
		}
	}
	resumedOnceWith(t, rig, rig, "use main")
	rig.finished("t1")
}

// suspectOnTheLastAttempt records R1 with every resume failing, runs beforeLast
// — the attempts it spends before the last one — and then makes the next
// attempt's claim land and report a failure, running slow before the failure is
// reported: the series' last failure leaves a claim on the row that no resume
// holds. Returns R1 and the seat's hold.
func suspectOnTheLastAttempt(t *testing.T, rig *coordRig, beforeLast, slow func()) (*holdSpy, *events.Event) {
	t.Helper()
	holds := rig.withHold()
	parkOnAQuestion(t, rig)
	rig.coordinator.lease = func(string) Fence { return rigLease }
	r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
	rig.resumer.failWith(errors.New("the model provider is overloaded"))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	beforeLast()
	rig.coordinator.pending = &claimLandsThenFails{PendingStore: rig.pending, fails: 1, slow: slow}
	if rig.fireRetries() != 1 {
		t.Fatal("nothing was scheduled for the answer's last attempt")
	}
	if got := rig.get("t1"); got.Status != StatusResumed || got.Answer == nil || got.Answer.Taken() {
		t.Fatalf("run %q answer %+v, want the premise: the last claim landed, no turn took the answer",
			got.Status, got.Answer)
	}
	if got := rig.handedBack(); len(got) != 0 {
		t.Fatalf("handed back %v while the answer's last claim still held it", got)
	}
	return holds, r1
}

// declinedOnceAfterTheClaim asserts the claim the last attempt left was given
// back to its answer and the answer then let go of exactly once: the run waiting
// on its question again, R1's copy handed back and R1 spent once, the seat's
// inbox lifted and still lifted after a recount, and nothing left scheduled.
func declinedOnceAfterTheClaim(t *testing.T, rig *coordRig, holds *holdSpy, r1 *events.Event) {
	t.Helper()
	if rig.fireRetries() != 1 {
		t.Fatal("nothing was scheduled to give the last claim back before the decline")
	}
	got := rig.get("t1")
	if got.Status != StatusAwaiting || got.Answer != nil {
		t.Fatalf("run %q with answer %+v, want it waiting on its question again, not left in the claim",
			got.Status, got.Answer)
	}
	if handed := rig.handedBack(); !slices.Equal(handed, []string{declinedCopyID(r1.ID).String()}) {
		t.Fatalf("handed back %v, want R1's copy exactly once", handed)
	}
	if spent := rig.spentDeliveries(); len(spent) != 1 || spent[0].id != r1.ID.String() {
		t.Fatalf("spent %+v, want R1's delivery spent once", spent)
	}
	if left := rig.retries.delays(); len(left) != 0 {
		t.Fatalf("still scheduled %v after the answer was let go", left)
	}
	if got := rig.failures(); len(got) != 0 {
		t.Fatalf("announced %+v: a decline does not end the run", got)
	}
	rig.coordinator.syncSeat(t.Context(), "swe")
	if holds.holding("swe") {
		t.Fatalf("the seat's inbox is held after the answer was let go: %v", holds.log)
	}
	if held, awaiting := rig.coordinator.SeatRuns("swe"); held || !awaiting {
		t.Fatalf("the seat reads held %v, awaiting %v; want it free with the question open", held, awaiting)
	}
}

// A CLAIM THAT LANDED UNSEEN ON THE ANSWER'S LAST ATTEMPT IS GIVEN BACK BEFORE
// THE DECLINE. The attempt that spends the budget claims the run and the store's
// reply is lost; the decline that used to follow at once found the run in that
// claim rather than answered, was refused, and the series settled on the
// refusal: the run left in a claim nothing drove, the person's answer untaken
// on it, nothing handed back — and the seat held behind it on its next recount.
func TestAClaimThatLandedUnseenOnTheLastAttemptIsGivenBackBeforeTheDecline(t *testing.T) {
	rig := newCoordRig(t)
	holds, r1 := suspectOnTheLastAttempt(t, rig, func() {
		for i := range MaxAnswerAttempts - 2 {
			if rig.fireRetries() != 1 {
				t.Fatalf("after %d failed resumes nothing was scheduled", i+1)
			}
		}
	}, nil)
	declinedOnceAfterTheClaim(t, rig, holds, r1)
}

// AND ON THE RUN'S WINDOW. An attempt whose claim lands unseen as the run's
// pause_ttl_seconds lapses — a claim slow to fail, an admission that waited
// behind a pause — is the series' last just the same, and its claim is given
// back before the decline.
func TestAClaimThatLandedUnseenAsTheWindowLapsedIsGivenBackBeforeTheDecline(t *testing.T) {
	rig := newCoordRig(t)
	holds, r1 := suspectOnTheLastAttempt(t, rig, func() {}, func() {
		window := answerWindow(rig.get("t1"))
		if window <= 0 {
			t.Fatalf("the premise: the rig's run declares an awaiting window, got %v", window)
		}
		rig.now = rig.now.Add(window)
	})
	declinedOnceAfterTheClaim(t, rig, holds, r1)
}

// A SUSPECTED CLAIM IS NEVER SETTLED AWAY. Whatever path settles the series of
// an answer whose claim it suspects — a refusal that says only that the row is
// not what that path expected — the series stays, with an attempt armed, and the
// seat's mail stays behind it: nothing but the series ever looks at that claim,
// and a series dropped on such a refusal left the run in it, undriven.
func TestASuspectedClaimKeepsItsSeries(t *testing.T) {
	rig := newCoordRig(t)
	holds := rig.withHold()
	parkOnAQuestion(t, rig)
	ctx := t.Context()
	rig.coordinator.owe(ctx, "swe", "t1")
	rig.coordinator.suspectClaim(rig.get("t1"))
	rig.coordinator.settleOwed(ctx, "swe", "t1")
	if !rig.coordinator.owes("t1") || !holds.holding("swe") {
		t.Fatalf("owes %v, inbox held %v: the series of a suspected claim was settled away",
			rig.coordinator.owes("t1"), holds.holding("swe"))
	}
	if armed := rig.retries.delays(); len(armed) != 1 {
		t.Fatalf("armed %v, want one attempt to look at the suspected claim", armed)
	}
}
