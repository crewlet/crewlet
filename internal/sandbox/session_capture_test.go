package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// WITH THE SWITCH OFF, A LOST RUN'S BOX ON ANOTHER HOST IS NOT ASKED FOR ITS
// SESSION: the take would keep nothing, and a silent host's read holds the
// seat's mail for the minutes it costs. The turn's end is still recorded.
//
// Mutation: gate the loss's read on the wiring alone, and the host is asked.
func TestTheSwitchOffAsksNoHostForALostRunsSession(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	keep.policy.Keep = false
	asks := &boxAsks{result: Result{}}
	rig.onNode(t, "node-b", asks, &nudges{})
	run := rig.launchedBy("t1", "node-a")
	rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
	rig.coordinator.finish(t.Context(), rig.get("t1"), ending{fence: fenceOf(run),
		note: &failureNote{reason: "engine_restarted", detail: "the engine that ran the job restarted"}})
	rig.fireRetries()
	rig.awaitKeeps()
	rig.fireRetries()
	if got := asks.reads(); slices.Contains(got, BoxReadLoss) {
		t.Fatalf("asked the host %v with the switch off, want no read for a session nothing keeps", got)
	}
	if ends := writes.endedRecords(); len(ends) != 1 {
		t.Fatalf("recorded %d ends with the switch off, want the lost turn's one", len(ends))
	}
}

// A SPOOL A CRASH LEFT BEHIND IS SWEPT BY THE FIRST TAKE AFTER IT AGES, and by
// every take after — a younger one, a keep under way, never. Swept once a
// process, a node restarted soon after a crash found that crash's spools young
// at its first take and never swept again.
//
// Mutation: sweep once a coordinator, sweep a young spool, or sweep another
// directory than the coordinator's own, and a check below goes red.
func TestASpoolACrashLeftIsSweptByTheTakesAfterItAges(t *testing.T) {
	t.Parallel()
	rig, _, _ := keepingRig(t)
	plant := func(name string, age time.Duration) string {
		file := filepath.Join(rig.coordinator.spoolDirectory(), spoolPrefix+name)
		if err := os.WriteFile(file, []byte("sealed"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(file, at, at); err != nil {
			t.Fatal(err)
		}
		return file
	}
	take := func(turnID string) {
		rig.launchKept(t, turnID)
		session := rig.runner.Session
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		collectHeld(t, rig, turnID)
	}
	crashed := plant("crashed", 3*SessionCaptureBudget)
	young := plant("under-way", time.Minute)
	take("t1")
	if _, err := os.Stat(crashed); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a spool older than a capture can take was left by the take after it (%v)", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("a spool younger than a capture can take was swept: %v", err)
	}
	later := plant("crashed-later", 3*SessionCaptureBudget)
	take("t2")
	if _, err := os.Stat(later); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a spool aged past the cutoff was left by a later take (%v)", err)
	}
}

// A SPOOL NEVER OUTLIVES ITS CAPTURE: it goes once the keep is done (the
// collection's case), when a node that stopped drops the keep, and when the
// seal into it failed.
//
// Mutation: drop the removal from the drop or from the spool's failure, and a
// spool is left on the node's disk below.
func TestASpoolNeverOutlivesItsCapture(t *testing.T) {
	t.Parallel()
	t.Run("a keep a stopped node drops", func(t *testing.T) {
		t.Parallel()
		rig, _, _ := keepingRig(t)
		run := rig.launchKept(t, "t1")
		spooled, kept := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID),
			rig.runner, readCapture(BoxReadCollect, Result{}, false))
		if spooled == nil {
			t.Fatalf("the take spooled nothing: %+v", kept)
		}
		rig.coordinator.Stop()
		rig.coordinator.keepLater(spooled)
		if left := rig.spools(t); len(left) != 0 {
			t.Errorf("a keep the stopped node dropped left its spool: %v", left)
		}
		if c := rig.get("t1").Capture; c == nil || c.State != CaptureRefused {
			t.Errorf("the dropped keep's capture stands %+v, want it refused", c)
		}
	})
	t.Run("a seal that failed", func(t *testing.T) {
		t.Parallel()
		rig, _, keep := keepingRig(t)
		keep.sealErr = errors.New("the keyring would not seal")
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		collectHeld(t, rig, "t1")
		if left := rig.spools(t); len(left) != 0 {
			t.Errorf("a spool whose seal failed was left: %v", left)
		}
		if c := rig.get("t1").Capture; c == nil || c.State != CaptureRefused {
			t.Errorf("the capture whose seal failed stands %+v, want it refused", c)
		}
	})
}
