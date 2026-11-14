package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// unsettledHost makes the rig's coordinator node-b, collecting t1 off its box on
// node-a's host, whose coordination store does not answer node-a's read of the
// run's row while "Get" is named silent on what it returns; node-b's own store
// answers throughout.
func unsettledHost(t *testing.T, rig *coordRig) (*silentStore, PendingRun) {
	t.Helper()
	store := &silentStore{PendingStore: rig.pending, silent: []string{"Get"}}
	rig.onNode(t, "node-b", hostAnswers{host: rig.hostOn(t, "node-a", store)}, nil)
	run := rig.launchedBy("t1", "node-a")
	rig.runner.Finish(wholeAccount())
	return store, run
}

// handedBackUnsettled collects t1 once and holds the collection, at step, to a
// claim handed back uncounted for a store's silence: the completion handed back
// for its retry, the run back in running, and nothing ended or resumed.
func handedBackUnsettled(t *testing.T, rig *coordRig, step string) {
	t.Helper()
	if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
		t.Fatalf("the collection %s = %v; want it handed back for the store's silence", step, err)
	}
	if got := rig.get("t1"); got.Status != StatusRunning || got.Launch.CollectFailures != 0 {
		t.Fatalf("the collection %s left the run %s with %d failed collections; want it back in running, "+
			"uncounted", step, got.Status, got.Launch.CollectFailures)
	}
	if failed, resumed := rig.failures(), rig.resumer.calls(); len(failed) != 0 || len(resumed) != 0 {
		t.Fatalf("the collection %s ended the run (%+v) or resumed it (%d)", step, failed, len(resumed))
	}
}

// lostWithItsHost collects t1 past the bound and holds it to a job its
// collection recorded LOST with the host its box is on, because that host's
// store alone did not answer it for the box timeout: nothing handed back for a
// retry, nothing ended or announced, the loss on the claim's row at the
// collection's instant as the turn resumes, and the turn resumed once to carry
// the round on in a fresh box, the round's first loss; and the clock on the host
// gone with it.
func lostWithItsHost(t *testing.T, rig *coordRig) {
	t.Helper()
	at := rig.now
	var (
		mu    sync.Mutex
		onRow *JobLoss
	)
	rig.resumer.during = func(ctx context.Context, _ PendingRun) {
		mu.Lock()
		defer mu.Unlock()
		onRow = rig.getIn(ctx, "t1").Lost
	}
	if err := collectAgain(t, rig); err != nil {
		t.Fatalf("the collection past the box timeout = %v; want the job lost with its host and its round "+
			"carried on", err)
	}
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("announced %+v; want the job lost with its host and its round carried on, never its run "+
			"ended as unreachable", failed)
	}
	mu.Lock()
	recorded := onRow
	mu.Unlock()
	if recorded == nil || recorded.Cause != LossLaunchNodeStoreSilent || recorded.Node != "node-a" ||
		!recorded.At.Equal(at) {
		t.Fatalf("as the turn resumed the claim's row recorded %+v; want the job lost with node-a, at %v",
			recorded, at)
	}
	resumed := rig.resumer.calls()
	if len(resumed) != 1 || resumed[0].Lost == nil || resumed[0].Lost.Cause != LossLaunchNodeStoreSilent ||
		resumed[0].Lost.BoxKept || resumed[0].Lost.Losses != 1 {
		t.Fatalf("the turn resumed %d times (%+v); want its round carried on once, in a fresh box, the "+
			"round's first loss", len(resumed), resumed)
	}
	if clocked(rig) {
		t.Fatal("the clock on the host's store outlived the loss it led to")
	}
}

// clocked reports whether the rig's coordinator keeps a clock on the store of
// t1's box's host.
func clocked(rig *coordRig) bool {
	rig.coordinator.mu.Lock()
	defer rig.coordinator.mu.Unlock()
	_, kept := rig.coordinator.unsettled["t1"]
	return kept
}

