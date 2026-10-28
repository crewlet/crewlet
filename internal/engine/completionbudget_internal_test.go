package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// cappedSWEDoc is the drain rig's company with a day's ceiling on its swe
// seat, which the case spends whole before the seat's run finishes.
const cappedSWEDoc = `
name: Nimbus
roles:
  - name: SWE
    handle: swe
    token_budget:
      day: 1000
`

// A COMPLETION THAT ARRIVES ON A FULL WINDOW IS COLLECTED, CHARGED IN THAT
// WINDOW AND HELD, its box paused and left asleep. The job is over and its
// tokens are spent, so the gate asks a completion no budget: the claim
// collects the box, the charge lands in the window the run ran in, the box is
// paused, and the review the full window refuses holds the job on its row,
// which the waiter skips until the window turns over. Refused at the gate
// instead, before its claim, the run was not collected until the refusing
// window had turned over — its tokens charged into the next one — and the
// waiter, finding its row running, woke and kept its box alive at every poll
// for the rest of the window.
//
// Mutation: ask a completion's gate the full window again (seatCannotRun), and
// the run is never collected while the window is full.
func TestACompletionOnAFullWindowIsChargedThereAndHeldAsleep(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	r.doc = cappedSWEDoc
	e := r.start(t)
	agentID, _ := org.DeriveAgentID("Nimbus", "swe")
	scope := coord.AgentScope(agentID.String())
	day := func() coord.WindowUsage {
		t.Helper()
		used, err := e.backends.Fleet.Used(t.Context(), scope, coord.WindowsAt(time.Now(), time.UTC))
		if err != nil {
			t.Fatalf("Used: %v", err)
		}
		return used.In(period.Day)
	}
	// THE WINDOW IS FULL before the run finishes: another of the seat's
	// turns spent it.
	if _, err := e.backends.Fleet.PostCharge(t.Context(), scope, 1000,
		coord.WindowsAt(time.Now(), time.UTC)); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	r.seedRun(t, e, "t1")
	box := r.provider.Box(r.boxID)

	r.runner.Finish(sandbox.Result{Success: true, Text: "fixed the flaky test",
		InputTokens: 70, OutputTokens: 30})
	eventually(t, "the completion to be held on its row for the full window", func() bool {
		return r.run(t, e, "t1").HeldFor == waitBudget
	})
	got := r.run(t, e, "t1")
	if got.Status != sandbox.StatusRunning || !got.Launch.Collected || got.HeldUntil.IsZero() {
		t.Fatalf("run = %q (collected %v, held until %v), want it running, collected and "+
			"held until the window turns over", got.Status, got.Launch.Collected, got.HeldUntil)
	}
	if n := r.collected.Load(); n != 1 {
		t.Fatalf("the box was collected %d times, want once", n)
	}
	if used := day().Used; used != 1100 {
		t.Fatalf("the seat's day holds %d tokens, want the run's 100 on the full window's 1000", used)
	}
	if !box.Paused() {
		t.Fatal("the collected box was not paused")
	}

	// THE WAITER LEAVES IT: twenty polls later nothing was collected again,
	// and the box was neither woken nor kept alive.
	keepalives := box.Keepalives()
	time.Sleep(time.Second)
	if n := r.collected.Load(); n != 1 {
		t.Errorf("the held job was collected %d times while its window was full, want once", n)
	}
	if !box.Paused() || box.Keepalives() != keepalives {
		t.Errorf("the held job's box: paused %v, kept alive %d more times, want it left asleep",
			box.Paused(), box.Keepalives()-keepalives)
	}
}
