//go:build unix

package sandbox

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// endedJob starts a job in box that exits at once and waits until it reads as
// ended: a box whose collection is done, as a parked one is.
func endedJob(t *testing.T, box Sandbox) {
	t.Helper()
	handle, err := box.StartBackground(t.Context(), "exit 0", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		state, err := box.Job(t.Context(), handle)
		return err == nil && state != JobRunning
	}, "a job that exits at once still reads as running")
}

// THE ORPHAN REAPER TAKES ONLY A BOX THAT RECORDS NO RUN OR CHECK. A box that
// records the run it was made for is its host's sweep's, which asks the run's
// row: a parked run's box has an exited job and a stamp nothing touches, and
// the reaper taking it at the host grace would lose it inside its pause TTL.
// One that records its check's attempt is its node's check runner's, which
// asks the attempt's row. A box that records neither, or one whose record
// cannot be used, is the reaper's, whichever cell's backend makes the next box.
//
// Mutations: drop the record check from the reaper, and the recorded boxes are
// reaped; read a check's record as none, and the check's box is reaped; skip a
// box whatever its record says, or one whose record cannot be read, and the box
// that is nobody's survives.
func TestTheOrphanReaperTakesOnlyABoxThatRecordsNoRun(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		record func(t *testing.T, box Sandbox)
		reaped bool
	}{
		{"records its run", func(*testing.T, Sandbox) {}, false},
		{"records its run in the other cell", func(t *testing.T, box Sandbox) {
			if err := recordRun(layoutOf(t, box), boxRun{TurnID: "t1", Placement: Container}); err != nil {
				t.Fatalf("recordRun: %v", err)
			}
		}, false},
		{"records its check", func(t *testing.T, box Sandbox) {
			if err := recordRun(layoutOf(t, box), boxRun{Check: "attempt-1", Placement: Direct}); err != nil {
				t.Fatalf("recordRun: %v", err)
			}
		}, false},
		{"records both a run and a check", func(t *testing.T, box Sandbox) {
			if err := recordRun(layoutOf(t, box), boxRun{TurnID: "t1", Check: "attempt-1", Placement: Direct}); err != nil {
				t.Fatalf("recordRun: %v", err)
			}
		}, true},
		{"records no run", func(t *testing.T, box Sandbox) {
			if err := os.Remove(layoutOf(t, box).runFile()); err != nil {
				t.Fatalf("remove the record: %v", err)
			}
		}, true},
		{"records a run that cannot be read", func(t *testing.T, box Sandbox) {
			if err := os.WriteFile(layoutOf(t, box).runFile(), []byte("{not json"), 0o600); err != nil {
				t.Fatalf("spoil the record: %v", err)
			}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			local := newDirect(t)
			box, err := local.Create(t.Context(), Spec{TurnID: "t1", Placement: Direct})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			endedJob(t, box)
			c.record(t, box)
			ageBox(t, box, 2*DefaultHostGrace)

			mustCreate(t, local, Spec{})

			_, err = os.Stat(box.Home())
			if reaped := errors.Is(err, os.ErrNotExist); reaped != c.reaped {
				t.Fatalf("a box whose job ended, untouched past the host grace: reaped %v, want %v (stat: %v)",
					reaped, c.reaped, err)
			}
		})
	}
}

// A PARKED BOX PAST THE HOST GRACE IS ITS RUN'S UNTIL ITS ROW LETS IT GO. The
// run parked on a question after its job ended and its box was paused, so its
// stamp ages untouched past the host grace well inside its pause TTL: a launch
// beside it leaves it, the host's sweep keeps it while the row names it, and
// reclaims it through its backend once the row is gone.
//
// Mutations: drop the record check from the reaper, and the launch takes the
// parked box; free a parked row's box in the sweep, and the sweep takes it;
// reclaim nothing in the sweep, and it outlives its run.
func TestAParkedBoxPastTheHostGraceIsItsRunsUntilItsRowLetsItGo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rig := newWaiterRig(t)
	local := newDirect(t)
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: local},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rig.manager = manager
	rig.current.Store(manager)

	box, err := local.Create(ctx, Spec{TurnID: "t-parked", Placement: Direct})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(context.WithoutCancel(ctx)) })
	if _, err := rig.pending.BeginLaunch(ctx, PendingRun{
		TurnID: "t-parked", AgentHandle: "swe", AgentID: sweID, Role: "SWE",
		Harness: "claude-code", TraceID: "tr-1", CreatedAt: rig.now,
	}, rigLease); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := rig.pending.AttachSandbox(ctx, "t-parked", BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", Placement: string(Direct),
		Harness: "claude-code", PauseTTLSec: DefaultPauseTTL.Seconds(),
	}, rigLease); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	rig.suspend("t-parked")
	endedJob(t, box)
	rig.park("t-parked")
	if err := box.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// PAST THE GRACE, INSIDE THE TTL: halfway between the two.
	age := (DefaultHostGrace + DefaultPauseTTL) / 2
	if age <= DefaultHostGrace || age >= DefaultPauseTTL {
		t.Fatalf("no age is past the %v host grace and inside the %v pause TTL", DefaultHostGrace, DefaultPauseTTL)
	}
	ageBox(t, box, age)

	mustCreate(t, local, Spec{})
	if _, err := os.Stat(box.Home()); err != nil {
		t.Fatalf("a launch took the box of a run parked inside its pause TTL: %v", err)
	}
	rig.now = rig.now.Add(age)
	rig.tick()
	if _, err := os.Stat(box.Home()); err != nil {
		t.Fatalf("the host's sweep took the box its parked run's row still names: %v", err)
	}

	decided, _, err := rig.pending.DecideEnding(ctx, "t-parked", Decision{
		License: License{WhileIn: Active, Launch: EveryLaunch}, Reason: "x", Detail: "x",
	})
	if err != nil {
		t.Fatalf("DecideEnding: %v", err)
	}
	if _, _, err := rig.pending.Finish(ctx, "t-parked", decided.Ending.ID); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	rig.tick()
	if _, err := os.Stat(box.Home()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the parked box outlived its run's row: %v", err)
	}
}
