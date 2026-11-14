package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executorkeep"
)

// crossingWrites is a pending store whose connection takes crossing to carry a
// run's row at its bound ([PendingStore.RowCrossing]), and which records, for
// each write of a row a case watches, the instant it was handed the write and
// its context's deadline, on the case's clock. Every other call is the store
// underneath's.
//
// An ending's decision takes decide on the case's clock and is declined, so a
// path that decides one ends there, and the next write it makes comes that much
// later.
type crossingWrites struct {
	PendingStore
	crossing time.Duration
	decide   time.Duration

	mu    sync.Mutex
	given map[string][]handed
}

// handed is one write as [crossingWrites] saw it.
type handed struct {
	at, deadline time.Time
	set          bool
}

func (s *crossingWrites) RowCrossing() time.Duration { return s.crossing }

// note records the write named write, handed ctx now.
func (s *crossingWrites) note(ctx context.Context, write string) {
	deadline, set := ctx.Deadline()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.given[write] = append(s.given[write], handed{at: time.Now(), deadline: deadline, set: set})
}

func (s *crossingWrites) writes(write string) []handed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]handed(nil), s.given[write]...)
}

func (s *crossingWrites) MarkResumeBegun(ctx context.Context, turnID, launch string, fence Fence,
	carry ResumeCarry) (bool, error) {
	s.note(ctx, "MarkResumeBegun")
	return s.PendingStore.MarkResumeBegun(ctx, turnID, launch, fence, carry)
}

func (s *crossingWrites) StampCapture(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	s.note(ctx, "StampCapture")
	return s.PendingStore.StampCapture(ctx, turnID, capture)
}

func (s *crossingWrites) OweEnd(ctx context.Context, turnID string, ending json.RawMessage) (PendingRun, bool, error) {
	s.note(ctx, "OweEnd")
	return s.PendingStore.OweEnd(ctx, turnID, ending)
}

func (s *crossingWrites) MarkHarnessEnd(ctx context.Context, turnID, launch string, end HarnessEnd) (bool, error) {
	s.note(ctx, "MarkHarnessEnd")
	return s.PendingStore.MarkHarnessEnd(ctx, turnID, launch, end)
}

func (s *crossingWrites) MarkEndedAtDeadline(ctx context.Context, turnID, launch string) (bool, error) {
	s.note(ctx, "MarkEndedAtDeadline")
	return s.PendingStore.MarkEndedAtDeadline(ctx, turnID, launch)
}

func (s *crossingWrites) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	s.note(ctx, "ReleaseClaim")
	return s.PendingStore.ReleaseClaim(ctx, turnID, release)
}

func (s *crossingWrites) ReleaseBox(ctx context.Context, turnID string) error {
	s.note(ctx, "ReleaseBox")
	return s.PendingStore.ReleaseBox(ctx, turnID)
}

func (s *crossingWrites) DecideEnding(ctx context.Context, _ string, _ Decision) (PendingRun, bool, error) {
	s.note(ctx, "DecideEnding")
	time.Sleep(s.decide) //fixedwait:simulated a decision the store takes a while over, on the case's bubble clock
	return PendingRun{}, false, nil
}

