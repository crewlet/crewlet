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
// are collected and ended with no turn, each read holding one of the node's
// collections as an admitted completion's does, so they wait for the admitted
// ones rather than read beside them, and every run is collected once.
//
// Mutation: hold no collection for a read made on the node, or give it back
// before the read, and more reads than turns run at once.
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

// A READ THAT CANNOT START HANDS ITS CLAIM BACK UNCOUNTED: a completion,
// admitted or past its deadline, whose delivery ends while every collection of
// its node is taken reads nothing, and its claim goes back with no failure
// counted on the job's record, since the box was never asked. The delivery
// comes back and the run is collected once a collection frees.
//
// Mutation: count the read that never started as the box's failure, and the
// job's record carries a failure its box never had.
func TestAReadThatCannotStartHandsItsClaimBackUncounted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		deadline bool
	}{
		{"admitted", false},
		{"at its deadline", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				rig := bubbleRig(t)
				rig.coordinator.collections = make(chan struct{}, 1)
				taken, err := rig.coordinator.collectionSlot(t.Context(), "elsewhere")
				if err != nil {
					t.Fatalf("taking the node's one collection: %v", err)
				}
				held := holdCollections(rig)
				if tc.deadline {
					gated(rig).refuse(true)
					rig.launchUntil("t1", time.Now().Add(-time.Minute))
				} else {
					rig.launch("t1")
				}
				rig.coordinator.countRun("swe", StatusRunning)
				rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
				payload, ev := rig.completion("t1")

				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				err = rig.coordinator.OnCompleted(ctx, payload, ev)
				if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errReadUnstarted) {
					t.Fatalf("the completion whose delivery ended waiting = %v, want the delivery's own "+
						"error on a read that never started", err)
				}
				got := rig.get("t1")
				if got.Status != StatusRunning || got.Launch.CollectFailures != 0 {
					t.Fatalf("the run is %q with %d collection failures once its read could not start, "+
						"want it running with none counted", got.Status, got.Launch.CollectFailures)
				}
				if reads := held.done.Load() + held.in.Load(); reads != 0 {
					t.Fatalf("the box was read %d times by a read that never started", reads)
				}

				taken()
				go held.letOneGo()
				if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
					t.Fatalf("the completion delivered again: %v", err)
				}
				if tc.deadline {
					if !rig.gone("t1") || !rig.endedWith("t1", types.ExecutorFailureDeadline) {
						t.Fatalf("the run is not ended at its deadline once a collection freed: failures %+v",
							rig.failures())
					}
				} else if resumed := rig.resumer.calls(); len(resumed) != 1 {
					t.Fatalf("the turn resumed %d times once a collection freed, want once", len(resumed))
				}
			})
		})
	}
}

// A LOST JOB'S READ THAT NEVER STARTED IS HANDED BACK, NOT LEFT BEHIND: past
// the give-up a lost job's round is carried on without its read, but a read
// that never started says nothing of the box, so the claim goes back for the
// read to be asked again rather than the round going on unread.
//
// Mutation: judge a read that never started as given up, and the round is
// carried on with what the job did never read.
func TestALostJobsReadThatNeverStartedIsHandedBackNotLeftBehind(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := newCoordRig(t)
		rig.runner.CollectErr = errors.New("connection reset")
		if err := loseAndClaim(t, rig, Result{InputTokens: 500}); err == nil {
			t.Fatal("the premise: a failed read of the lost job's box was not handed back")
		}
		rig.now = rig.now.Add(ConnectGiveUp)
		if got := rig.get("t1").Launch; got.CollectFailures+1 < MinConnectFailures {
			t.Fatalf("the premise: %d failures counted, so the next would not be given up", got.CollectFailures)
		}
		rig.runner.CollectErr = nil
		rig.coordinator.collections = make(chan struct{}, 1)
		taken, err := rig.coordinator.collectionSlot(t.Context(), "elsewhere")
		if err != nil {
			t.Fatalf("taking the node's one collection: %v", err)
		}
		defer taken()

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(ctx, payload, ev); !errors.Is(err, errReadUnstarted) {
			t.Fatalf("the lost job's completion whose read never started = %v, want it handed back", err)
		}
		if requests := rig.resumer.calls(); len(requests) != 0 {
			t.Fatalf("the round was carried on without a read that never started: %+v", requests)
		}
		if got := rig.get("t1"); got.Status != StatusRunning || got.Lost == nil || got.Launch.CollectFailures != 1 {
			t.Fatalf("the run is %q with loss %+v and %d failures, want it handed back running, its verdict and "+
				"its one failure kept", got.Status, got.Lost, got.Launch.CollectFailures)
		}
	})
}

