package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/tracker"
)

var nativeItem = types.WorkItem{Backend: types.WorkNative, ID: "t-1", Key: "ENG-1", Project: "ENG"}

// segmentSpend is one segment's runner tally, with every figure a charge reads
// set to something distinct.
func segmentSpend(scale int) runner.Spend {
	return runner.Spend{
		InputTokens: 1000 * scale, OutputTokens: 200 * scale,
		CacheRead: 300 * scale, CacheWrite: 40 * scale,
		Workers: 2, WorkerInput: 500 * scale, WorkerOutput: 60 * scale,
		Judged: 1, JudgeInput: 70 * scale, JudgeOutput: 9 * scale,
		Rounds: 4, SentBack: 1, Phases: []string{"execute", "review"},
	}
}

// rollupOf is what a segment's own published records add up to: its phases'
// tokens, its workers', its judge's, and the job it collected. The per-seat
// history and the turn's completion event are built from these same records,
// so no charge to a task may ever come to more than they do.
func rollupOf(s runner.Spend, jobIn, jobOut int) int {
	return s.Total() + s.WorkerTokens() + s.JudgeTokens() + jobIn + jobOut
}

func dispatchTel(item *types.WorkItem) turnTelemetry {
	t := turnTelemetry{
		handle: "dev", runID: "run-1", trigger: types.Trigger{Type: "work_item"},
		startedAt: time.Unix(1_700_000_000, 0).UTC(), written: &turnctx.Written{},
	}
	if item != nil {
		copied := *item
		t.workItem, t.workItemBasis = &copied, types.BasisTrigger
	}
	return t
}

func resumeTel(item *types.WorkItem, carried *execstate.Uncharged, jobIn, jobOut int) turnTelemetry {
	t := dispatchTel(item)
	if item != nil {
		t.workItemBasis = types.BasisResume
	}
	t.resumed, t.launchID = true, "launch-1"
	t.jobInput, t.jobOutput, t.uncharged = jobIn, jobOut, carried
	return t
}

// A TASK'S SPEND NEVER EXCEEDS THE ROLLUP.
//
// What a turn charges its work item is a SHARE of what the turn's own records
// state it spent — the phases, the workers, the judge and the coding runs it
// collected — and, across every segment of a turn, never more than their sum.
// Folding anything in twice (a resumed phase's pre-park half, a worker counted
// inside its host phase and again beside it) would show a task costing more
// than the company paid for it.
func TestTaskSpendNeverExceedsTheRollup(t *testing.T) {
	t.Parallel()
	ended := time.Unix(1_700_000_060, 0).UTC()

	for _, tc := range []struct {
		name string
		item *types.WorkItem
		// sole is the item the turn's writes committed to, for a turn
		// dispatch named nothing for.
		sole *types.WorkItem
	}{
		{name: "an item from the trigger", item: &nativeItem},
		{name: "an item from a sole write", sole: &nativeItem},
		{name: "no item at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := segmentSpend(1), segmentSpend(3)
			jobIn, jobOut := 4000, 700

			dispatch := dispatchTel(tc.item)
			parked := dispatch.chargeFor(first, turn.Result{Suspended: true}, nil, ended)

			resume := resumeTel(tc.item, parked.carry, jobIn, jobOut)
			if tc.sole != nil {
				resume.written = turnctx.WrittenFrom([]types.WorkItem{*tc.sole}, false)
			}
			finished := resume.chargeFor(second, turn.Result{Decision: phase.Done}, nil, ended)

			charged := 0
			for _, c := range []segmentCharge{parked, finished} {
				if c.native() {
					charged += c.record.Spend.Tokens()
				}
			}
			rollup := rollupOf(first, 0, 0) + rollupOf(second, jobIn, jobOut)
			if charged > rollup {
				t.Fatalf("the task was charged %d tokens for a turn whose records "+
					"add up to %d", charged, rollup)
			}
			// AND PAID IN FULL wherever the turn is charged at all: a
			// share short of the rollup is spend that went nowhere.
			if (tc.item != nil || tc.sole != nil) && charged != rollup {
				t.Fatalf("the task was charged %d tokens of the %d its turn "+
					"spent", charged, rollup)
			}
			if tc.item == nil && tc.sole == nil && charged != 0 {
				t.Fatalf("a turn on nothing charged %d tokens", charged)
			}
		})
	}
}

