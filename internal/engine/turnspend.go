package engine

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Spend is on the task: what one completed turn SEGMENT charges to the work
// item the turn is on (ADR-0022).
//
// # A segment, not a turn
//
// A turn that detaches a coding run completes more than once under one run id:
// the dispatch segment parks, and each collected run resumes a segment of its
// own — minutes later, often on another node. Each segment charges what IT
// spent, under an operation id naming the segment:
//
//	turn/<run id>/dispatch
//	turn/<run id>/resume/<launch id>
//
// so a segment re-run after a failed resume, or a record delivered twice,
// lands on one turn row and is counted once. ONLY THE DISPATCH SEGMENT COUNTS
// A TURN (`turns=1`): a resumed segment is more of the same turn, and counting
// it again would put three turns on a task one turn worked on.
//
// # What a segment's tokens are
//
// Its own phases, as their records state them — a phase that resumed across a
// park states both halves, which is why a suspended phase is paid by the
// segment it finished in and never twice — plus the workers it delegated to
// and the extension judge, whose calls are metered beside the phases rather
// than inside them. A resumed segment adds the collected coding run it resumed
// from, which no phase of the engine's own ran. REFLECTION IS NOT IN IT: the
// learning pass runs after the turn is over, on the seat's behalf rather than
// the item's, and charging it to whatever the turn was on would make a task's
// cost depend on how much its seat had to remember.
//
// # A segment charged to nothing hands its spend on
//
// A turn nothing at dispatch named an item for may still be charged at its end
// by a sole write, and a segment that parks cannot conclude that. So such a
// segment charges nothing and CARRIES what it spent on the suspended
// conversation (execstate.State.Uncharged), and the segment that finishes the
// turn pays it with its own if it is charged, turn count included. If it is
// not, nothing ever is — which is what an unattributed turn is.
//
// ONLY A NATIVE ITEM IS CHARGED: the counters are the engine's own tracker's
// rows. A turn on a Jira issue or a pull request is attributed on its events
// and charges nothing here.

// segmentCharge is what one completed segment charges, decided once.
type segmentCharge struct {
	// item is the work item the segment is charged to, nil for none.
	item *types.WorkItem
	// opID is the segment's operation id, and the turn row's.
	opID string
	// record is the turn record to publish when item is native.
	record tracker.TurnRecord
	// carry is what a parked segment charged to nothing hands on to the
	// segment that finishes the turn; nil otherwise.
	carry *execstate.Uncharged
}

// native reports a charge this engine's own tracker takes: one on a native
// work item. A turn on nothing charges nothing, and a turn on another
// tracker's item is attributed on its events and has no row here to add to.
func (c segmentCharge) native() bool {
	return c.item != nil && c.item.Backend == types.WorkNative
}

// segmentOpID is the operation id of one segment of a run.
func segmentOpID(runID, launch string, resumed bool) string {
	if !resumed {
		return "turn/" + runID + "/dispatch"
	}
	return "turn/" + runID + "/resume/" + launch
}

// chargeFor decides what this segment charges: its item from the turn's rules,
// and its spend from the runner's tally, the collected run it resumed from and
// whatever an earlier segment handed on.
func (t turnTelemetry) chargeFor(spend runner.Spend, res turn.Result, err error,
	ended time.Time,
) segmentCharge {
	item, _ := completedWorkItem(t.workItem, t.workItemBasis, t.written, res.Suspended)
	own := tracker.TurnSpend{
		Rounds:     spend.Rounds,
		Input:      spend.InputTokens + spend.WorkerInput + spend.JudgeInput + t.jobInput,
		Output:     spend.OutputTokens + spend.WorkerOutput + spend.JudgeOutput + t.jobOutput,
		CacheRead:  spend.CacheRead,
		CacheWrite: spend.CacheWrite,
		WallMs:     int(max(ended.Sub(t.startedAt), 0) / time.Millisecond),
		Workers:    spend.Workers,
		SentBack:   spend.SentBack,
	}
	if !t.resumed {
		own.Turns = 1
	}
	total := addUncharged(own, t.uncharged)
	charge := segmentCharge{item: item, opID: segmentOpID(t.runID, t.launchID, t.resumed)}
	if item == nil {
		if res.Suspended {
			charge.carry = unchargedOf(total)
		}
		return charge
	}
	charge.record = tracker.TurnRecord{
		Task: item.ID, Seat: t.handle, TurnID: t.runID, Trigger: t.trigger.Type,
		Outcome: segmentOutcome(res, err), Phases: spend.Phases, Spend: total,
	}
	return charge
}

