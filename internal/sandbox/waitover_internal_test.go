package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
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

// A CARRYING ON REFUSED ON A CONDITION IS WATCHED AGAIN. A resume refused on
// a condition that ends by waiting (a model at its limit, the seat's budget
// held) gives the claim back to waiting, and the seat's watch is told the run
// waits again (Parked, which internal/engine's runParked starts the watch
// from): the watch ends at a read finding nothing waiting, which a read while
// the claim stood was, and nothing else would wake the run. Its next wake
// carries it on.
//
// Mutation: drop the re-arm from carryOnAdmitted, and the refused run is never
// told back to its watch.
func TestACarryingOnRefusedOnAConditionIsWatchedAgain(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]func(now time.Time) error{
		"a model at its limit": func(now time.Time) error {
			return &TurnRefused{Cause: errors.New("llm chain exhausted: usage limit"),
				Refusal: Refusal{Condition: conditionLimit, Reason: "provider limit: default",
					Until: now.Add(time.Hour)}}
		},
		"the seat's budget held": func(now time.Time) error { return budgetHeld(now.Add(time.Hour)) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var (
				mu     sync.Mutex
				parked []PendingRun
			)
			rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
				o.Parked = func(_ context.Context, run PendingRun) {
					mu.Lock()
					defer mu.Unlock()
					parked = append(parked, run)
				}
			})
			told := func() []PendingRun {
				mu.Lock()
				defer mu.Unlock()
				return slices.Clone(parked)
			}
			parkOnItsCalls(t, rig)
			before := len(told())
			rig.resumer.failWith(refusal(rig.now))
			if err := rig.coordinator.OnEvent(t.Context(), wakeOf(t, rig)); err != nil {
				t.Fatalf("a carrying on refused on a condition handed its wake back: %v", err)
			}
			if got := rig.get("t1"); got.Status != StatusAwaiting || !got.WaitsOnOwnCalls() || got.WaitOver.Taken() {
				t.Fatalf("run %q, own calls %v, claim %+v; want it waiting again, its claim untaken", got.Status,
					got.WaitsOnOwnCalls(), got.WaitOver)
			}
			again := told()[before:]
			if len(again) != 1 || again[0].TurnID != "t1" || !again[0].EngineAnswers() ||
				!slices.Contains(Awaiting, again[0].Status) {
				t.Fatalf("the watch was told %+v; want t1 once, waiting on what the engine answers", again)
			}
			rig.resumer.failWith(nil)
			if err := rig.coordinator.OnEvent(t.Context(), wakeOf(t, rig)); err != nil {
				t.Fatalf("OnEvent: %v", err)
			}
			if calls := rig.resumer.calls(); len(calls) != 1 || !askedOf(calls[0]).CallsReturned {
				t.Fatalf("resumed %+v; want the run carried on once its watch woke it again", calls)
			}
		})
	}
}