// EVERY WRITE OF A RUN'S ROW UNDER A BUDGET OF ITS CALLER'S IS GIVEN THE ROW'S
// CROSSING BESIDE ITS REASON, and each keeps its reason.
//
// A row is written whole, up to six megabytes, and a node reaching the store
// across a slow link sends it at the link's pace; the store keeps a caller's
// deadline as it is, so every caller that bounds a write of a row for a reason
// of its own — a resumed turn's carry, a store's slow moment; a capture's stamp,
// a leader's election outlasted; a teardown's writes, a box call's grace —
// bounds it by that reason and the time a row at its bound takes to cross
// (RowBudget). A teardown's writes each take a bound of their own, one run's
// after another's, and a stamp a read makes is still held to the read's own
// room. Exact, in a bubble.
//
// Mutation: hold any of these writes to its reason alone, and its row reads the
// crossing short; share one teardown bound across the runs a stop ends, and the
// second run's write is handed what the first left of it; give a stamp the
// crossing past the read's room, and the read's row reads past it.
func TestEveryWriteOfARunsRowIsGivenTheRowsCrossingBesideItsReason(t *testing.T) {
	t.Parallel()
	const crossing = 7 * time.Minute
	stamp := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureBegun}
	for _, c := range []struct {
		name string
		// write is the store call the path makes, and want what each of
		// those calls is given, in order.
		write string
		want  []time.Duration
		// runs is how many of the seat's runs the case launches first, one
		// where unset; path takes it on the first.
		runs int
		path func(ctx context.Context, c *Coordinator, run PendingRun)
	}{
		{
			name: "a resumed turn's carry", write: "MarkResumeBegun",
			want: []time.Duration{resumeCarryRecordBudget + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.markResumeBegun(ctx, run, ResumeCarriesOn)
			},
		},
		{
			name: "a capture's stamp off a read", write: "StampCapture",
			want: []time.Duration{sessionRecordBudget + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.stampSettled(ctx, time.Time{}, run.TurnID, stamp)
			},
		},
		{
			name: "a capture's stamp within a read's room", write: "StampCapture",
			want: []time.Duration{time.Minute},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.stampSettled(ctx, time.Now().Add(time.Minute), run.TurnID, stamp)
			},
		},
		{
			name: "a turn's owed end", write: "OweEnd",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				_, _ = c.OweEnd(ctx, run.TurnID, executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted})
			},
		},
		{
			name: "a job ended at its turn's cap", write: "MarkEndedAtDeadline",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.markEndedAtDeadline(ctx, run)
			},
		},
		{
			name: "how a collected job's harness ended", write: "MarkHarnessEnd",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.markHarnessEnd(ctx, run, HarnessEndOwnRun)
			},
		},
		{
			name: "a claim handed back", write: "ReleaseClaim",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				run.Status, run.ClaimedFrom = StatusResumed, StatusRunning
				_ = c.unclaimAt(ctx, run, true, "", failure{}, nil)
			},
		},
		{
			name: "a box let go", write: "ReleaseBox",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.teardown(ctx, run)
			},
		},
		{
			name: "a claim's ending", write: "DecideEnding",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				_, _ = c.endClaim(ctx, run, ending{})
			},
		},
		{
			name: "a run's ending", write: "DecideEnding",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.finish(ctx, run, ending{fence: fenceOf(run)})
			},
		},
		{
			name: "an abandoned launch's ending", write: "DecideEnding",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.abandon(ctx, c.mgr(), LaunchRequest{
					Turn: TurnRef{TurnID: run.TurnID, AgentHandle: run.AgentHandle}}, fenceOf(run), "")
			},
		},
		{
			name: "every run a person's stop ends", write: "DecideEnding", runs: 2,
			want: []time.Duration{discardGrace + crossing, discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				_ = c.StopSeat(ctx, run.AgentHandle, RunStop{By: "ana"})
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				var store *crossingWrites
				rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
					store = &crossingWrites{PendingStore: o.Pending, crossing: crossing,
						decide: time.Minute, given: map[string][]handed{}}
					o.Pending = store
				})
				run := rig.launch("t1")
				for n := 2; n <= c.runs; n++ {
					rig.launch(fmt.Sprintf("t%d", n))
				}
				c.path(t.Context(), rig.coordinator, run)
				got := store.writes(c.write)
				if len(got) != len(c.want) {
					t.Fatalf("%s was asked %d times, want %d", c.write, len(got), len(c.want))
				}
				for i, w := range got {
					if given := w.deadline.Sub(w.at); !w.set || given != c.want[i] {
						t.Errorf("%s #%d was given %v (set %t), want %v: its reason and the crossing of "+
							"a row at its bound, %v", c.write, i+1, given, w.set, c.want[i], crossing)
					}
				}
			})
		})
	}
}

