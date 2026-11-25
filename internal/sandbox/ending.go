package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/redact"
)

// reportStopped tells the engine one suspended turn has stopped: one helper, so
// "a stop is reported" is one statement, a no-op where nothing is wired. Its
// callers are [Coordinator.park] and [Coordinator.endRecord] and no other may
// become one: an ending reports by deleting the record. See
// [CoordinatorOptions.Stopped].
func (c *Coordinator) reportStopped(ctx context.Context, run PendingRun) {
	if c.stopped == nil {
		return
	}
	c.stopped(ctx, run.AgentHandle, run.TurnID)
}

// FailRun settles a run the turn that launched it cannot suspend into. The engine
// calls it when the conversation a resume would re-enter never reached the record
// (none recorded, not serializable, write failed): the job already executes in its
// box, and marking the record alone would leave it running for nobody, since an
// inactive record is read by no recovery pass. So it is settled like any lost turn while
// this node owns the seat. ONLY A RUN STILL LAUNCHING is settled, the one state the
// failed suspension proves nothing else will act on: a gone record was ended by
// somebody else, a running run carries a conversation after all (a write reported
// failed can have landed) and the poll resumes it, any other state belongs to a
// party that moved it on. An unreadable record is returned (the box is unknown).
func (c *Coordinator) FailRun(ctx context.Context, turnID, reason, detail string) error {
	run, found, err := c.pending.Get(ctx, turnID)
	if err != nil {
		return fmt.Errorf("sandbox: reading run %s to settle it: %w", turnID, err)
	}
	if !found || run.Status != StatusLaunching {
		return nil
	}
	c.settleFailed(ctx, run, reason, detail)
	return nil
}

// announceEnding publishes a decided ending's announcement (the lost run, to the
// board and its seat) under the identity the decision recorded: TWO PUBLISHES, the
// events copy (dashboard, event store) and the per-seat control copy reaching the
// node that ran the turn. THE SAME EVENT ON EVERY ATTEMPT: its id derives from the
// run and ending ([endingEventID]) and its instant is the decision's, so a repeat
// after a crash is one event to the event store (key (event_time, event_id)). Made
// BEFORE the record is deleted (after it, a crash between lost it). The events copy
// must land, being the only account of how the run ended: a refused publish keeps
// the ending for retry ([Coordinator.oweEnding]); the control copy is best effort. A
// STOP IS ANNOUNCED AS ONE ([types.ExecutorStopped]), never a loss ([PendingRun.Stop]).
func (c *Coordinator) announceEnding(ctx context.Context, run PendingRun) error {
	ending := run.Ending
	trace := events.TraceContext{TraceID: run.TraceID, ParentSpanID: run.SpanID}
	var ev *events.Event
	what := ending.Reason
	if stop := run.Stop; stop != nil {
		what = "stopped by " + stop.By
		ev = events.New(types.ExecutorStopped{
			Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
			TurnID: run.TurnID, WorkKey: run.WorkKey, SandboxID: run.SandboxID,
			Harness: run.Harness, LaunchID: run.LaunchID,
			StoppedBy: stop.By, StoppedByKind: stop.ByKind,
			OperatorID: stop.OperatorID, Reason: stop.Reason,
		}, trace)
	} else {
		detail := ending.Detail
		switch {
		case run.EndingReturnsRefusal:
			detail += refusedReturnedDetail
		case ending.Returned:
			detail += replyReturnedDetail
		}
		ev = events.New(types.ExecutorFailed{
			Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
			TurnID: run.TurnID, WorkKey: run.WorkKey, SandboxID: run.SandboxID,
			Harness: run.Harness,
			Reason:  ending.Reason, Detail: redact.Secrets(detail), ErrorKind: ending.ErrorKind,
		}, trace)
	}
	ev.ID, ev.Timestamp = endingEventID(run), ending.At.UTC()
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		return fmt.Errorf("sandbox: announcing the end of run %s (%s): %w", run.TurnID, what, err)
	}
	if control := controlSubject(run.AgentID); control != "" {
		if err := c.queue.Publish(ctx, control, ev); err != nil {
			log.WarnContext(ctx, "sandbox_failure_control_failed",
				"turn_id", run.TurnID, "reason", what, "error", err.Error())
		}
	}
	return nil
}

