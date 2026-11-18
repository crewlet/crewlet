package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// cancelledAtTheDrain is a run store whose watch of a run's log finds the
// delivery cancelled under it, as a drain, a restart or the seat moving away
// cancels a collection this node is making once its box has answered.
type cancelledAtTheDrain struct {
	PendingStore
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (s *cancelledAtTheDrain) WatchLog(ctx context.Context, turnID string) (<-chan LogView, error) {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		return s.PendingStore.WatchLog(ctx, turnID)
	}
	cancel()
	return nil, fmt.Errorf("sandbox: watch the log of run %s: %w", turnID, ctx.Err())
}

// stopping arms the store to cancel the next collection's delivery at its
// drain, and returns that delivery's context.
func (s *cancelledAtTheDrain) stopping(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(t.Context())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = cancel
	return ctx
}

// answering has the store watch every later log as the store beneath it does.
func (s *cancelledAtTheDrain) answering() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = nil
}

// A BOX THAT ANSWERED ENDS THE RUN OF FAILURES BEHIND IT, WHATEVER STEP FAILS
// AFTER ITS READ. The bound on a collection's retries is on CONSECUTIVE failures
// of the box, and a box that answered while a later step of its collection
// failed uncounted (a store that did not answer the drain's watch or the record
// of the sessions its job ran out of context in or whose request its provider
// refused, or a read of what the run pushed or a drain this node cut short) is
// a box that answered. Handed back
// with that run left open, the failures from before were measured across every
// read it answered, and once the store was back the first blip gave up as lost
// a box that had answered each time: collect_unreachable, "its result is lost",
// or for a lost job its round carried on without its read, uncharged.
//
// Mutation: drop the release's end of the run (the store's clear, or the field
// from the release), or the mark the read sets on a collection or on a lost
// job's carry, or set the collection's mark only once its push read is done:
// each turns a case red.
func TestABoxThatAnsweredEndsTheRunOfFailuresWhateverFailsAfterItsRead(t *testing.T) {
	t.Parallel()
	partial := Result{Text: "half way through", InputTokens: 900, OutputTokens: 100}
	for _, tc := range []struct {
		name string
		// box is what t1's job left in its box, and lost whether the job was
		// lost with its host (its round carried on).
		box  Result
		lost bool
		// repos launches t1 with repositories on Gitlet, so its collection
		// reads what it pushed once its box has answered.
		repos bool
		// arrange makes every collection after the first blip read the box
		// and then fail at a later step, uncounted, and returns the context
		// one such collection runs under and what has every step answer again.
		arrange func(t *testing.T, rig *coordRig) (func() context.Context, func())
	}{
		{name: "the drain's watch the store did not answer", box: wholeAccount(),
			arrange: silentAfterTheRead("WatchLog")},
		{name: "the record of the sessions its job ran out of context in or whose request its provider refused",
			box:     Result{Error: "Prompt is too long", ContextExhausted: true, SessionID: "s-1"},
			arrange: silentAfterTheRead("RetireSessions")},
		{name: "a drain this node stopped", box: wholeAccount(), arrange: stoppedAtTheDrain},
		{name: "a read of what the run pushed this node stopped", box: wholeAccount(), repos: true,
			arrange: stoppedAtThePushRead},
		{name: "a lost job's drain the store did not answer", box: partial, lost: true,
			arrange: silentAfterTheRead("WatchLog")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			next, heal := tc.arrange(t, rig)
			if tc.repos {
				rig.launchWith("t1", withNative)
			} else {
				rig.launch("t1")
			}
			rig.coordinator.countRun("swe", StatusRunning)
			if tc.lost {
				rig.runner.Lose(tc.box)
				if fired := rig.tick(); fired != 1 {
					t.Fatalf("announced %d completions for a lost job, want 1", fired)
				}
			} else {
				rig.runner.Finish(tc.box)
			}

			// ONE BLIP OF THE BOX, counted: the run of failures starts here.
			rig.runner.CollectErr = errBoxBlip
			if err := collectAgain(t, rig); !errors.Is(err, errBoxBlip) {
				t.Fatalf("the blip = %v", err)
			}
			rig.runner.CollectErr = nil
			if got := rig.get("t1").Launch; got.CollectFailures != 1 {
				t.Fatalf("the premise: one blip counted %d failures", got.CollectFailures)
			}

			// THEN THE BOX ANSWERS EVERY READ while a later step fails, for
			// longer than the give-up window.
			var reads int
			rig.runner.CollectFunc = func(context.Context) error { reads++; return nil }
			for range MinConnectFailures + 1 {
				rig.now = rig.now.Add(ConnectGiveUp)
				payload, ev := rig.completion("t1")
				if err := rig.coordinator.OnCompleted(next(), payload, ev); err == nil {
					t.Fatal("a collection whose later step failed was not handed back")
				}
			}
			rig.runner.CollectFunc = nil
			if got := rig.get("t1").Launch; got.CollectFailures != 0 || !got.CollectFailingSince.IsZero() {
				t.Errorf("the box answered %d reads, and the run of failures stands at %d since %v; want it "+
					"ended by the first read that answered", reads, got.CollectFailures, got.CollectFailingSince)
			}

			// ONE BLIP ONCE EVERY STEP ANSWERS AGAIN: a run of its own.
			heal()
			rig.runner.CollectErr = errBoxBlip
			blipErr := collectAgain(t, rig)
			if failed, resumed := rig.failures(), rig.resumer.calls(); len(failed) != 0 || len(resumed) != 0 {
				t.Fatalf("ONE blip after the box answered %d reads gave the run up (%v): announced %+v, "+
					"resumed %d times", reads, blipErr, failed, len(resumed))
			}
			if got := rig.get("t1").Launch; got.CollectFailures != 1 || !got.CollectFailingSince.Equal(rig.now.UTC()) {
				t.Fatalf("the blip after the box answered left %d failures since %v; want one since %v",
					got.CollectFailures, got.CollectFailingSince, rig.now.UTC())
			}

			rig.runner.CollectErr = nil
			if err := collectAgain(t, rig); err != nil {
				t.Fatalf("the collection once the box answered again: %v", err)
			}
			resumed := rig.resumer.calls()
			if len(resumed) != 1 {
				t.Fatalf("resumed %d times; want once", len(resumed))
			}
			if tc.lost {
				if resumed[0].Lost == nil || !strings.Contains(resumed[0].Answer, partial.Text) {
					t.Fatalf("the round was carried on from %+v; want it carried on once, on its box's read",
						resumed[0])
				}
				if charged := rig.accountant.total(); charged != 1000 {
					t.Fatalf("charged %d tokens; want the 1000 the lost job's box read", charged)
				}
			}
		})
	}
}

