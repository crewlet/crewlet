package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// OnCompleted claims the run, collects, accounts, then resumes the loop.
func (c *Coordinator) OnCompleted(ctx context.Context, ev types.ExecutorFinished, trigger *events.Event) error {
	fence, held := c.leaseOf(ev.AgentHandle)
	if !held {
		// NOT THIS NODE'S TO CLAIM: a ROUTING failure, handed back to reach the
		// holder; the poll fires it again while the job's row runs.
		return fmt.Errorf("%w: %w", ErrResumeElsewhere,
			notHeld(ctx, ev.AgentHandle, ev.TurnID, "claiming a completion"))
	}
	// ADMITTED BEFORE THE CLAIM, held through the resume ([TurnGate]). A refusal
	// leaves the run unclaimed and running, and the waiter announces again
	// (reannounce.go); its condition decides what the caller does with this
	// delivery, unless no waiter can announce the job (the clock on its box's
	// host running), when it is handed back whatever the condition (renew.go).
	// An acknowledged one is first written on the job's record
	// ([Coordinator.oweRefused]). What was signalled is read first
	// ([Coordinator.keepRefused]) and handed to the resume, whose seat's budget
	// may hold it ([Coordinator.resumeAndSettle]).
	seen := c.signalsSeen()
	// HELD BY THIS NODE before its gate is asked: the last resume here met a
	// post-claim refusal that lifts at an instant the gate cannot see
	// ([Coordinator.keepRefused]). Refused as the gate refuses; quietly, since
	// the hold was said when taken and announcements repeat it.
	if refusal, held := c.stillRefused(ev); held {
		if c.pastDeadline(ctx, ev) {
			return c.completeAtDeadline(ctx, ev, fence, refusal.Reason)
		}
		log.DebugContext(ctx, "sandbox_completion_held_here",
			"turn_id", ev.TurnID, "agent", ev.AgentHandle, "condition", string(refusal.Condition),
			"until", refusal.Until.UTC().Format(time.RFC3339))
		return fmt.Errorf("%w: %w", ErrResumeUnavailable, &TurnRefused{Refusal: refusal})
	}
	// NO CONVERSATION FOR A COMPLETION: its run holds the conversation's one
	// turn, which this resume continues (ADR-0169).
	adm, err := c.admitTurn(ctx, ev.AgentHandle, "", ResumeCompletion)
	if err != nil {
		if c.pastDeadline(ctx, ev) {
			// PAST ITS TURN'S DEADLINE A REFUSED RESUME IS NOT WAITED FOR
			// ([Coordinator.completeAtDeadline]).
			return c.completeAtDeadline(ctx, ev, fence, err.Error())
		}
		log.InfoContext(ctx, "sandbox_completion_not_admitted",
			"turn_id", ev.TurnID, "agent", ev.AgentHandle, "reason", err.Error(),
			"detail", "this node may not start the turn the completion resumes, so the "+
				"run is left unclaimed for the waiter to announce again, or for this delivery "+
				"where the box's host cannot read the coordination store, here once the "+
				"condition clears or on the seat's next holder")
		c.keepRefused(ev, trigger, refusalOf(err), false, seen)
		return fmt.Errorf("%w: %w", ErrResumeUnavailable, err)
	}
	defer adm.release()
	c.forgetAdmitted(ev)
	err = c.completeAdmitted(adm, ev, trigger, fence, seen)
	if errors.Is(err, errRunMovedOn) {
		// THE RUN MOVED ON BEFORE THIS ATTEMPT COULD END IT and this delivery is
		// SETTLED: the next holder fenced the claim and its recovery reaps and
		// announces the run, or somebody ended it first; a redelivery finds no
		// running job to claim. Handed back it would be a message failure on the
		// control topic (internal/engine's controlOutcome), where this is a
		// routing failure that could send a healthy completion to the poison.
		// The answer route reads it as not this node's to say
		// ([Coordinator.resumeOwedAdmitted]).
		log.InfoContext(ctx, "sandbox_completion_moved_on",
			"turn_id", ev.TurnID, "launch_id", ev.LaunchID, "agent", ev.AgentHandle,
			"detail", "the run moved on to the seat's next holder, or was ended by another "+
				"party, before this node could end it; that party drives what becomes of it, "+
				"and this delivery is settled")
		return nil
	}
	if refusal, refused := waitingRefusal(err); refused {
		// REVERTED ON A WAIT: the refusal is kept for the signal that ends it and
		// handed back by its condition, as the gate's own is, against what was
		// signalled before the gate was asked, so a signal landing during the
		// resume re-fires at once. HELD HERE until its instant, since the gate
		// admitted it and would again: this node's own, never the run's row
		// ([Coordinator.keepRefused]).
		held := !refusal.Until.IsZero()
		if held {
			log.InfoContext(ctx, "sandbox_completion_held_here",
				"turn_id", ev.TurnID, "agent", ev.AgentHandle, "condition", string(refusal.Condition),
				"reason", refusal.Reason, "until", refusal.Until.UTC().Format(time.RFC3339),
				"detail", "the resume met a condition its gate could not see; this node refuses "+
					"the job's announcements until the condition's instant or its signal, and a "+
					"restart or the seat's next holder asks its own models")
		}
		c.keepRefused(ev, trigger, refusal, held, seen)
	}
	return err
}