// unrecordedLoss is a run store that does not answer the record of a claimed
// job's loss while silent is set, as a coordination store that did not answer
// the write does, and answers every other call as the store beneath it does.
type unrecordedLoss struct {
	PendingStore
	mu     sync.Mutex
	silent bool
}

func (s *unrecordedLoss) MarkClaimLost(ctx context.Context, turnID, launch string, loss JobLoss,
	fence Fence,
) (bool, error) {
	s.mu.Lock()
	silent := s.silent
	s.mu.Unlock()
	if silent {
		return false, errStoreUnavailable("record the run's claimed job as lost", turnID)
	}
	return s.PendingStore.MarkClaimLost(ctx, turnID, launch, loss, fence)
}

func (s *unrecordedLoss) answer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent = false
}

// movedOnBeforeLoss is a run store on which the claim a collection holds moves
// on just as the collection records its job lost: moveOn runs first, as a
// newer lease fencing the run, or its ending decided, would land between the
// claim and the record.
type movedOnBeforeLoss struct {
	PendingStore
	moveOn func(ctx context.Context)
}

func (s *movedOnBeforeLoss) MarkClaimLost(ctx context.Context, turnID, launch string, loss JobLoss,
	fence Fence,
) (bool, error) {
	s.moveOn(ctx)
	return s.PendingStore.MarkClaimLost(ctx, turnID, launch, loss, fence)
}