// A ROW'S WRITE IN THE STORE'S OWN PROCESS IS GIVEN ITS REASON ALONE.
//
// The crossing is the store's to say, and the store in a node's own process —
// the in-memory twin a single node runs — says none, so every budget a row's
// write is held to there is the reason it always was.
//
// Mutation: have the twin say a crossing, or count one in RowBudget whatever
// the store says, and a single node's budgets grow by minutes for bytes that
// cross nothing.
func TestARowsWriteInTheStoresOwnProcessIsGivenItsReasonAlone(t *testing.T) {
	t.Parallel()
	store := NewCoordStore(memory.NewFleet())
	if got := store.RowCrossing(); got != 0 {
		t.Errorf("the store in the node's own process says a row takes %v to cross, want none", got)
	}
	if got := RowBudget(store, sessionRecordBudget); got != sessionRecordBudget {
		t.Errorf("RowBudget in process = %v, want the reason alone, %v", got, sessionRecordBudget)
	}
	// AND THE READ STANDING BY, which any node may be, waits for the
	// slowest node's record whatever this node's store says: past the
	// capture it stands by for and the read that sees how it came out,
	// room for the record's reason and a row's crossing at the slowest
	// pace a link is allowed.
	if room := standbyBudget - SessionCaptureBudget - captureWaitTick; room < sessionRecordBudget+coord.MaxValueCrossing {
		t.Errorf("a read standing by leaves %v for its record of the next place, want at least the "+
			"record's %v and the slowest node's crossing of a row, %v", room, sessionRecordBudget,
			coord.MaxValueCrossing)
	}
}

// A CAPTURE'S BUDGET COUNTS ITS STAMP OF WHAT CAME OF IT, AND SO DOES EVERY WAIT
// AFTER IT, AT THE SLOWEST NODE'S CROSSING.
//
// A keep stamps what came of its capture after the keep's own budget, and the
// stamp writes the run's row whole, which across a slow link takes the row's
// crossing; the node keeping it may be any node, and the node reading the
// capture's age any other. So a capture begun is in flight until its take, its
// keep and that stamp can all have run; a read standing by for it stands until
// that and its own stamp of the next place; and a turn's end waits for the
// standby's keep and that keep's stamp past both. Counted without the stamp,
// a reader aged out a capture whose stamp was still crossing: a standby took
// its place over and uploaded the session again, and the turn's end named it
// begun and never finished.
//
// Mutation: drop the stamp from a capture's budget, from the standby's, or
// from the end's wait, and the last instant it can still be crossing reads as
// past.
func TestACapturesBudgetCountsItsStampAtTheSlowestNodesCrossing(t *testing.T) {
	t.Parallel()
	stamp := sessionRecordBudget + coord.MaxValueCrossing
	begun := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	capture := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, Launch: "launch-1", State: CaptureBegun, At: begun}
	// THE LAST INSTANT EACH CAN STILL BE UNDER WAY, a nanosecond short of
	// its bound, as an age past the capture's own instant.
	stamped := SessionTakeBudget + SessionKeepBudget + stamp
	if !captureInFlight(PendingRun{Capture: &capture}, begun.Add(stamped-time.Nanosecond)) {
		t.Errorf("a capture begun %v ago, its stamp of what came of it still crossing, reads as past its budget "+
			"of %v, want in flight until its take, its keep and that stamp at the slowest node's crossing",
			stamped-time.Nanosecond, SessionCaptureBudget)
	}
	standing := capture
	standing.StandbyAt, standing.StandbyTaker = begun, "node-b:standby"
	stood := stamped + captureWaitTick + stamp
	if !standing.standing(begun.Add(stood - time.Nanosecond)) {
		t.Errorf("a read standing by %v reads as gone, want it standing until the capture it stands by for "+
			"can have stamped what came of it, one read after, and its own stamp of the next place: %v",
			stood-time.Nanosecond, standbyBudget)
	}
	if kept := stood + SessionKeepBudget + stamp; endCaptureWait < kept {
		t.Errorf("a turn's end waits %v for its captures, want at least the standby's stand, its keep and that "+
			"keep's stamp of what came of it, %v", endCaptureWait, kept)
	}
}
