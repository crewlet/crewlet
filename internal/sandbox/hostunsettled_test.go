package sandbox

import (
	"errors"
	"fmt"
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

// givenUpUnreachable holds t1 to a run its collection gave up on because its
// host's store alone did not answer: ended, once, as collect_unreachable, saying
// why, and its record gone.
func givenUpUnreachable(t *testing.T, rig *coordRig) {
	t.Helper()
	if err := collectAgain(t, rig); err != nil {
		t.Fatalf("the collection past the box timeout = %v; want it given up", err)
	}
	failed := rig.failures()
	if len(failed) != 1 || failed[0].Reason != types.ExecutorFailureCollect ||
		failed[0].Detail != collectUnsettledDetail {
		t.Fatalf("announced %+v; want one %q saying the host could not read the run's record",
			failed, types.ExecutorFailureCollect)
	}
	rig.finished("t1")
}

// A COLLECTION WHOSE HOST ALONE HAS A SILENT STORE IS GIVEN UP AS A SILENT HOST
// IS. The box is up, and so is its host; only the host's read of the run's row,
// which licenses reading the box, goes unanswered, while this node's own store
// answers every claim, every release and every other step, as in a partition
// that cuts the host off from the store's leader and not this node. That is no
// verdict about the box, so each collection is handed back uncounted and asked
// again, and once the host's store answers, the box is read and the turn
// resumed. But only the host can read the box, and a host that cannot for the
// box timeout is as good as away: past it the collection is given up, a
// finished job's run ended as unreachable and a lost job's round carried on
// without its read, as a host absent for the box timeout has its jobs taken for
// lost. Handed back with no bound, it held the run, and its seat, for as long
// as the partition lasted. The clock is the host's silence one answer after
// another: a host that answered in between starts it again, and a host that
// did not answer at all says nothing of its store and leaves it running. And a
// step this node's own store did not answer is never given up on it.
//
// Mutation: drop the bound (collectGivenUp gives up no store's silence); keep
// the clock through a host whose store answered; start it again at a host that
// did not answer; judge a step this node's own store did not answer by the
// host's clock. Each turns a case red.
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
		givenUpUnreachable(t, rig)
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
		givenUpUnreachable(t, rig)
	})

	t.Run("a host that did not answer in between", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		rig.now = rig.now.Add(timeout - time.Minute)
		asks := rig.coordinator.boxes
		rig.coordinator.boxes = &boxAsks{err: fmt.Errorf("%w: node-a, within a minute", ErrBoxHostSilent)}
		if err := collectAgain(t, rig); err != nil {
			t.Fatalf("a collection the host did not answer = %v; want it handed back and acknowledged", err)
		}
		rig.coordinator.boxes = asks
		rig.now = rig.now.Add(time.Minute)
		givenUpUnreachable(t, rig)
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
