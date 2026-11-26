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
	inBubble(t, func(t *testing.T) {
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
	})
}

// A PROCESS'S SPOOLS ARE SWEPT ONCE ITS LOCK IS GONE, AND NEVER BEFORE. Each
// process spools into a directory of its own, holding its lock while it lives;
// a take sweeps every directory whose lock it can take (a crash's, a staging
// one a crash left half made), and never one whose process lives, its own
// included. The other process is played by a directory made as a coordinator
// makes its own: flock locks conflict across descriptors, so one test process
// plays both.
//
// Mutation: sweep a directory whatever its lock, or sweep only as a
// coordinator starts, and a check below goes red.
func TestASpoolDirectoryIsSweptOnlyOnceItsProcessHasLetGo(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		take := func(turnID string) {
			rig.launchKept(t, turnID)
			session := rig.runner.Session
			rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
			collectHeld(t, rig, turnID)
		}
		other := func() (string, string, func()) {
			dir, lock, err := makeSpoolDir(rig.coordinator.spoolDir)
			if err != nil {
				t.Fatal(err)
			}
			spool := filepath.Join(dir, spoolPrefix+"under-way")
			if err := os.WriteFile(spool, []byte("sealed"), 0o600); err != nil {
				t.Fatal(err)
			}
			return dir, spool, lock.Release
		}
		gone := func(what, dir string) {
			t.Helper()
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s was left by the take after it (%v)", what, err)
			}
		}
		live, liveSpool, ends := other()
		crashed, _, crash := other()
		crash()
		half := filepath.Join(rig.coordinator.spoolDir, spoolDirPrefix+"half-made"+spoolStaging)
		if err := os.Mkdir(half, 0o700); err != nil {
			t.Fatal(err)
		}
		take("t1")
		gone("a crashed process's spool directory", crashed)
		gone("a staging directory a crash left", half)
		if _, err := os.Stat(liveSpool); err != nil {
			t.Errorf("a live process's spool was swept: %v", err)
		}
		own := rig.coordinator.ownSpoolDir()
		if _, err := os.Stat(filepath.Join(own, spoolLockName)); own == "" || err != nil {
			t.Errorf("the take's own spool directory %q is gone (%v)", own, err)
		}
		ends()
		take("t2")
		gone("a process's spool directory once it let go", live)
		if _, err := os.Stat(filepath.Join(own, spoolLockName)); err != nil {
			t.Errorf("the coordinator's own spool directory was swept while it lives: %v", err)
		}
	})
}

// EVERY START SWEEPS WHAT A GONE PROCESS SPOOLED, before it takes anything, so
// a node restarted after a crash that then takes nothing for hours holds none
// of its spools that long: a coordinator built over a spool directory holding
// a crashed process's directory, a staging one a crash left half made, and a
// live process's, removes the first two and keeps the live one's spool.
//
// Mutation: drop the sweep from NewCoordinator, or sweep a directory whatever
// its lock, and a check below goes red.
func TestEveryStartSweepsTheSpoolsOfAProcessThatLetGo(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig := newCoordRig(t)
		parent := t.TempDir()
		other := func() (string, string, func()) {
			dir, lock, err := makeSpoolDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			spool := filepath.Join(dir, spoolPrefix+"under-way")
			if err := os.WriteFile(spool, []byte("sealed"), 0o600); err != nil {
				t.Fatal(err)
			}
			return dir, spool, lock.Release
		}
		_, liveSpool, ends := other()
		defer ends()
		crashed, _, crash := other()
		crash()
		half := filepath.Join(parent, spoolDirPrefix+"half-made"+spoolStaging)
		if err := os.Mkdir(half, 0o700); err != nil {
			t.Fatal(err)
		}
		c, err := NewCoordinator(CoordinatorOptions{
			Answers: rigAnswers(), Leads: rigLeads(),
			Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
			Resume: &resumeSpy{}, Audience: &audienceSpy{}, Pushes: &pushSpy{},
			Lease:    leased(rigLease),
			SpoolDir: parent,
		})
		if err != nil {
			t.Fatalf("NewCoordinator: %v", err)
		}
		defer c.Stop()
		for what, dir := range map[string]string{"a crashed process's spool directory": crashed,
			"a staging directory a crash left": half} {
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s was left by the start (%v)", what, err)
			}
		}
		if _, err := os.Stat(liveSpool); err != nil {
			t.Errorf("a live process's spool was swept by the start: %v", err)
		}
	})
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
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			run := rig.launchKept(t, "t1")
			spooled, kept := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID),
				rig.runner, readCapture(BoxReadCollect, Result{}, true))
			if spooled == nil {
				t.Fatalf("the take spooled nothing: %+v", kept)
			}
			rig.coordinator.Stop()
			rig.coordinator.keepLater(t.Context(), spooled)
			if left := rig.spools(t); len(left) != 0 {
				t.Errorf("a keep the stopped node dropped left its spool: %v", left)
			}
			if c := rig.get("t1").Capture; c == nil || c.State != CaptureRefused {
				t.Errorf("the dropped keep's capture stands %+v, want it refused", c)
			}
		})
	})
	t.Run("a seal that failed", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
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
	})
}