// wholeAccount is a box's whole, successful account of its job.
func wholeAccount() Result {
	return Result{Success: true, Text: "the box's account", InputTokens: 900, OutputTokens: 100}
}

// silentAfterTheRead has the store not answer calls once the box has answered
// a read, from the collections after the first blip on.
func silentAfterTheRead(calls ...string) func(*testing.T, *coordRig) (func() context.Context, func()) {
	return func(t *testing.T, rig *coordRig) (func() context.Context, func()) {
		store := &silentStore{PendingStore: rig.coordinator.pending}
		rig.coordinator.pending = store
		rig.runner.CollectFunc = nil
		silenced := false
		return func() context.Context {
				if !silenced {
					store.silence(calls...)
					silenced = true
				}
				return t.Context()
			}, func() {
				store.answer()
			}
	}
}

// stoppedAtTheDrain has this node cancel each collection after the first blip
// at its drain, once its box has answered the read.
func stoppedAtTheDrain(t *testing.T, rig *coordRig) (func() context.Context, func()) {
	store := &cancelledAtTheDrain{PendingStore: rig.coordinator.pending}
	rig.coordinator.pending = store
	return func() context.Context { return store.stopping(t) }, store.answering
}

// stoppedAtThePushRead has this node cancel each collection after the first
// blip at its read of what the run pushed, which follows the box's read inside
// the collection's own read: a drain, a restart or the seat moving away, as
// the push read takes it ([Coordinator.readPushes]).
func stoppedAtThePushRead(t *testing.T, rig *coordRig) (func() context.Context, func()) {
	return func() context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			reading := make(chan struct{})
			rig.pushes.mu.Lock()
			rig.pushes.held = reading
			rig.pushes.mu.Unlock()
			go func() {
				select {
				case <-reading:
					cancel()
				case <-ctx.Done():
				}
			}()
			return ctx
		}, func() {
			rig.pushes.mu.Lock()
			defer rig.pushes.mu.Unlock()
			rig.pushes.held = nil
		}
}

