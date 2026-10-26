package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// STOPPING A SEAT'S RUNNING WORK, when a person asks for it.
//
// A pause with `stop_running` is a person saying the seat's work in flight must
// not go on. The turn the seat is on hears it at its next round, through the
// turn's own fence; a run that turn launched has no rounds the engine sees — it
// is its coding CLI's own process in a box, minutes or hours long — so the
// fence alone let it run to its end, and only the turn that came back to
// collect it was stopped. [Coordinator.StopSeat] ends it instead, at once.
//
// # An ending like every other
//
// A stop is decided on the run's row through the one protocol every ending
// takes ([Coordinator.endRecord]): DECIDED first, under the seat's lease and
// only while the run is still launching or running the job it was read on, and
// then finished from the row by whoever reads it next — this node, or the
// seat's next holder after a crash. What makes it a stop rather than a loss is
// recorded in the deciding write ([PendingRun.Stop]): who stopped it, which is
// what its announcement ([types.SandboxRunStopped]) and the stopped turn's
// record name.
//
// Finishing it is the ordinary ending's steps with two added for the stop
// (see [Coordinator.finishEnding]): before the box goes, the run's tool-bridge
// session is closed and what the run had spent is read off its box, charged to
// the token counters and published as its phase ([Coordinator.collectStopped])
// — a run stopped halfway still spent what it spent, and nothing can read its
// box once it is reclaimed; and after the announcement, the turn parked on the
// run is closed on the record as stopped ([CoordinatorOptions.CloseStopped]).
//
// # Not the seat fence
//
// The seat fence is about OWNERSHIP, and it deliberately leaves a running run
// alone: a seat that moves to another node on an ordinary rebalance must not
// kill an hour of somebody's coding run (see seat.Host.Fence). A stop is a
// person's decision about the work, not about where it runs, so it is its own
// call, made by the node that holds the seat when the pause reaches it.

// StopSeat ENDS every run of a seat that is launching or running ([Stoppable]),
// because a person paused the seat and asked for its running work to stop. by
// is who, as the pause records it.
//
// UNDER THE SEAT'S LEASE: a node that does not hold the seat stops nothing and
// says so ([ErrSeatNotHeld]) — the node that does hold it reads the same pause
// and stops the runs itself, and the store would refuse this node's decision
// under the holder's lease besides.
//
// EACH RUN UNDER A NARROW LICENSE, the job it was read on in the statuses a stop
// ends, so a run that moved on since the listing is left to whatever moved it:
// a completion CLAIMED first is a resumed turn the turn's own fence stops at its
// next round, and a run that parked on a question since waits behind the pause
// for its answer. A run whose ending is already decided is that ending's. And a
// job a collection already read — back in running because its resume was held
// or handed back for a retry ([LaunchRecord.Collected]) — is its turn's, for the
// reason a claimed one is: the store refuses its stop, and the turn's fence ends
// its resume at the first round, charging the job once, as the resume it is.
//
// IDEMPOTENT, which is what lets every way a stop can reach a seat call it — the
// pause landing, a complete re-read of the pauses, the seat's acquisition while
// paused: a run already stopped has no row left to list. An ending this node
// could not finish yet is kept for its retry, with the seat's mail held behind
// it ([Coordinator.oweEnding]), and reported only in the log.
func (c *Coordinator) StopSeat(ctx context.Context, handle string, by RunStop) error {
	fence, held := c.leaseOf(handle)
	if !held {
		return notHeld(ctx, handle, "", "stopping its running work")
	}
	runs, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		return fmt.Errorf("sandbox: listing seat %s's runs to stop them: %w", handle, err)
	}
	// A CONTEXT OF ITS OWN, as every teardown takes ([detached]): the caller
	// is a watch whose context ends with the node, and a stop it started must
	// still reach the box it is reclaiming.
	endCtx, cancel := detached(ctx)
	defer cancel()
	for _, run := range runs {
		if run.Ending != nil || !slices.Contains(Stoppable, run.Status) {
			continue
		}
		if run.Launch.Collected {
			log.InfoContext(ctx, "sandbox_stop_left_to_turn", "turn_id", run.TurnID, "seat", handle,
				"launch_id", run.LaunchID, "held", run.HeldFor != "",
				"detail", "the run's job is over and was collected; its turn's resume, held or "+
					"retried, is stopped by the seat's fence at its first round")
			continue
		}
		stop := by
		decided, err := c.endRecord(endCtx, run, ending{
			fence: fence, whileIn: Stoppable, launch: run.LaunchID, reclaim: true, stop: &stop,
		})
		switch {
		case err != nil:
			log.WarnContext(ctx, "sandbox_stop_kept", "turn_id", run.TurnID, "seat", handle,
				"error", err.Error(),
				"detail", "the run's stop could not be finished yet; this node finishes it, "+
					"with the seat's mail held behind it, and the seat's next holder does if "+
					"the seat moves first")
		case decided == nil:
			// MOVED ON SINCE THE LISTING: claimed by its completion, parked
			// on a question, relaunched, or fenced by a newer lease — see
			// above for who stops each.
			log.InfoContext(ctx, "sandbox_stop_declined", "turn_id", run.TurnID, "seat", handle,
				"detail", "the run moved on before it could be stopped; the turn's own fence "+
					"or the pause itself holds it from here")
			continue
		default:
			log.InfoContext(ctx, "sandbox_run_stopped", "turn_id", run.TurnID, "seat", handle,
				"launch_id", run.LaunchID, "status", run.Status, "by", by.By,
				"by_kind", by.ByKind, "operator_id", by.OperatorID)
		}
		// THE SEAT GOES BACK out of the set the run held it in, as every
		// ending this node made gives it back ([Coordinator.settleFailed]):
		// the run is over, or ending under this node's retry.
		c.uncountRun(handle, run.Status)
	}
	return nil
}

