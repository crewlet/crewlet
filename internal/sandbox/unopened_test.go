package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// unansweredRuns is the fleet's run records with one write armed to go
// unanswered and land nothing: the next opening of a run's log (log), or the
// next read-decide-write of a run's record (row).
type unansweredRuns struct {
	fleetRuns
	log, row atomic.Bool
}

// fleetRuns names the records unansweredRuns embeds: embedded as
// coord.SandboxRuns, the field would take the name of one of its methods.
type fleetRuns = coord.SandboxRuns

func (s *unansweredRuns) MutateSandboxRunPart(ctx context.Context, turnID string, part coord.RunPart,
	decide func(coord.Record, bool) ([]byte, error)) (coord.Record, bool, error) {
	if part == coord.RunLog && s.log.CompareAndSwap(true, false) {
		return coord.Record{}, false, errUnanswered
	}
	return s.fleetRuns.MutateSandboxRunPart(ctx, turnID, part, decide)
}

func (s *unansweredRuns) MutateSandboxRun(ctx context.Context, turnID string,
	decide func(coord.Record) (coord.SandboxRunChange, error)) (coord.Record, bool, error) {
	if s.row.CompareAndSwap(true, false) {
		return coord.Record{}, false, errUnanswered
	}
	return s.fleetRuns.MutateSandboxRun(ctx, turnID, decide)
}

// unansweredRig is a coordinator rig, its options set by with, whose run
// records are unansweredRuns.
func unansweredRig(t *testing.T, with func(*CoordinatorOptions)) (*coordRig, *unansweredRuns) {
	t.Helper()
	rig := newCoordRigWith(t, with)
	runs := &unansweredRuns{fleetRuns: rig.pending.runs}
	rig.pending.runs = runs
	return rig, runs
}

// sendBack drives t1's collected job into a resumed turn that relaunches it on
// the same coordinator, with the write arm picks armed just before, and then
// breaks, as a review's send-back that fails; then it runs the retries that
// owed, as the frame's return asks for them. It answers the rig, the job
// before, the relaunch's error and whom Recorded told.
func sendBack(t *testing.T, arm func(*unansweredRuns) *atomic.Bool) (rig *coordRig, before PendingRun,
	launchErr error, recorded string) {
	t.Helper()
	var runs *unansweredRuns
	rig, runs = unansweredRig(t, func(o *CoordinatorOptions) {
		o.Resume = relaunchThenBreak{relaunch: func(ctx context.Context, r PendingRun) {
			arm(runs).Store(true)
			req := launchReq(r.TurnID)
			req.Recorded = func(launch string) { recorded = launch }
			_, launchErr = rig.coordinator.Launch(ctx, rig.manager, req)
		}}
	})
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "first pass", InputTokens: 1000})
	before = rig.get("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	payload, ev := rig.completion("t1")
	_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
	rig.fireRetries()
	return rig, before, launchErr, recorded
}

// A LAUNCH THAT WROTE ITS ROW AND OPENED NO LOG ENDS ITS RUN. The row names the
// launch's job, launching, and nothing polls or claims a launching row, so left
// there it held the seat until the seat moved. The launch ends that job: the row
// gone, the seat free, the box the row named reclaimed, and the caller told the
// record held its notes, whose ending reports them.
//
// Mutation: return from Launch without ending the job, and the row stays.
func TestALaunchThatWroteItsRowAndOpenedNoLogEndsItsRun(t *testing.T) {
	t.Parallel()
	t.Run("a send-back", func(t *testing.T) {
		t.Parallel()
		rig, before, launchErr, recorded := sendBack(t, func(r *unansweredRuns) *atomic.Bool { return &r.log })
		unopened, ok := errors.AsType[*UnopenedLaunchError](launchErr)
		if !ok {
			t.Fatalf("the relaunch = %v, want it naming the job its row may hold", launchErr)
		}
		rig.finished("t1")
		if rig.coordinator.SeatHeldBySandbox("swe") {
			t.Error("the seat is held by a run that has no row")
		}
		if killed := rig.provider.KilledIDs(); !slices.Contains(killed, before.SandboxID) {
			t.Errorf("killed %v, want the box the row named, %s", killed, before.SandboxID)
		}
		if recorded != unopened.LaunchID {
			t.Errorf("the caller was told of %q, want the job the ended row named, %q", recorded,
				unopened.LaunchID)
		}
		if fired := rig.tick(); fired != 0 {
			t.Errorf("the poll fired %d completions for a run that has none", fired)
		}
	})
	t.Run("a turn's first launch", func(t *testing.T) {
		t.Parallel()
		rig, runs := unansweredRig(t, nil)
		runs.log.Store(true)
		req := launchReq("t1")
		var recorded string
		req.Recorded = func(launch string) { recorded = launch }
		_, err := rig.coordinator.Launch(t.Context(), rig.manager, req)
		unopened, ok := errors.AsType[*UnopenedLaunchError](err)
		if !ok {
			t.Fatalf("the launch = %v, want it naming the job its row may hold", err)
		}
		rig.finished("t1")
		if rig.coordinator.SeatHeldBySandbox("swe") {
			t.Error("the seat is held by a run that has no row")
		}
		if recorded != unopened.LaunchID {
			t.Errorf("the caller was told of %q, want the job the ended row named, %q", recorded,
				unopened.LaunchID)
		}
	})
}

// A LAUNCH WHOSE RESET NEVER LANDED LEAVES THE CLAIM TO ITS HAND-BACK. The
// reset may have landed, so the launch names its job and ends it; but the row
// still names the claimed job, which is the resumed turn's to hand back. Ended
// instead, a collected job's turn was lost for a write that changed nothing.
//
// Mutation: end the run under every launch rather than the launch's own job,
// and the claimed job's row is ended.
func TestALaunchWhoseResetNeverLandedLeavesTheClaimToItsHandBack(t *testing.T) {
	t.Parallel()
	rig, before, launchErr, recorded := sendBack(t, func(r *unansweredRuns) *atomic.Bool { return &r.row })
	if _, ok := errors.AsType[*UnopenedLaunchError](launchErr); !ok || !errors.Is(launchErr, errUnanswered) {
		t.Fatalf("the relaunch = %v, want the store's error naming the job its row may hold", launchErr)
	}
	got := rig.get("t1")
	if got.Status != StatusRunning || got.LaunchID != before.LaunchID || got.Ending != nil {
		t.Fatalf("the row is %s under %s (ending %+v), want the claimed job %s handed back running",
			got.Status, got.LaunchID, got.Ending, before.LaunchID)
	}
	if recorded != "" {
		t.Errorf("the caller was told the record holds its notes under %q; no write named the job", recorded)
	}
}
