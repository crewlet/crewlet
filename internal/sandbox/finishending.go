package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// finishEnding does what a decided ending ([PendingRun.Ending]) still has to do,
// in a fixed order, deleting the record last: let a held reply go, publish owed
// copies, end and read a stopped or lost job before its box goes (a lost one's
// served notes settled, then the row read again), reclaim the box, announce, hand
// a loss to the engine or close a stopped turn, publish unreported notes, record
// the turn's end on the executor log, delete. Waits not the frame's own run only
// on the ending's retry (mayAsk); elsewhere it is handed over ([errAskAway]).
// Every step is harmless repeated, so a crash resumes where it stopped; a refused
// delete ([ErrHandBackOwed], [ErrAnswerOwed], [ErrEndOwed]) reruns the steps. See
// the package doc, "Finishing an ending".
func (c *Coordinator) finishEnding(ctx context.Context, run PendingRun, mayAsk, counted bool) error {
	// THE ROW AS IT STANDS, not as the caller's decision read it: an earlier
	// attempt may have owed or recorded the turn's end, and a step off a stale copy
	// is made twice. A gone row or another ending was finished by whoever got
	// there first. AND WHETHER THIS NODE'S STORE ANSWERED, by which a stop's read of
	// a box whose host's store is silent is judged ([Coordinator.readStopped]): the
	// silence is the host's alone only where this node's is not.
	rowRead := false
	readCtx, readDone := c.endingRead(ctx)
	current, found, err := c.pending.Get(readCtx, run.TurnID)
	readDone()
	switch {
	case err != nil:
		// Read as handed: each step is harmless repeated, the delete decides.
	case !found || current.Ending == nil || current.Ending.ID != run.Ending.ID:
		return nil
	default:
		run, rowRead = current, true
	}
	var handed []string
	reclaimed, announced, ended, named := false, false, false, false
	for range endingRounds {
		if run.answerOwed() {
			owing, err := c.letGoAnswer(ctx, run)
			if err != nil {
				return fmt.Errorf("sandbox: ending run %s, which holds a reply it could not let go "+
					"of: %w", run.TurnID, err)
			}
			run = owing
		}
		if len(run.HandBack) > 0 {
			owed := handBackIDs(run.HandBack)
			if slices.Equal(owed, handed) {
				// THE SAME COPIES AGAIN: they are out and clearing them did not land;
				// republishing changes nothing, so the ending waits for its retry.
				return fmt.Errorf("sandbox: ending run %s: the reply it owed its seat is handed "+
					"back but could not be cleared from it", run.TurnID)
			}
			if err := c.deliverHandBack(ctx, run); err != nil {
				return fmt.Errorf("sandbox: ending run %s, which owes its seat a reply it could not "+
					"hand back: %w", run.TurnID, err)
			}
			handed = owed
		}
		if run.Stop != nil && !run.Stop.Collected {
			// THE SESSION FIRST: see [Coordinator.collectStopped].
			if c.ended != nil {
				c.ended(run.TurnID)
			}
			// A JOB THAT STARTED IN A BOX IS ENDED BY ITS READ, which waits for
			// it ([Coordinator.endJob]), never on this frame.
			if !mayAsk && run.SandboxID != "" && run.CommandID != "" {
				return errAskAway
			}
			collected, err := c.collectStopped(ctx, run, rowRead)
			if err != nil {
				return err
			}
			run = collected
		}
		if run.Ending.Reclaim && !reclaimed && c.capturesLoss(run) {
			// THE SESSION FIRST, as a stop's: a lost job may still be working, and
			// nothing it does after this acts as the seat (it kept acting through
			// its take when the reclaim was first to close it).
			if c.ended != nil {
				c.ended(run.TurnID)
			}
			// THE LAST READ OF THE BOX, before the reclaim takes it (session.go).
			if !mayAsk {
				return errAskAway
			}
			if err := c.keepLostSession(ctx, run, rowRead); err != nil {
				return fmt.Errorf("sandbox: ending run %s, whose lost job's box is not read yet: %w",
					run.TurnID, err)
			}
			if run.asksReceipts() {
				// THE ROW AGAIN, holding the verdict the read wrote, which step 8
				// reports from.
				readCtx, readDone := c.endingRead(ctx)
				current, found, err := c.pending.Get(readCtx, run.TurnID)
				readDone()
				switch {
				case err != nil:
					return fmt.Errorf("sandbox: ending run %s, whose row could not be read again after its "+
						"lost job's box: %w", run.TurnID, err)
				case !found || current.Ending == nil || current.Ending.ID != run.Ending.ID:
					return nil
				}
				run, rowRead = current, true
			}
		}
		if run.Ending.Reclaim && !reclaimed {
			// THE BOX THE ROW NAMES, on a context of its own (often dead on failure
			// paths). One whose kill failed keeps its own record, and its host's
			// sweep tries again once the row is gone.
			killCtx, cancel := detached(ctx)
			_ = c.reclaimBox(ctx, killCtx, run)
			cancel()
			reclaimed = true
		}
		if run.announces() && !announced {
			if err := c.announceEnding(ctx, run); err != nil {
				return err
			}
			announced = true
		}
		if len(run.OwedEnd) == 0 && !run.EndRecorded {
			// AFTER THE ANNOUNCEMENT, the record of the loss or stop the end and the
			// colleague's answer tell of, and ONCE: owed on the row and recorded
			// there once the log has it ([PendingRun.EndRecorded]), found by every
			// later attempt. NOT WHILE THE TURN'S OWN FRAME RUNS HERE, which ends the
			// turn itself ([Coordinator.framing]).
			if c.framing(run.TurnID) {
				return errAwaitsFrame
			}
			// THE RUN'S CALLS STILL IN FLIGHT are waited for before the turn's end
			// is decided on its log ([Coordinator.drainEnding]), only by the
			// ending's retry off any frame; other attempts hand such a run to it
			// ([errAskAway]).
			if err := c.drainEnding(ctx, run, mayAsk); err != nil {
				return err
			}
			ending, owes, err := c.endingOfRun(ctx, run)
			if err != nil {
				return fmt.Errorf("sandbox: ending run %s, whose stopped turn could not be closed "+
					"on the record: %w", run.TurnID, err)
			}
			if owes {
				owing, err := c.oweEnd(ctx, run, ending)
				if err != nil {
					return err
				}
				run = owing
			}
		}
		if !ended && c.steersEnded != nil && run.endsNotes() {
			if err := c.steersEnded(ctx, run); err != nil {
				return fmt.Errorf("sandbox: ending run %s, whose notes' ends could not be "+
					"recorded: %w", run.TurnID, err)
			}
			ended = true
		}
		if len(run.OwedEnd) > 0 {
			if !mayAsk && c.keepsSessions() {
				return errAskAway
			}
			waited, found, err := c.awaitEndCaptures(ctx, run)
			switch {
			case err != nil:
				return fmt.Errorf("sandbox: ending run %s, whose turn's end waits for its capture: %w",
					run.TurnID, err)
			case !found:
				// GONE WHILE IT WAITED: another attempt recorded the end and
				// deleted the record.
				return nil
			}
			published, err := c.publishEnd(ctx, waited)
			if err != nil {
				return fmt.Errorf("sandbox: ending run %s, whose turn's end the executor log has not "+
					"taken yet: %w", run.TurnID, err)
			}
			run = published
		}
		// AND WHAT IS STILL IN FLIGHT AS THE RECORD GOES, named: its update will
		// find no record, so this line is what is left.
		if !named {
			c.nameDeletedInFlight(ctx, run)
			named = true
		}
		settled, deleted, err := c.deleteRecord(ctx, run, counted)
		switch {
		case err == nil:
			// GONE, by this delete or whoever finished first. A delete of this
			// call's moves the seat for a recount already reading; the box host's
			// store clock went with the decision ([Coordinator.endRun]).
			if deleted {
				c.moved(run.AgentHandle)
			}
			return nil
		case errors.Is(err, ErrHandBackOwed), errors.Is(err, ErrAnswerOwed), errors.Is(err, ErrEndOwed):
			run = settled
		default:
			return fmt.Errorf("sandbox: ending run %s, whose record could not be deleted: %w",
				run.TurnID, err)
		}
	}
	return fmt.Errorf("sandbox: ending run %s: it kept owing its seat a reply, or the log its turn's "+
		"end, after recording one", run.TurnID)
}

