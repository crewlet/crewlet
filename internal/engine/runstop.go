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

// A PERSON'S STOP, CARRIED OUT ON THE SEAT'S RUNNING WORK. A pause with
// `stop_running` ends the seat's work in flight. Its running turn hears it at
// the next round (seatpause.go); the executor the turn launched has no rounds
// the engine sees, so the seat's holder ends it through the idempotent
// [sandbox.Coordinator.StopSeat] wherever a stop reaches the seat:
// [Engine.applySeatPause], [Engine.adoptSeatPauses], [Engine.prepareSeatSandbox].
// The turn waiting on the run closes as stopped, as one stopped at a round
// does, from whichever frame holds it: [Engine.persistSuspension] where the
// stop beat the park, [Engine.closeStoppedTurn] where the turn had parked.
//
// The stop is not the seat fence: a seat moved by an ordinary rebalance must
// not kill a run (seat.Host.Fence); a stop is a person's decision.

// stopSeatRuns ends the launching and running runs of a seat whose pause asked
// for its running work to stop, where this node holds the seat. A seat held
// elsewhere is its holder's to stop; one nobody holds, its next acquirer's,
// before its mailbox opens.
//
// Synchronous and waiting on no job: the frame decides each stop (a few writes
// under the seat's lease) and closes the run's tool-bridge session; the
// coordinator's own retry finishes a job started in a box — its end (at most
// [sandbox.JobEndBudget]), spend and session read, reclaim, announcement — on
// any host ([sandbox.Coordinator.StopSeat]). Only a launch stopped before its
// command was recorded finishes here, within the teardown grace. A pause is
// rare and its watch waits only a moment; a goroutine per stop would be unowned.
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

// closeStoppedAtPark closes a turn whose park a person's stop refused, from the
// refused park itself ([Engine.persistSuspension]), after the parking segment
// has ended and charged ([Engine.endSegment]).
//
// Charged from what the park left unpaid: the close's telemetry is fresh
// ([turnTelemetry.closingAfter]) — no job, auxiliary call or span — carrying
// only what the parking segment handed on uncharged, its carry and its
// measurement of the turn. The parking segment's own telemetry would charge
// its job, bridged spend, auxiliary calls and wall time twice, and lose its
// spend outright where the park charged it to nothing.
func (e *Engine) closeStoppedAtPark(ctx context.Context, tel turnTelemetry, parked segmentCharge,
	stop error,
) {
	at := time.Now().UTC()
	e.closeStopped(ctx, tel.closingAfter(parked, at), stop, at)
}

// closeStopped publishes the completion that ends a turn a person stopped while
// it was parked on its run: a segment of its own after the `suspended` one,
// which ran no phase and ends the turn `stopped`, never `failed`
// ([Engine.publishTurnCompleted]). It is charged to the turn's work item with
// what tel says it pays ([turnTelemetry.chargeFor]): nothing of its own, what
// earlier segments handed on and, for a close the run's ending made, the run's
// collected spend. tel is the close's own, never a charged segment's
// ([Engine.closeStoppedAtPark]).
func (e *Engine) closeStopped(ctx context.Context, tel turnTelemetry, stop error, at time.Time) {
	tel.closesStop = true
	var spend runner.Spend
	var res turn.Result
	charge := tel.chargeFor(spend, res, stop, at)
	e.endSegment(ctx, tel, spend, res, stop, charge)
}

// closeStoppedTurn ends the turn of a run a person stopped
// ([sandbox.CoordinatorOptions.CloseStopped]) and answers, for the executor
// log, who stopped it and why, whether its reply reached anybody
// ([sandbox.RunCalls]) and what the colleague whose ask it took up was told.
//
// A turn parked on the run is closed on the record here ([Engine.closeParkedTurn]);
// one stopped before its park is closed by its launching frame when the park
// is refused. Its colleague's answer and its end are this step's either way:
// only the run's ending records them. It reads the row alone, since any node
// may finish the ending long after the turn's frames are gone. A parked state
// this build cannot read closes nothing on the record, but the turn still ends:
// refusing the ending would keep a person's stopped run in the store for good.
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
	// The colleague whose ask the turn took up is told it was stopped: the
	// frame that saw the ask answered nothing for a turn that parked or was
	// stopped at launch (a2a.go). Best effort, never this step's error, and
	// once: a repeat of the ending finds the owed end on the row.
	answered := e.answerColleague(ctx, company, answeringRun(run), run.ColleagueAsk, turn.Result{}, stop)
	return e.runEnding(ctx, company, run, bridged, answered), nil
}

// closeParkedTurn closes ON THE RECORD the turn that was parked on a run a
// person stopped: its `stopped` completion and its AgentTurnStopped, charged
// what the run had spent.
//
// Its telemetry is assembled as a resume's ([Engine.describeResume]), so the
// close is charged and stated as a resumed segment stopped at its first round,
// under the ending's identity ([turnTelemetry.closing]): a repeated close
// publishes the same records, and the item's charge, keyed on the segment,
// collapses in the tracker's ledger.
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
	// The parked segment's trace, and the ending's instant as this segment's
	// whole span: nothing of it ran.
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
