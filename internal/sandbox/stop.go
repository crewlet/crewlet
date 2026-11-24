package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// Stopping a seat's running work when a person asks ([Coordinator.StopSeat]):
// the turn's fence alone would let a run in a box go to its end. A stop is an
// ending like every other ([Coordinator.endRecord]), decided under the seat's
// lease and recorded ([PendingRun.Stop]), with the job ended and its spend read
// and charged before the box goes ([Coordinator.collectStopped]). The read is
// off the deciding frame, on the ending's retry; a box on another node's host
// is asked there ([Coordinator.readStopped]). Not the seat fence, which is
// about ownership. See the package doc, "Stopping a seat's running work".

// StopSeat ENDS every run of a seat that is launching or running ([Stoppable]),
// because a person paused the seat. by is who, as the pause records it.
//
// UNDER THE SEAT'S LEASE: a node not holding the seat stops nothing and says
// so ([ErrSeatNotHeld]). Each run is stopped on the job it was read on, so a
// run that moved on is left to what moved it: a CLAIMED completion, or a job a
// collection already read ([LaunchRecord.Collected]), is a resumed turn the
// turn's fence stops; the store refuses their stop, and what that resume
// WAITS on is ended here ([Coordinator.endCollectedWait]). IDEMPOTENT, so
// every way a stop reaches a seat may call it. A run with a box job is
// finished by the ending's own retry ([Coordinator.oweEnding]); a launch
// stopped before its command was recorded is finished here.
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
	// A RUN THAT MOVED ON BEFORE ITS STOP has no delete of this node's to move
	// it, and one somebody else ended meanwhile is gone: the seat is
	// recounted from the store once the stops are made, which reads each such
	// row as it stands, or none.
	movedOn := false
	for _, run := range runs {
		if run.Ending != nil || !slices.Contains(Stoppable, run.Status) {
			continue
		}
		if c.stopRun(ctx, handle, fence, by, run) {
			movedOn = true
		}
	}
	if movedOn {
		// UNDER A CONTEXT OF ITS OWN, as every teardown takes ([detached]):
		// the caller is a watch whose context ends with the node.
		recountCtx, cancel := detached(ctx)
		c.syncSeat(recountCtx, handle)
		cancel()
	}
	return nil
}

// stopRun stops one of the seat's runs [Coordinator.StopSeat] listed, as a
// person's stop by asks, and reports whether the run had moved on before its
// stop could be decided, which leaves the seat to StopSeat's recount.
//
// UNDER A CONTEXT OF ITS OWN, as every ending a teardown makes runs
// ([detachedEnding]): the caller is a watch whose context ends with the node,
// and a stop it started must still reach the box it is reclaiming. EACH WRITE
// OF EACH RUN'S ROW IS BOUNDED ON ITS OWN, the teardown's grace and the row's
// crossing ([Coordinator.endingCall]): one grace shared by every run of the
// seat gave whichever row crossed first the time every other's needed, and
// one shared by an ending's writes gave its first the time its next needed.
func (c *Coordinator) stopRun(ctx context.Context, handle string, fence Fence, by RunStop, run PendingRun) bool {
	endCtx, done := detachedEnding(ctx)
	defer done()
	if run.Launch.Collected {
		heldHere := c.endCollectedWait(endCtx, run)
		log.InfoContext(ctx, "sandbox_stop_left_to_turn", "turn_id", run.TurnID, "seat", handle,
			"launch_id", run.LaunchID, "held", run.HeldFor != "", "held_here", heldHere,
			"detail", "the run's job is over and was collected; whatever its turn's resume "+
				"waited on is ended, and the resume is stopped by the seat's fence at its "+
				"first round")
		return false
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
		// [Coordinator.StopSeat] for who stops each.
		log.InfoContext(ctx, "sandbox_stop_declined", "turn_id", run.TurnID, "seat", handle,
			"detail", "the run moved on before it could be stopped; the turn's own fence "+
				"or the pause itself holds it from here")
		return true
	default:
		log.InfoContext(ctx, "executor_stopped", "turn_id", run.TurnID, "seat", handle,
			"launch_id", run.LaunchID, "status", run.Status, "by", by.By,
			"by_kind", by.ByKind, "operator_id", by.OperatorID)
	}
	return false
}