// A JOB THAT CONTINUES IN ITS TURN'S BOX IS SCRUBBED OF WHAT THE JOBS BEFORE IT
// WERE HANDED. The relaunch resumes the session the box holds, earlier jobs'
// lines included, so its record seals their secrets beside its own: a token
// rotated while the box waited, handed only to the first job, is still in the
// session and the box. A continued box whose record says nothing of what the
// jobs before were handed keeps no capture rather than one scrubbed of less.
//
// Mutation: seal only the relaunch's own secrets, or seal them alone where the
// earlier jobs' are unknown, and a check below goes red.
func TestAJobContinuingInItsTurnsBoxScrubsWhatTheJobsBeforeItWereHanded(t *testing.T) {
	t.Parallel()
	for name, earlier := range map[string]bool{
		"the earlier job's secrets kept beside the record": true,
		"none kept for the earlier job":                    false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
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
				sealed := rig.handedOf(t, "t1")
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
		})
	}
}

// A LOGIN THE BOX'S CODE GREW PAST WHAT A LAUNCH MAY HAND REFUSES THE CAPTURE.
// The box can write its login (its harness refreshes the file), and every
// string in it becomes a scrub value held in one replacer, so an unbounded file
// of short strings costs the engine host memory without limit. A login past
// what a whole launch may hand is no harness's: the capture is refused, saying
// why.
//
// Mutation: read the login again without its bound, and the session is taken.
func TestALoginTheBoxGrewPastItsBoundRefusesTheCapture(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
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
	})
}

// timedStamps is a run store that records the deadline every stamp of a
// capture is asked under, by the state it stamps; and, where late is set,
// applies a place's reservation and answers it only once its caller has given
// up — a coordination store that took the write and whose answer came too late
// — stamping it, where steal is set, as another read racing this one reserved
// the same place: at another instant. begun, when set, runs once a place's
// reservation it did not hold back has landed, before the take goes on.
type timedStamps struct {
	PendingStore
	late, steal bool
	begun       func()

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
		took, err := s.PendingStore.StampCapture(ctx, turnID, stamp)
		if stamp.State == CaptureBegun && s.begun != nil {
			s.begun()
		}
		return took, err
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
// records, a grace for each of the two, up to by, and for its box's pause
// ([Coordinator.takeContext]).
func readUntil(t *testing.T) (read context.Context, by time.Time) {
	t.Helper()
	read, cancel := context.WithTimeout(t.Context(), 3*discardGrace+2*time.Second)
	t.Cleanup(cancel)
	deadline, _ := read.Deadline()
	return read, deadline.Add(-discardGrace)
}

// A CAPTURE'S OUTCOME IS STAMPED IN THE ROOM THE READ LEAVES IT. A reserved
// take whose spool failed is stamped refused under its own context ending at
// the read's by: not the take's deadline, which may be what ended the spool,
// and not past by, which the read needs for the box's pause. The take spends a
// second on the bubble's clock between reserving and refusing, so a stamp
// bounded by the room the read had when the take began ends past by.
//
// Mutation: bound the refusal's stamp by the take's own deadline, by the
// record's budget alone, or by the room the read had when the take began, and
// its deadline is not the read's by.
func TestACapturesOutcomeIsStampedInTheRoomTheReadLeavesIt(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, keep := keepingRig(t)
		keep.sealErr = errors.New("the keyring would not seal")
		run := rig.launchKept(t, "t1")
		stamps := &timedStamps{PendingStore: rig.coordinator.pending,
			begun: func() { time.Sleep(time.Second) }}
		rig.coordinator.pending, rig.coordinator.recordBudget = stamps, 10*time.Minute
		read, by := readUntil(t)
		spooled, _ := rig.coordinator.takeCapture(read, run, rig.provider.Box(run.SandboxID), rig.runner,
			readCapture(BoxReadCollect, Result{}, true))
		if spooled != nil {
			t.Fatalf("a take whose seal failed spooled %+v", spooled)
		}
		if len(stamps.askedUnder(CaptureBegun)) == 0 {
			t.Fatal("the take reserved no place before its refusal, so its clock never moved between the two")
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
	})
}