// A STOP'S READ OF ITS BOX HOLDS ONE OF THE NODE'S COLLECTIONS: its ending,
// which no gate admits, waits for one with the run and its box kept, and
// finishes once one frees.
//
// Mutation: hold no collection for a read made on the node, and the stop reads
// its box and ends the run while every collection is taken.
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
// Mutation: take no collection in readLostHere, and the loss reads its box and
// reclaims it while every collection is taken.
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
// Mutation: hold the collection from the claim until the completion returns,
// and the run past its deadline waits for the whole resumed turn.
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

// A LOST JOB'S ROUND IS CARRIED ON WITH NO COLLECTION HELD: its read gives its
// collection back as it returns, so a run past its deadline that the node will
// not admit is collected and ended while the carried round's turn still works.
//
// Mutation: hold the collection from the claim until the completion returns,
// and the run past its deadline waits for the whole carried round.
func TestALostJobsCarriedRoundHoldsNoCollection(t *testing.T) {
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
		rig.runner.Lose(Result{Text: "half way through"})
		if fired := rig.tick(); fired != 1 {
			t.Fatalf("the premise: announced %d completions for a lost job, want 1", fired)
		}
		rig.launchUntil("d1", time.Now().Add(-time.Minute))
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})

		var wg sync.WaitGroup
		lost, lostEv := rig.completion("a1")
		wg.Go(func() { _ = rig.coordinator.OnCompleted(t.Context(), lost, lostEv) })
		<-resuming
		if got := rig.resumer.calls(); len(got) != 1 || got[0].Lost == nil {
			t.Fatalf("the premise: resumed %+v, want the lost job's round carried on", got)
		}
		late, lateEv := rig.completion("d1")
		wg.Go(func() { _ = rig.coordinator.OnCompleted(t.Context(), late, lateEv) })
		synctest.Wait()
		if !rig.gone("d1") || !rig.endedWith("d1", types.ExecutorFailureDeadline) {
			t.Errorf("the run past its deadline is not ended while the lost job's round is carried on: "+
				"failures %+v", rig.failures())
		}
		close(resumed)
		wg.Wait()
	})
}

// A COLLECTION HOLDS ITS COLLECTION FOR THE READ ALONE: the wait for its job's
// bridge calls still in flight, which can run for the call's whole bound, reads
// no box, so a run past its deadline that the node will not admit reads its box
// while that wait goes on.
//
// Mutation: hold the collection from the claim until the completion returns,
// and the run past its deadline reads its box only once the call's hour is up.
func TestACollectionHoldsItsCollectionForTheReadAlone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
			o.After, o.Now, o.Collections = nil, time.Now, 1
		})
		rig.now = time.Now()
		rig.coordinator.gate = (&turnsGate{free: 1}).admit
		var mu sync.Mutex
		var readAt []time.Time
		rig.runner.CollectFunc = func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			readAt = append(readAt, time.Now())
			return nil
		}
		rig.launch("a1")
		rig.coordinator.countRun("swe", StatusRunning)
		call := BridgeCall{Name: "delegate", Seq: 1, StartedAt: rig.now, Until: rig.now.Add(time.Hour)}
		beginInFlight(t, rig, "a1", call)
		rig.launchUntil("d1", time.Now().Add(-time.Minute))
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})

		start := time.Now()
		var wg sync.WaitGroup
		admitted, admittedEv := rig.completion("a1")
		wg.Go(func() { _ = rig.coordinator.OnCompleted(t.Context(), admitted, admittedEv) })
		synctest.Wait()
		late, lateEv := rig.completion("d1")
		wg.Go(func() { _ = rig.coordinator.OnCompleted(t.Context(), late, lateEv) })
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		if len(readAt) != 2 {
			t.Fatalf("%d box reads, want the admitted run's and the late one's", len(readAt))
		}
		if !readAt[1].Equal(start) {
			t.Errorf("the run past its deadline read its box %v after the first read, while the admitted "+
				"run waited for its bridge call in flight and read no box", readAt[1].Sub(start))
		}
	})
}