// deleteRecord is an ending's delete of run's record ([PendingStore.Finish]) as
// an ending's store call ([Coordinator.endingCall]). For a run this node counts
// ([ending.counted]) it moves the run out of the seat's sets as its answer is
// taken: recorded before sending so a recount meanwhile counts the run where it
// is, row listed or not, and settled on every way out (a panic included) so an
// untaken answer moves nothing (countwrite.go).
func (c *Coordinator) deleteRecord(ctx context.Context, run PendingRun, counted bool,
) (PendingRun, bool, error) {
	if !counted {
		finishCtx, finishDone := c.endingCall(ctx)
		defer finishDone()
		return c.pending.Finish(finishCtx, run.TurnID, run.Ending.ID)
	}
	write := c.writingCount(run, run.Status, "")
	defer c.settleWrite(write)
	finishCtx, finishDone := c.endingCall(ctx)
	settled, deleted, err := c.pending.Finish(finishCtx, run.TurnID, run.Ending.ID)
	finishDone()
	c.mu.Lock()
	moved := c.wroteCountLocked(write, err == nil && deleted)
	c.mu.Unlock()
	moved()
	return settled, deleted, err
}

// endingOfRun is how the turn of run (a loss or a stop) ended, from the engine,
// and whether it owes the executor log an end: a loss's account
// ([CoordinatorOptions.TurnLost]) or a stop's, its parked turn closed first
// ([CoordinatorOptions.CloseStopped]). A turn that came back owes none here: its
// own frame decided its end.
func (c *Coordinator) endingOfRun(ctx context.Context, run PendingRun) (executorkeep.Ending, bool, error) {
	switch {
	case run.lost() && c.turnLost != nil:
		return c.turnLost(ctx, run, c.runCalls(ctx, run)), true, nil
	case run.Stop != nil && c.closeStopped != nil:
		ending, err := c.closeStopped(ctx, run, c.runCalls(ctx, run))
		if err != nil {
			return executorkeep.Ending{}, false, err
		}
		return ending, true, nil
	}
	return executorkeep.Ending{}, false, nil
}