// completeAdmitted is a completion's claim, collection and resume, under the
// admission [Coordinator.OnCompleted] took for it.
//
// IT TAKES THE ADMISSION, NOT A CONTEXT ([Coordinator.resumeOwedAdmitted]):
// everything from the claim on belongs to the admitted turn. seen is what was
// signalled before the admission was asked, for a claim the hold on the run's
// row refuses ([Coordinator.keepHeldClaim]) and a resume its seat's budget holds
// ([Coordinator.resumeAndSettle]).
func (c *Coordinator) completeAdmitted(adm admitted, ev types.ExecutorFinished,
	trigger *events.Event, fence Fence, seen map[signal]uint64,
) error {
	ctx := adm.ctx
	// NOTHING OF THE JOB IS CLAIMED WHILE THIS NODE GIVES BACK, OR OWES, THE
	// RELEASE OF ITS LAST CLAIM ([Coordinator.claimCompletion]): that release,
	// made again, would give back a claim taken now. The completion comes back on
	// the queue's backoff. NOR WHILE THE SEAT'S RECOVERY PASS RUNS HERE: the
	// claim's gate refuses it (recoverygate.go). Let through, the claim is this
	// node's live attempt until this frame returns.
	run, won, done, err := c.claimCompletion(ctx, ev, fence)
	if done != nil {
		defer done()
	}
	if err != nil {
		return fmt.Errorf("sandbox: claiming %s: %w", ev.TurnID, err)
	}
	if !won {
		// Not this signal's to run: already claimed by a duplicate (poll ticks
		// firing before the claim landed, a redelivery, a failed resume's retry),
		// or the row no longer holds its job running (parked, replaced by the next
		// launch, over), or a newer lease owns it, or its resume is held (a
		// duplicate queued behind the resume that held it, announced again once
		// the hold lifts; kept for the hold here, [Coordinator.keepHeldClaim]).
		// Where no poll can announce it, the delivery is handed back (renew.go).
		log.InfoContext(ctx, "sandbox_completion_already_claimed",
			"turn_id", ev.TurnID, "launch_id", ev.LaunchID)
		c.keepHeldClaim(ctx, ev, trigger, fence, seen)
		return nil
	}
	// THE CLAIM SETTLES THE JOB from here, so what is kept for its run goes,
	// including a keep for the row's hold ([Coordinator.forgetAdmitted]).
	c.forgetRefused(ev.TurnID)

	// THE JOB WAS LOST WITH ITS HOST ([PendingRun.Lost]) and this completion is
	// the waiter's announcement of that, or the collection below finds it lost
	// with its box's host ([Coordinator.loseWithHost]): what it left is no result.
	// Its round is carried on, resumed HERE ALONE under this frame's admission: a
	// helper between this and resumeOwedAdmitted would be a road to a turn
	// internal/node's admission guard refuses (ADR-0063).
	lost := run.Lost != nil
	var result Result
	if !lost {
		var collected bool
		run, result, collected, err = c.collectClaimed(ctx, run)
		switch {
		case !collected && run.Lost != nil:
			lost = true
		case !collected:
			return err
		}
	}
	if lost {
		var carry *lostCarry
		carry, err = c.carryOnLost(ctx, run)
		if err != nil || carry == nil {
			return err
		}
		_, err = c.resumeAndSettle(adm, carry.run, carry.report, false, trigger, carry.outcome, seen)
		return err
	}

	if run.asked() {
		return c.park(ctx, run, result)
	}

	// THE ERROR IS THIS ROUTE'S WHOLE ANSWER. A completion is handed back by
	// NAKing, which spends one of the broker's own 25 deliveries, so the retry is
	// bounded where it stands; an answer needs a bound that stops SHORT of the
	// dead-letter boundary because it spends a person's reply
	// ([AnswerDeliveryReserve], off the message's own count, which a per-process
	// ceiling cannot match across handoffs). Every disposition returning an error
	// is the retry, every other an ending.
	_, err = c.resumeAndSettle(adm, run, runReport(run, result), result.Success, trigger, runOutcome{
		DeliveredRefs: result.DeliveredRefs, DeliveredRefsElided: result.DeliveredRefsElided,
		Pushed: result.Pushed, PushedElided: result.PushedElided, PushedUnread: result.PushedUnread,
		InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
		CacheReadTokens: result.CacheReadTokens, CacheWriteTokens: result.CacheWriteTokens,
		Engine: run.Launch.Bridged.Plus(EngineSpend{Aux: result.Condensed}),
		Ended: RunEnding{Text: result.Text, Error: result.Error, Event: result.Event,
			Limits: result.Limits, NoLimitExit: result.NoLimitExit, Success: result.Success, At: run.Launch.CollectedAt, Refused: requestRefused(run, result)},
	}, seen)
	return err
}

