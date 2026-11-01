package sandbox

import (
	"context"
	"slices"
	"sync"
	"testing"
)

// A RUN STILL WORKING IN ITS BOX IS HANDED TO THE NODE THAT RECOVERS ITS SEAT,
// so what its box calls on the engine is served there again: a run's
// tool bridge lived in the process that launched it, and a restart or a seat
// move left the box calling a session nobody held. A run parked on a question
// has a paused box that calls nothing, so it is not handed over; nor is a run
// a newer lease already owns, which is that lease's to serve.
//
// Mutation: drop the hook from the running case, and the recovered node serves
// nothing the box calls; call it whatever the claim answered, and a node behind
// a newer lease serves a run it no longer holds.
func TestARunStillWorkingIsHandedToTheNodeThatRecoversItsSeat(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	working := rig.launch("t1")
	rig.launch("t2")
	rig.park("t2")
	var mu sync.Mutex
	var handed []PendingRun
	rig.coordinator.recovered = func(_ context.Context, run PendingRun) {
		mu.Lock()
		defer mu.Unlock()
		handed = append(handed, run)
	}

	if err := rig.recoverSeat(t.Context(), "node-b:1", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	mu.Lock()
	got := slices.Clone(handed)
	mu.Unlock()
	if len(got) != 1 || got[0].TurnID != "t1" || got[0].LaunchID != working.LaunchID {
		t.Fatalf("handed over %+v, want the one run still working, under its own job", got)
	}

	// A NODE BEHIND A NEWER LEASE: the row is the newer holder's.
	stale := rig.withResumer(t, &resumeSpy{})
	stale.recovered = func(_ context.Context, run PendingRun) {
		t.Errorf("a node whose lease is older than the row's was handed %s", run.TurnID)
	}
	if err := rig.recoverSeat(t.Context(), "node-c:1", 3); err != nil {
		t.Fatalf("RecoverSeat at the newer lease: %v", err)
	}
	if err := stale.RecoverSeat(t.Context(), "swe", "node-b:1", 2); err != nil {
		t.Fatalf("RecoverSeat at the older lease: %v", err)
	}
}
