package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
)

// RetireSeat ends every run of a seat that left the company, called by the mailbox
// retirement, the one moment such runs are known to be nobody's (absent the whole
// grace; the caller HOLDS ITS LEASE). Until then they are kept (a seat made an
// agent's again returns to its questions and jobs). Runs under ctx, NOT detached (the
// lease lasts the caller's budget); an unended run is an error and the next tick
// retries. Each ending is FINISHED BY THIS CALL ([Coordinator.endRun], mayAsk): the
// host's retry needs the seat's lease, which [Coordinator.ReleaseSeat] drops. A run
// whose log holds a call a collection would wait for (drain.go) is not ended: its
// session is ended to new calls and it is answered as waiting ([SeatRunsWaiting])
// beside any error, keeping the mailbox (waiting in the loop held every later
// ending). Runs are found by seat id ([PendingStore.ListActiveForSeatID]), never
// handle (a broker-found mailbox has none).
func (c *Coordinator) RetireSeat(ctx context.Context, seat uuid.UUID, handle, owner string, epoch int64,
) (SeatRunsWaiting, error) {
	runs, err := c.pending.ListActiveForSeatID(ctx, seat)
	if err != nil {
		return SeatRunsWaiting{}, fmt.Errorf("sandbox: listing the runs of retired seat %s: %w",
			seatLabel(seat, handle), err)
	}
	if handle == "" && len(runs) > 0 {
		// ONE HANDLE PER ID (the id derives from it).
		handle = runs[0].AgentHandle
	}
	label := seatLabel(seat, handle)
	fence := Fence{Owner: owner, Epoch: epoch}
	var (
		errs []error
		wait seatWait
	)
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("sandbox: run %s of retired seat %s was not ended: %w",
				run.TurnID, label, err))
			break
		}
		if outranked(run, fence) {
			// Claimed under a newer lease than the retirement holds (which a held
			// lease rules out): left to that owner, not killed under it.
			errs = append(errs, fmt.Errorf("sandbox: run %s of retired seat %s is owned by a newer "+
				"lease (epoch %d) than the retirement's (%d)", run.TurnID, label, run.OwnerEpoch, epoch))
			continue
		}
		// ITS CALLS STILL RUNNING FIRST, its session ended to new ones: such a run
		// waits for the next tick, one with an unreadable log was not ended.
		waiting, _, logErr := c.callsStillRunning(ctx, run)
		if logErr != nil {
			errs = append(errs, fmt.Errorf("sandbox: run %s of retired seat %s was not ended: %w",
				run.TurnID, label, logErr))
			continue
		}
		if len(waiting) > 0 {
			wait.add(run, waiting)
			continue
		}
		// A box that could not be reclaimed keeps its run's record, and the
		// seat its mailbox: the next tick retries, each within the
		// retirement's budget, until the box is gone. Nothing else waits on
		// it (a settle leaves such a box to its host's sweep, off its own
		// record).
		if err := c.reclaimBox(ctx, ctx, run); err != nil {
			errs = append(errs, fmt.Errorf("sandbox: run %s of retired seat %s was not ended: %w",
				run.TurnID, label, err))
			continue
		}
		// THE SAME ENDING EVERY OTHER PATH MAKES, keeping the stop report out of
		// this function: a retirement does not use [Coordinator.finish] (it keeps
		// the record of an unreclaimed box and runs under the caller's context) but
		// the delete and report are one helper both reach, so a gone seat tells
		// whoever holds something up for its turn without this path remembering.
		// AND THE SAME ANNOUNCEMENT through the ending's note: a held reply goes
		// back to the seat's inbox before the record, like every ending's.
		decided, err := c.endRun(ctx, run, ending{
			fence: fence, whileIn: Active, launch: EveryLaunch,
			note: &failureNote{reason: types.ExecutorFailureSeatRemoved,
				detail: "the seat left the company's agent seats — removed from the chart, or " +
					"made a person's and not an agent's again within the retirement grace — so " +
					"its run was ended; any work it pushed is on its branch"},
		}, true)
		if err != nil {
			errs = append(errs, fmt.Errorf("sandbox: finishing run %s of retired seat %s: %w",
				run.TurnID, label, err))
			continue
		}
		if decided == nil {
			// Settled by somebody else since the listing, who announced a loss.
			continue
		}
		log.InfoContext(ctx, "sandbox_retired_seat_run_ended",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "seat", seat.String(),
			"status", run.Status, "sandbox_id", run.SandboxID)
	}
	if handle != "" {
		// Nothing to release for a seat no handle names: this node counted no run
		// of it.
		c.ReleaseSeat(handle)
	}
	return wait.answer(), errors.Join(errs...)
}

