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
	"testing/synctest"
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
			rig.runner, readCapture(BoxReadCollect, Result{}, true))
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
		"the earlier job's secrets kept beside the record": true,
		"none kept for the earlier job":                    false,
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
		readCapture(BoxReadCollect, Result{}, true))
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
// end waiting on it, with nobody left to settle it. The take stamps its place
// refused in the room the read leaves its outcome stamp, decided on the row as
// it stands: its own begun place is advanced, and a place another read reserved
// in its stead is left as it stands, the take standing by for that read's
// capture as one whose reservation was refused outright does
// (TestAReadWhoseReservationWentUnansweredBesideAnotherStandsByForIt has what it
// keeps).
//
// Mutation: leave a reservation that ended unanswered as it is, or settle the
// place whoever reserved it, and a case goes red; walk away from the place
// another read reserved, as such a take once did, and that case spools
// nothing.
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
				readCapture(BoxReadCollect, Result{}, true))
			switch {
			case !steal && spooled != nil:
				t.Fatalf("a take whose own place was never answered spooled %+v", spooled)
			case steal && (spooled == nil || !spooled.standby):
				t.Fatalf("a take that found its place another read's spooled %+v, want it standing by for "+
					"that read's capture", spooled)
			}
			c := rig.get("t1").Capture
			switch {
			case c == nil:
				t.Fatal("the place the store took is gone from the row")
			case steal && c.State != CaptureBegun:
				t.Fatalf("another read's place stands %s (%q), want it begun as that read left it",
					c.State, c.Reason)
			case steal && !c.standsBy(spooled.stamp):
				t.Fatalf("another read's place stands %+v, want the take named standing by for it", c)
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
// that hears nothing of its reservation by its own deadline settles its place
// refused, as one stamp decided on the row as it stands when the store writes
// it: where the store applies the reservation only after its asker gave up, the
// settle lands first and the reservation, arriving after it, finds the place
// answered and takes nothing; and where the store, its stream's leader stepping
// down after it took the reservation, answers nothing for a while, the settle is
// asked again until it lands, within the read's time. Either way the place is
// refused on the row, never left begun.
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

// A TAKE'S OWN RESERVATION, ITS ANSWER LOST, IS ITS OWN. The store applied the
// take's reservation and its answer never came back, so the take asked again —
// and the place it asks for is the one its first ask already holds. Answered as
// another read's, the take walked away from it, begun, with nobody to settle it
// and nothing kept. So the store answers a stamp the row already holds as
// taken: the take goes on under its own place, and its session is kept.
//
// IN A SYNCTEST BUBBLE, so the ask again is held to the rig's record budget on
// the bubble's clock, which only the asks' own waits advance: on the wall clock
// a starved runner spent the budget before the second ask, and it was refused.
//
// Mutation: answer a reservation the row already holds as not taken, and the
// take keeps nothing and leaves its place begun.
func TestATakesOwnReservationItsAnswerLostIsItsOwn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		run := rig.launchKept(t, "t1")
		rig.coordinator.pending = &lostAnswers{PendingStore: rig.coordinator.pending}
		spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID), rig.runner,
			readCapture(BoxReadCollect, Result{}, false))
		if spooled == nil {
			t.Fatal("a take whose reservation the store applied and whose answer was lost spooled nothing, " +
				"want it gone on under its own place")
		}
		rig.coordinator.keepLater(spooled)
		rig.awaitKeeps()
		if c := rig.get("t1").Capture; c == nil || c.State != CaptureLanded ||
			c.CaptureSeq != spooled.stamp.CaptureSeq {
			t.Fatalf("the take's place stands %+v, want its capture landed", c)
		}
	})
}
