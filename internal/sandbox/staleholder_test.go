package sandbox

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

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
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.coordinator.lease = func(string) Fence { return rigLease }
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
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.coordinator.lease = func(string) Fence { return rigLease }
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