// endingNamespace derives the ids decided endings are announced under.
var endingNamespace = uuid.MustParse("3c1f5a52-9d0e-5b8a-8f47-2a6d1e9b7c40")

// endingEventID is the id a decided ending's announcement is published under:
// derived from the run and the ending rather than minted per publish, so every
// attempt that finishes the ending publishes the same event.
func endingEventID(run PendingRun) uuid.UUID {
	return uuid.NewSHA1(endingNamespace, []byte(run.TurnID+"/"+run.Ending.ID))
}

// teardown reclaims a run's box and clears the record's reference to it, for
// a run that goes on without its box: one parked straight into reseed. A box
// whose kill failed is its host's sweep's then, the row naming it no more.
func (c *Coordinator) teardown(ctx context.Context, run PendingRun) {
	killCtx, cancel := detached(ctx)
	_ = c.reclaimBox(ctx, killCtx, run)
	cancel()
	if run.SandboxID == "" {
		return
	}
	// THE RECORD'S OWN CONTEXT, not the kill's: that grace is a box call's and the
	// release writes the row whole.
	releaseCtx, release := c.detachedRecord(ctx)
	defer release()
	if err := c.pending.ReleaseBox(releaseCtx, run.TurnID); err != nil {
		log.WarnContext(ctx, "sandbox_release_failed", "turn_id", run.TurnID, "error", err.Error())
	}
}

// ending is one decision to end a run as its caller makes it: the license it is
// taken under, whether a reply its turn took went unused, whether the box is
// reclaimed, and what it announces. ONE VALUE, because a decision can outlive its
// call: one the store could not confirm is kept and made again by this node's
// retry ([Coordinator.oweEnding]) on these terms; once RECORDED on the row
// ([RecordedEnding]) the recorded terms count, whoever finishes it.
type ending struct {
	// fence, whileIn and launch are the license: see [License].
	fence   Fence
	whileIn []string
	launch  string

	// unused is whether the ending lets the recorded answer go back to the seat
	// though a turn TOOK it: the claim's own ending after its turn gave the claim
	// back as a retry (so nothing it did reached anybody) and the release could
	// not land, so no retry will hand the reply to a turn again. Every other
	// ending lets go only a reply no turn took, which the store refuses to delete
	// with the row ([ErrAnswerOwed]), and spends one a turn took.
	unused bool

	// reclaim is whether the box is reclaimed after the decision, by whichever
	// attempt finishes it: every ending not having reclaimed its own box, the
	// store's fence check deciding whether this call may kill it
	// ([Coordinator.finish]).
	reclaim bool

	// note is what the ending announces, nil for none ([Coordinator.announceEnding]).
	note *failureNote

	// stop is the person whose pause ended the run, nil except for a stop's
	// ([Coordinator.StopSeat]); announced in place of a note.
	stop *RunStop

	// counted is whether this node counts the run in the set its status names, so
	// the delete moves it out of the seat's counts as its answer is taken, once
	// (countwrite.go): a run this node launched or claimed and settles itself
	// ([Coordinator.settleNoted], [Coordinator.StopSeat], [Coordinator.abandon]).
	// Kept with the ending so the retry moves the count as its own delete lands.
	// Every other ending moves nothing at its delete: its run is one this node does
	// not count yet (a recovery's) or its caller recounts afterwards.
	counted bool
}

// decision is the ending as the store records it.
func (e ending) decision() Decision {
	d := Decision{
		License: License{Fence: e.fence, WhileIn: e.whileIn, Launch: e.launch, Unused: e.unused},
		Reclaim: e.reclaim,
		Stop:    e.stop,
	}
	if e.note != nil {
		d.Reason, d.Detail, d.ErrorKind = e.note.reason, e.note.detail, e.note.errorKind
	}
	return d
}

// failureNote is a lost run's announcement: its reason and the sentence that
// reaches an operator's board.
type failureNote struct{ reason, detail, errorKind string }

// replyReturnedDetail is what an announcement adds when the ending hands a
// person's answer back to the seat ([RecordedEnding.Returned]), and
// refusedReturnedDetail when what goes back is a reply too large for the run,
// whose refusal it held untold ([PendingRun.EndingReturnsRefusal]).
const (
	replyReturnedDetail   = "; the answer a person gave it goes back to the seat"
	refusedReturnedDetail = "; a reply too large for it to take, which it was never told of, goes back " +
		"to the seat as the message it was"
)