// A PLACE RESERVED AS THE TAKE'S TIME RAN OUT IS SETTLED. Answered after the
// take's deadline, the reservation would leave the place begun, read as a keep
// under way that the turn's end waits on, with nobody to settle it. The take
// stamps it refused in the room the read leaves, decided on the row as it
// stands: its own begun place is advanced, and one another read reserved is
// left as that read left it. The take's time is spent, so it spools nothing to
// stand by with; one with time left stands by
// (TestAReadWhoseReservationWentUnansweredBesideAnotherStandsByForIt).
//
// Mutation: leave a reservation that ended unanswered as it is, or settle the
// place whoever reserved it, and a case goes red; spool past the take's
// deadline, and the case another read reserved spools as one standing by.
func TestAPlaceReservedAsTheTakesTimeRanOutIsSettled(t *testing.T) {
	t.Parallel()
	for name, steal := range map[string]bool{
		"the take's own place":          false,
		"a place another read reserved": true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, _ := keepingRig(t)
				run := rig.launchKept(t, "t1")
				stamps := &timedStamps{PendingStore: rig.coordinator.pending, late: true, steal: steal}
				rig.coordinator.pending, rig.coordinator.recordBudget = stamps, 10*time.Minute
				read, by := readUntil(t)
				spooled, _ := rig.coordinator.takeCapture(read, run, rig.provider.Box(run.SandboxID), rig.runner,
					readCapture(BoxReadCollect, Result{}, true))
				if spooled != nil {
					t.Fatalf("a take whose time ran out spooled %+v, want nothing", spooled)
				}
				c := rig.get("t1").Capture
				switch {
				case c == nil:
					t.Fatal("the place the store took is gone from the row")
				case steal && c.State != CaptureBegun:
					t.Fatalf("another read's place stands %s (%q), want it begun as that read left it",
						c.State, c.Reason)
				case steal && !c.StandbyAt.IsZero():
					t.Fatalf("another read's place stands %+v, want nobody standing by for it", c)
				case !steal && (c.State != CaptureRefused || !strings.Contains(c.Reason, "time ran out")):
					t.Fatalf("the take's own place stands %s (%q), want it refused, saying why", c.State, c.Reason)
				}
				for _, deadline := range stamps.askedUnder(CaptureRefused) {
					if deadline.After(by) {
						t.Errorf("the place was settled under a deadline %s past the read's by", deadline.Sub(by))
					}
				}
			})
		})
	}
}

// A TAKE'S READ OF WHO READS ITS MEDIA COSTS ITS PACK AND SCRUB NOTHING: the
// fleet's word on keeping is read before the take's budget begins
// ([Coordinator.placingPolicy]), in a room [BoxReadBudget] counts beside it, so
// a store that takes the whole room to say still leaves the session's stream
// the take's budget and its reservation's room. Bubbled.
//
// Mutation: read the policy inside the take's budget, and the stream is cut a
// room early.
func TestATakesReadOfWhoReadsItsMediaCostsItsPackAndScrubNothing(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, keep := keepingRig(t)
		run := rig.launchKept(t, "t1")
		keep.mu.Lock()
		keep.policyHook = func(context.Context) { time.Sleep(MediaReadersRoom) }
		keep.mu.Unlock()
		streamed := make(chan time.Duration, 1)
		rig.runner.Session.Streaming = func(ctx context.Context) {
			start := time.Now()
			<-ctx.Done()
			streamed <- time.Since(start)
		}
		spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID), rig.runner,
			readCapture(BoxReadStop, Result{}, false))
		rig.coordinator.keepLater(t.Context(), spooled)
		rig.awaitKeeps()
		if took, whole := <-streamed, SessionTakeBudget+rig.coordinator.reserveRoom(); took <= whole-MediaReadersRoom {
			t.Errorf("the stream was cut %v after it began, behind a policy read of %v, want the take's "+
				"whole %v", took, MediaReadersRoom, whole)
		}
	})
}

