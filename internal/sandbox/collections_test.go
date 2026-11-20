package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// errAtItsTurns is a [turnsGate]'s refusal.
var errAtItsTurns = errors.New("the node runs as many turns as it may")

// turnsGate is a [TurnGate] of a node running free turns at most: it admits a
// resume while one is free and refuses it otherwise, as a node at its ceiling
// whose wait for a slot ended does.
type turnsGate struct {
	mu   sync.Mutex
	free int
}

func (g *turnsGate) admit(ctx context.Context, _ string, _ ResumeRoute) (context.Context, func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.free == 0 {
		return nil, nil, &TurnRefused{Cause: errAtItsTurns, Refusal: Refusal{Reason: errAtItsTurns.Error()}}
	}
	g.free--
	return ctx, sync.OnceFunc(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.free++
	}), nil
}

// heldCollections counts the reads of a box its rig's runner is in, and holds
// each until the case lets one go ([heldCollections.letOneGo]).
type heldCollections struct {
	in, most, done atomic.Int32
	next           chan struct{}
}

// holdCollections makes every Collect of rig's runner one of h's.
func holdCollections(rig *coordRig) *heldCollections {
	h := &heldCollections{next: make(chan struct{})}
	rig.runner.CollectFunc = func(ctx context.Context) error {
		n := h.in.Add(1)
		defer h.in.Add(-1)
		for most := h.most.Load(); n > most; most = h.most.Load() {
			if h.most.CompareAndSwap(most, n) {
				break
			}
		}
		select {
		case <-h.next:
		case <-ctx.Done():
			return ctx.Err()
		}
		h.done.Add(1)
		return nil
	}
	return h
}

// letOneGo ends one held read, waiting for one to take it.
func (h *heldCollections) letOneGo() { h.next <- struct{}{} }

// A NODE AT ITS TURNS COLLECTS NO MORE RUNS AT ONCE THAN IT RUNS TURNS, AND
// LOSES NONE: completions past their turns' deadlines, which its gate refuses,
// are collected and ended with no turn, each holding one of the node's
// collections as an admitted completion does, so they wait for the admitted
// ones rather than read beside them, and every run is collected once.
//
// Mutation: take no collection in completeAtDeadline, or in completeAdmitted,
// and more reads than turns run at once.
func TestANodeAtItsTurnsCollectsNoMoreRunsAtOnceThanItsTurns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const turns = 2
		rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
			o.After, o.Now, o.Collections = nil, time.Now, turns
		})
		rig.now = time.Now()
		rig.coordinator.gate = (&turnsGate{free: turns}).admit
		held := holdCollections(rig)
		admitted, late := []string{"a1", "a2"}, []string{"d1", "d2", "d3"}
		for _, id := range admitted {
			rig.launch(id)
			rig.coordinator.countRun("swe", StatusRunning)
		}
		for _, id := range late {
			rig.launchUntil(id, time.Now().Add(-time.Minute))
			rig.coordinator.countRun("swe", StatusRunning)
		}
		rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})

		var wg sync.WaitGroup
		outcomes := make(map[string]error)
		var mu sync.Mutex
		complete := func(id string) {
			payload, ev := rig.completion(id)
			wg.Go(func() {
				err := rig.coordinator.OnCompleted(t.Context(), payload, ev)
				mu.Lock()
				defer mu.Unlock()
				outcomes[id] = err
			})
		}
		for _, id := range admitted {
			complete(id)
		}
		synctest.Wait()
		for _, id := range late {
			complete(id)
		}
		synctest.Wait()
		if got := held.in.Load(); got != turns {
			t.Fatalf("%d boxes are being read with the node at its %d turns and %d runs past their deadlines; "+
				"want %d, the rest waiting", got, turns, len(late), turns)
		}
		for range len(admitted) + len(late) {
			held.letOneGo()
			synctest.Wait()
		}
		wg.Wait()

		if most := held.most.Load(); most != turns {
			t.Errorf("at most %d boxes were read at once, want %d, the node's turns", most, turns)
		}
		if got := held.done.Load(); got != int32(len(admitted)+len(late)) {
			t.Errorf("%d runs were collected, want every one of %d", got, len(admitted)+len(late))
		}
		for id, err := range outcomes {
			if err != nil {
				t.Errorf("the completion of %s = %v, want it settled", id, err)
			}
		}
		if resumed := rig.resumer.calls(); len(resumed) != len(admitted) {
			t.Errorf("%d turns resumed, want the %d the node admitted", len(resumed), len(admitted))
		}
		for _, id := range late {
			if !rig.gone(id) || !rig.endedWith(id, types.ExecutorFailureDeadline) {
				t.Errorf("%s is not ended at its deadline: gone %v, failures %+v", id, rig.gone(id), rig.failures())
			}
		}
	})
}