// errEndingOwed marks an ending KEPT for this node's retry
// ([Coordinator.oweEnding]): its decision could not be confirmed or a step (the
// reply handed back, the announcement, the delete) did not land.
var errEndingOwed = errors.New("sandbox: the run's ending is kept until it can be finished")

// endRecord ends a run: it DECIDES the ending ([PendingStore.DecideEnding]) or finds
// one decided, which it finishes in place of its own, and FINISHES it
// ([Coordinator.finishEnding]): reply handed back, box reclaimed, announced, deleted.
// THE ONE PLACE A RUN IS ENDED, so the STOP is reported here, keyed to the decision
// (enumerated endings missed one each round) and only where it is this call's, so a
// racer neither announces a wrong reason nor drops another's hold; also for a turn
// that came back (nothing to drop). Over-reporting is a map lookup, under-reporting
// the defect ([CoordinatorOptions.Stopped]). EXACTLY ONE ANNOUNCEMENT, whoever
// finishes ([Coordinator.announceEnding]). AN UNFINISHED ENDING IS KEPT, retried here
// with the inbox held ([Coordinator.oweEnding]) or by the next holder. Returns the
// decided ending (maybe another's) or nil, and [errEndingOwed] for retrying callers.
func (c *Coordinator) endRecord(ctx context.Context, run PendingRun, e ending) (*RecordedEnding, error) {
	return c.endRun(ctx, run, e, false)
}

// endRun is [Coordinator.endRecord], saying whether its finish may ASK another
// node's host to read a box ([Coordinator.finishEnding]): only this node's own
// retry of a kept ending may ([Coordinator.retryOwed], on the coordinator's own
// context). Every other caller is a frame somebody waits on (pause watch, seat
// acquisition, completion handler), so an ending needing such a read is decided
// there and finished by that retry at once, off the frame.
func (c *Coordinator) endRun(ctx context.Context, run PendingRun, e ending, mayAsk bool,
) (*RecordedEnding, error) {
	decideCtx, decideDone := c.endingCall(ctx)
	decided, ok, err := c.pending.DecideEnding(decideCtx, run.TurnID, e.decision())
	decideDone()
	if err != nil {
		c.reportStopped(ctx, run)
		// FORGOTTEN ON THE STOP'S GATE, for its asymmetry: the decision may have
		// landed, and a count for a run nothing will offer a delivery again is a map
		// entry never dropped ([answerBudget]).
		c.clearAnswerAttempts(run.AgentHandle, run.TurnID)
		return nil, c.keepEnding(ctx, run, e, fmt.Errorf("sandbox: deciding the end of run %s: %w",
			run.TurnID, err))
	}
	if !ok {
		return nil, nil
	}
	// THE RUN IS SETTLED HERE, whatever finishes it: its end is decided, so no
	// collection of its job is made again and the clock on its box host's store
	// goes now ([hostUnsettled]), a lost job's carried round included; not at the
	// delete, which an end the executor log is owed keeps minutes behind. So does a
	// re-fire's announcement still owed ([refirePublish]).
	c.runSettled(decided.TurnID)
	c.reportStopped(ctx, decided)
	c.clearAnswerAttempts(decided.AgentHandle, decided.TurnID)
	c.noteLaunchStop(decided)
	ending := *decided.Ending
	finished, err := c.finishOnce(ctx, decided, mayAsk, e.counted)
	switch {
	case !finished && !mayAsk:
		// ANOTHER ATTEMPT ON THIS NODE IS FINISHING IT, and this node's retry keeps
		// it too, holding the seat's mail until it lands and asked again when that
		// attempt returns ([Coordinator.finishOnce]): finished, the retry finds the
		// record gone; kept, it finishes it. Left to that attempt alone, the ending
		// held nothing whenever its series was gone (a retry under way while the
		// seat was released and retaken, waiting on a keep or the log) and the
		// seat's mail ran while the record still owed the turn's end. Not this
		// frame's failure. Kept ON THE DECISION'S TERMS, licensed for every status
		// and job: the retry finishes the decision recorded.
		kept := e
		kept.whileIn, kept.launch = Active, EveryLaunch
		c.oweEndingAfter(ctx, decided, kept, answerRetryCeiling)
		return &ending, nil
	case !finished:
		return &ending, c.keepEnding(ctx, decided, e, errEndingBusy)
	case errors.Is(err, errAskAway):
		// DECIDED, its session closed, FINISHED BY THIS NODE'S OWN RETRY now: not a
		// failure.
		c.oweEndingAfter(ctx, decided, e, 0)
		return &ending, nil
	case errors.Is(err, errAwaitsFrame) && !mayAsk:
		// THE TURN'S FRAME IS STILL RUNNING HERE (this frame's own, a relaunch it
		// made being abandoned) and ends the turn: its settle finishes the ending
		// when it returns, and this node's retry if not ([Coordinator.framing]).
		c.oweEndingAfter(ctx, decided, e, answerRetryCeiling)
		return &ending, nil
	case err != nil:
		return &ending, c.keepEnding(ctx, decided, e, err)
	}
	return &ending, nil
}

