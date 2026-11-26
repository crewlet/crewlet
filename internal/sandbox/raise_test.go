package sandbox

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
)

// A capture no read places is reserved at zero and raised after its take
// (savepoint.go), through the save task and a take as they run.

// continuingJob launches t1's job as the second of its turn, its record naming
// the job before's kept capture at 7, taken up by its box in the same round and
// of the newest compaction msg_1, its brief having counted the reads 3 and 12
// and the late calls 5 and 13 without listing them, as edit leaves it; and has
// the job's take name that compaction, of a release whose resume was read.
func continuingJob(t *testing.T, rig *coordRig, edit func(*PendingRun)) PendingRun {
	t.Helper()
	rig.launch("t1")
	if _, _, err := rig.pending.mutate(t.Context(), "t1", func(run *PendingRun) bool {
		run.LaunchSeq = 2
		run.Launch.Brief, run.Launch.BriefOmitted = BriefContinued, []int{3, 12}
		run.Launch.BriefLateOmitted = []int{5, 13}
		run.Launch.SessionStart, run.Launch.BasisRestart = SessionStartContinued, new("msg_1")
		run.Launch.Continued = &ContinuedPoint{LaunchSeq: 1, Through: 7}
		if edit != nil {
			edit(run)
		}
		return true
	}); err != nil {
		t.Fatalf("recording the continued capture: %v", err)
	}
	rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Session.Count = &ResumedCount{Tokens: 1, Restart: new("msg_1")}
	rig.runner.Session.ResumeRead = true
	return rig.get("t1")
}

// reservedAs records the stamp the row holds for a take's place while its
// archive is first read: its place reserved, its spool not yet read.
type reservedAs struct {
	mu    sync.Mutex
	stamp *SessionCapture
}

// watch has rig's take record, as it spools, the stamp of reads(row).
func (r *reservedAs) watch(t *testing.T, rig *coordRig, reads func(PendingRun) *SessionCapture) {
	rig.runner.Session.Streaming = func(ctx context.Context) {
		row := rig.getIn(context.WithoutCancel(ctx), "t1")
		r.mu.Lock()
		defer r.mu.Unlock()
		if held := reads(row); held != nil {
			stamp := *held
			r.stamp = &stamp
		}
	}
	t.Cleanup(func() { rig.runner.Session.Streaming = nil })
}

// atZero fails t unless the reservation recorded states a zero save point and
// nothing an older build reads, nor any read left out, and every late call its
// launch's brief counted without listing it (continuingJob's 5 and 13).
func (r *reservedAs) atZero(t *testing.T, what string) SessionCapture {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stamp == nil {
		t.Fatalf("%s: no reservation was seen while the take spooled", what)
	}
	if through, stated := r.stamp.SavePoint(); !stated || through != 0 || r.stamp.BridgeSeq != 0 ||
		r.stamp.BridgeOpen != nil || r.stamp.Omitted != nil {
		t.Errorf("%s: the reservation states %d (stated %v), %d, %v and %v; want a stated zero and nothing else",
			what, through, stated, r.stamp.BridgeSeq, r.stamp.BridgeOpen, r.stamp.Omitted)
	}
	if !slices.Equal(r.stamp.LateOmitted, []int{5, 13}) {
		t.Errorf("%s: the reservation states late calls %v left out, want its launch's [5 13]: a continuation "+
			"credits no telling of one", what, r.stamp.LateOmitted)
	}
	return *r.stamp
}