// A COLLECTION THAT CANNOT START LOSES NOTHING: a completion past its deadline
// whose delivery ends while every collection of its node is taken leaves its
// run unclaimed and its box unread, with the delivery's own error, so it comes
// back and is ended at its deadline once one frees.
//
// Mutation: take the collection after the claim, and the run is left claimed
// by a read that never started.
func TestACollectionThatCannotStartLeavesItsRunUnclaimed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := bubbleRig(t)
		rig.coordinator.collections = make(chan struct{}, 1)
		taken, err := rig.coordinator.collectionSlot(t.Context(), "elsewhere")
		if err != nil {
			t.Fatalf("taking the node's one collection: %v", err)
		}
		gate := gated(rig)
		gate.refuse(true)
		held := holdCollections(rig)
		rig.launchUntil("d1", time.Now().Add(-time.Minute))
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
		payload, ev := rig.completion("d1")

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		err = rig.coordinator.OnCompleted(ctx, payload, ev)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the completion whose delivery ended waiting = %v, want the delivery's own error", err)
		}
		if got := rig.get("d1"); got.Status != StatusRunning {
			t.Fatalf("the run is %q once its collection could not start, want it running and unclaimed",
				got.Status)
		}
		if got := held.done.Load() + held.in.Load(); got != 0 {
			t.Fatalf("the box was read %d times by a collection that could not start", got)
		}

		taken()
		go held.letOneGo()
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("the completion delivered again: %v", err)
		}
		if !rig.gone("d1") || !rig.endedWith("d1", types.ExecutorFailureDeadline) {
			t.Fatalf("the run is not ended at its deadline once a collection freed: failures %+v", rig.failures())
		}
	})
}

// A STOP'S READ OF ITS BOX HOLDS ONE OF THE NODE'S COLLECTIONS: its ending,
// which no gate admits, waits for one with the run and its box kept, and
// finishes once one frees.
//
// Mutation: take no collection in collectStopped, and the stop reads its box
// and ends the run while every collection is taken.
func TestAStopsReadWaitsForOneOfTheNodesCollections(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := newCoordRig(t)
		rig.coordinator.collections = make(chan struct{}, 1)
		taken, err := rig.coordinator.collectionSlot(t.Context(), "elsewhere")
		if err != nil {
			t.Fatalf("taking the node's one collection: %v", err)
		}
		watchStops(rig)
		running := rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "stopped half way"})

		rig.stop(t)
		var retried sync.WaitGroup
		retried.Go(func() { rig.fireRetries() })
		synctest.Wait()
		if _, found, _ := rig.pending.Get(t.Context(), "t1"); !found {
			t.Fatal("the stop ended the run while every collection of its node was taken")
		}
		if slices.Contains(rig.provider.KilledIDs(), running.SandboxID) {
			t.Fatal("the stop reclaimed the box it has not read")
		}

		taken()
		retried.Wait()
		if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
			t.Fatal("the stop is still kept once a collection freed")
		}
	})
}

// A LOSS'S READ OF ITS BOX HOLDS ONE OF THE NODE'S COLLECTIONS: its ending
// waits for one with the box kept, and reads and reclaims it once one frees.
//
// Mutation: take no collection in keepLostSession, and the loss reads its box
// and reclaims it while every collection is taken.
func TestALossReadWaitsForOneOfTheNodesCollections(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		rig.coordinator.collections = make(chan struct{}, 1)
		taken, err := rig.coordinator.collectionSlot(t.Context(), "elsewhere")
		if err != nil {
			t.Fatalf("taking the node's one collection: %v", err)
		}
		launched := lostWithServe(t, rig)
		var retried sync.WaitGroup
		retried.Go(func() { rig.fireRetries() })
		synctest.Wait()
		if slices.Contains(rig.provider.KilledIDs(), launched.SandboxID) {
			t.Fatal("the loss reclaimed its box while every collection of its node was taken")
		}
		if _, found, _ := rig.pending.Get(t.Context(), "t1"); !found {
			t.Fatal("the loss's ending finished while every collection of its node was taken")
		}

		taken()
		retried.Wait()
		rig.awaitKeeps()
		rig.fireRetries()
		if !slices.Contains(rig.provider.KilledIDs(), launched.SandboxID) {
			t.Fatal("the loss's box was not reclaimed once a collection freed")
		}
	})
}

// AN ADMITTED COLLECTION GIVES ITS COLLECTION BACK BEFORE ITS RESUME: a turn
// can run for an hour, and a run past its deadline that the node will not
// admit is collected and ended while the turn before it still works.
//
// Mutation: give the collection back only as the completion returns, and the
// run past its deadline waits for the whole resumed turn.
func TestAnAdmittedCollectionIsGivenBackBeforeItsResume(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := bubbleRig(t)
		rig.coordinator.collections = make(chan struct{}, 1)
		rig.coordinator.gate = (&turnsGate{free: 1}).admit
		resuming, resumed := make(chan struct{}), make(chan struct{})
		rig.resumer.during = func(context.Context, PendingRun) {
			close(resuming)
			<-resumed
		}
		rig.launch("a1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.launchUntil("d1", time.Now().Add(-time.Minute))
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})

		var wg sync.WaitGroup
		admitted, admittedEv := rig.completion("a1")
		wg.Go(func() { _ = rig.coordinator.OnCompleted(t.Context(), admitted, admittedEv) })
		<-resuming
		late, lateEv := rig.completion("d1")
		wg.Go(func() { _ = rig.coordinator.OnCompleted(t.Context(), late, lateEv) })
		synctest.Wait()
		if !rig.gone("d1") || !rig.endedWith("d1", types.ExecutorFailureDeadline) {
			t.Errorf("the run past its deadline is not ended while the admitted turn resumes: failures %+v",
				rig.failures())
		}
		close(resumed)
		wg.Wait()
	})
}
