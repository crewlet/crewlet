package sandbox

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// timedStamps is a run store that records the deadline every stamp of a
// capture is asked under, by the state it stamps; and, where late is set,
// applies a place's reservation and answers it only once its caller has given
// up — a coordination store that took the write and whose answer came too late
// — stamping it, where steal is set, as another read racing this one reserved
// the same place: at another instant.
type timedStamps struct {
	PendingStore
	late, steal bool

	mu        sync.Mutex
	deadlines map[CaptureState][]time.Time
}

func (s *timedStamps) StampCapture(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	deadline, _ := ctx.Deadline()
	s.mu.Lock()
	if s.deadlines == nil {
		s.deadlines = map[CaptureState][]time.Time{}
	}
	s.deadlines[stamp.State] = append(s.deadlines[stamp.State], deadline)
	s.mu.Unlock()
	if !s.late || stamp.State != CaptureBegun {
		return s.PendingStore.StampCapture(ctx, turnID, stamp)
	}
	applied := stamp
	if s.steal {
		applied.At = applied.At.Add(-time.Second)
	}
	if _, err := s.PendingStore.StampCapture(context.WithoutCancel(ctx), turnID, applied); err != nil {
		return false, err
	}
	<-ctx.Done()
	return false, ctx.Err()
}

// askedUnder is every deadline a stamp of state was asked under.
func (s *timedStamps) askedUnder(state CaptureState) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.deadlines[state])
}

// readUntil is a read of a box whose deadline leaves its take two seconds — its
// own deadline, T — and, after that, the room the read keeps for the take's
// outcome stamp, up to by, and for its box's pause ([Coordinator.takeContext]).
func readUntil(t *testing.T) (read context.Context, by time.Time) {
	t.Helper()
	read, cancel := context.WithTimeout(t.Context(), 2*discardGrace+2*time.Second)
	t.Cleanup(cancel)
	deadline, _ := read.Deadline()
	return read, deadline.Add(-discardGrace)
}

// A CAPTURE'S OUTCOME IS STAMPED IN THE ROOM THE READ LEAVES IT. A take whose
// place was reserved and whose spool then failed is stamped refused under a
// context of its own that ends at the read's by — never at the take's own
// deadline, which may be what ended the spool and would leave the stamp no
// time, and never past by, where the read needs the rest of its time for the
// box's pause.
//
// Mutation: bound the refusal's stamp by the take's own deadline, or by the
// record's budget alone, and its deadline is not the read's by.
func TestACapturesOutcomeIsStampedInTheRoomTheReadLeavesIt(t *testing.T) {
	t.Parallel()
	rig, _, keep := keepingRig(t)
	keep.sealErr = errors.New("the keyring would not seal")
	run := rig.launchKept(t, "t1")
	stamps := &timedStamps{PendingStore: rig.coordinator.pending}
	rig.coordinator.pending, rig.coordinator.recordBudget = stamps, 10*time.Minute
	read, by := readUntil(t)
	spooled, _ := rig.coordinator.takeCapture(read, run, rig.provider.Box(run.SandboxID), rig.runner,
		readCapture(BoxReadCollect, Result{}, false))
	if spooled != nil {
		t.Fatalf("a take whose seal failed spooled %+v", spooled)
	}
	refused := stamps.askedUnder(CaptureRefused)
	if len(refused) == 0 {
		t.Fatal("the take's refusal was never stamped")
	}
	for _, deadline := range refused {
		if !deadline.Equal(by) {
			t.Errorf("the refusal was stamped under a deadline %s off the read's by, want it at by",
				deadline.Sub(by))
		}
	}
	if c := rig.get("t1").Capture; c == nil || c.State != CaptureRefused {
		t.Fatalf("the capture whose seal failed stands %+v, want it refused", c)
	}
}

// A PLACE RESERVED AS THE TAKE'S TIME RAN OUT IS SETTLED. The coordination store
// took the reservation of the take's place and answered after the take's own
// deadline, so the take never spooled — and the place stood begun on the run's
// row, read as a keep under way for as long as a capture can take, the turn's
// end waiting on it, with nobody left to settle it. The take reads the row again
// in the room the read leaves its outcome stamp and stamps its own place
// refused; a place another read reserved in its stead is left as it stands.
//
// Mutation: leave a reservation that ended unanswered as it is, or settle the
// place whoever reserved it, and a case goes red.
func TestAPlaceReservedAsTheTakesTimeRanOutIsSettled(t *testing.T) {
	t.Parallel()
	for name, steal := range map[string]bool{
		"the take's own place":          false,
		"a place another read reserved": true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig, _, _ := keepingRig(t)
			run := rig.launchKept(t, "t1")
			stamps := &timedStamps{PendingStore: rig.coordinator.pending, late: true, steal: steal}
			rig.coordinator.pending, rig.coordinator.recordBudget = stamps, 10*time.Minute
			read, by := readUntil(t)
			spooled, _ := rig.coordinator.takeCapture(read, run, rig.provider.Box(run.SandboxID), rig.runner,
				readCapture(BoxReadCollect, Result{}, false))
			if spooled != nil {
				t.Fatalf("a take whose place was never answered spooled %+v", spooled)
			}
			c := rig.get("t1").Capture
			switch {
			case c == nil:
				t.Fatal("the place the store took is gone from the row")
			case steal && c.State != CaptureBegun:
				t.Fatalf("another read's place stands %s (%q), want it begun as that read left it",
					c.State, c.Reason)
			case !steal && (c.State != CaptureRefused || !strings.Contains(c.Reason, "time ran out")):
				t.Fatalf("the take's own place stands %s (%q), want it refused, saying why", c.State, c.Reason)
			}
			for _, deadline := range stamps.askedUnder(CaptureRefused) {
				if deadline.After(by) {
					t.Errorf("the place was settled under a deadline %s past the read's by", deadline.Sub(by))
				}
			}
		})
	}
}