// A TAKE WHOSE SCRUB RUNS PAST ITS WAIT FAILS CLOSED: a session still
// streaming out of its scrub when the take's time ends, which holds the
// scrub's whole wait ([SessionScrubWait]) beside the pack's, is cut there,
// its place stamped refused and nothing uploaded, and the run's row still
// names the copy kept before it: a box whose session scrubs too slowly costs
// only that session's newest copy (docs/concepts/code-sandbox.md, "Seven
// minutes for the scrub").
//
// Mutation: hold the take to the pack's wait alone, or spool what a cut
// stream read, and a check goes red.
func TestATakeWhoseScrubRunsPastItsWaitIsNotKept(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, keep := keepingRig(t)
		run := rig.launchKept(t, "t1")
		box := rig.provider.Box(run.SandboxID)
		first, _ := rig.coordinator.takeCapture(t.Context(), run, box, rig.runner,
			readCapture(BoxReadStop, Result{}, false))
		if first == nil {
			t.Fatal("the premise: the first take spooled nothing")
		}
		rig.coordinator.keepLater(t.Context(), first)
		rig.awaitKeeps()
		kept := rig.get("t1").Kept
		if kept == nil || kept.State != CaptureLanded {
			t.Fatalf("the premise: the first take was kept as %+v, want it landed", kept)
		}
		uploads := len(keep.uploads())
		streamed := make(chan time.Duration, 1)
		rig.runner.Session.Streaming = func(ctx context.Context) {
			start := time.Now()
			<-ctx.Done()
			streamed <- time.Since(start)
		}
		spooled, _ := rig.coordinator.takeCapture(t.Context(), rig.get("t1"), box, rig.runner,
			readCapture(BoxReadStop, Result{}, false))
		rig.coordinator.keepLater(t.Context(), spooled)
		rig.awaitKeeps()
		if spooled != nil {
			t.Errorf("a take whose stream ran out its time spooled %+v, want nothing", spooled)
		}
		if took := <-streamed; took < SessionScrubWait || took > SessionTakeBudget+rig.coordinator.reserveRoom() {
			t.Errorf("the stream was cut %v after it began, want within the take's budget and no sooner than "+
				"the scrub's wait of %v", took, SessionScrubWait)
		}
		row := rig.get("t1")
		if c := row.Capture; c == nil || c.CaptureSeq != kept.CaptureSeq+1 || c.State != CaptureRefused {
			t.Errorf("the cut take's capture stands %+v, want its place refused", c)
		}
		if row.Kept == nil || row.Kept.CaptureSeq != kept.CaptureSeq || row.Kept.State != CaptureLanded {
			t.Errorf("the run's kept copy is %+v after the cut take, want the one kept before it, %+v",
				row.Kept, kept)
		}
		if got := len(keep.uploads()); got != uploads {
			t.Errorf("the cut take uploaded %d sessions, want none", got-uploads)
		}
	})
}

// lateStore is a run store whose answer to a take's reservation of its place
// never reaches the take in time, as a coordination store's does when the
// leader of the stream under it stalls or steps down: the reservation is
// applied — before its asker gives up, or, where afterwards is set, only once
// the row has been read or written again after its asker gave up, a write
// already on its way that lands late — and its asker hears nothing before it
// gives up; and the first fails reads and stamps of the row after the
// reservation are answered with no leader, as a store electing one answers
// every request meanwhile.
type lateStore struct {
	PendingStore
	afterwards bool
	fails      int

	mu       sync.Mutex
	reserved bool
	failed   int
	// pending is a reservation the store has taken and not yet applied.
	pending *SessionCapture
}

// errNoLeader is a store answering while its stream has no leader.
var errNoLeader = errors.New("the stream has no leader")

// leaderless reports whether the store answers this read or stamp with no
// leader: one of the first fails after the reservation.
func (s *lateStore) leaderless() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reserved || s.failed >= s.fails {
		return false
	}
	s.failed++
	return true
}

// land applies a reservation still on its way, as made against the row before
// whatever was read or written since: asked again on the row as it stands now.
// Under ctx's values and never its cancellation, as the late write is already
// the store's: whatever became of the request that carried it in, nothing
// takes it back.
func (s *lateStore) land(ctx context.Context, turnID string) {
	s.mu.Lock()
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()
	if pending != nil {
		_, _ = s.PendingStore.StampCapture(context.WithoutCancel(ctx), turnID, *pending)
	}
}

func (s *lateStore) StampCapture(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	if stamp.State == CaptureBegun {
		s.mu.Lock()
		s.reserved = true
		if s.afterwards {
			held := stamp
			s.pending = &held
		}
		s.mu.Unlock()
		if !s.afterwards {
			if _, err := s.PendingStore.StampCapture(context.WithoutCancel(ctx), turnID, stamp); err != nil {
				return false, err
			}
		}
		<-ctx.Done()
		return false, ctx.Err()
	}
	if s.leaderless() {
		return false, errNoLeader
	}
	defer s.land(ctx, turnID)
	return s.PendingStore.StampCapture(ctx, turnID, stamp)
}

