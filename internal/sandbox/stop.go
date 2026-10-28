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
// # A box on another node's host
//
// A `direct` or `container` box is read on its own host ([BoxReader]), and that
// node may be draining or restarting while its seat has moved here. So the
// frame that decides the stop — a pause watch, a seat's acquisition — closes
// the run's session and goes on; this node's own retry of the kept ending asks
// the host, at once and then on the hand-back's spacing while the host is
// silent, and finishes the rest ([Coordinator.endRun]). A silent host is no
// verdict about the box, and its spend is not written off for it: only a box
// found gone, or a host silent for as long as the fleet waits before it takes a
// launch node for gone, is ended with nothing read ([Coordinator.readStopped]).
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
// What that resume was WAITING on is ended here, so it comes at once rather than
// when the wait would have ended ([Coordinator.endCollectedWait]).
//
// IDEMPOTENT, which is what lets every way a stop can reach a seat call it — the
// pause landing, a complete re-read of the pauses, the seat's acquisition while
// paused: a run already stopped has no row left to list. An ending this node
// could not finish yet is kept for its retry, with the seat's mail held behind
// it ([Coordinator.oweEnding]), and reported only in the log; and one whose box
// is read on another node's host is finished by that retry at once, so no
// caller's frame waits on another node ([Coordinator.endRun]).
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
			heldHere := c.endCollectedWait(endCtx, run)
			log.InfoContext(ctx, "sandbox_stop_left_to_turn", "turn_id", run.TurnID, "seat", handle,
				"launch_id", run.LaunchID, "held", run.HeldFor != "", "held_here", heldHere,
				"detail", "the run's job is over and was collected; whatever its turn's resume "+
					"waited on is ended, and the resume is stopped by the seat's fence at its "+
					"first round")
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