// settledConnect is run as a collection reads it, where its row holds no
// record that its harness connected its bridge: once this node's write of one
// under way is over ([CoordinatorOptions.ConnectSettled]), with the record
// read again, and where this node saw the harness connect with no write
// landed, with the record written here. An error (the wait's, the read's or
// the write's) fails the read, which is retried: never a run read as one
// that did not connect because a write had not landed.
func (c *Coordinator) settledConnect(ctx context.Context, run PendingRun, read BoxRead) (PendingRun, error) {
	if read != BoxReadCollect || run.Launch.BridgeConnected || c.connectSettled == nil {
		return run, nil
	}
	unrecorded, err := c.connectSettled(ctx, run.TurnID)
	if err != nil {
		return run, fmt.Errorf("sandbox: waiting for the record that run %s's harness connected its bridge: %w",
			run.TurnID, err)
	}
	if unrecorded {
		if _, err = c.pending.MarkBridgeConnected(ctx, run.TurnID, run.LaunchID); err != nil {
			return run, fmt.Errorf("sandbox: run %s's harness connected its bridge, and the record of it "+
				"could not be written: %w", run.TurnID, err)
		}
	}
	again, found, err := c.pending.Get(ctx, run.TurnID)
	if err != nil {
		return run, fmt.Errorf("sandbox: reading run %s's record of its bridge connection: %w", run.TurnID, err)
	}
	if found && again.LaunchID == run.LaunchID {
		run.Launch.BridgeConnected = again.Launch.BridgeConnected
	}
	return run, nil
}