// RunCalls is a run's bridged log as its ending read it, for the engine's account
// of whether the turn's reply reached anybody ([CoordinatorOptions.TurnLost],
// [CoordinatorOptions.CloseStopped]): Log, the run's job's calls and the fold of
// earlier rounds ([BridgeLog.PriorCalls]); Unread, why the store could not
// answer, nil where it did. AN UNREAD LOG IS A DELIVERY NOT KNOWN, never "no
// calls": read as empty, a turn that did reply would be recorded as one that did
// not, the false account the turn's end exists to prevent.
type RunCalls struct {
	Log    BridgeLog
	Unread error
}

// runCalls reads run's log for its ending ([RunCalls]): ONE READ PER ENDING, at
// the step deciding the turn's end, as a resume reads its own
// ([ResumeRequest.Bridged]), but the turn's calls whatever job the log is on
// ([PendingStore.TurnLog]): an ending reaps a row whose relaunch never opened the
// log to its job as readily as any, and the rounds' calls are the turn's all the
// same.
func (c *Coordinator) runCalls(ctx context.Context, run PendingRun) RunCalls {
	readCtx, readDone := c.endingRead(ctx)
	defer readDone()
	calls, err := c.pending.TurnLog(readCtx, run)
	if err != nil {
		return RunCalls{Unread: err}
	}
	return RunCalls{Log: calls}
}

// endingRounds bounds [Coordinator.finishEnding]'s rounds. A round repeats only
// when the delete is refused for what the row still owes (copies, an answer or the
// turn's end another attempt left after this round read it); a decided ending
// takes no new answer ([ErrRunEnding]), so a refusal names one of a finite set: an
// answer to let go, an earlier decline's copies, the let-go's copies, an end. Five
// rounds is one each plus the delete. The same-copies guard stops sooner, so the
// bound is reached only by another node finishing in step, the ending then kept
// for this node's retry.
const endingRounds = 5

// keepEnding keeps an ending that could not be finished for this node's retry
// ([Coordinator.oweEnding]) and returns cause as the kept ending's error.
func (c *Coordinator) keepEnding(ctx context.Context, run PendingRun, e ending, cause error) error {
	c.oweEnding(ctx, run, e)
	return fmt.Errorf("%w: %w", errEndingOwed, cause)
}

// letGoAnswer lets go of the recorded answer a run with a decided ending still owes
// the seat ([PendingStore.OweHandBack]): its copies are recorded on the row as owed
// in the write taking the answer off it, for the ending to hand back before the
// delete. Returns the row as left. SPENT BEFORE THE WRITE
// ([CoordinatorOptions.Spent]): the carrying delivery may still come round, and
// spent but not let go the reply stays on the row (the next reader lets go), let go
// but not spent original and copy would both reach the seat. A ROW ALREADY OWING
// COPIES (an earlier decline's) hands them back first, keeping its debt to one
// decline ([PendingRun.HandBack]). THE DECISION SETTLED WHETHER THE REPLY GOES BACK:
// one no turn took is let go, one a turn took is the run's unless the ending says it
// went unused; an unmade let-go (another attempt's) leaves the delete to refuse.
func (c *Coordinator) letGoAnswer(ctx context.Context, run PendingRun) (PendingRun, error) {
	answer := *run.Answer
	c.spend(ctx, run.AgentHandle, answer)
	letGo := LetGo{Ending: run.Ending.ID, Answer: answer.EventIDs, HandBack: handBackOf(answer)}
	var handed []string
	for range letGoRounds {
		owing, won, err := c.oweHandBack(ctx, run.TurnID, letGo)
		switch {
		case err == nil && won:
			return owing, nil
		case err == nil, errors.Is(err, ErrAnswerTaken):
			// NOT THIS ATTEMPT'S TO LET GO: the delete refuses for anything owed.
			run.Answer, run.AnswerRefusal = nil, nil
			return run, nil
		case !errors.Is(err, ErrHandBackOwed):
			return PendingRun{}, err
		}
		owed := handBackIDs(owing.HandBack)
		if slices.Equal(owed, handed) {
			return PendingRun{}, fmt.Errorf("sandbox: letting go of run %s's answer: the reply it "+
				"already owed its seat is handed back but could not be cleared from it", run.TurnID)
		}
		if err := c.deliverHandBack(ctx, owing); err != nil {
			return PendingRun{}, err
		}
		handed = owed
	}
	return PendingRun{}, fmt.Errorf("sandbox: letting go of run %s's answer: it kept owing its seat "+
		"an earlier reply", run.TurnID)
}