func (s *lateStore) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	if s.leaderless() {
		return PendingRun{}, false, errNoLeader
	}
	defer s.land(ctx, turnID)
	return s.PendingStore.Get(ctx, turnID)
}

// A RESERVATION THE STORE TAKES LATE IS SETTLED WHICHEVER WAY IT LANDS. A take
// unanswered by its deadline settles its place refused, one stamp decided on
// the row as the store writes it: a reservation applied after its asker gave up
// finds the place answered and takes nothing; a store answering nothing while
// its stream elects a leader is asked again until the settle lands, within the
// read's time. Either way the place is refused, never left begun.
//
// Mutation: decide the settle off one read of the row, or give up on a read or
// stamp the store did not answer, and a case leaves the place begun.
func TestAReservationTheStoreTakesLateIsSettledWhicheverWayItLands(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]*lateStore{
		"applied once its asker gave up":   {afterwards: true},
		"answered by no leader for a time": {fails: 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, _ := keepingRig(t)
				run := rig.launchKept(t, "t1")
				store.PendingStore = rig.coordinator.pending
				rig.coordinator.pending, rig.coordinator.recordBudget = store, 10*time.Minute
				read, _ := readUntil(t)
				spooled, kept := rig.coordinator.takeCapture(read, run, rig.provider.Box(run.SandboxID), rig.runner,
					readCapture(BoxReadCollect, Result{}, false))
				if spooled != nil {
					t.Fatalf("a take whose place was never answered spooled %+v", spooled)
				}
				// AND ANY RESERVATION STILL ON ITS WAY, landed now.
				store.land(t.Context(), "t1")
				c := rig.get("t1").Capture
				switch {
				case c == nil:
					t.Fatal("the row holds no capture of the take, want its place refused")
				case c.State != CaptureRefused || !strings.Contains(c.Reason, "time ran out"):
					t.Fatalf("the take's place stands %s (%q), want it refused, saying why", c.State, c.Reason)
				case kept == nil || kept.NotKept == "":
					t.Errorf("the read reports %+v of the session, want it not kept, saying why", kept)
				}
			})
		})
	}
}

// lostAnswers is a run store that applies a take's first reservation of its
// place and loses its answer, as a store whose reply never reached its asker
// does: the asker hears an error, and asks again.
type lostAnswers struct {
	PendingStore
	mu   sync.Mutex
	lost bool
}

func (s *lostAnswers) StampCapture(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	s.mu.Lock()
	lose := stamp.State == CaptureBegun && !s.lost
	s.lost = s.lost || lose
	s.mu.Unlock()
	if !lose {
		return s.PendingStore.StampCapture(ctx, turnID, stamp)
	}
	if _, err := s.PendingStore.StampCapture(ctx, turnID, stamp); err != nil {
		return false, err
	}
	return false, errors.New("the store's answer never reached its asker")
}

// A TAKE'S OWN RESERVATION, ITS ANSWER LOST, IS ITS OWN. Asked again, the place
// is the one its first ask already holds; answered as another read's, the take
// would walk away from it begun, nothing kept. So the store answers a stamp the
// row already holds as taken, and the take goes on under its own place.
//
// IN A SYNCTEST BUBBLE, so the record budget runs on the bubble's clock, which
// only the asks' own waits advance: a starved runner's wall clock could spend
// it before the second ask.
//
// Mutation: answer a reservation the row already holds as not taken, and the
// take keeps nothing and leaves its place begun.
func TestATakesOwnReservationItsAnswerLostIsItsOwn(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		run := rig.launchKept(t, "t1")
		rig.coordinator.pending = &lostAnswers{PendingStore: rig.coordinator.pending}
		spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID), rig.runner,
			readCapture(BoxReadCollect, Result{}, false))
		if spooled == nil {
			t.Fatal("a take whose reservation the store applied and whose answer was lost spooled nothing, " +
				"want it gone on under its own place")
		}
		rig.coordinator.keepLater(t.Context(), spooled)
		rig.awaitKeeps()
		if c := rig.get("t1").Capture; c == nil || c.State != CaptureLanded ||
			c.CaptureSeq != spooled.stamp.CaptureSeq {
			t.Fatalf("the take's place stands %+v, want its capture landed", c)
		}
	})
}