// collectClaimed reads a claimed run's job back and records its result, charge
// and launch record on the claimed row, so a handed-back claim carries them.
// collected is false where the box could not be read (err is
// [Coordinator.collectFailed]'s answer), or where only the box's host kept it
// unread for the host grace, when the run comes back lost with that host
// ([Coordinator.loseWithHost]). One collection serves a resume and an ending at
// the turn's deadline ([Coordinator.completeAtDeadline]), so an unresumed run is
// still charged and recorded for what its job did.
func (c *Coordinator) collectClaimed(ctx context.Context, run PendingRun) (PendingRun, Result, bool, error) {
	// THE CLAIM AS THE READ LEAVES IT, marked once its box answered
	// ([Coordinator.collect]): every later hand-back carries it.
	run, result, ended, err := c.collect(ctx, run)
	if err != nil {
		// ONE VERDICT ON THE READ'S FAILURE ([collectVerdict]), acted on as it is,
		// never re-judged at a later clock reading. A read only the box's host
		// kept from it for the host grace is a job lost with that host, unless
		// this node itself cut the collection short.
		verdict := c.collectGivenUp(run, err, c.now())
		if verdict.lostWithHost() && !errors.Is(ctx.Err(), context.Canceled) {
			lost, lostErr := c.loseWithHost(ctx, run, err, verdict)
			return lost, Result{}, false, lostErr
		}
		return run, Result{}, false, c.collectFailedAt(ctx, run, err, verdict)
	}
	// THE JOB'S CALLS STILL IN FLIGHT are waited for on the run's log before
	// anything reads what the job did ([Coordinator.drainCalls]): its session
	// ends to new calls first, and a call its harness left running is recorded,
	// so the resume reads what it committed. A wait this node's stop cut hands
	// the claim back uncounted ([Coordinator.collectFailed]).
	drained, err := c.drainCalls(ctx, run)
	if err != nil {
		return run, Result{}, false, c.collectFailed(ctx, run, err)
	}
	// WHAT THE JOB'S CALLS COST ONCE IT IS OVER is read again where the claim's
	// read cannot be the whole: a run ENDED AT ITS TURN'S CAP made bridge calls
	// until the end, their spend on the record, not the claim, and a drained
	// call wrote its own. The resume reads the calls themselves from the log
	// ([Coordinator.resumeAndSettle]), judging and charging all of them.
	if ended || drained {
		run = c.rereadSpend(ctx, run)
	}
	if ended {
		// NO QUESTION, whatever it asked on its way: a run being stopped is not
		// one a person's answer could continue ([endedAtDeadline]), so it is
		// collected and resumed like any ending, nothing parked.
		run = withoutQuestion(run)
	}

	// WRITTEN BEFORE THE TURN RESUMES, since its reviewer's send-back relaunches
	// on that frame and reads the row: a store that does not take it is a
	// collection to ask again, uncounted, since its silence is no failure of the
	// box's ([ErrStoreSilent]).
	if spent := sessionSpent(run, result); spent != "" {
		sessions := sessionsRanOutOfContext(run, result)
		if err = c.pending.RetireSessions(ctx, run.TurnID, spent, sessions); err != nil {
			return run, Result{}, false, c.collectFailed(ctx, run, storeSilent(ctx, err))
		}
		run.SpentSessions = withSpent(run.SpentSessions, spent, sessions)
	}

	// Carried on the claimed row from here, so a handed-back claim carries every
	// record. First that the box answered, which ends any run of failed
	// collections behind this one ([Release.Collected]).
	run.Collected = true
	// AND WHEN, the first time: the instant a later collection reads the job's
	// ending against ([LaunchRecord.CollectedAt]).
	if run.Launch.CollectedAt.IsZero() {
		run.Launch.CollectedAt = c.now().UTC()
	}
	// AND HOW ITS HARNESS ENDED, as the read just recorded it ([LaunchRecord.HarnessEnd]):
	// the claim was read before that write, and the resume on this claim (a round
	// a replaced sign-in carries on in the box) reads it here. The same for the
	// newest request's size ([LaunchRecord.Context]), the harness's count, its
	// provider and release ([LaunchRecord.counted]: what the copy may have
	// compacted away) and its next-request estimate ([LaunchRecord.nextRequested]),
	// each by the rule its record follows.
	run.Launch.HarnessEnd = run.Launch.HarnessEnd.after(result.HarnessEnd)
	run.Launch.Context = run.Launch.measured(result.LastRequest, run.LaunchID)
	run.Launch.ResumedCount, run.Launch.CountProvider, run.Launch.CountRelease = run.Launch.counted(
		result.ResumedCount, result.CountProvider, result.HarnessVersion)
	run.Launch.setNextRequest(run.Launch.nextRequested(result.NextRequest))
	run.Charged, run.CompanyCharged = c.charge(ctx, run, result)
	run.Launch = c.publishCollected(ctx, run, result)
	return run, result, true, nil
}

// rereadSpend is run with what its job's bridged calls cost the engine read
// again off its row ([LaunchRecord.Bridged]): the run as claimed where the row
// cannot be read or no longer holds the job.
//
// Every append, a session's carry of what it owed included, writes the running
// total onto the job's record ([PendingStore.AppendBridgeCall]), and the resume
// charges the engine spend off this run ([ResumeRequest.Engine]). Not read
// again, a carried call's work (a `delegate`'s workers above all) is in the log
// and charged nowhere, since the next launch resets the record.
func (c *Coordinator) rereadSpend(ctx context.Context, run PendingRun) PendingRun {
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		// The claim's spend stands; the line says a late call's cost may be missing.
		log.WarnContext(ctx, "sandbox_run_spend_reread_failed",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "error", err.Error())
	case found && latest.LaunchID == run.LaunchID:
		// THE NEWER TOTAL ([EngineSpend.Newest]): each figure only grows within
		// one session's meter.
		run.Launch.Bridged = run.Launch.Bridged.Newest(latest.Launch.Bridged)
	}
	return run
}

// collectFailed answers a collection that could not read its box back: ask
// again within a bound, then give the run up. Safe because nothing the tail
// writes precedes a successful collection, so the run stays as found and the
// completion is NAK'd. The bound is the waiter's ([MinConnectFailures]
// consecutive attempts spanning [ConnectGiveUp]), counted on the job's record; a
// read that answered ends the run of failures. A gone box ([ErrBoxGone]) is
// settled at once; a collection this node cancelled or whose read never
// started ([errReadUnstarted]) and a step the store did not answer
// ([ErrStoreSilent]) are handed back uncounted; a hand-back the store
// does not answer is kept, never an ending. One verdict per failure
// ([collectVerdict]). See the package doc, "Collection failures".
func (c *Coordinator) collectFailed(ctx context.Context, run PendingRun, cause error) error {
	return c.collectFailedAt(ctx, run, cause, c.collectGivenUp(run, cause, c.now()))
}