// A COLLECTION WHOSE HOST ALONE HAS A SILENT STORE IS GIVEN UP AS A SILENT HOST
// IS. The box is up, and so is its host; only the host's read of the run's row,
// which licenses reading the box, goes unanswered, while this node's own store
// answers every claim, every release and every other step, as in a partition
// that cuts the host off from the store's leader and not this node. That is no
// verdict about the box, so each collection is handed back uncounted and asked
// again, and once the host's store answers, the box is read and the turn
// resumed. But only the host can read the box, and a host that cannot for the
// box timeout is as good as away: past it the job is LOST with that host, as a
// host absent for the box timeout has its jobs taken for lost. A finished job is
// recorded lost on the claim its collection holds and its round carried on in a
// box of its own, and a lost job's round is carried on without its read. Handed
// back with no bound, it held the run, and its seat, for as long as the
// partition lasted; and given up as a box that could not be read, it ENDED the
// run of a job that had finished, collect_unreachable, where the round's calls
// and its pushed branch were there to carry on from. The clock is the host's
// silence one answer after another: a host that answered in between starts it
// again, and a host that did not answer at all says nothing of its store and
// leaves it running, its collection handed back for the delivery's retry and
// never acknowledged, since the host's own waiter reads the run through the
// silent store. A step this node's own store did not answer is never
// given up on it, the record of the loss included, which keeps the clock for
// the next collection to record it at once; a collection this node itself cut
// short is handed back uncounted, never judged lost; a claim the record finds
// moved on (a newer lease took the run, or its ending was decided) is left to
// whoever moved it, nothing recorded or carried on from it; and at its turn's
// deadline a job lost with its host is ended as a lost job is, never as
// unreachable.
//
// Mutation: drop the bound (collectGivenUp gives up no store's silence); keep
// the clock through a host whose store answered; start it again at a host that
// did not answer, or acknowledge that host's collection; judge a step this node's own store did not answer by the
// host's clock; end the run as unreachable past the bound, or hand it back
// unlost; carry the round on with nothing recorded on the claim; forget the
// clock before the loss lands; judge a collection this node cut short; carry a
// round on from a claim the record found moved on; or leave a lost run unended
// at its deadline. Each turns a case red.
func TestACollectionWhoseHostAloneHasASilentStoreIsGivenUpAsASilentHostIs(t *testing.T) {
	t.Parallel()
	timeout := DefaultBoxTimeout

	t.Run("a host whose store stays silent for the box timeout", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		rig.now = rig.now.Add(timeout - time.Second)
		handedBackUnsettled(t, rig, "within the box timeout")
		rig.now = rig.now.Add(time.Second)
		lostWithItsHost(t, rig)
	})

	t.Run("a host whose store answers within it", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		store, _ := unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		rig.now = rig.now.Add(timeout - time.Second)
		store.answer()
		if err := collectAgain(t, rig); err != nil {
			t.Fatalf("the collection once the host's store answered: %v", err)
		}
		resumed := rig.resumer.calls()
		if len(resumed) != 1 || resumed[0].Ended.Text != wholeAccount().Text {
			t.Fatalf("the turn resumed on %+v; want it resumed once on its box's account", resumed)
		}
		if failed := rig.failures(); len(failed) != 0 {
			t.Fatalf("announced %+v", failed)
		}
	})

	t.Run("a host whose store answered in between", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		store, _ := unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		rig.now = rig.now.Add(timeout - time.Minute)
		// THE HOST'S STORE ANSWERS ITS READ OF THE ROW, and the box fails
		// the read the host then makes, which counts.
		store.answer()
		rig.runner.CollectErr = errBoxBlip
		if err := collectAgain(t, rig); err == nil || errors.Is(err, ErrStoreSilent) {
			t.Fatalf("the box's failed read = %v; want it handed back as the box's", err)
		}
		rig.runner.CollectErr = nil
		store.silence("Get")
		rig.now = rig.now.Add(time.Second)
		if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
			t.Fatalf("the collection once the host's store was silent again = %v", err)
		}
		rig.now = rig.now.Add(time.Minute)
		if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
			t.Fatalf("a collection the box timeout after the first silence, a minute into the second, = %v; "+
				"want it handed back on the second's clock", err)
		}
		if failed := rig.failures(); len(failed) != 0 {
			t.Fatalf("announced %+v for a host whose store answered a minute ago", failed)
		}
		rig.now = rig.now.Add(timeout - time.Minute)
		lostWithItsHost(t, rig)
	})

	t.Run("a host that did not answer in between", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		rig.now = rig.now.Add(timeout - time.Minute)
		asks := rig.coordinator.boxes
		rig.coordinator.boxes = &boxAsks{err: fmt.Errorf("%w: node-a, within a minute", ErrBoxHostSilent)}
		// HANDED BACK FOR ITS RETRY, NEVER ACKNOWLEDGED: the clock on the
		// host's store runs, so the host's own waiter, which reads the run
		// through that store, announces nothing, and the delivery is all
		// that asks again (renew.go).
		if err := collectAgain(t, rig); !errors.Is(err, ErrBoxHostSilent) {
			t.Fatalf("a collection the host did not answer = %v; want it handed back for its retry while "+
				"the clock on the host's store runs", err)
		}
		if got := rig.get("t1"); got.Status != StatusRunning || got.Launch.CollectFailures != 0 {
			t.Fatalf("the collection the host did not answer left the run %s with %d failed collections; "+
				"want it back in running, uncounted", got.Status, got.Launch.CollectFailures)
		}
		rig.coordinator.boxes = asks
		rig.now = rig.now.Add(time.Minute)
		lostWithItsHost(t, rig)
	})

	t.Run("a loss this node's own store does not record", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		mine := &unrecordedLoss{PendingStore: rig.coordinator.pending, silent: true}
		rig.coordinator.pending = mine
		handedBackUnsettled(t, rig, "the host's store did not answer")
		rig.now = rig.now.Add(timeout)
		handedBackUnsettled(t, rig, "past the box timeout, its loss this node's store did not record")
		if got := rig.get("t1"); got.Lost != nil {
			t.Fatalf("the row records %+v for a loss the store did not take", *got.Lost)
		}
		if !clocked(rig) {
			t.Fatal("the clock on the host's store went with a loss that did not land")
		}
		// THE NEXT COLLECTION RECORDS IT AT ONCE, on the clock kept.
		mine.answer()
		lostWithItsHost(t, rig)
	})

	// A CLAIM THE LOSS'S RECORD FINDS MOVED ON is no longer this node's to
	// carry anything on from: the store refuses the verdict, and the run is
	// left to the newer lease that took it, or to the ending decided on it.
	for _, moved := range []struct {
		name   string
		moveOn func(ctx context.Context, t *testing.T, rig *coordRig)
	}{
		{"a claim a newer lease took as its loss is recorded",
			func(ctx context.Context, t *testing.T, rig *coordRig) {
				won, err := rig.pending.ClaimOwnership(ctx, "t1", "node-c:1", rigLease.Epoch+1)
				if err != nil || !won {
					t.Errorf("the newer lease's fence = %v, %v", won, err)
				}
			}},
		{"a claim whose ending was decided as its loss is recorded",
			func(ctx context.Context, t *testing.T, rig *coordRig) {
				launch := rig.getIn(ctx, "t1").LaunchID
				if _, ok, err := rig.pending.DecideEnding(ctx, "t1", Decision{
					License: License{WhileIn: []string{StatusResumed}, Launch: launch},
					Reason:  types.ExecutorFailureSeatRemoved, Detail: "the seat left the company",
				}); err != nil || !ok {
					t.Errorf("the ending's decision = %v, %v", ok, err)
				}
			}},
	} {
		t.Run(moved.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			unsettledHost(t, rig)
			handedBackUnsettled(t, rig, "the host's store did not answer")
			rig.now = rig.now.Add(timeout)
			rig.coordinator.pending = &movedOnBeforeLoss{PendingStore: rig.coordinator.pending,
				moveOn: func(ctx context.Context) { moved.moveOn(ctx, t, rig) }}
			if err := collectAgain(t, rig); err != nil {
				t.Fatalf("the collection whose claim moved on = %v; want it left to whatever moved it", err)
			}
			if resumed := rig.resumer.calls(); len(resumed) != 0 {
				t.Fatalf("the turn resumed %d times from a claim this node no longer holds", len(resumed))
			}
			if failed := rig.failures(); len(failed) != 0 {
				t.Fatalf("announced %+v about a run whose claim moved on", failed)
			}
			if got := rig.get("t1"); got.Lost != nil {
				t.Fatalf("the row records %+v on a claim that had moved on", *got.Lost)
			}
		})
	}

	t.Run("a collection this node cuts short past the box timeout", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		rig.now = rig.now.Add(timeout)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		rig.coordinator.boxes = &steppedRead{BoxReader: rig.coordinator.boxes, step: cancel}
		if err := collectAgainIn(ctx, t, rig); err == nil {
			t.Fatal("a collection this node cut short was not handed back for a retry")
		}
		if got := rig.get("t1"); got.Status != StatusRunning || got.Lost != nil || got.Launch.CollectFailures != 0 {
			t.Fatalf("the collection cut short left the run %s, lost %+v, with %d failed collections; want it "+
				"back in running, unlost and uncounted", got.Status, got.Lost, got.Launch.CollectFailures)
		}
		if failed, resumed := rig.failures(), rig.resumer.calls(); len(failed) != 0 || len(resumed) != 0 {
			t.Fatalf("the collection cut short ended the run (%+v) or resumed it (%d)", failed, len(resumed))
		}
	})

	t.Run("a job lost with its host at its turn's deadline", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		rig.now = rig.now.Add(timeout)
		payload, _ := rig.completion("t1")
		if err := rig.coordinator.completeAtDeadline(t.Context(), payload, rigLease,
			"the seat's model is at its usage limit"); err != nil {
			t.Fatalf("the collection at the deadline = %v; want the run ended there", err)
		}
		failed := rig.failures()
		if len(failed) != 1 || failed[0].Reason != types.ExecutorFailureDeadline {
			t.Fatalf("announced %+v; want the run ended once at its deadline, as a lost job's is, never as "+
				"unreachable", failed)
		}
		if resumed := rig.resumer.calls(); len(resumed) != 0 {
			t.Fatalf("the turn resumed %d times past its deadline", len(resumed))
		}
	})

	t.Run("a lost job's read on a host whose store stays silent", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		_, run := unsettledHost(t, rig)
		if marked, err := rig.pending.MarkLost(t.Context(), "t1", run.LaunchID,
			JobLoss{Cause: LossEngineRestarted, Node: "node-a", At: rig.now}); err != nil || !marked {
			t.Fatalf("MarkLost = %v, %v", marked, err)
		}
		rig.runner.Lose(Result{Text: "half way through", InputTokens: 900, OutputTokens: 100})
		handedBackUnsettled(t, rig, "the host's store did not answer")
		rig.now = rig.now.Add(timeout)
		if err := collectAgain(t, rig); err != nil {
			t.Fatalf("the lost job's carry past the box timeout = %v; want its round carried on", err)
		}
		resumed := rig.resumer.calls()
		if len(resumed) != 1 || resumed[0].Lost == nil || resumed[0].Lost.Cause != LossEngineRestarted {
			t.Fatalf("the turn resumed %d times (%+v); want its round carried on once, without its read",
				len(resumed), resumed)
		}
		if failed := rig.failures(); len(failed) != 0 {
			t.Fatalf("announced %+v for a lost job whose round is carried on", failed)
		}
	})

	t.Run("a lost job's drain this node's own store does not answer", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		_, run := unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		// THE JOB IS LOST WITH ITS HOST AFTER ALL, which leaves no box to read,
		// and this node's own store does not answer the drain that is left.
		if marked, err := rig.pending.MarkLost(t.Context(), "t1", run.LaunchID,
			JobLoss{Cause: LossLaunchNodeGone, Node: "node-a", At: rig.now}); err != nil || !marked {
			t.Fatalf("MarkLost = %v, %v", marked, err)
		}
		mine := &silentStore{PendingStore: rig.coordinator.pending, silent: []string{"WatchLog"}}
		rig.coordinator.pending = mine
		rig.now = rig.now.Add(timeout + time.Second)
		handedBackUnsettled(t, rig, "its drain this node's store did not answer, past the host's clock")
		mine.answer()
		if err := collectAgain(t, rig); err != nil {
			t.Fatalf("the lost job's carry once this node's store answered = %v", err)
		}
		if resumed := rig.resumer.calls(); len(resumed) != 1 || resumed[0].Lost == nil {
			t.Fatalf("the turn resumed %d times; want its round carried on once", len(resumed))
		}
	})
}