// A READ ASKED OF ANOTHER NODE'S HOST HOLDS NONE OF THIS NODE'S COLLECTIONS:
// its read runs on that host, under that host's own slots, so a collection of
// a box on this node's own host reads while the ask is out.
//
// Mutation: take a collection for every read, wherever its box is, and the
// read here waits for the ask with no box read on this node.
func TestAReadAskedOfAnotherNodesHostHoldsNoCollection(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
			o.After, o.Now, o.Collections = nil, time.Now, 1
		})
		rig.now = time.Now()
		release := make(chan struct{})
		asks := &boxAsks{result: Result{Success: true, Text: "read on node-a's host"},
			during: func() { <-release }}
		rig.onNode(t, "node-b", asks, nil)
		local := holdCollections(rig)
		rig.launchedBy("r1", "node-a")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.launchedBy("l1", "node-b")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "done"})

		var wg sync.WaitGroup
		remote, remoteEv := rig.completion("r1")
		wg.Go(func() { _ = rig.coordinator.OnCompleted(t.Context(), remote, remoteEv) })
		synctest.Wait()
		if got := len(asks.reads()); got != 1 {
			t.Fatalf("the premise: %d reads asked of node-a, want the remote run's", got)
		}
		here, hereEv := rig.completion("l1")
		wg.Go(func() { _ = rig.coordinator.OnCompleted(t.Context(), here, hereEv) })
		synctest.Wait()
		if got := local.in.Load(); got != 1 {
			t.Errorf("%d boxes are being read on this node with a read asked of node-a out, want the "+
				"local run's: the ask holds this node's one collection", got)
		}
		close(release)
		local.letOneGo()
		wg.Wait()
	})
}

// A STOP OF A LAUNCH WITH NO BOX COMMAND WAITS FOR NO COLLECTION: it reads no
// box, and it is finished on the frame that decided it, a pause watch's or a
// seat's acquisition among them, which must not wait for the node's reads.
//
// Mutation: take a collection for a stop's ending before it knows there is a
// box to read, and the stop waits while every collection is taken.
func TestAStopOfALaunchWithNoCommandWaitsForNoCollection(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := newCoordRig(t)
		rig.coordinator.collections = make(chan struct{}, 1)
		taken, err := rig.coordinator.collectionSlot(t.Context(), "elsewhere")
		if err != nil {
			t.Fatalf("taking the node's one collection: %v", err)
		}
		defer taken()
		watchStops(rig)
		if _, err := rig.pending.BeginLaunch(t.Context(), PendingRun{
			TurnID: "t1", AgentHandle: "swe", AgentID: sweID, Role: "SWE",
			Harness: "claude-code", CreatedAt: rig.now,
		}, rigLease); err != nil {
			t.Fatalf("BeginLaunch: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- rig.coordinator.StopSeat(t.Context(), "swe", stopper) }()
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("StopSeat: %v", err)
			}
		default:
			t.Error("the stop of a launch with no box command waits for one of the node's collections")
			taken()
			<-done
		}
	})
}

// A STOP'S READ HOLDS ITS COLLECTION WHILE IT READS: a completion past its
// deadline whose node will not admit it does not read beside it.
//
// Mutation: give the collection back before the read, and two boxes are read
// at once on a node with one collection.
func TestAStopsReadHoldsItsCollectionWhileItReads(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
			o.After, o.Now, o.Collections = nil, time.Now, 1
		})
		rig.now = time.Now()
		rig.coordinator.gate = (&turnsGate{free: 0}).admit
		watchStops(rig)
		held := holdCollections(rig)
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "stopped half way"})
		rig.stop(t)
		var wg sync.WaitGroup
		wg.Go(func() { rig.fireRetries() })
		synctest.Wait()
		if got := held.in.Load(); got != 1 {
			t.Fatalf("the premise: %d stop reads under way, want 1", got)
		}
		rig.launchUntil("d1", time.Now().Add(-time.Minute))
		rig.coordinator.countRun("swe", StatusRunning)
		late, lateEv := rig.completion("d1")
		wg.Go(func() { _ = rig.coordinator.OnCompleted(t.Context(), late, lateEv) })
		synctest.Wait()
		if got := held.in.Load(); got != 1 {
			t.Errorf("%d boxes are being read at once with the node's one collection, want 1", got)
		}
		for held.in.Load() > 0 {
			held.letOneGo()
			synctest.Wait()
		}
		wg.Wait()
		if got := held.done.Load(); got != 2 {
			t.Errorf("%d boxes read, want the stop's and the late completion's", got)
		}
	})
}