// collectStopped is the stop's own step of its ending: the run's bridge session
// closed, what the run had spent read off its box, charged and published as its
// phase, and all of it recorded on the row in one write
// ([PendingStore.CollectStop]). Returns the row as that step left it.
//
// THE SESSION FIRST. The run's CLI is still working until its box is reclaimed,
// and every tool call it makes through the bridge acts as the seat: closed
// here, the stop takes effect on the seat's surface at once — no post, no write,
// no submission after it — rather than after a read of the box that can take as
// long as the box takes to answer. Closing a closed session is nothing, which
// is what the reclaim that follows does again.
//
// A BOX THAT CANNOT BE READ IS RECORDED AS READ, with nothing spent: a stop
// cannot wait on a box that may be gone, and the run's spend is then unknown —
// which the log says — rather than the stop never finishing. ONCE PER ENDING,
// for the reason a release records its charge: the collection is recorded with
// the charge and the publish it made, so an attempt after a crash finds it done.
func (c *Coordinator) collectStopped(ctx context.Context, run PendingRun) (PendingRun, error) {
	if c.ended != nil {
		c.ended(run.TurnID)
	}
	collection := StopCollection{Ending: run.Ending.ID}
	if result, read := c.readStopped(ctx, run); read {
		collection.InputTokens, collection.OutputTokens = result.InputTokens, result.OutputTokens
		collection.CacheReadTokens, collection.CacheWriteTokens = result.CacheReadTokens, result.CacheWriteTokens
		collection.Condensed = result.Condensed
		collection.Charged, collection.CompanyCharged = c.charge(ctx, run, result)
		collection.Published = c.publishPhase(ctx, run, result).Published
	}
	written, won, err := c.pending.CollectStop(ctx, run.TurnID, collection)
	if err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: recording what stopped run %s had spent: %w",
			run.TurnID, err)
	}
	if won {
		return written, nil
	}
	// NOT THIS ATTEMPT'S TO RECORD: another attempt at the same ending did
	// it first, or the ending finished meanwhile. The row as it stands says
	// which; the delete that follows decides the rest.
	current, found, err := c.pending.Get(ctx, run.TurnID)
	if err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: reading stopped run %s back: %w", run.TurnID, err)
	}
	if !found || current.Stop == nil || !current.Stop.Collected {
		stop := *run.Stop
		stop.Collected = true
		run.Stop = &stop
		return run, nil
	}
	return current, nil
}

// readStopped reads what a stopped run's job had done by now off its box,
// reporting whether there was anything to read: a run whose job never started
// — a launch stopped before its command was recorded — spent nothing in a box,
// and one whose box cannot be reached is logged and read as nothing.
//
// The result is the run's own, as a collection reads it — floored, held to the
// record's bound — and marked as a run that did not finish: never a success,
// never a question, whatever it had written ([stoppedResult]).
func (c *Coordinator) readStopped(ctx context.Context, run PendingRun) (Result, bool) {
	if run.SandboxID == "" || run.CommandID == "" {
		return Result{}, false
	}
	// READ WHERE THE BOX IS — here, or on the node whose host it is on
	// ([Coordinator.boxResult]) — and fitted here, under the turn it closes.
	result, _, err := c.boxResult(ctx, run, BoxReadStop)
	if err == nil {
		return stoppedResult(c.fitText(ctx, run, result)), true
	}
	log.WarnContext(ctx, "sandbox_stop_unread", "turn_id", run.TurnID,
		"sandbox_id", run.SandboxID, "gone", errors.Is(err, ErrBoxGone), "error", err.Error(),
		"detail", "what the stopped run had spent could not be read off its box, so none "+
			"of it is charged; the run is ended all the same")
	return Result{}, false
}

// stoppedResult is what a run a person stopped reports: what it said up to
// then, never a success — it did not get to say it was done — and never a
// question, which nothing will answer. Its phase record says who stopped it
// ([runPhase]).
func stoppedResult(result Result) Result {
	result.Success = false
	result.NeedsInput, result.Question, result.AskTo = false, "", ""
	return result
}