// errEndingBusy is a decided ending another attempt on this node is finishing
// ([Coordinator.finishOnce]): left to it, and asked again by this node's retry
// the moment it returns.
var errEndingBusy = errors.New("sandbox: another attempt on this node is finishing the run's ending")

// errAwaitsFrame is a decided ending whose turn's end is still its resumed
// frame's to decide, that frame running on this node ([Coordinator.framing]).
var errAwaitsFrame = errors.New("sandbox: the run's turn is still running on this node, and decides its own end")

// finishOnce is [Coordinator.finishEnding] made by ONE attempt on this node at a
// time per turn, answering false and doing nothing where another is finishing it.
// An ending's steps are harmless to REPEAT, not to make TWICE AT ONCE: two attempts
// reading the row before either owed the turn's end both answered the colleague
// whose ask the turn took up ([CoordinatorOptions.CloseStopped],
// [CoordinatorOptions.TurnLost]) (a stop decided on the pause watch while its job
// launched was finished there and by the launch, the asker waking to two answers).
// The other attempt keeps the ending for the retry ([Coordinator.endRun]), asked
// again the moment the first returns, not on the hand-back's spacing.
func (c *Coordinator) finishOnce(ctx context.Context, run PendingRun, mayAsk, counted bool) (bool, error) {
	c.mu.Lock()
	if c.finishing[run.TurnID] {
		c.mu.Unlock()
		return false, nil
	}
	if c.finishing == nil {
		c.finishing = map[string]bool{}
	}
	c.finishing[run.TurnID] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.finishing, run.TurnID)
		if r := c.retries[run.TurnID]; r != nil && r.ending != nil && r.stop != nil {
			c.scheduleOwedLocked(run.TurnID, 0)
		}
	}()
	return true, c.finishEnding(ctx, run, mayAsk, counted)
}

// frameBegins and frameEnds bracket a resumed turn's frame on this node
// ([Coordinator.resumeAndSettle]); framing reports whether one of turnID's runs
// here. An ending decided while the frame still runs (its relaunch abandoned under
// it, [Coordinator.abandon]) leaves the turn's end to that frame, which owes its
// own account on the row ([Coordinator.OweEnd]); decided by the ending instead,
// the end was a second account (the colleague answered twice) or none (the row and
// capture stamps deleted first). In memory, since another node's frame is one the
// seat moved off.
func (c *Coordinator) frameBegins(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frames == nil {
		c.frames = map[string]int{}
	}
	c.frames[turnID]++
}

func (c *Coordinator) frameEnds(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frames[turnID] > 1 {
		c.frames[turnID]--
		return
	}
	delete(c.frames, turnID)
	// AN ENDING THAT WAITED ON THE FRAME is asked again when it returns.
	if r := c.retries[turnID]; r != nil && r.ending != nil {
		c.scheduleOwedLocked(turnID, 0)
	}
}

func (c *Coordinator) framing(turnID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frames[turnID] > 0
}

// errAskAway is a decided ending whose next step may not be made on the frame
// that reached it ([Coordinator.endRun]): a read of a box on another node's host,
// a job's session take, or the turn's end on the executor log, each waiting on
// something not this frame's to wait on. Handed to this node's retry, which runs
// on the coordinator's own lifetime and may.
var errAskAway = errors.New("sandbox: the ending's next step is made off this frame, by its retry")
