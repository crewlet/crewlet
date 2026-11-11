package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A PERSON'S STOP, CARRIED OUT ON THE SEAT'S RUNNING WORK.
//
// A pause with `stop_running` asks for the seat's work in flight to end. Its
// running TURN hears it at the next round, through the turn's own fence
// (seatpause.go). The EXECUTOR the turn launched has no rounds the engine sees,
// so the node holding the seat ends it through the coordinator
// ([sandbox.Coordinator.StopSeat]) — at every moment a stop can reach the seat:
// the pause landing on a watch that holds it ([Engine.applySeatPause]), a
// complete re-read of the pauses ([Engine.adoptSeatPauses]), and the seat's
// acquisition while it is paused ([Engine.prepareSeatSandbox]). The call is
// idempotent, so reaching one run twice costs a listing.
//
// AND THE TURN THAT WAS WAITING ON THE RUN IS CLOSED AS STOPPED, in the shape a
// turn stopped at its next round takes — its completion carrying `stopped`, and
// [types.AgentTurnStopped] naming who — by whichever frame still holds it: the
// launching frame, where the stop beat the turn's park ([Engine.persistSuspension]),
// or the run's ending, where the turn had parked ([Engine.closeStoppedTurn]).
//
// THE STOP IS NOT THE SEAT FENCE. The fence is about who holds a seat, and a
// seat that moves on an ordinary rebalance must not kill a run (see
// seat.Host.Fence); a stop is a person's decision about the work.

// stopSeatRuns ends the launching and running runs of a seat whose pause asked
// for its running work to stop, where this node holds the seat.
//
// A SEAT THIS NODE DOES NOT HOLD IS LEFT TO ITS HOLDER, which reads the same
// pause and stops them under its own lease; a seat nobody holds is stopped by
// whoever acquires it, before its mailbox opens. A node with no sandbox runtime
// has no runs to stop.
//
// SYNCHRONOUS, on the caller's frame, which waits on no job: the frame DECIDES
// each stop — a few writes under the seat's lease — and closes the run's
// tool-bridge session. A run whose job started in a box is finished by the
// coordinator's own retry, which it arms at once and owns: the job's end,
// which is waiting for it (at most [sandbox.JobEndBudget]), the read of what
// it spent and of its session, the box's reclaim and the stop's announcement
// — on this node's host or another's, and whatever
// `conversation_session.enabled` says ([sandbox.Coordinator.StopSeat]). Only a
// stop with no job to end, a launch stopped before its command was recorded,
// is finished on the frame, its box reclaimed within the coordinator's
// teardown grace. The pause it follows is rare — a watch that waits on it
// delivers the next change a moment later — where a goroutine per stop would
// have a lifetime nobody owns.
func (e *Engine) stopSeatRuns(ctx context.Context, seat uuid.UUID, p coord.SeatPause) {
	if !p.StopRunning {
		return
	}
	rt := e.sandbox.Load()
	if rt == nil {
		return
	}
	handle, ok := e.seatHandle(seat.String())
	if !ok {
		return
	}
	if _, held := e.launchFence(handle); !held {
		return
	}
	if err := rt.coordinator.StopSeat(ctx, handle, runStopOf(p)); err != nil {
		log.WarnContext(ctx, "seat_runs_not_stopped", "seat", handle, "by", p.By, "error", err,
			"detail", "the seat's executor could not be stopped yet; the pause "+
				"stops it when this node next reads the pauses, or when the seat is acquired")
	}
}

// runStopOf is who a pause names, as a stop records it on the run.
func runStopOf(p coord.SeatPause) sandbox.RunStop {
	return sandbox.RunStop{By: p.By, ByKind: string(p.ByKind), OperatorID: p.OperatorID, Reason: p.Reason}
}