// endCollectedWait ends whatever the resume of run's collected job waits on,
// for a person's stop: a hold on its row ([PendingRun.HeldFor]) is lifted and a
// completion this node kept for it ([Coordinator.keepRefused],
// [Coordinator.keepHeldOnRow]) re-fired at once, so the turn's fence closes it
// as stopped. It reports whether this node was holding the job itself. A keep
// for another job of the turn is dropped; a hold the store did not lift keeps
// its keep. See the package doc, "Stopping a seat's running work".
func (c *Coordinator) endCollectedWait(ctx context.Context, run PendingRun) bool {
	unlifted := false
	if run.HeldFor != "" {
		liftCtx, liftDone := c.endingCall(ctx)
		lifted, err := c.pending.LiftHold(liftCtx, run.TurnID, run.HeldFor, run.Holds)
		liftDone()
		switch {
		case err != nil:
			unlifted = true
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
	if !ok || (unlifted && !kept.heldOnRow.IsZero() && kept.ev.LaunchID == run.LaunchID) {
		return false
	}
	c.dropKeptLocked(run.TurnID)
	if kept.ev.LaunchID != run.LaunchID || c.closed {
		return false
	}
	c.refireLocked(kept)
	return !kept.heldUntil.IsZero()
}

// collectStopped is the stop's own step of its ending: the run's spend read off
// its box, charged and published, recorded in one write
// ([PendingStore.CollectStop]), once per ending. It returns the row as left, or
// why the box cannot be read yet, keeping the ending. The session is closed
// before it ([Coordinator.finishEnding]). A stop ended with nothing read is
// said (`sandbox_stop_unread`) only once that write lands. rowRead is whether
// this attempt read the run's row off this node's store
// ([Coordinator.readStopped]). See the package doc, "Stopping a seat's running
// work".
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
	writeCtx, writeDone := c.endingCall(ctx)
	written, won, err := c.pending.CollectStop(writeCtx, run.TurnID, collection)
	writeDone()
	if err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: recording what stopped run %s had spent: %w",
			run.TurnID, err)
	}
	if won {
		if unread.err != nil {
			c.sayStopUnread(ctx, run, unread)
		}
		return written, nil
	}
	// NOT THIS ATTEMPT'S TO RECORD: another attempt at the same ending did
	// it first, or the ending finished meanwhile. The row as it stands says
	// which; the delete that follows decides the rest.
	readCtx, readDone := c.endingRead(ctx)
	current, found, err := c.pending.Get(readCtx, run.TurnID)
	readDone()
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

// sayStopUnread logs that stopped run's spend could not be read off its box
// and none of it is charged (`sandbox_stop_unread`), once the stop's record of
// that has landed ([Coordinator.collectStopped]): why, and where its host was
// given up, what gave it up by this node's clock, the silence this node
// measured, if any, and the stop's age.
func (c *Coordinator) sayStopUnread(ctx context.Context, run PendingRun, unread stopUnread) {
	attrs := []any{"turn_id", run.TurnID, "sandbox_id", run.SandboxID,
		"gone", errors.Is(unread.err, ErrBoxGone), "host_silent", errors.Is(unread.err, ErrBoxHostSilent),
		"store_silent", errors.Is(unread.err, ErrStoreSilent), "error", unread.err.Error()}
	if unread.givenUp {
		now := c.now()
		if !unread.since.IsZero() {
			attrs = append(attrs, "silent_s", now.Sub(unread.since).Seconds())
		}
		if run.Ending != nil {
			attrs = append(attrs, "stop_age_s", now.Sub(run.Ending.At).Seconds())
		}
	}
	attrs = append(attrs, "detail", "what the stopped run had spent could not be read off its box, "+
		"so none of it is charged; the run is ended all the same")
	log.WarnContext(ctx, "sandbox_stop_unread", attrs...)
}