// segmentOutcome is how a segment ended, in the vocabulary a task's turn list
// reads: parked on a coding run, failed, or the turn's own decision.
func segmentOutcome(res turn.Result, err error) string {
	switch {
	case res.Suspended:
		return "suspended"
	case err != nil || res.Decision == phase.Failed:
		return string(phase.Failed)
	}
	return string(res.Decision)
}

// addUncharged folds what an earlier segment handed on into this one's spend.
func addUncharged(s tracker.TurnSpend, u *execstate.Uncharged) tracker.TurnSpend {
	if u == nil {
		return s
	}
	s.Turns += u.Turns
	s.Rounds += u.Rounds
	s.Input += u.Input
	s.Output += u.Output
	s.CacheRead += u.CacheRead
	s.CacheWrite += u.CacheWrite
	s.WallMs += u.WallMs
	s.Workers += u.Workers
	s.SentBack += u.SentBack
	return s
}

// unchargedOf is a spend in the suspended conversation's own shape.
func unchargedOf(s tracker.TurnSpend) *execstate.Uncharged {
	return &execstate.Uncharged{
		Turns: s.Turns, Rounds: s.Rounds, Input: s.Input, Output: s.Output,
		CacheRead: s.CacheRead, CacheWrite: s.CacheWrite, WallMs: s.WallMs,
		Workers: s.Workers, SentBack: s.SentBack,
	}
}

// turnRecorder is the one write a segment's charge needs, declared by its one
// caller.
type turnRecorder interface {
	RecordTurn(ctx context.Context, opID string, turn tracker.TurnRecord) (tracker.WriteResult, error)
}

// recordTurnSpend publishes a segment's charge to its native work item.
//
// TELEMETRY NEVER FAILS THE WORK, so this returns nothing, on the terms
// [Engine.publishTurnCompleted] states: the turn has finished and its result is
// already the caller's answer. A charge that could not be written is logged
// naming the item and the segment — the spend is still on the seat's counters
// and in the usage domain; what is lost is the task's share of it.
func (e *Engine) recordTurnSpend(ctx context.Context, charge segmentCharge) {
	if !charge.native() {
		return
	}
	writer := e.TrackerWriter()
	if writer == nil {
		// A NATIVE ITEM ON A NODE WITH NO NATIVE TRACKER is a company that
		// moved its tracker off the engine while the turn ran: there are
		// no rows here to charge.
		return
	}
	e.chargeSegment(ctx, writer.As(charge.record.Seat, tracker.AuthorAgent,
		tracker.Provenance{TurnID: charge.record.TurnID}), charge)
}

// chargeSegment is [Engine.recordTurnSpend] over the write it needs.
func (e *Engine) chargeSegment(ctx context.Context, w turnRecorder, charge segmentCharge) {
	if _, err := w.RecordTurn(ctx, charge.opID, charge.record); err != nil {
		level := log.WarnContext
		if errors.Is(err, context.Canceled) {
			level = log.InfoContext
		}
		level(ctx, "turn_spend_unrecorded", "turn_id", charge.record.TurnID,
			"op_id", charge.opID, "task", charge.item.ID, "key", charge.item.Key,
			"tokens", charge.record.Spend.Tokens(), "error", err.Error(),
			"detail", "the segment's spend is on the seat's counters and the "+
				"usage history, and not on the task it worked on")
	}
}