// collectFailedAt is [Coordinator.collectFailed] on the verdict its caller
// judged cause by ([Coordinator.carryOnLost]), so the hand-back never decides
// apart from it at a later clock reading.
func (c *Coordinator) collectFailedAt(ctx context.Context, run PendingRun, cause error,
	verdict collectVerdict,
) error {
	var answered *boxAnsweredError
	if errors.As(cause, &answered) {
		// THE BOX ANSWERED before a later step failed ([boxAnsweredError]): it
		// ends the run of failed collections, and the hand-back's own write
		// records how the job's harness ended ([Release.HarnessEnd]).
		run.BoxAnswered, run.BoxHarnessEnd = true, answered.end
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(cause, errReadUnstarted) {
		log.InfoContext(ctx, "sandbox_collect_interrupted", "turn_id", run.TurnID,
			"error", cause.Error(),
			"detail", "the collection was stopped by this node, not by its box; the claim is "+
				"handed back uncounted and the completion is retried")
		if err := c.unclaimAt(ctx, run, collectUnrevertedDetail, failure{}, nil,
			BudgetWindow{}); err != nil {
			//nolint:nilerr // As below: the run has been ended and announced.
			return nil
		}
		return fmt.Errorf("sandbox: collecting %s was interrupted: %w", run.TurnID, cause)
	}
	if errors.Is(cause, ErrBoxHostSilent) {
		// The box's host did not answer ([ErrBoxHostSilent]), which says nothing
		// about the box: handed back uncounted and acknowledged, the host's waiter
		// announcing again (reannounce.go), unless this node keeps a clock on that
		// host's store ([Coordinator.hostClocked]): its waiter announces nothing
		// through a silent store, so the completion is handed back for the queue's
		// backoff and renewed before its last delivery (renew.go).
		clocked := c.hostClocked(run.TurnID, run.LaunchID)
		log.WarnContext(ctx, "sandbox_collect_host_silent", "turn_id", run.TurnID,
			"node", run.LaunchNode, "store_silent", clocked, "error", cause.Error(),
			"detail", "the node whose host the run's box is on did not answer the collection; "+
				"the claim is handed back uncounted and that node announces the completion again, "+
				"or, where it last found its coordination store silent, the completion is asked "+
				"again on the queue's backoff")
		// A hand-back the store refused ended and announced the run itself; one it
		// did not answer is this node's to repeat ([Coordinator.unclaimAt]).
		if err := c.unclaimAt(ctx, run, collectUnrevertedDetail, failure{}, nil,
			BudgetWindow{}); err != nil || !clocked {
			//nolint:nilerr // Ended and announced where the hand-back was refused, as below; acknowledged
			// otherwise, no clock running on that host's store.
			return nil
		}
		return fmt.Errorf("sandbox: collecting %s: %w", run.TurnID, cause)
	}
	now := verdict.at
	if errors.Is(cause, ErrStoreSilent) {
		// The coordination store did not answer a step the collection needs
		// ([ErrStoreSilent]): handed back uncounted and NAK'd. For this node's own
		// store the NAK suffices: the waiter announces a running unheld row within
		// [reannounceCeilingPolls] polls, inside the delivery budget. Where only
		// the box host's store is silent no waiter announces, so the completion is
		// renewed before its last delivery (renew.go) until the store answers or
		// the bound records the job lost with that host.
		log.WarnContext(ctx, "sandbox_collect_unsettled", "turn_id", run.TurnID,
			"error", cause.Error(),
			"detail", "the coordination store did not answer a step the collection cannot go on "+
				"without; the claim is handed back uncounted and the collection is asked again "+
				"until the store answers, or until the host grace where only the box's host "+
				"finds it silent")
		if err := c.unclaimAt(ctx, run, collectUnrevertedDetail, failure{}, nil,
			BudgetWindow{}); err != nil {
			//nolint:nilerr // As below: the run has been ended and announced.
			return nil
		}
		return fmt.Errorf("sandbox: collecting %s: %w", run.TurnID, cause)
	}
	facts := run.Launch
	attempts := facts.CollectFailures + 1
	since := facts.CollectFailingSince
	if since.IsZero() {
		since = now
	}
	gone := errors.Is(cause, ErrBoxGone)
	if verdict.givenUp {
		log.ErrorContext(ctx, "sandbox_collect_failed", "turn_id", run.TurnID,
			"attempts", attempts, "failing_for_s", now.Sub(since).Seconds(),
			"box_gone", gone, "error", cause.Error())
		c.runSettled(run.TurnID)
		// The job is OVER though collection failed: free the seat and settle the
		// row whatever the network cleanup manages.
		c.settleFailed(ctx, run, types.ExecutorFailureCollect,
			"the executor's launch finished but its box could not be read back, so its "+
				"result is lost; the work it pushed, if any, is on its branch")
		return nil
	}
	log.WarnContext(ctx, "sandbox_collect_retried", "turn_id", run.TurnID,
		"attempt", attempts, "failing_for_s", now.Sub(since).Seconds(), "error", cause.Error(),
		"detail", "the box could not be read back; the claim is handed back and the "+
			"collection is retried until it has failed for the poll's give-up window")
	// COUNTED: the claim did not move the seat's holding count (running and
	// resumed both hold it). A release the store does not answer counts the
	// failure once, when it lands ([Coordinator.unclaimAt]). AND COUNTED ONTO THE
	// RUN OF FAILURES BEHIND IT even where the box answered this read: a step
	// after it was cut by the delivery's deadline, and a collection always
	// outlasting its delivery would otherwise start a new run each time and never
	// be given up ([Release.BoxAnswered]).
	run.BoxAnswered = false
	if err := c.unclaimAt(ctx, run, collectUnrevertedDetail, failure{collectFailed: now}, nil,
		BudgetWindow{}); err != nil {
		//nolint:nilerr // Deliberate, as at every other hand-back: the claim
		// did not go back, so the run has been ended and announced, and a
		// redelivered completion would find nothing to claim.
		return nil
	}
	return fmt.Errorf("sandbox: collecting %s (attempt %d): %w", run.TurnID, attempts, cause)
}

// runOutcome is what a finished run reported about itself, for the resumed
// phase's record and segment charge. The refs are empty where no run finished (a
// person's answer collects nothing); the tokens are the job's either way
// ([ResumeRequest.InputTokens]). Fields mirror [ResumeRequest]'s.
type runOutcome struct {
	DeliveredRefs       []string
	DeliveredRefsElided int
	Pushed              []PushedRef
	PushedElided        int
	PushedUnread        string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheWriteTokens    int
	Engine              EngineSpend
	Ended               RunEnding
	Asked               *AskedRun
	Lost                *LostRun
}

// parkedEngineSpend is what the engine spent on a parked job outside every
// segment, for the resume its answer drives (bridged calls and condensing the
// parking collection), off the job's own record since nothing was collected.
func parkedEngineSpend(run PendingRun) EngineSpend {
	facts := run.Launch
	return facts.Bridged.Plus(EngineSpend{Aux: facts.Condensed})
}

// collect reconnects, reads the result, and pauses the box rather than tearing
// it down: a relaunch continues in it, a parked run waits in it, a retried
// resume reads it again; the pause reaper bounds it ([PendingRun.HeldSince]).
// ended reports a run its turn's wall-clock cap ended ([PendingRun.Deadline]):
// the read ends the job first, its bridge session closed before
// ([CoordinatorOptions.Ended]), and the result says it was cut off. The claim is
// marked the moment the box answered ([PendingRun.BoxAnswered]). See the
// package doc, "Collecting a box".
func (c *Coordinator) collect(ctx context.Context, run PendingRun) (PendingRun, Result, bool, error) {
	if c.ended != nil && run.PastDeadline(c.now()) {
		c.ended(run.TurnID)
	}
	// THE BOX IS READ WHERE IT IS ([Coordinator.boxResult]); the rest is this
	// node's work under the turn the result is fitted for.
	result, ended, err := c.boxResult(ctx, run, BoxReadCollect)
	if err != nil {
		return run, Result{}, false, err
	}
	// THE BOX ANSWERED, ending the run of failed collections behind this one
	// whatever a later step does ([Release.BoxAnswered], [Release.HarnessEnd]).
	// Marked here and not once this read returns: reading the pushes follows, and
	// a read cut short there is one of those steps. The delivery knows it, so
	// it reads no row for what the host's store says (renew.go).
	run.BoxAnswered, run.BoxHarnessEnd = true, result.HarnessEnd
	readTheBox(ctx)
	if ended {
		result = endedAtDeadline(result, run.Deadline, run.Question)
	}
	c.warnNotesUnasked(ctx, run, result)
	// WHAT IT PUSHED, read here, where the result is used: the push record is
	// Gitlet's, which the seat's node reaches ([RunPushes]).
	if result, err = c.readPushes(ctx, run, result); err != nil {
		return run, Result{}, false, err
	}
	// HELD TO THE RECORD'S BOUND here, where every collected result enters,
	// so the launch's record, the park and the run's report carry one text.
	return run, c.fitText(ctx, run, result), ended, nil
}

// noteReader is a [Runner] whose harness takes a person's notes to its run's turn
// between its own steps (internal/sandbox/executor's Runner). Optional: a runner
// that reads none says nothing about notes.
type noteReader interface{ ReadsNotes() bool }

// warnNotesUnasked says, as steer_reader_never_ran, that a run which succeeded
// never asked for its turn's notes ([RunSteer.Pulled]), telling a run whose
// harness's reading was switched off from one with nothing to read.
//
// RELIABLE for a harness that reads notes: its reader asks before the run can
// succeed. A run that succeeded without asking had its reading disabled, as
// its runner says what can do that ([notesOffWhy]), and every note waited for
// its review. Only for a run whose record names its seat
// ([LaunchRequest.SteerSeat]); logged on the collecting node.
func (c *Coordinator) warnNotesUnasked(ctx context.Context, run PendingRun, result Result) {
	if !result.Success || len(run.Steer.Seat) == 0 || run.Steer.Pulled == run.LaunchID {
		return
	}
	manager := c.mgr()
	if manager == nil {
		return
	}
	runner, err := manager.RunnerFor(run.Harness)
	if err != nil {
		return
	}
	if reader, ok := runner.(noteReader); !ok || !reader.ReadsNotes() {
		return
	}
	detail := "the run succeeded and its harness never asked for its turn's notes, so a note sent " +
		"while it worked waited for its review instead of reaching it between its steps. Something in " +
		"the box switched the harness's reading off"
	if why := notesOffWhy(runner); why != "" {
		detail += ": " + why
	}
	log.WarnContext(ctx, "steer_reader_never_ran", "turn_id", run.TurnID,
		"launch_id", run.LaunchID, "agent", run.Harness, "detail", detail)
}

// readBox is the part of a collection touching the box, made on its host's node:
// reconnect, end the job where its cap passed and it still works, read and floor
// the result, bound transcript and refs ([boundBox]), take the session
// ([Coordinator.takeCapture]), and for a completion record how the harness ended
// ([LaunchRecord.HarnessEnd]) and pause the box. ENDED is the run its cap ended.
// A STOPPED RUN'S READ ([BoxReadStop]) ENDS ITS JOB FIRST ([Coordinator.endJob]):
// its script packs the session as its own exit does, so it is final, and PAUSES
// NOTHING (the ending reclaims the box). So does a cap-passed job's collection:
// packed beside a writing harness the session was never final. That the cap ended
// the job is recorded before it is asked or killed ([LaunchRecord.EndedAtDeadline]).
func (c *Coordinator) readBox(ctx context.Context, run PendingRun, read BoxRead) (Result, bool, error) {
	return c.readBoxFor(ctx, run, read, false)
}

// readBoxFor is [Coordinator.readBox] for the node collecting the run: away
// where that is another node than this, the box's host, so the run's seat
// moved since its launch ([RunHandle.BridgeHolderChanged]).
func (c *Coordinator) readBoxFor(ctx context.Context, run PendingRun, read BoxRead, away bool) (Result, bool, error) {
	manager := c.mgr()
	box, runner, err := manager.Reconnect(ctx, Placement(run.Placement), run.SandboxID, run.Harness)
	if err != nil {
		return Result{}, false, err
	}
	// WHETHER THE JOB IS OVER: a stop's is ended first, and a collection's that
	// still works or whose poll is silent once its cap passed (by this node's clock
	// or an earlier recorded end, [LaunchRecord.EndedAtDeadline]). Asked first, so a
	// job that finished by itself is collected normally, not swept or called cut
	// off; only one still working after its end is packed non-final ([readCapture]).
	// ENDED marks the result cut off at the cap: a job SEEN working past it (poll
	// said running, or its script heard the end request, [Coordinator.endJob]) and
	// RECORDED when seen, before it is asked or killed, since the kill can take the
	// script before it records the request. A silent poll shows nothing, and never
	// read it off whether the job is over after its end (a stopped job is over too).
	capped := read == BoxReadCollect && (run.Launch.EndedAtDeadline || run.PastDeadline(c.now()))
	ended, over := capped && run.Launch.EndedAtDeadline, true
	sawWorking := func() {
		if !ended {
			ended = true
			c.markEndedAtDeadline(ctx, run)
		}
	}
	switch {
	case read == BoxReadStop:
		over = c.endJob(ctx, run, box, runner, nil)
	case capped:
		state, pollErr := runner.Poll(ctx, box, run.Handle())
		if pollErr == nil && state == JobRunning {
			sawWorking()
		}
		if pollErr != nil || !state.Over() {
			over = c.endJob(ctx, run, box, runner, sawWorking)
		}
	}
	handle := run.Handle()
	handle.BridgeHolderChanged = away
	result, err := runner.Collect(ctx, box, handle, c.answerBudget)
	if err != nil {
		return Result{}, false, err
	}
	// A MINTING JOB'S SESSION IS BOUND AS IT IS COLLECTED (minted.go).
	if run, result, err = c.bindCollected(ctx, run, runner, result); err != nil {
		return Result{}, false, err
	}
	// THE JOB'S OWN RECORD THAT A REQUEST ENDED IT ([Result.JobEnd]) is the same
	// sight where the job's record did not take this read's: only the cap asks a
	// collected job to end (stops and losses are endings no completion collects),
	// so the script that heard it was still working. It covers a request whose
	// answer was lost and a repeat after a record that did not land; otherwise
	// the run would be parked on a question asked on its way out
	// ([Coordinator.collectClaimed]).
	if capped && result.JobEnd != "" {
		ended = true
	}
	// WHAT THE RUN SAYS IT SPENT IS NEVER A REFUND — see [Result.usageFloored].
	var floored bool
	if result, floored = result.usageFloored(); floored {
		log.WarnContext(ctx, "sandbox_usage_negative",
			"turn_id", run.TurnID, "sandbox_id", run.SandboxID,
			"detail", "the executor reported a negative token count or cost; it "+
				"is read as nothing spent rather than subtracted from the seat's budget")
	}
	result = boundBox(result)
	// THE JOB'S SESSION, TAKEN HERE on the box's host before the box is paused or
	// reclaimed (a paused box cannot be read), whichever node asked, spooled
	// sealed on this node and KEPT off the read from the moment its take is
	// spooled (session.go), never behind what the read does after. Recording the
	// harness end, pausing and recording the pause each wait on a store or box
	// backend, and a read of a box this node hosts has no deadline: a keep started
	// after the read waited on all of it, no capture budget could bound it, and
	// a turn's end could read a capture still to be kept as abandoned. The keep
	// reads the spool and the claim's copy of the run only. A stop's read is the
	// last anything makes of the box.
	at := readCapture(read, result, over)
	// WHETHER IT RAN OUT OF CONTEXT is judged on the run as its collection
	// records it ([asCollected]): a run its cap ended drops its parting
	// question, so judged on the claimed row its session would be kept and then
	// retired by the same collection.
	at.exhausted = ranOutOfContext(asCollected(run, result, ended))
	at.refused = requestRefused(asCollected(run, result, ended))
	// THE JOB'S MAIN THREAD, read once on the box's host before the take
	// (mainthread.go): the harness's count of the session and its provider (an
	// OpenCode count's reply), and its next-request estimate, kept on the result
	// and written with how the harness ended after the take; and the receipts of
	// its hooks' serves, settled BEFORE the take, a verdict the store does not
	// answer handing the read back box-answered ([boxAnsweredError]).
	asked := c.asksSummary(runner)
	reading := c.readMainThread(ctx, run, box, runner, read, at, result, asked)
	result.ResumedCount, result.CountProvider, result.NextRequest = reading.Count, reading.Provider, reading.Next
	if err := c.settleReceipts(ctx, run, reading.Receipts, read); err != nil {
		return Result{}, false, &boxAnsweredError{err: err, end: at.harnessEnd()}
	}
	// ITS NEWEST SUMMARY, which the take keeps beside the session (summary.go).
	at.summary = c.foundSummary(ctx, run, runner, asked, reading.Summary)
	spooled, kept := c.takeCapture(ctx, run, box, runner, at)
	result.Kept = kept
	c.keepLater(ctx, spooled)
	// HOW THE JOB'S HARNESS ENDED, drawn by the line the take was: carried back
	// with the result, and recorded by a collection's read whether or not the
	// take kept a capture ([LaunchRecord.HarnessEnd], harnessend.go), after the
	// take so a slow store costs neither the take nor the pause their time. A
	// stop's and a loss's read record nothing: nothing carries the round on.
	result.HarnessEnd = at.harnessEnd()
	if read != BoxReadCollect {
		return result, ended, nil
	}
	// AND the copy's newest request size, the harness's count with its provider
	// and release, and its next-request estimate, in the same write.
	c.markHarnessEnd(ctx, run, EndRead{End: result.HarnessEnd, Request: result.LastRequest,
		Count: result.ResumedCount, CountProvider: result.CountProvider, CountRelease: result.HarnessVersion,
		Next: result.NextRequest})
	if err := box.Pause(ctx); err != nil {
		log.WarnContext(ctx, "sandbox_pause_failed", "turn_id", run.TurnID, "error", err.Error())
	} else if err := c.markBoxPaused(ctx, run); err != nil {
		// WARN, NOT FAIL: refusing a collected result over a timestamp would
		// throw the run away. A row that parks names a held box whatever this
		// write did, and [PendingRun.HeldSince] dates the wait from the park.
		log.WarnContext(ctx, "sandbox_pause_record_failed", "turn_id", run.TurnID, "error", err.Error(),
			"detail", "the pause instant was not recorded; a run that parks on a question is "+
				"expired from its park instead, and one that resumes settles its own box")
	}
	return result, ended, nil
}