// seatLabel is what a retirement's errors call a seat: its handle, beside the
// id it is retired by, or the id alone for a seat nothing names any more.
func seatLabel(seat uuid.UUID, handle string) string {
	if handle == "" {
		return seat.String()
	}
	return fmt.Sprintf("%q (%s)", handle, seat)
}

// ReleaseSeat stops tracking a seat's runs.
//
// NOTHING IS TORN DOWN: a detached run belongs to its row, not this process, and
// the next owner recovers it through RecoverSeat; reaping the box would destroy work
// the successor is about to resume. AND NOTHING OUT MOVES THE SEAT'S COUNTS AFTER
// IT: the writes out on its runs go with its counts, so a later answer moves
// nothing and this node's recovery counts each run once where it finds it
// (countwrite.go).
func (c *Coordinator) ReleaseSeat(handle string) {
	// The seat's answer-attempt counts go with it: the successor gets a clean set
	// per inherited run, and this node will never be offered a delivery for the
	// seat again ([MaxAnswerAttempts]).
	c.releaseAnswerAttempts(handle)
	// AND ANY ANSWER IT WAS STILL RESUMING: it stays recorded on the run, driven by
	// the successor's recovery.
	c.releaseOwed(handle)
	c.mu.Lock()
	delete(c.runs, handle)
	// AND THE WRITES OUT ON THE SEAT'S RUNS, same step: a later answer moves
	// nothing and a recovery here counts each run afresh (countwrite.go).
	maps.DeleteFunc(c.writing, func(_ string, w *runWrites) bool { return w.handle == handle })
	// AND ITS SLOTS AND HELD RUNS with them: a recovery here counts afresh.
	delete(c.reserved, handle)
	delete(c.freed, handle)
	delete(c.asideFull, handle)
	maps.DeleteFunc(c.heldRuns, func(_ string, r heldRun) bool { return r.handle == handle })
	maps.DeleteFunc(c.waitingRuns, func(_ string, r heldRun) bool { return r.handle == handle })
	// AND A STOP OF ONE OF ITS LAUNCHES NO FRAME TOOK: its turns run on the next
	// holder.
	maps.DeleteFunc(c.launchStops, func(_ string, s launchStop) bool { return s.handle == handle })
	// AND ANY COMPLETION IT WAITED TO RE-FIRE: the run is unclaimed, and the box's
	// waiter announces it to the successor, or, while that waiter cannot, the delivery
	// handed back does (renew.go). Every refusal this node acknowledged is on the
	// job's record ([LaunchRecord.RefusalOwed]), which the successor's recovery
	// re-arms ([Coordinator.rearmOwed]), including a job no collection had read since
	// its host's store went silent, which no waiter announces while it is silent.
	for turnID, kept := range c.refused {
		if kept.ev.AgentHandle == handle {
			c.dropKeptLocked(turnID)
		}
	}
	// AND EVERY RESUME OF ITS RUNS WAITING HERE FOR THE OPERATOR: the successor's
	// attempts say whether one still does ([Coordinator.ResumesBlocked]).
	maps.DeleteFunc(c.blocked, func(_ string, b blockedResume) bool { return b.agent == handle })
	// AND THE CLOCK ON ANY HOST WHOSE STORE ALONE WAS SILENT: the successor's
	// collections start their own. NOT A RE-FIRE'S ANNOUNCEMENT STILL OWED
	// ([refirePublish]): it needs no lease and reaches the successor on the control
	// topic, nothing else telling it of the job (the delivery was acknowledged and
	// the host's waiter announces nothing while its store is silent); made until one
	// lands.
	maps.DeleteFunc(c.unsettled, func(_ string, u hostUnsettled) bool { return u.handle == handle })
	c.mu.Unlock()
	// AND THE SEAT'S INBOX HOLD, already dropped with the attachment by the
	// release's detach: forgotten, never lifted (its mail is the successor's to
	// order). See [Coordinator.forgetHold].
	c.forgetHold(handle)
	// AND THE ROUTER OF ITS HELD INBOX stops: the seat's mail is the successor's.
	if c.busy != nil {
		c.busy(handle)
	}
	log.Debug("sandbox_seat_released", "seat", handle)
}