// A SUSPENDED TURN IS COUNTED ONCE AND PAID IN FULL.
//
// One turn, two segments, one turn on the task: only the dispatch segment
// counts a turn, the resumed segment pays for the coding run it collected, and
// a turn charged by its sole write — which a parked segment cannot conclude —
// is paid for its first half by the segment that finishes it.
func TestASuspendedTurnIsCountedOnceAndPaidInFull(t *testing.T) {
	t.Parallel()
	ended := time.Unix(1_700_000_060, 0).UTC()

	t.Run("charged at dispatch", func(t *testing.T) {
		parked := dispatchTel(&nativeItem).chargeFor(segmentSpend(1),
			turn.Result{Suspended: true}, nil, ended)
		finished := resumeTel(&nativeItem, nil, 4000, 700).chargeFor(segmentSpend(1),
			turn.Result{Decision: phase.Done}, nil, ended)
		if !parked.native() || !finished.native() {
			t.Fatal("a segment of a turn on a native item charged nothing")
		}
		if parked.record.Spend.Turns+finished.record.Spend.Turns != 1 {
			t.Fatalf("one turn counted %d times across its segments",
				parked.record.Spend.Turns+finished.record.Spend.Turns)
		}
		if parked.carry != nil {
			t.Errorf("a charged segment handed on %+v — it would be paid twice", parked.carry)
		}
		if parked.record.Outcome != "suspended" || finished.record.Outcome != "done" {
			t.Errorf("outcomes %q/%q, want the park told apart from the end",
				parked.record.Outcome, finished.record.Outcome)
		}
		if parked.opID == finished.opID {
			t.Errorf("both segments recorded under %q, so the second is dropped as "+
				"a redelivery of the first", parked.opID)
		}
		if got, want := finished.record.Spend.Input, 1000+500+70+4000; got != want {
			t.Errorf("the resumed segment's input is %d, want its phases, workers, "+
				"judge and the collected job: %d", got, want)
		}
	})

	t.Run("charged at the end by a sole write", func(t *testing.T) {
		parked := dispatchTel(nil).chargeFor(segmentSpend(1),
			turn.Result{Suspended: true}, nil, ended)
		if parked.native() || parked.carry == nil || parked.carry.Turns != 1 {
			t.Fatalf("a parked segment on nothing charged %v and handed on %+v, "+
				"want nothing charged and the turn handed on", parked.native(), parked.carry)
		}
		resume := resumeTel(nil, parked.carry, 4000, 700)
		resume.written = turnctx.WrittenFrom([]types.WorkItem{nativeItem}, false)
		finished := resume.chargeFor(segmentSpend(1), turn.Result{Decision: phase.Done}, nil, ended)
		if !finished.native() || finished.record.Task != "t-1" {
			t.Fatalf("the finishing segment charged %+v, want its sole write", finished.record)
		}
		if finished.record.Spend.Turns != 1 {
			t.Errorf("the turn counted %d times, want the dispatch's one carried",
				finished.record.Spend.Turns)
		}
		if got, want := finished.record.Spend.Input, 2*(1000+500+70)+4000; got != want {
			t.Errorf("the turn's input charged is %d, want both halves and the job: %d", got, want)
		}
	})
}