// readStopped reads what a stopped run's job did off its box, the job ended
// first ([Coordinator.readBox]), reporting whether there was anything to read
// or why not yet; [stopUnread] says why nothing was read. Nothing to read for a
// job never started, a gone box, or a failed local read. Not yet for a read
// here that never started ([errReadUnstarted]), nor for a silent host
// ([ErrBoxHostSilent]) until the host grace of silence by this node's
// clock ([Coordinator.hostSilence]) or a stop the host grace plus twice
// [objstore.AssumedClockSkew] old; nor for a store's silence on the row's read
// or its served notes' verdict ([ErrStoreSilent]), bounded where the host's
// alone (another node's box, rowRead) and kept where this node's, a box on its
// own host above all. See the package doc, "Stopping a seat's running work".
func (c *Coordinator) readStopped(ctx context.Context, run PendingRun, rowRead bool) (Result, bool, stopUnread,
	error) {
	if run.SandboxID == "" || run.CommandID == "" {
		return Result{}, false, stopUnread{}, nil
	}
	// READ WHERE THE BOX IS — here, or on the node whose host it is on
	// ([Coordinator.boxResult]) — and fitted here, under the turn it closes.
	// Its start is what a silence it finds is counted from: the whole ask
	// went unanswered, and a stop's ask can outlast the host grace alone.
	asked := c.now()
	result, _, err := c.boxResult(ctx, run, BoxReadStop)
	silent := errors.Is(err, ErrBoxHostSilent)
	unsettled := errors.Is(err, ErrStoreSilent)
	switch {
	case err == nil:
		return stoppedResult(c.fitText(ctx, run, result)), true, stopUnread{}, nil
	case errors.Is(err, errReadUnstarted):
		// NOT ASKED, so nothing of the box is known: the stop is kept for its retry.
		return Result{}, false, stopUnread{}, fmt.Errorf("sandbox: reading what stopped run %s had spent: %w",
			run.TurnID, err)
	case unsettled && (!rowRead || c.hosts(run)):
		log.InfoContext(ctx, "sandbox_stop_read_waits", "turn_id", run.TurnID,
			"sandbox_id", run.SandboxID, "node", run.LaunchNode, "waits_for", "store", "shared", true,
			"error", err.Error(), "detail", "the coordination store did not answer this node: its read of "+
				"the stopped run's row, or of the verdict on the served notes of a box on its own host; "+
				"the stop is kept, with its session closed, "+
				"and its box asked again until the store answers, whatever the host grace says")
		return Result{}, false, stopUnread{}, fmt.Errorf("sandbox: reading what stopped run %s had spent: %w",
			run.TurnID, err)
	case unsettled || silent:
		since := c.hostSilence(run, asked, err)
		if !c.hostGivenUp(run, since) {
			waitsFor, detail := "host", "the node whose host the stopped run's box is on gave no account of it; "+
				"the stop is kept, with its session closed, and its box asked again until that "+
				"node answers, has been silent for the host grace by this node's own clock "+
				"since this node first asked it, or the stop is the host grace and two hours "+
				"old by this node's clock"
			if unsettled {
				waitsFor, detail = "store", "the coordination store did not answer the host's read of the "+
					"stopped run's row, which licenses its read, or the verdict on its served notes; "+
					"the stop is kept, with its session "+
					"closed, and its box asked again until the store answers the host, the host has gone "+
					"without it for the host grace by this node's own clock since this node first asked "+
					"it, or the stop is the host grace and two hours old by this node's clock"
			}
			attrs := []any{"turn_id", run.TurnID, "sandbox_id", run.SandboxID, "node", run.LaunchNode,
				"waits_for", waitsFor}
			if unsettled {
				attrs = append(attrs, "shared", false)
			} else {
				attrs = append(attrs, "busy", errors.Is(err, ErrBoxHostBusy),
					"not_asked", errors.Is(err, ErrBoxNotAsked))
			}
			log.InfoContext(ctx, "sandbox_stop_read_waits", append(attrs, "error", err.Error(), "detail", detail)...)
			return Result{}, false, stopUnread{}, fmt.Errorf("sandbox: reading what stopped run %s had spent: %w",
				run.TurnID, err)
		}
		// NOTHING READ, the host given up: said once the stop's record of
		// that lands ([Coordinator.collectStopped]), with what gave it up.
		return Result{}, false, stopUnread{err: err, givenUp: true, since: since}, nil
	}
	// NOTHING READ, and the stop ends with nothing charged: said once the stop's
	// record of that lands ([Coordinator.collectStopped]), never here, before
	// the write that can fail.
	return Result{}, false, stopUnread{err: err}, nil
}