// A STOP'S READ THAT CANNOT START KEEPS THE STOP: nothing of the box is known,
// so nothing is recorded and the ending waits for its retry, rather than ending
// the run with nothing charged.
//
// Mutation: read a read that never started as a failed local read, and the
// stop records nothing spent and goes on to end the run.
func TestAStopsReadThatCannotStartKeepsTheStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := newCoordRig(t)
		rig.coordinator.collections = make(chan struct{}, 1)
		taken, err := rig.coordinator.collectionSlot(t.Context(), "elsewhere")
		if err != nil {
			t.Fatalf("taking the node's one collection: %v", err)
		}
		defer taken()
		watchStops(rig)
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "stopped half way", InputTokens: 300})
		rig.stop(t)
		stopped := rig.get("t1")
		if stopped.Stop == nil || stopped.Stop.Collected {
			t.Fatalf("the premise: the stop is %+v, want it decided and not collected", stopped.Stop)
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		if _, err := rig.coordinator.collectStopped(ctx, stopped, true); !errors.Is(err, errReadUnstarted) {
			t.Fatalf("the stop whose read never started = %v, want it kept for its retry", err)
		}
		if got := rig.get("t1"); got.Stop == nil || got.Stop.Collected {
			t.Fatalf("the stop is %+v once its read could not start, want it kept uncollected", got.Stop)
		}
	})
}

// A LOSS'S READ HOLDS ITS COLLECTION WHILE IT READS, as a stop's does.
//
// Mutation: give the collection back before the loss's read, and the node
// reads it with its one collection free.
func TestALossReadHoldsItsCollectionWhileItReads(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		rig.coordinator.collections = make(chan struct{}, 1)
		reading, release := make(chan struct{}), make(chan struct{})
		read := sync.OnceFunc(func() { close(reading) })
		show := sessionShows("take-n1").read
		launched := lostWithServe(t, rig)
		rig.runner.MainThread = func(ctx context.Context, box Sandbox, ask MainThreadAsk) MainThreadReading {
			read()
			<-release
			return show(ctx, box, ask)
		}
		var retried sync.WaitGroup
		retried.Go(func() { rig.fireRetries() })
		<-reading
		if held := len(rig.coordinator.collections); held != 1 {
			t.Errorf("%d of the node's one collection is held while the loss reads its box, want it", held)
		}
		close(release)
		retried.Wait()
		rig.awaitKeeps()
		rig.fireRetries()
		if !slices.Contains(rig.provider.KilledIDs(), launched.SandboxID) {
			t.Fatal("the loss's box was not reclaimed once it was read")
		}
	})
}

// A LOSS'S READ THAT CANNOT START KEEPS ITS ENDING, its box with it, rather than
// reclaiming a box nobody read.
//
// Mutation: read a read that never started as a failed one, and the loss goes
// on to reclaim its box unread.
func TestALossReadThatCannotStartKeepsItsEnding(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		rig.coordinator.collections = make(chan struct{}, 1)
		taken, err := rig.coordinator.collectionSlot(t.Context(), "elsewhere")
		if err != nil {
			t.Fatalf("taking the node's one collection: %v", err)
		}
		defer taken()
		lostWithServe(t, rig)
		ending := rig.get("t1")
		if ending.Ending == nil || !rig.coordinator.capturesLoss(ending) {
			t.Fatalf("the premise: the loss's ending is %+v, want one that reads its box", ending.Ending)
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		if err := rig.coordinator.keepLostSession(ctx, ending, true); !errors.Is(err, errReadUnstarted) {
			t.Fatalf("the loss whose read never started = %v, want its ending kept", err)
		}
	})
}