// AN UNATTRIBUTED TURN CHARGES NOTHING.
//
// A turn on nothing, or on another tracker's item, has no native row to add
// to — and a charge that guessed an item would put one turn's cost on work it
// was not doing.
func TestAnUnattributedTurnChargesNothing(t *testing.T) {
	t.Parallel()
	ended := time.Unix(1_700_000_060, 0).UTC()
	jira := types.WorkItem{Backend: types.WorkJira, ID: "10001", Key: "OPS-1"}
	two := dispatchTel(nil)
	two.written.Add(nativeItem)
	two.written.Add(types.WorkItem{Backend: types.WorkNative, ID: "t-2", Key: "ENG-2"})

	for name, c := range map[string]segmentCharge{
		"on nothing": dispatchTel(nil).chargeFor(segmentSpend(1),
			turn.Result{Decision: phase.Done}, nil, ended),
		"on a jira issue": dispatchTel(&jira).chargeFor(segmentSpend(1),
			turn.Result{Decision: phase.Done}, nil, ended),
		"writing two items": two.chargeFor(segmentSpend(1),
			turn.Result{Decision: phase.Done}, nil, ended),
	} {
		if c.native() {
			t.Errorf("a turn %s charged %+v to the native tracker", name, c.record)
		}
		if c.carry != nil {
			t.Errorf("a finished turn %s handed on %+v, which nothing will ever pay", name, c.carry)
		}
	}
}

// A SEGMENT'S OPERATION ID NAMES THE SEGMENT, and nothing else.
//
// The id is the turn row's own, so a resume re-run after a failed attempt must
// reach the SAME id — its spend is then counted once — while two launches of
// one turn must reach two.
func TestASegmentsOperationIDIsStableAcrossARetry(t *testing.T) {
	t.Parallel()
	first := dispatchTel(&nativeItem)
	first.resumed, first.launchID = true, "launch-1"
	retry := first
	retry.startedAt = retry.startedAt.Add(time.Minute)
	ended := time.Unix(1_700_000_600, 0).UTC()
	if a, b := first.chargeFor(segmentSpend(1), turn.Result{Decision: phase.Done}, nil, ended),
		retry.chargeFor(segmentSpend(2), turn.Result{Decision: phase.Done}, nil, ended); a.opID != b.opID {
		t.Fatalf("a re-run segment named a new operation: %q then %q", a.opID, b.opID)
	}
	for _, pair := range [][2]string{
		{segmentOpID("run-1", "", false), segmentOpID("run-1", "launch-1", true)},
		{segmentOpID("run-1", "launch-1", true), segmentOpID("run-1", "launch-2", true)},
		{segmentOpID("run-1", "", false), segmentOpID("run-2", "", false)},
	} {
		if pair[0] == pair[1] {
			t.Errorf("two segments share the operation %q", pair[0])
		}
	}
}

// fakeRecorder is a turn recorder that remembers what it was asked.
type fakeRecorder struct {
	calls []string
	err   error
}

func (f *fakeRecorder) RecordTurn(_ context.Context, opID string, _ tracker.TurnRecord) (tracker.WriteResult, error) {
	f.calls = append(f.calls, opID)
	return tracker.WriteResult{}, f.err
}

// A CHARGE THAT CANNOT BE WRITTEN DOES NOT FAIL THE TURN.
//
// The turn has finished and its answer is delivered; the charge is telemetry,
// and a tracker that refuses it costs the task its share and nothing else.
func TestAnUnwritableChargeIsLoggedNotRaised(t *testing.T) {
	t.Parallel()
	c := dispatchTel(&nativeItem).chargeFor(segmentSpend(1),
		turn.Result{Decision: phase.Done}, nil, time.Unix(1_700_000_060, 0).UTC())
	rec := &fakeRecorder{err: errors.New("the tracker is behind")}
	(&Engine{}).chargeSegment(t.Context(), rec, c)
	if len(rec.calls) != 1 || rec.calls[0] != "turn/run-1/dispatch" {
		t.Fatalf("the recorder was asked %v, want the dispatch segment once", rec.calls)
	}
}