// A SEAT THIS NODE LETS GO OF TAKES THE CLOCK ON ITS HOSTS' STORES WITH IT. The
// clock on a host whose store alone is silent is this node's, kept for the
// seat's collections here; the seat's next holder starts its own, and this node,
// taking the seat again later, must not give a collection up on silences it
// measured before it let the seat go.
//
// Mutation: keep the clock through the seat's release, and it is still there.
func TestASeatLetGoOfForgetsTheClockOnItsHostsStore(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	unsettledHost(t, rig)
	handedBackUnsettled(t, rig, "the host's store did not answer")
	rig.coordinator.mu.Lock()
	_, kept := rig.coordinator.unsettled["t1"]
	rig.coordinator.mu.Unlock()
	if !kept {
		t.Fatal("the premise: no clock was started on the host whose store did not answer")
	}
	rig.coordinator.ReleaseSeat("swe")
	rig.coordinator.mu.Lock()
	_, kept = rig.coordinator.unsettled["t1"]
	rig.coordinator.mu.Unlock()
	if kept {
		t.Fatal("the clock on the host's store outlived the seat's release")
	}
}

// A CLOCK ON A HOST'S STORE GOES WITH THE COLLECTION IT TIMED, whatever that
// comes to. It is this node's, kept in memory per job to bound a collection the
// box's host keeps handing back unread, and it means nothing once the job's
// collection is over: the turn launched its next job, or the run's record is
// gone, at its turn's end or its deadline. Kept until the seat was let go, it
// outlived every such run, a lost job's round carried on without its read
// among them, on a seat a node may hold for weeks. But a lost job's round whose
// resume failed is collected again, and its clock is kept for that: the next
// collection finds the bound passed at once, rather than a box timeout later.
//
// Mutation: keep the clock through the turn's next launch, or through the
// run's record deleted; or drop it as the round is carried on, before its
// resume has landed.
func TestAClockOnAHostsStoreGoesWithTheCollectionItTimed(t *testing.T) {
	t.Parallel()
	timeout := DefaultBoxTimeout

	// lostOnASilentHost has t1's job lost with its engine on node-a's host
	// while node-a's store does not answer node-a's reads of the row, and its
	// first carry handed back for that, the clock started, and the box timeout
	// passed since.
	lostOnASilentHost := func(t *testing.T) *coordRig {
		t.Helper()
		rig := newCoordRig(t)
		_, run := unsettledHost(t, rig)
		if marked, err := rig.pending.MarkLost(t.Context(), "t1", run.LaunchID,
			JobLoss{Cause: LossEngineRestarted, Node: "node-a", At: rig.now}); err != nil || !marked {
			t.Fatalf("MarkLost = %v, %v", marked, err)
		}
		rig.runner.Lose(Result{Text: "half way through"})
		handedBackUnsettled(t, rig, "the host's store did not answer")
		if !clocked(rig) {
			t.Fatal("the premise: no clock was started on the host whose store did not answer")
		}
		rig.now = rig.now.Add(timeout)
		return rig
	}

	t.Run("a round carried on whose turn launches its next job", func(t *testing.T) {
		t.Parallel()
		rig := lostOnASilentHost(t)
		rig.resumer.during = func(ctx context.Context, _ PendingRun) {
			if _, err := rig.coordinator.Launch(ctx, rig.manager, launchReq("t1")); err != nil {
				t.Errorf("the turn's relaunch: %v", err)
			}
		}
		if err := collectAgain(t, rig); err != nil {
			t.Fatalf("the carry past the box timeout = %v", err)
		}
		if resumed := rig.resumer.calls(); len(resumed) != 1 || resumed[0].Lost == nil {
			t.Fatalf("the turn resumed %d times; want the lost round carried on once", len(resumed))
		}
		if clocked(rig) {
			t.Fatal("the clock on the last job's host outlived the turn's launch of its next job")
		}
	})

	t.Run("a round carried on whose turn ends", func(t *testing.T) {
		t.Parallel()
		rig := lostOnASilentHost(t)
		if err := collectAgain(t, rig); err != nil {
			t.Fatalf("the carry past the box timeout = %v", err)
		}
		rig.finished("t1")
		if clocked(rig) {
			t.Fatal("the clock on the job's host outlived the run's record")
		}
	})

	t.Run("a run ended at its turn's deadline", func(t *testing.T) {
		t.Parallel()
		rig := lostOnASilentHost(t)
		payload, _ := rig.completion("t1")
		if err := rig.coordinator.completeAtDeadline(t.Context(), payload, rigLease,
			"the seat's model is at its usage limit"); err != nil {
			t.Fatalf("the ending at the deadline = %v", err)
		}
		rig.finished("t1")
		if clocked(rig) {
			t.Fatal("the clock on the job's host outlived the run ended at its deadline")
		}
	})

	t.Run("a round carried on whose resume failed", func(t *testing.T) {
		t.Parallel()
		rig := lostOnASilentHost(t)
		rig.resumer.failWith(errors.New("the model provider did not answer"))
		if err := collectAgain(t, rig); err == nil {
			t.Fatal("the carry whose resume failed was not handed back for a retry")
		}
		if !clocked(rig) {
			t.Fatal("the clock went with a round its resume handed back to be collected again")
		}
		// THE NEXT COLLECTION finds the bound passed at once, a minute on.
		rig.resumer.failWith(nil)
		rig.now = rig.now.Add(time.Minute)
		if err := collectAgain(t, rig); err != nil {
			t.Fatalf("the collection after the failed resume = %v; want the round carried on at once", err)
		}
		if resumed := rig.resumer.calls(); len(resumed) != 1 || resumed[0].Lost == nil {
			t.Fatalf("the turn resumed %d times; want the round carried on at once by the collection after "+
				"the resume that failed", len(resumed))
		}
	})
}