// oweHandBack is the let-go's write ([PendingStore.OweHandBack]) as an ending's
// store call ([Coordinator.endingCall]).
func (c *Coordinator) oweHandBack(ctx context.Context, turnID string, letGo LetGo) (PendingRun, bool, error) {
	writeCtx, done := c.endingCall(ctx)
	defer done()
	return c.pending.OweHandBack(writeCtx, turnID, letGo)
}

// endingCall is [Coordinator.rowContext] for one of an ending's changes of its
// run's row (decision, step write, delete) within ctx, the ending's lifetime
// ([detachedEnding], or the coordinator's for its retry): the teardown's grace
// its reason, each call its own crossing.
func (c *Coordinator) endingCall(ctx context.Context) (context.Context, context.CancelFunc) {
	return c.rowContext(ctx, discardGrace)
}

// endingRead is [Coordinator.endingCall] for one of an ending's READS of its
// run's row or of its log: the teardown's grace and the reply's crossing alone
// ([Coordinator.rowReadContext]), never a change's, since a read sends no write.
func (c *Coordinator) endingRead(ctx context.Context) (context.Context, context.CancelFunc) {
	return c.rowReadContext(ctx, discardGrace)
}

// letGoRounds bounds [Coordinator.letGoAnswer]'s rounds: one repeats only to hand
// back copies an earlier decline owes (one decline's worth at a time,
// [PendingRun.HandBack], and a decided ending takes no new one), so the second
// records the let-go or finds another attempt did, and a third is a margin. An
// unlanded clear is stopped sooner by the same-copies guard.
const letGoRounds = 3

// handBackIDs are the ids of what a row owes the seat, in order.
func handBackIDs(owed []HandedBack) []string {
	ids := make([]string, 0, len(owed))
	for _, copied := range owed {
		ids = append(ids, copied.ID)
	}
	return ids
}

// finish ends a run the caller has read and decided is over: the ending is DECIDED
// and finished ([Coordinator.endRecord]), the stop REPORTED where it was this call's.
// Reports the decided ending or nil ([RecordedEnding.Returned] says whether an answer
// goes back) and whether the ending is THIS NODE'S; false is somebody else's run, of
// which the caller says nothing. THE BOX GOES AFTER THE DECISION, never on the
// caller's snapshot: a lapsed lease lets the next holder REVIVE the claim, owed a
// resume into the box left paused, and a kill before the decision destroyed it. The
// attempt that FINISHES reclaims it ([Decision.Reclaim]) once the fence check
// allows, before the record, via the recorded ending so a next holder does too. A
// failed kill does not keep the record (it would park the seat busy); its host's
// sweep reclaims a box no run names. LICENSED FOR EVERY LIVE STATUS AND JOB; the FENCE guards kill and
// delete, and an untaken reply still goes back first ([ErrAnswerOwed]).
func (c *Coordinator) finish(ctx context.Context, run PendingRun, e ending) (*RecordedEnding, bool) {
	if outranked(run, e.fence) {
		// A newer lease owns the run, as the snapshot says; the store would refuse
		// the decision too.
		log.WarnContext(ctx, "sandbox_finish_outranked", "turn_id", run.TurnID,
			"owner_epoch", run.OwnerEpoch, "epoch", e.fence.Epoch)
		return nil, false
	}
	e.whileIn, e.launch, e.reclaim = Active, EveryLaunch, true
	endCtx, done := detachedEnding(ctx)
	defer done()
	decided, err := c.endRecord(endCtx, run, e)
	if err != nil {
		log.WarnContext(ctx, "sandbox_finish_kept", "turn_id", run.TurnID, "error", err.Error(),
			"detail", "the run's ending could not be finished yet; this node finishes it — its "+
				"box included, once the decision is confirmed — and the seat's next recovery "+
				"pass reaps the run if the seat moves first")
		return decided, true
	}
	if decided == nil {
		log.InfoContext(ctx, "sandbox_finish_declined", "turn_id", run.TurnID,
			"detail", "the run is no longer this node's to end — a newer lease owns it, or "+
				"somebody else ended it first — so its box and its record are left to them")
	}
	return decided, decided != nil
}