// pauseOfStop is the pause a run's recorded stop came from, as the turn's
// stop records name it ([stopError]).
func pauseOfStop(s sandbox.RunStop) coord.SeatPause {
	return coord.SeatPause{By: s.By, ByKind: iam.ActorKind(s.ByKind), OperatorID: s.OperatorID,
		Reason: s.Reason, StopRunning: true}
}

// closeStoppedAtPark closes a turn whose park a person's stop refused, for the
// frame that was parking it — a dispatch, or a resume that relaunched — from
// the refused park itself ([Engine.persistSuspension]): the segment that would
// have parked has already ended and charged ([Engine.endSegment]), and the
// close is the turn's end after it.
//
// CHARGED FROM WHAT THE PARK LEFT UNPAID, never from the parking segment's own
// telemetry, which that segment's charge has already paid out of: the close's
// is fresh ([turnTelemetry.closingAfter]) — no job of its own, no auxiliary
// call, no span — and carries only what the parking segment handed on, charged
// to nothing, to the segment that ends the turn: its carry, which only the park
// the stop refused would have written, and its measurement of the turn. Closed
// with the parking segment's telemetry instead, the close charged the item that
// segment's job, bridged spend, auxiliary calls and wall time a second time,
// under the stop's own segment id, and a turn the park charged to nothing lost
// that segment's spend outright.
func (e *Engine) closeStoppedAtPark(ctx context.Context, tel turnTelemetry, parked segmentCharge,
	stop error,
) {
	at := time.Now().UTC()
	e.closeStopped(ctx, tel.closingAfter(parked, at), stop, at)
}

// closeStopped publishes the completion that ends a turn a person stopped while
// it was parked on its run: a segment that ran no phase, ended on the stop.
//
// ITS OWN SEGMENT, after the one that parked: that segment's completion said
// `suspended`, and this one is the turn's end — `stopped`, never `failed`
// ([Engine.publishTurnCompleted]) — charged to the turn's work item with
// whatever tel says the segment pays: nothing of its own, and what the segments
// before it handed on — for a closing the run's ending made, the run's
// collected spend too ([turnTelemetry.chargeFor]). tel is the close's own,
// never a segment's that has charged: see [Engine.closeStoppedAtPark].
func (e *Engine) closeStopped(ctx context.Context, tel turnTelemetry, stop error, at time.Time) {
	tel.closesStop = true
	var spend runner.Spend
	var res turn.Result
	charge := tel.chargeFor(spend, res, stop, at)
	e.endSegment(ctx, tel, spend, res, stop, charge)
}

// closeStoppedTurn ends the turn of a run a person stopped — the coordinator's
// [sandbox.CoordinatorOptions.CloseStopped] — and answers how it ended, for the
// executor log: stopped, by whom and why, whether its reply had reached anybody
// as the turn's own calls say ([sandbox.RunCalls]), and what the engine told
// the colleague whose ask the turn took up.
//
// A TURN THAT PARKED ON THE RUN IS CLOSED ON THE RECORD FIRST ([Engine.closeParkedTurn]).
// One that did not — a launch stopped before its turn could park, the turn
// still in the frame that launched it — is closed on the record by that frame
// when its park is refused; its colleague's answer and its end are this
// step's all the same, because only the run's ending records them after the
// stop's read has kept the job's session.
//
// FROM THE ROW ALONE, because this can run on any node that finishes the run's
// ending — the one that stopped it, or the seat's next holder after a crash —
// long after the turn's own frames are gone.
//
// A PARKED STATE THIS BUILD CANNOT READ CLOSES NOTHING ON THE RECORD and is
// logged, and the turn still ends: its colleague is answered and its end
// owed, since neither reads the parked state. Refusing the ending over a turn
// record would keep a person's stopped run in the store for good.
func (e *Engine) closeStoppedTurn(ctx context.Context, run sandbox.PendingRun,
	bridged sandbox.RunCalls) (executorkeep.Ending, error) {

	if run.Ending == nil || run.Stop == nil {
		return executorkeep.Ending{}, fmt.Errorf("engine: run %s's ending is no stop", run.TurnID)
	}
	company := e.Company()
	stop := &stopError{pause: pauseOfStop(*run.Stop), handle: run.AgentHandle}
	if len(run.ExecuteState) > 0 {
		if err := e.closeParkedTurn(ctx, company, run, stop); err != nil {
			return executorkeep.Ending{}, err
		}
	}
	// AND THE COLLEAGUE WHOSE ASK THE TURN TOOK UP, told it was stopped: this
	// is the turn's end, and the frame that saw the ask answered nothing for
	// a turn that parked, or that launched and was stopped before it could
	// (a2a.go). Best effort, never this step's error — the turn is over — and
	// once: the end this answers is owed on the row, and a repeat of the
	// ending finds it there.
	answered := e.answerColleague(ctx, company, answeringRun(run), run.ColleagueAsk, turn.Result{}, stop)
	return e.runEnding(ctx, company, run, bridged, answered), nil
}