// EVERY CAPTURE NO READ PLACES IS RESERVED AT ZERO, AS THE STORE HOLDS IT: a
// save, the take of a job still working, and one cut off, each of a launch
// whose own landed capture states 9 with places an older build reads and a
// read left out, and whose record names a continued capture at 7, all of one
// newest compaction. Raised after the take, the outcome states 9, nothing an
// older build reads, the launch's own reads left out at or below it, and every
// late call its brief counted, whatever its place: a telling is credited by the
// note cursor, never by the save point (internal/agent/runner's heldByTelling).
//
// Mutation: raise before the instant, keep the places of the capture it was
// raised to, or reserve or raise with fewer than the launch's late calls, and a
// case goes red.
func TestEveryCaptureNoReadPlacesIsReservedAtZeroAsTheStoreHoldsIt(t *testing.T) {
	t.Parallel()
	for name, take := range map[string]func(t *testing.T, rig *coordRig, run PendingRun){
		"a save": func(t *testing.T, rig *coordRig, _ PendingRun) {
			if out := saveOf(rig, "t1", nil); out.class != saveLanded {
				t.Fatalf("the save came to %+v, want it landed", out)
			}
		},
		"the take of a job still working": func(t *testing.T, rig *coordRig, run PendingRun) {
			takeAndKeep(t, rig, run, readCapture(BoxReadStop, Result{}, false))
		},
		"a capture cut off": func(t *testing.T, rig *coordRig, run PendingRun) {
			takeAndKeep(t, rig, run, readCapture(BoxReadStop, Result{}, true))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
				run := continuingJob(t, rig, nil)
				own := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
					State: CaptureLanded, At: rig.now, Interrupted: true, Through: new(9), BridgeSeq: 11,
					BridgeOpen: []int{10}, Omitted: []int{4}, Object: objstore.KeyAt(rig.now).String(),
					Harness: "claude-code", SessionID: "s-1", ArchiveFormat: FakeArchiveFormat,
					ResumedCount: &ResumedCount{Tokens: 1, Restart: new("msg_1")}}
				if took, err := rig.pending.StampCapture(t.Context(), "t1", own); err != nil || !took {
					t.Fatalf("landing the launch's own capture = %v, %v", took, err)
				}
				var reserved reservedAs
				reserved.watch(t, rig, func(row PendingRun) *SessionCapture {
					if row.Saving != nil {
						return row.Saving
					}
					return row.Capture
				})
				take(t, rig, run)
				reserved.atZero(t, name)
				kept := rig.get("t1").Kept
				if kept == nil || kept.CaptureSeq != 2 {
					t.Fatalf("the row keeps %+v, want the take's capture in the place after its own", kept)
				}
				if through, _ := kept.SavePoint(); through != 9 || kept.BridgeSeq != 0 || kept.BridgeOpen != nil ||
					!slices.Equal(kept.Omitted, []int{3}) || !slices.Equal(kept.LateOmitted, []int{5, 13}) {
					t.Errorf("the kept capture states %d, %d, %v, %v left out and late %v; want 9, zero, none, [3] "+
						"and [5 13]", through, kept.BridgeSeq, kept.BridgeOpen, kept.Omitted, kept.LateOmitted)
				}
			})
		})
	}
}

// takeAndKeep takes run's session at at, and keeps it.
func takeAndKeep(t *testing.T, rig *coordRig, run PendingRun, at capturePoint) {
	t.Helper()
	spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID), rig.runner, at)
	if spooled == nil {
		t.Fatal("the take spooled nothing")
	}
	if kept := rig.coordinator.keepCapture(t.Context(), spooled); kept == nil || kept.NotKept != "" {
		t.Fatalf("the take's keep came to %+v, want the session kept", kept)
	}
}