// endCollectedWait ends whatever the resume of run's collected job waits on, for
// a person's stop: a hold on its row ([PendingRun.HeldFor]) is lifted, so the
// waiter's next poll announces the job, and a completion of it this node kept
// for a condition, held here or refused at the gate ([Coordinator.keepRefused]),
// is re-fired at once. Reports whether this node was holding the job itself.
//
// WHY A STOP ENDS THE WAIT. The stop leaves a collected job to its turn, whose
// fence closes it as stopped at its first round, before any model call or
// launch. But that round comes only when the resume does, and a resume held for
// a model's limit waits up to the hour the entry is sat out, one held for the
// budget until its window turns over: the turn stayed open, its job neither
// resumed nor ended, for as long as the condition lasted after a person had
// asked for the work to stop. A resume of a seat a person stopped is refused
// nothing by those conditions (the engine's gate admits it), since the turn
// it re-enters ends before it could meet either.
//
// A KEEP FOR ANOTHER JOB of the same turn is dropped, as an announcement drops
// it ([Coordinator.stillRefused]): the stop decides this job.
func (c *Coordinator) endCollectedWait(ctx context.Context, run PendingRun) bool {
	if run.HeldFor != "" {
		lifted, err := c.pending.LiftHold(ctx, run.TurnID, run.HeldFor)
		switch {
		case err != nil:
			log.WarnContext(ctx, "sandbox_stop_hold_unlifted", "turn_id", run.TurnID,
				"agent", run.AgentHandle, "condition", string(run.HeldFor), "error", err.Error(),
				"detail", "the stopped seat's collected job stays held until its condition "+
					"clears, and its turn is stopped then")
		case lifted:
			log.InfoContext(ctx, "sandbox_resume_hold_lifted", "turn_id", run.TurnID,
				"agent", run.AgentHandle, "condition", string(run.HeldFor))
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	kept, ok := c.refused[run.TurnID]
	if !ok {
		return false
	}
	delete(c.refused, run.TurnID)
	if kept.ev.LaunchID != run.LaunchID || c.closed {
		return false
	}
	c.refireLocked(kept)
	return !kept.heldUntil.IsZero()
}

// collectStopped is the stop's own step of its ending: what the run had spent
// read off its box, charged and published as its phase, and all of it recorded
// on the row in one write ([PendingStore.CollectStop]). Returns the row as that
// step left it, or why the box could not be read YET — which keeps the ending
// for this node's retry.
//
// THE SESSION IS CLOSED BEFORE IT, by the ending ([Coordinator.finishEnding]).
// The run's CLI is still working until its box is reclaimed, and every tool
// call it makes through the bridge acts as the seat: closed first, the stop
// takes effect on the seat's surface at once — no post, no write, no submission
// after it — rather than after a read of the box that can take as long as the
// box's host takes to answer. Closing a closed session is nothing, which is
// what the reclaim that follows does again.
//
// ONCE PER ENDING, for the reason a release records its charge: the collection
// is recorded with the charge and the publish it made, so an attempt after a
// crash finds it done.
func (c *Coordinator) collectStopped(ctx context.Context, run PendingRun) (PendingRun, error) {
	collection := StopCollection{Ending: run.Ending.ID}
	result, read, err := c.readStopped(ctx, run)
	if err != nil {
		return PendingRun{}, err
	}
	if read {
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

// readsAway reports whether a stopped run's read is a question for another
// node's host: its job is in a box, and the box is not on this node's host
// ([Coordinator.hosts]).
func (c *Coordinator) readsAway(run PendingRun) bool {
	return run.SandboxID != "" && run.CommandID != "" && !c.hosts(run)
}

// readStopped reads what a stopped run's job had done by now off its box,
// reporting whether there was anything to read, or why it cannot be read yet.
//
// NOTHING TO READ, and the stop finishes with nothing charged, where nothing
// can be read: a job that never started — a launch stopped before its command
// was recorded — spent nothing in a box; a box found GONE ([ErrBoxGone]) has
// nothing left to read, and a stop waiting on it would never finish; and a box
// on this node's own host that fails to be read is reclaimed next by this very
// ending, which a stop must not hold up while the job in it still works.
//
// NOT YET, where the box's host did not answer ([ErrBoxHostSilent]). That is no
// failure of the box — only its host reclaims it, and only after it is back —
// so the ending is kept and this node asks again on the hand-back's spacing
// ([Coordinator.oweEnding]), as a collection hands its claim back uncounted for
// the same answer. For as long as the fleet's waiter waits before it judges a
// silent launch node gone: the box timeout ([Manager.BoxTimeout]), from the
// stop's decision. Past it, the host is taken for gone and the run's spend is
// unknowable, which the log says.
//
// The result is the run's own, as a collection reads it — floored, held to the
// record's bound — and marked as a run that did not finish: never a success,
// never a question, whatever it had written ([stoppedResult]).
func (c *Coordinator) readStopped(ctx context.Context, run PendingRun) (Result, bool, error) {
	if run.SandboxID == "" || run.CommandID == "" {
		return Result{}, false, nil
	}
	// READ WHERE THE BOX IS — here, or on the node whose host it is on
	// ([Coordinator.boxResult]) — and fitted here, under the turn it closes.
	result, _, err := c.boxResult(ctx, run, BoxReadStop)
	silent := errors.Is(err, ErrBoxHostSilent)
	switch {
	case err == nil:
		return stoppedResult(c.fitText(ctx, run, result)), true, nil
	case silent && !c.hostGivenUp(run):
		log.InfoContext(ctx, "sandbox_stop_read_waits", "turn_id", run.TurnID,
			"sandbox_id", run.SandboxID, "node", run.LaunchNode,
			"detail", "the node whose host the stopped run's box is on did not answer; the stop "+
				"is kept, with its session closed, and its box asked again until that node is "+
				"back or the box timeout since the stop has passed")
		return Result{}, false, fmt.Errorf("sandbox: reading what stopped run %s had spent: %w",
			run.TurnID, err)
	}
	log.WarnContext(ctx, "sandbox_stop_unread", "turn_id", run.TurnID,
		"sandbox_id", run.SandboxID, "gone", errors.Is(err, ErrBoxGone), "host_silent", silent,
		"error", err.Error(),
		"detail", "what the stopped run had spent could not be read off its box, so none "+
			"of it is charged; the run is ended all the same")
	return Result{}, false, nil
}

// hostGivenUp reports whether a stopped run's silent host has been waited for
// as long as a silent launch node is before the fleet takes it for gone: the box
// timeout, from the stop's decision ([Coordinator.readStopped]).
func (c *Coordinator) hostGivenUp(run PendingRun) bool {
	m := c.mgr()
	if m == nil || run.Ending == nil {
		return true
	}
	return c.now().Sub(run.Ending.At) >= m.BoxTimeout()
}

// launchStop is a stop decided on a run of this node's while it was still
// launching ([Coordinator.LaunchStopped]).
type launchStop struct {
	handle string
	stop   RunStop
}

// noteLaunchStop keeps, for the frame launching it, the stop a run was decided
// to end on while it was still LAUNCHING ([Coordinator.LaunchStopped]).
func (c *Coordinator) noteLaunchStop(decided PendingRun) {
	if decided.Stop == nil || decided.Status != StatusLaunching {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.launchStops[decided.TurnID] = launchStop{handle: decided.AgentHandle, stop: *decided.Stop}
}

// LaunchStopped TAKES the stop a person's pause decided on the turn's run while
// it was still launching, reporting whether there was one: the frame launching
// it asks when its launch or its park is refused under it.
//
// THE FRAME'S ANSWER CANNOT BE THE PAUSE, nor the row. The stop finishes in the
// time a box takes to die — often less than the turn takes to unwind to its
// park — and deletes the row as it goes, and the person may resume the seat
// before the frame asks: read off the pause then, the stop was no stop, the
// refused park was logged as a suspension with nowhere to go, and the turn's
// last record was its park, so it read parked for good while its run read
// stopped. So the decision is kept here, from the deciding write to the
// frame's question, whatever the row and the pause say by then.
//
// IN THIS NODE'S MEMORY, because the two parties are this node's: a launching
// run's stop is decided by the node holding its seat, which is the node running
// the turn that launched it — a seat that moves takes its launching runs with it
// as abandoned tails, never stops, and its new holder's frame launches its own.
// Kept until the frame takes it, the turn's next launch on this node opens
// ([Coordinator.Launch]), or the seat leaves this node ([Coordinator.ReleaseSeat]).
func (c *Coordinator) LaunchStopped(turnID string) (RunStop, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.launchStops[turnID]
	delete(c.launchStops, turnID)
	return s.stop, ok
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
