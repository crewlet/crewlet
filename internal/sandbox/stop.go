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
// is its harness's own process in a box, minutes or hours long — so the
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
// what its announcement ([types.ExecutorStopped]) and the stopped turn's
// record name.
//
// Finishing it is the ordinary ending's steps with two added for the stop
// (see [Coordinator.finishEnding]): before the box goes, the run's tool-bridge
// session is closed, its job ENDED — its script asked to stop its harness and
// pack its session as its own exit does, and what is left of the job killed
// past the wait ([Coordinator.endJob]) — and what the run had spent read off
// its box, charged to the token counters and published as its launch's record
// ([Coordinator.collectStopped]) — a run stopped halfway still spent what it
// spent, and nothing can read its box once it is reclaimed; and after the
// announcement, the turn parked on the run is closed on the record as stopped
// ([CoordinatorOptions.CloseStopped]).
//
// # Off the frame that decided it
//
// Ending a job is waiting for it — seconds for a harness to go and its session
// to be packed, at most [JobEndBudget] — so the frame that decides a stop — a
// pause watch, a seat's acquisition — closes the run's session and goes on,
// and the ending's own retry, armed at once, makes the read: for every job that
// started in a box, on this node's host or another's, and whatever
// `conversation_session.enabled` says, which governs what is kept of a
// session and not how a stop ends a job.
//
// # A box on another node's host
//
// A `direct` or `container` box is read on its own host ([BoxReader]), and that
// node may be draining or restarting while its seat has moved here. This
// node's retry of the kept ending asks the host, at once and then on the
// hand-back's spacing while the host is silent, and the host ends the job and
// reads it ([Coordinator.ReadHostedBox]), licensed by the stop the run's row
// records and nothing else; the rest is finished here ([Coordinator.endRun]).
// A silent host is no verdict about the box, and its spend is not written off
// for it: only a box found gone, or a host silent for as long as the fleet
// waits before it takes a launch node for gone, is ended with nothing read
// ([Coordinator.readStopped]). A host whose coordination store did not answer
// its read of the run's row ([ErrStoreSilent]) is waited for the same way where
// this node's own store answers, since only that host can read the box; where
// this node's store is silent too, nothing the ending writes would land, and
// the stop is kept for as long as that lasts.
//
// # Not the seat fence
//
// The seat fence is about OWNERSHIP, and it deliberately leaves a running run
// alone: a seat that moves to another node on an ordinary rebalance must not
// kill an hour of an executor's work (see seat.Host.Fence). A stop is a
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
// paused: a run already stopped has no row left to list.
//
// WAITING ON NO JOB: the call decides each stop and closes the run's session,
// and a run whose job started in a box — on this node's host or another's,
// whatever `conversation_session.enabled` says — is finished by the ending's
// own retry, armed at once, which ends the job, reads it, reclaims its box and
// announces the stop, the seat's mail held behind it until it lands
// ([Coordinator.oweEnding], [Coordinator.endRun]). Only a stop with no job to
// end, a launch stopped before its command was recorded, is finished here,
// its box reclaimed within the teardown grace. An ending that could not be
// finished yet is kept for the same retry, and reported only in the log.
func (c *Coordinator) StopSeat(ctx context.Context, handle string, by RunStop) error {
	fence, held := c.leaseOf(handle)
	if !held {
		return notHeld(ctx, handle, "", "stopping its running work")
	}
	// SAID FIRST, for a completion whose resume is out as the stop lands: the
	// listing below leaves that run to its turn, and a refusal its turn then
	// meets is re-fired rather than kept ([Coordinator.keepRefused]).
	c.mu.Lock()
	c.signals[signal{cond: stopLanded, handle: handle}]++
	c.mu.Unlock()
	runs, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		return fmt.Errorf("sandbox: listing seat %s's runs to stop them: %w", handle, err)
	}
	// A CONTEXT OF ITS OWN, as every teardown takes ([detached]): the caller
	// is a watch whose context ends with the node, and a stop it started must
	// still reach the box it is reclaiming.
	endCtx, cancel := detached(ctx)
	defer cancel()
	// A RUN THAT MOVED ON BEFORE ITS STOP has no delete of this node's to move
	// it, and one somebody else ended meanwhile is gone: the seat is
	// recounted from the store once the stops are made, which reads each such
	// row as it stands, or none.
	movedOn := false
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
		// OUT OF THE SET THE RUN HELD THE SEAT IN as the delete that ends it
		// lands, as every ending this node made of a run it counts gives it
		// back ([ending.counted]): the run is over once its record is gone,
		// and until then it is still on its row, ending under this node's
		// retry, which holds the seat's mail behind it.
		stop := by
		decided, err := c.endRecord(endCtx, run, ending{
			fence: fence, whileIn: Stoppable, launch: run.LaunchID, reclaim: true, stop: &stop,
			counted: true,
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
			movedOn = true
		default:
			log.InfoContext(ctx, "executor_stopped", "turn_id", run.TurnID, "seat", handle,
				"launch_id", run.LaunchID, "status", run.Status, "by", by.By,
				"by_kind", by.ByKind, "operator_id", by.OperatorID)
		}
	}
	if movedOn {
		c.syncSeat(endCtx, handle)
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
// read off its box, charged and published as its launch's record, and all of
// it recorded on the row in one write ([PendingStore.CollectStop]). Returns the
// row as that step left it, or why the box could not be read YET — which keeps
// the ending for this node's retry.
//
// THE SESSION IS CLOSED BEFORE IT, by the ending ([Coordinator.finishEnding]).
// The run's harness is still working until the read ends it, and every tool
// call it makes through the bridge acts as the seat: closed first, the stop
// takes effect on the seat's surface at once — no call the box makes after it
// is run, so no post, write or submission is asked for after it — rather than
// after a read of the box that ends the job and can take as long as the box's
// host takes to answer. A CALL ADMITTED BEFORE IT RUNS ON to its own end: the
// close answers it that the run has ended and the call is still running, and
// waits for none (internal/api/mcpbridge, "An opening ends without waiting for
// its calls"), so what such a call does lands after this read, and is in no
// collection of the stop's. Closing a closed session is nothing, which is what
// the reclaim that follows does again.
//
// ONCE PER ENDING, for the reason a release records its charge: the collection
// is recorded with the charge and the publish it made, so an attempt after a
// crash finds it done.
//
// A STOP ENDED WITH NOTHING READ IS SAID SO ONCE THAT WRITE LANDS
// (`sandbox_stop_unread`), and never before it. The write is a step of this
// node's store, which can fall silent after the attempt's own read of the run's
// row answered, and the stop is then kept for this node's retry, which reads
// the box again and charges whatever that read finds. Said before the write,
// the log had a stop still kept ended with its spend written off, whatever its
// read had come to, and a stop kept on a silence both nodes share said ended
// all the same. A write whose answer was lost though it landed is found
// recorded by the next attempt, which goes on past it and says nothing of it:
// no attempt can tell its own landed write from another's.
//
// rowRead is whether this attempt at the ending read the run's row off this
// node's own store ([Coordinator.finishEnding]), which the read judges a silent
// store by ([Coordinator.readStopped]).
func (c *Coordinator) collectStopped(ctx context.Context, run PendingRun, rowRead bool) (PendingRun, error) {
	collection := StopCollection{Ending: run.Ending.ID}
	result, read, unread, err := c.readStopped(ctx, run, rowRead)
	if err != nil {
		return PendingRun{}, err
	}
	if read {
		collection.InputTokens, collection.OutputTokens = result.InputTokens, result.OutputTokens
		collection.CacheReadTokens, collection.CacheWriteTokens = result.CacheReadTokens, result.CacheWriteTokens
		collection.Condensed = result.Condensed
		collection.Charged, collection.CompanyCharged = c.charge(ctx, run, result)
		collection.Published = c.publishCollected(ctx, run, result).Published
	}
	written, won, err := c.pending.CollectStop(ctx, run.TurnID, collection)
	if err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: recording what stopped run %s had spent: %w",
			run.TurnID, err)
	}
	if won {
		if unread != nil {
			log.WarnContext(ctx, "sandbox_stop_unread", "turn_id", run.TurnID,
				"sandbox_id", run.SandboxID, "gone", errors.Is(unread, ErrBoxGone),
				"host_silent", errors.Is(unread, ErrBoxHostSilent),
				"store_silent", errors.Is(unread, ErrStoreSilent), "error", unread.Error(),
				"detail", "what the stopped run had spent could not be read off its box, so none "+
					"of it is charged; the run is ended all the same")
		}
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

// readStopped reads what a stopped run's job had done by now off its box — the
// job ended first, as the read's own step ([Coordinator.readBox]) — reporting
// whether there was anything to read, or why it cannot be read yet. Where the
// box was asked and nothing could be read off it, unread is why: the stop ends
// with nothing charged, which [Coordinator.collectStopped] says once its record
// of that lands, and never here, before the write that can fail.
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
// unknowable, which the log says once the stop's record of that lands.
//
// NOT YET, TOO, where the coordination store did not answer a step the read
// cannot go on without ([ErrStoreSilent]): the host's read of the run's row,
// which licenses the read. The box and its host are up, so the ending is kept
// and asked again on the same spacing: ended uncharged at once for it, the stop
// wrote off a spend the box still held. For how long turns on whose silence it
// is, which rowRead says: whether this attempt's own read of the row, off this
// node's store, was answered ([Coordinator.finishEnding]).
//   - Answered, the silence is the host's alone (a partition that cuts the host
//     off from the store's leader and not this node), and only the host can
//     read the box: a host that cannot is as good as away, so the stop waits
//     for it as for a silent host, the box timeout from its decision, and is
//     then ended with nothing read. Unbounded, it held the seat's mail behind
//     it for as long as the partition lasted, while every write of the
//     ending's would land.
//   - Unanswered, the silence is this node's too: nothing the ending writes
//     would land, so the stop is kept whatever the box timeout says, and never
//     said ended for it. An attempt reaches the read in such a silence only
//     where it began after the retry's own read of the row and its decision,
//     which come first and keep the ending as they fail
//     ([Coordinator.retryOwed]).
//
// The result is the run's own, as a collection reads it — floored, held to the
// record's bound — and marked as a run that did not finish: never a success,
// never a question, whatever it had written ([stoppedResult]).
func (c *Coordinator) readStopped(ctx context.Context, run PendingRun, rowRead bool) (result Result, read bool,
	unread, err error) {
	if run.SandboxID == "" || run.CommandID == "" {
		return Result{}, false, nil, nil
	}
	// READ WHERE THE BOX IS — here, or on the node whose host it is on
	// ([Coordinator.boxResult]) — and fitted here, under the turn it closes.
	result, _, err = c.boxResult(ctx, run, BoxReadStop)
	silent := errors.Is(err, ErrBoxHostSilent)
	unsettled := errors.Is(err, ErrStoreSilent)
	switch {
	case err == nil:
		return stoppedResult(c.fitText(ctx, run, result)), true, nil, nil
	case unsettled && (!rowRead || !c.hostGivenUp(run)):
		detail := "the coordination store did not answer the host's read of the stopped run's row, " +
			"which licenses its read; the stop is kept, with its session closed, and its box asked " +
			"again until the store answers the host or the box timeout since the stop has passed"
		if !rowRead {
			detail = "the coordination store did not answer the host's read of the stopped run's row, " +
				"nor this node's own; the stop is kept, with its session closed, and its box asked " +
				"again until the store answers, whatever the box timeout says"
		}
		log.InfoContext(ctx, "sandbox_stop_read_waits", "turn_id", run.TurnID,
			"sandbox_id", run.SandboxID, "node", run.LaunchNode, "waits_for", "store", "shared", !rowRead,
			"error", err.Error(), "detail", detail)
		return Result{}, false, nil, fmt.Errorf("sandbox: reading what stopped run %s had spent: %w",
			run.TurnID, err)
	case silent && !c.hostGivenUp(run):
		log.InfoContext(ctx, "sandbox_stop_read_waits", "turn_id", run.TurnID,
			"sandbox_id", run.SandboxID, "node", run.LaunchNode, "waits_for", "host",
			"detail", "the node whose host the stopped run's box is on did not answer; the stop "+
				"is kept, with its session closed, and its box asked again until that node is "+
				"back or the box timeout since the stop has passed")
		return Result{}, false, nil, fmt.Errorf("sandbox: reading what stopped run %s had spent: %w",
			run.TurnID, err)
	}
	// NOTHING READ, and the stop ends with nothing charged: said once the stop's
	// record of that lands ([Coordinator.collectStopped]), never here, before
	// the write that can fail.
	return Result{}, false, err, nil
}

// hostGivenUp reports whether a stopped run's silent host, or a host whose
// store alone is silent, has been waited for as long as a silent launch node is
// before the fleet takes it for gone: the box timeout, from the stop's decision
// ([Coordinator.readStopped]).
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
// then, never a success, since it did not get to say it was done. Its
// launch's record says who stopped it ([collectedRecord]), and its ending
// parks it on no question, which nothing would answer.
func stoppedResult(result Result) Result {
	result.Success = false
	return result
}