// A SAVE'S RESERVATION STATES ZERO AND ITS OUTCOME THE RAISED POINT. The save
// reads the run's row once, before its pack; its place is reserved at zero, and
// the outcome of its keep, at the same instant, states the continued capture's
// point its take's count allows, off that one read: where the box had said it
// took the session up by then, 7; where it says so only during the pack, zero,
// since the read that places the save held no word of it.
//
// Mutation: raise before the instant (at the place), raise after the keep (the
// kept save states zero), or raise off a fresh read of the row rather than the
// one read, and a case goes red.
func TestASavesReservationStatesZeroAndItsOutcomeTheRaisedPoint(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		saidLate bool
		through  int
	}{
		"the box said it took the session up before the save read its row": {through: 7},
		"the box said so only during the pack":                             {saidLate: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
				run := continuingJob(t, rig, func(run *PendingRun) {
					if tc.saidLate {
						run.Launch.SessionStart = ""
					}
				})
				if tc.saidLate {
					rig.runner.Packing = func(ctx context.Context, _ Sandbox) {
						if _, err := rig.pending.MarkSessionStart(context.WithoutCancel(ctx), "t1", run.LaunchID,
							SessionStartContinued); err != nil {
							t.Errorf("MarkSessionStart: %v", err)
						}
					}
				}
				var reserved reservedAs
				reserved.watch(t, rig, func(row PendingRun) *SessionCapture { return row.Saving })
				if out := saveOf(rig, "t1", nil); out.class != saveLanded {
					t.Fatalf("the save came to %+v, want it landed", out)
				}
				reservation := reserved.atZero(t, name)
				kept := rig.get("t1").Kept
				if kept == nil || !kept.sameTake(reservation) {
					t.Fatalf("the row keeps %+v, want the save reserved as %+v", kept, reservation)
				}
				if through, _ := kept.SavePoint(); through != tc.through || !slices.Equal(kept.LateOmitted, []int{5, 13}) {
					t.Errorf("the kept save states %d and late %v left out, want %d and its launch's [5 13], "+
						"raised or not", through, kept.LateOmitted, tc.through)
				}
			})
		})
	}
}

// takeOvers records each takeover a standby asks of the store.
type takeOvers struct {
	PendingStore
	mu    sync.Mutex
	asked []SessionCapture
}

func (s *takeOvers) TakeOver(ctx context.Context, turnID string, standby, capture SessionCapture) (bool, error) {
	s.mu.Lock()
	s.asked = append(s.asked, capture)
	s.mu.Unlock()
	return s.PendingStore.TakeOver(ctx, turnID, standby, capture)
}

// A STANDBY'S TAKEOVER RESERVES AT THE POINT ITS OWN TAKE RAISED. Two reads of
// one cut-off job take its session at once, and the second stands by; its spool
// is raised by its own take's count, so where the first's keep is refused, the
// takeover reserves the next place at that point, the one reservation above
// zero, and its outcome states it.
//
// Mutation: hand the standby's spool nothing to raise it by, or place the
// takeover at zero, and this goes red.
func TestAStandbysTakeoverReservesAtThePointItsOwnTakeRaised(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		run := continuingJob(t, rig, nil)
		held := newHeldArchive(placeHolderArchive)
		placed := holdingPlace(t, rig, run, held)
		standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
		if standby == nil || !standby.standingBy() {
			t.Fatalf("the second read spooled %+v, want it standing by", standby)
		}
		held.end <- errScrubRefused
		if spooled := <-placed; spooled != nil {
			t.Fatalf("a take whose scrub refused its archive spooled %+v", spooled)
		}
		takeovers := &takeOvers{PendingStore: rig.coordinator.pending}
		rig.coordinator.pending = takeovers
		if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
			t.Fatalf("the standby's keep came to %+v, want the session kept", kept)
		}
		takeovers.mu.Lock()
		asked := takeovers.asked
		takeovers.mu.Unlock()
		if len(asked) != 1 || asked[0].CaptureSeq != 2 {
			t.Fatalf("the standby asked the takeovers %+v, want one, of the next place", asked)
		}
		if through, _ := asked[0].SavePoint(); through != 7 {
			t.Errorf("the takeover reserves at %d, want the 7 its own take raised", through)
		}
		kept := rig.get("t1").Kept
		if kept == nil {
			t.Fatal("the row keeps no capture, want the takeover's")
		}
		if through, _ := kept.SavePoint(); kept.CaptureSeq != 2 || through != 7 {
			t.Errorf("the row keeps the capture in place %d at %d, want the takeover's, in place 2 at 7",
				kept.CaptureSeq, through)
		}
	})
}
