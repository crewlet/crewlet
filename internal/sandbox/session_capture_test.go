package sandbox

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
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

// A JOB THAT CONTINUES IN ITS TURN'S BOX IS SCRUBBED OF WHAT THE JOBS BEFORE IT
// WERE HANDED. The relaunch runs no setup and resumes the session the box holds,
// which holds the earlier job's lines, so its record seals the earlier job's
// secrets beside its own: a token rotated while the box waited, handed only to
// the first job, is in the continued session and in the box, and no capture of
// the relaunch was scrubbed of it. And a box continued whose record says nothing
// of what the jobs before it were handed keeps no capture at all, rather than
// one scrubbed of less than the box holds.
//
// Mutation: seal only the relaunch's own secrets, or seal them alone where the
// earlier jobs' are unknown, and a check below goes red.
func TestAJobContinuingInItsTurnsBoxScrubsWhatTheJobsBeforeItWereHanded(t *testing.T) {
	t.Parallel()
	for name, earlier := range map[string]bool{
		"the earlier job's secrets on the record": true,
		"none on the record":                      false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig, _, keep := keepingRig(t)
			run := rig.launch("t1")
			if earlier {
				rig.handOver(t, "t1", HandedSecrets{Values: []string{"the-first-jobs-rotated-token"}})
			}
			var relaunch LaunchResult
			rig.resumer.during = func(ctx context.Context, r PendingRun) {
				req := launchReq(r.TurnID)
				req.Secrets = []string{"the-relaunchs-own-token"}
				res, err := rig.coordinator.Launch(ctx, rig.manager, req)
				if err != nil {
					t.Errorf("relaunch: %v", err)
				}
				relaunch = res
			}
			rig.runner.Finish(Result{Success: true, Text: "first pass"})
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
			rig.awaitKeeps()
			if !relaunch.Reused || relaunch.SandboxID != run.SandboxID {
				t.Fatalf("relaunch = %+v, want it continued in the turn's box %q", relaunch, run.SandboxID)
			}
			sealed := rig.get("t1").Handed
			if !earlier {
				if sealed != "" {
					t.Fatalf("sealed %q for a box whose earlier jobs' secrets are unknown, want nothing", sealed)
				}
				return
			}
			raw, err := keep.OpenHanded("t1", sealed)
			if err != nil {
				t.Fatalf("opening the relaunch's seal: %v", err)
			}
			handed, err := decodeHanded(raw)
			if err != nil {
				t.Fatalf("decoding the relaunch's seal: %v", err)
			}
			for _, want := range []string{"the-first-jobs-rotated-token", "the-relaunchs-own-token"} {
				if !slices.Contains(handed.Values, want) {
					t.Errorf("the relaunch's scrub holds %q, want %q among them", handed.Values, want)
				}
			}
		})
	}
}

// A LOGIN THE BOX'S CODE GREW PAST WHAT A LAUNCH MAY HAND REFUSES THE CAPTURE.
// The box can write its login — its harness refreshes the file there — and every
// string in it was a value of the scrub, held whole in one replacer: a file of
// millions of short strings cost the engine host a gibibyte before a byte of the
// session was read. A login past what a whole launch may hand is no harness's,
// and the capture is refused, saying why.
//
// Mutation: read the login again without its bound, and the session is taken.
func TestALoginTheBoxGrewPastItsBoundRefusesTheCapture(t *testing.T) {
	t.Parallel()
	rig, writes, _ := keepingRig(t)
	run := rig.launchKept(t, "t1")
	const rel = ".claude/.credentials.json"
	rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken},
		Logins: map[string]string{rel: filepath.Join(t.TempDir(), "absent.json")}})
	box := rig.provider.Box(run.SandboxID)
	grown := `["` + strings.Repeat(`abcdefgh","`, MaxHandedBytes/10) + `abcdefgh"]`
	if err := box.WriteFile(t.Context(), path.Join(box.Home(), rel), []byte(grown)); err != nil {
		t.Fatalf("writing the grown login: %v", err)
	}
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
	collectHeld(t, rig, "t1")

	if takes := rig.runner.Takes(); len(takes) != 0 {
		t.Fatalf("took %d sessions scrubbed by a login past its bound", len(takes))
	}
	if len(writes.captured()) != 0 {
		t.Fatal("recorded a capture scrubbed by a login past its bound")
	}
	if c := rig.get("t1").Capture; c == nil || c.State != CaptureRefused ||
		!strings.Contains(c.Reason, "a whole launch may hand") {
		t.Fatalf("the record holds %+v, want the capture refused for the login's bound", c)
	}
}
