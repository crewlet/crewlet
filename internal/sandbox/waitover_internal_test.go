package sandbox

import (
	"testing"
)

// parkOnItsCalls parks t1 on its own call alone: the ask the engine carries
// on once its job is collected.
func parkOnItsCalls(t *testing.T, rig *coordRig) {
	t.Helper()
	rig.launch("t1")
	rig.ask("t1", Ask{Call: &CallWait{Tool: "delegate", Seq: 1}})
	rig.runner.Finish(Result{})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || !got.WaitsOnOwnCalls() {
		t.Fatalf("status = %q, own calls %v; want the run parked on its own call", got.Status,
			got.WaitsOnOwnCalls())
	}
}

// claimedThenStopped has the rig's node claim t1 out of its wait, its turn
// taking the claim where took says, and then stop: what a holder killed
// between the claim and the resume leaves on the row.
func claimedThenStopped(t *testing.T, rig *coordRig, took bool) {
	t.Helper()
	launch := rig.get("t1").LaunchID
	if _, won, err := rig.pending.ClaimWaitOver(t.Context(), "t1", launch, rigLease); err != nil || !won {
		t.Fatalf("the dying node's claim = %v, %v", won, err)
	}
	if took {
		if ok, err := rig.pending.TakeWaitOver(t.Context(), "t1", launch, rigLease); err != nil || !ok {
			t.Fatalf("the dying node's turn taking the claim = %v, %v", ok, err)
		}
	}
	rig.coordinator.Stop()
}

// A HOLDER KILLED BETWEEN THE CLAIM AND THE RESUME LEAVES THE RUN CLAIMABLE, OR
// RESUMED ONCE. The engine carries a run on by claiming it straight out of its
// wait, never by recording an answer. Where the node holding the claim stops
// before its turn takes it, the seat's next holder gives the run back to its
// wait, counted, and carries it on once on the next wake; where the turn took
// it, the run was carried on, and the next holder reaps it rather than resume
// it a second time.
//
// Mutation: drop the wait's revival from reapTail, and a dead untaken claim is
// reaped as abandoned; revive a taken claim too (ignore TakenAt), and a run
// whose turn ran is carried on twice.
func TestAHolderKilledBetweenClaimAndResumeLeavesTheRunClaimableOrResumedOnce(t *testing.T) {
	t.Parallel()
	t.Run("before its turn took it", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		parkOnItsCalls(t, rig)
		claimedThenStopped(t, rig, false)
		next := reaper(t.Context(), t, rig, rig.pending, 2, nil)
		got := rig.get("t1")
		if got.Status != StatusAwaiting || got.OwnerEpoch != 2 || got.Answer != nil || got.WaitOver == nil ||
			got.WaitOver.Taken() || got.WaitOver.LostClaims != 1 {
			t.Fatalf("run %q at epoch %d, answer %+v, claim %+v; want it waiting again under epoch 2, "+
				"no answer on it and the lost claim counted", got.Status, got.OwnerEpoch, got.Answer, got.WaitOver)
		}
		if failed := rig.failures(); len(failed) != 0 {
			t.Fatalf("announced %+v for a run given back to its wait", failed)
		}
		wake, again := wakeOf(t, rig), wakeOf(t, rig)
		if err := next.coordinator.OnEvent(t.Context(), wake); err != nil {
			t.Fatalf("OnEvent: %v", err)
		}
		if err := next.coordinator.OnEvent(t.Context(), again); err != nil {
			t.Fatalf("OnEvent, a second wake: %v", err)
		}
		calls := next.resumer.calls()
		if len(calls) != 1 || !askedOf(calls[0]).CallsReturned || askedOf(calls[0]).By != "" {
			t.Fatalf("the next holder resumed %+v, want the run carried on once, its calls returned", calls)
		}
	})
	t.Run("after its turn took it", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		parkOnItsCalls(t, rig)
		claimedThenStopped(t, rig, true)
		next := reaper(t.Context(), t, rig, rig.pending, 2, nil)
		rig.finished("t1")
		abandonedOnce(t, rig, false)
		if calls := next.resumer.calls(); len(calls) != 0 {
			t.Fatalf("the next holder resumed %+v a run whose turn took its carrying on", calls)
		}
	})
}