// closeParkedTurn closes ON THE RECORD the turn that was parked on a run a
// person stopped: its `stopped` completion and its AgentTurnStopped, under the
// ending's identity, charged what the run had spent.
//
// The turn's telemetry is assembled as a resume assembles it
// ([Engine.describeResume]), so the closing segment is charged and stated
// exactly as a resumed segment that the stop reached at its first round would
// have been, and UNDER THE ENDING'S IDENTITY ([turnTelemetry.closing]): an
// attempt that repeats the close publishes the same records, and the work
// item's charge is keyed on the segment, which the tracker's ledger collapses.
func (e *Engine) closeParkedTurn(ctx context.Context, company *Company, run sandbox.PendingRun,
	stop *stopError) error {

	state, ok, err := execstate.Decode(run.ExecuteState)
	if err != nil || !ok {
		log.WarnContext(ctx, "stopped_turn_not_closed", "turn_id", run.TurnID, "seat", run.AgentHandle,
			"error", errors.Join(err, errNoParkedState(ok)),
			"detail", "the run is ended and its stop announced, and its turn's end is recorded, but its "+
				"parked turn could not be read to close it on the record")
		return nil
	}
	if company == nil || company.Org == nil {
		return fmt.Errorf("engine: closing turn %s, which a person stopped, on a node with no "+
			"applied company", run.TurnID)
	}
	seat := company.Org.AgentSeatByHandle(run.AgentHandle)
	in := resumeInput{
		Company: company, Run: run, State: state,
		Turn:        resumedTurn(run, seat, company.Org, e.rowHolds(run)),
		InputTokens: run.Stop.InputTokens, OutputTokens: run.Stop.OutputTokens,
		CacheRead: run.Stop.CacheReadTokens, CacheWrite: run.Stop.CacheWriteTokens,
		Engine: run.Launch.Bridged.Plus(sandbox.EngineSpend{Aux: run.Stop.Condensed}),
	}
	tel, err := e.describeResume(ctx, company, in)
	if err != nil {
		return fmt.Errorf("engine: closing turn %s, which a person stopped: %w", run.TurnID, err)
	}
	at := run.Ending.At.UTC()
	// THE TURN'S OWN TRACE, the one its parked segment ran under, and the
	// ending's instant as this segment's whole span: nothing of it ran.
	tel.trace = events.TraceContext{TraceID: run.TraceID, ParentSpanID: run.SpanID}
	tel.startedAt = at
	tel.closing = &closingIdentity{turnID: run.TurnID, ending: run.Ending.ID, at: at}
	e.closeStopped(ctx, tel, stop, at)
	e.observe(ctx, tel.stamp(turnStoppedEvent(run.AgentHandle, tel.role, tel.agentID,
		run.TurnID, run.WorkKey, stop.pause, tel.trace)))
	return nil
}

// errNoParkedState is the reason a row with no parked state cannot be closed,
// nil where it has one.
func errNoParkedState(ok bool) error {
	if ok {
		return nil
	}
	return errors.New("the row carries no parked turn state")
}