// A STEP BEFORE THE BOX WAS ASKED LEAVES THE RUN OF FAILURES AS IT STANDS. The
// host's read of the run's row licenses a read of its box, and a host whose
// store did not answer it never asked the box at all: the claim is handed back
// uncounted, and the run of failures behind it neither grows nor ends.
//
// Mutation: end the run of failures for every collection the store's silence
// hands back, and this one ends a run the box never answered.
func TestAStepBeforeTheBoxWasAskedLeavesTheRunOfFailuresAsItStands(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	store := &silentStore{PendingStore: rig.pending}
	rig.onNode(t, "node-b", hostAnswers{host: rig.hostOn(t, "node-a", store)}, nil)
	rig.launchedBy("t1", "node-a")
	rig.runner.Finish(wholeAccount())

	rig.runner.CollectErr = errBoxBlip
	if err := collectAgain(t, rig); err == nil || errors.Is(err, ErrStoreSilent) {
		t.Fatalf("the box's failed read = %v; want it handed back as the box's", err)
	}
	rig.runner.CollectErr = nil
	before := rig.get("t1").Launch
	if before.CollectFailures != 1 {
		t.Fatalf("the premise: one blip counted %d failures", before.CollectFailures)
	}

	store.silence("Get")
	rig.now = rig.now.Add(ConnectGiveUp)
	if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
		t.Fatalf("a host whose store did not answer its row read = %v", err)
	}
	if got := rig.get("t1").Launch; got.CollectFailures != before.CollectFailures ||
		!got.CollectFailingSince.Equal(before.CollectFailingSince) {
		t.Fatalf("a collection that never asked the box left %d failures since %v; want %d since %v as they "+
			"stood", got.CollectFailures, got.CollectFailingSince, before.CollectFailures, before.CollectFailingSince)
	}
}

// outlastedDrain is a run store whose watch of a run's log answers only once
// its asker's context has ended, as the store answers a request its caller's
// deadline cut.
type outlastedDrain struct{ PendingStore }

func (*outlastedDrain) WatchLog(ctx context.Context, turnID string) (<-chan LogView, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("sandbox: watch the log of run %s: %w", turnID, ctx.Err())
}

// A STEP AFTER THE READ THAT THE DELIVERY'S DEADLINE CUT IS COUNTED ONTO THE
// RUN OF FAILURES BEHIND IT. It is counted, as a read its deadline cut is, or a
// collection that always outlasts its delivery would be asked again for ever;
// and it continues the run rather than starting one of its own, though the box
// answered its read, or a collection that always outlasted its delivery after
// the read would start that run again each time and never be given up.
//
// Mutation: hand the counted release back with the box's answer on it, and
// the store refuses it as a release that both counts a failure and ends the
// run it counts onto.
func TestADeadlineAfterTheReadIsCountedOntoTheRunOfFailures(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(wholeAccount())

	rig.runner.CollectErr = errBoxBlip
	if err := collectAgain(t, rig); !errors.Is(err, errBoxBlip) {
		t.Fatalf("the blip = %v", err)
	}
	rig.runner.CollectErr = nil
	first := rig.get("t1").Launch.CollectFailingSince

	rig.coordinator.pending = &outlastedDrain{PendingStore: rig.coordinator.pending}
	rig.now = rig.now.Add(ConnectGiveUp / 2)
	short, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(short, payload, ev); err == nil || errors.Is(err, ErrStoreSilent) {
		t.Fatalf("OnCompleted = %v; want the deadline's failure handed back, never the store's silence", err)
	}
	row := rig.get("t1")
	if row.Status != StatusRunning || row.Launch.CollectFailures != 2 || !row.Launch.CollectFailingSince.Equal(first) {
		t.Fatalf("the run is %s with %d failed collections since %v after a deadline cut its drain; want it "+
			"handed back with 2 since %v", row.Status, row.Launch.CollectFailures, row.Launch.CollectFailingSince,
			first)
	}
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("announced %+v", failed)
	}
}