// stopUnread is why a stopped run's box was asked and nothing could be read
// off it ([Coordinator.readStopped]), for [Coordinator.collectStopped] to say
// once the stop's record of that lands: err is the read's failure, nil where
// the box was read or not asked; and where its host was given up (givenUp),
// since is when this node first heard nothing from it by its own clock, zero
// where the stop's age alone gave it up.
type stopUnread struct {
	err     error
	givenUp bool
	since   time.Time
}

// hostSilence is since when this node has heard nothing from the host of a
// stopped run's box, by its own clock, given that the ask started at asked came
// back with err: zero where nothing counts as silence
// ([Coordinator.readStopped]). Kept on the stop's retry series
// ([answerRetry.silent]): busy clears it, an ask not made leaves it, a silent
// host or a host-only store silence starts it; with no series, the silence is
// that ask's alone. See the package doc, "Stopping a seat's running work".
func (c *Coordinator) hostSilence(run PendingRun, asked time.Time, err error) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.retries[run.TurnID]
	var id string
	if run.Ending != nil {
		id = run.Ending.ID
	}
	switch {
	case errors.Is(err, ErrBoxHostBusy):
		if r != nil {
			r.silent = silence{}
		}
		return time.Time{}
	case errors.Is(err, ErrBoxNotAsked):
		if r != nil && r.silent.ending == id {
			return r.silent.since
		}
		return time.Time{}
	case r == nil:
		return asked
	}
	if r.silent.ending != id || r.silent.since.IsZero() {
		r.silent = silence{ending: id, since: asked}
	}
	return r.silent.since
}

// hostGivenUp reports whether a stopped run's host, which gave no account of
// its box, is taken for gone ([Coordinator.readStopped]): silent since since
// for the host grace, as the fleet waits on a silent launch node before it
// takes it for gone, both by this node's clock; or the stop the host grace and
// twice [objstore.AssumedClockSkew] old by this node's clock, the one reading
// of the decision's instant — another node's clock — that never comes before
// the stop is the host grace old in real time, on clocks within that bound.
func (c *Coordinator) hostGivenUp(run PendingRun, since time.Time) bool {
	m := c.mgr()
	if m == nil || run.Ending == nil {
		return true
	}
	now, grace := c.now(), m.HostGrace()
	return (!since.IsZero() && now.Sub(since) >= grace) ||
		now.Sub(run.Ending.At) >= grace+2*objstore.AssumedClockSkew
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
// THE FRAME'S ANSWER CANNOT BE THE PAUSE, nor the row: the stop finishes in the
// time a box takes to die, often before the turn unwinds, and deletes the row,
// and the person may resume the seat first. Read off the pause then, the stop
// was no stop and the turn would read parked for good while its run read
// stopped. So the decision is kept here from the deciding write to the frame's
// question, IN THIS NODE'S MEMORY, because both parties are this node's.
// Kept until the frame takes it, the turn's next launch here opens
// ([Coordinator.Launch]), or the seat leaves ([Coordinator.ReleaseSeat]).
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
