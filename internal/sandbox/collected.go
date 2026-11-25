package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/redact"
)

// markBoxPaused records on run's row that its box is paused
// ([PendingStore.MarkBoxPaused]) in the room a read leaves it
// ([Coordinator.pauseRoom]): a teardown's grace plus the row's crossing
// ([Coordinator.rowContext]). HELD TO ITS ROOM, NEVER TO THE READ'S CONTEXT
// ALONE: a read of a box this node hosts runs under its admission, which has no
// deadline, and an unanswering store would hold the read and the turn's resume
// indefinitely. A record so cut is the missing stamp the read already warns of.
func (c *Coordinator) markBoxPaused(ctx context.Context, run PendingRun) error {
	row, cancel := c.rowContext(ctx, discardGrace)
	defer cancel()
	return c.pending.MarkBoxPaused(row, run.TurnID, c.now())
}

// markEndedAtDeadline records on run's job's record that its collection saw it
// working past its turn's cap and is ending it ([PendingStore.MarkEndedAtDeadline]):
// every later collection reads it as the cap's verdict ([Coordinator.readBox]).
//
// UNDER A CONTEXT OF ITS OWN ([Coordinator.detachedRecord]), like the kill that
// may follow: the read can be cut short as it ends the job, and this record is
// what tells a later collection the job was one the cap stopped. A record that
// does not land is said and costs only that later sight: the job's own record
// of the request ([Result.JobEnd]) says so where its script left one.
func (c *Coordinator) markEndedAtDeadline(ctx context.Context, run PendingRun) {
	markCtx, cancel := c.detachedRecord(ctx)
	defer cancel()
	if _, err := c.pending.MarkEndedAtDeadline(markCtx, run.TurnID, run.LaunchID); err != nil {
		log.WarnContext(ctx, "sandbox_deadline_end_unrecorded", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "error", err.Error(),
			"detail", "the run's record does not say that its turn's cap ended the job this collection "+
				"saw working; this collection says so, and one made again after it reads it off the "+
				"job's own record of the request, where its script left one")
	}
}

// endedAtDeadline is what a run its turn's cap ended reports: what it said up to
// then, marked as cut off. Never a success, since it did not get to say it was
// done, and parked on no question ([withoutQuestion]). BUT THE QUESTION IT
// ASKED IS SAID: `asked` (redacted and bounded where recorded) is named in the
// note, so the judging turn and the seat's next turn know a person's answer was
// what it waited for; dropped with the park it would leave no trace.
func endedAtDeadline(result Result, deadline time.Time, asked string) Result {
	why := fmt.Sprintf("the run was ended at its turn's wall-clock cap, %s, before it "+
		"finished; what it had done by then is collected", deadline.UTC().Format(time.RFC3339))
	if asked != "" {
		why += ". It had asked: " + asked
	}
	result.Success = false
	if result.Error != "" {
		why += ". It had reported: " + result.Error
	}
	result.Error = why
	return result
}

// readPushes sets what a collected run pushed to Gitlet on its result, from the
// push record ([RunPushes]), for a run given a repository to push to (a read-only
// run's grants say it pushed none). THE FIELDS ARE THE COORDINATOR'S, cleared
// first (a runner's would be its own account). AN UNREADABLE RECORD DOES NOT FAIL
// THE COLLECTION (retrying would hold a finished run's seat and box while Gitlet
// is down): it is SAID ([Result.PushedUnread]), not "none"; except a read this node
// cancelled, the collection interrupted ([Coordinator.collectFailed]). AND THE CHANGES
// ([RunPushes.Changes]), read at the record's position ([PushRecord.At]) for one
// barrier: refs are named by their live change, and changes the run asked to open
// are opened only for a run that FINISHED (succeeded, asked nothing); a question's
// run is carried on by another job (ADR-0068), a failed one the seat decides
// (code_open_change). A failed read says each ask was not opened, and why.
func (c *Coordinator) readPushes(ctx context.Context, run PendingRun, result Result) (Result, error) {
	result.Pushed, result.PushedElided, result.PushedUnread, result.Opened = nil, 0, "", nil
	if len(Pushable(run.Repos)) == 0 {
		return result, nil
	}
	record, err := c.pushes.Pushed(ctx, run)
	switch {
	case err != nil && errors.Is(ctx.Err(), context.Canceled):
		return Result{}, fmt.Errorf("sandbox: reading what run %s pushed: %w", run.TurnID, err)
	case err != nil:
		// Redacted and bounded where every collected result is ([Coordinator.fitResult]).
		result.PushedUnread = err.Error()
		result.Opened = asked(run, unopenedUnread(err.Error()))
		log.WarnContext(ctx, "sandbox_pushes_unread", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"error", err.Error(),
			"detail", "what the run pushed to Gitlet could not be read; the run resumes "+
				"saying its pushes are unknown, never that it pushed nothing")
		return result, nil
	}
	result.Pushed, result.PushedElided = record.Refs, record.More
	finished := result.Success && !run.asked()
	if len(record.Refs) == 0 && (!finished || len(asked(run, "")) == 0) {
		// NOTHING TO NAME AND NOTHING TO OPEN: no read.
		return withUnopened(run, result, finished), nil
	}
	named, opened, err := c.pushes.Changes(ctx, run, record, finished)
	switch {
	case err != nil && errors.Is(ctx.Err(), context.Canceled):
		return Result{}, fmt.Errorf("sandbox: reading the changes run %s delivered to: %w", run.TurnID, err)
	case err != nil:
		log.WarnContext(ctx, "sandbox_changes_unread", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"error", err.Error(),
			"detail", "the changes the run's branches head could not be read; its pushed refs name none, "+
				"and no change it asked to open was opened")
		if finished {
			result.Opened = asked(run, unopenedNoChanges(err.Error()))
			return result, nil
		}
		return withUnopened(run, result, finished), nil
	}
	result.Pushed = named
	if finished {
		result.Opened = opened
		return result, nil
	}
	return withUnopened(run, result, finished), nil
}

// withUnopened is result with every change run asked to open said unopened (why,
// for a run that did not finish), or as is for one that finished and opened none.
func withUnopened(run PendingRun, result Result, finished bool) Result {
	switch {
	case finished:
	case run.asked():
		result.Opened = asked(run, unopenedAsked)
	default:
		result.Opened = asked(run, unopenedFailed)
	}
	return result
}

// charge post-accounts a collected run's tokens, ONCE per launch, and reports
// whether the run's spend is on the counter.
//
// The charge sits in the retried tail (a failed resume hands the claim back and the
// retry collects the same job again), so the run's row records it and a retry finding
// it charges nothing (memory would be lost to a retry elsewhere or after a restart).
// THE RECORD RIDES ON THE RELEASE, the only write letting a retry reach this charge
// ([PendingRun.Charged]). A CHARGE THE COUNTER NEVER ANSWERED IS NOT
// RECORDED (re-offering can only trip a cap early). One that reached the COMPANY and
// failed on the seat is recorded as that ([PendingRun.CompanyCharged]), the retry
// finishing the seat's alone. One OVER A CAP is recorded like any other
// ([Accountant]): "unrecorded" would charge an over-cap run once per retry.
func (c *Coordinator) charge(ctx context.Context, run PendingRun, result Result) (charged, companyCharged bool) {
	tokens := result.InputTokens + result.OutputTokens
	if run.Charged {
		log.InfoContext(ctx, "sandbox_charge_already_recorded",
			"agent_id", run.AgentID, "turn_id", run.TurnID, "tokens", tokens)
		return true, run.CompanyCharged
	}
	if c.account == nil || tokens == 0 {
		return false, run.CompanyCharged
	}
	over, err := c.account.Charge(ctx, run.AgentID, run.AgentHandle, tokens, run.CompanyCharged)
	var partial *coord.SeatUncountedError
	switch {
	case errors.As(err, &partial):
		log.WarnContext(ctx, "sandbox_accounting_partial", "turn_id", run.TurnID,
			"agent_id", run.AgentID, "tokens", tokens, "error", err.Error(),
			"detail", "the company's counter holds this run and the seat's does not; a "+
				"retry of the completion records the seat's share alone")
		return false, true
	case err != nil:
		log.WarnContext(ctx, "sandbox_accounting_failed", "turn_id", run.TurnID, "error", err.Error())
		return false, run.CompanyCharged
	}
	if over {
		log.WarnContext(ctx, "sandbox_spend_over_budget",
			"agent_id", run.AgentID, "turn_id", run.TurnID, "tokens", tokens)
	}
	// RECORDED EITHER WAY, because the counters moved either way.
	return true, true
}

// publishCollected publishes the collected launch's own record
// (`executor_collected`), ONCE per launch, and returns the job's record as it now
// stands. THE LAUNCH IS THE EXECUTOR'S SPEND: the executor publishes nothing when
// it suspends and its resumed phase has no tokens of its own, so here every token
// reaches the spend rollup and daily usage and the activity transcript is kept, for
// every collected launch (finished or asking). A RECORD OF ITS OWN, NOT A PHASE
// ([types.ExecutorCollected]). ONCE, like the charge: it rides the release
// ([LaunchRecord.Published]), a refused publish is NOT recorded so a retry re-offers
// it, and the one remaining duplicate (reported failed, landed) errs like the
// charge. Telemetry never fails the tail: a failure is logged.
func (c *Coordinator) publishCollected(ctx context.Context, run PendingRun, result Result) LaunchRecord {
	facts := run.Launch
	if facts.Published {
		log.InfoContext(ctx, "sandbox_collected_already_published",
			"turn_id", run.TurnID, "launch_id", run.LaunchID)
		return run.Launch
	}
	rec := collectedRecord(run, facts, result, c.pinnedWindow(run.Harness, facts.Model), c.now())
	ev := events.New(rec, events.TraceContext{TraceID: run.TraceID, ParentSpanID: run.SpanID})
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "sandbox_collected_publish_failed",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "error", err.Error())
		return run.Launch
	}
	// MEASURED WITH THE RECORD, under its guard: once per launch.
	c.meterCarry(facts, result, rec.SessionNotContinued)
	facts.Published = true
	return facts
}

// collectedRecord is a collected launch's own record.
//
// Everything on it is the LAUNCH's (tokens, cache share, cost, report,
// deliveries, transcript, why it did not finish); nothing is the executor's,
// which is on the resumed phase record, so the two never sum past the turn's
// spend. Its clock is the store's instant for the launch and this node's for the
// collection, so DURATION includes the waiter's poll interval.
func collectedRecord(run PendingRun, facts LaunchRecord, result Result, pinned int,
	collected time.Time) types.ExecutorCollected {
	// THE SESSION AND RELEASE THE BOX NAMED are held to the shapes a harness gives
	// them, whatever the runner or host made of them: each is the box's word, and
	// one of any other shape is none, never cut (a shortened id names another).
	session, release := result.SessionID, result.HarnessVersion
	if !ValidSessionID(session) {
		session = ""
	}
	if !ValidRelease(release) {
		release = ""
	}
	rec := types.ExecutorCollected{
		Agent: run.AgentID, RoleName: run.Role,
		TurnID: run.TurnID, WorkKey: run.WorkKey,
		// The executor round that launched it (a round can launch more than once).
		Iteration: facts.Iteration, LaunchID: run.LaunchID,
		// The entry and model the job launched on: the chain member that took the
		// run, which a fallback makes other than the head.
		Model: facts.Model, ProviderKey: facts.ProviderKey,
		// Redacted again at the publish, the boundary the record leaves by: a
		// runner that forgot would put a box's credentials in the event store.
		Response:           redact.Secrets(result.Text),
		ActivityTranscript: redact.Secrets(result.Transcript),
		// What the bound left out of the transcript's middle, so a screen says it
		// is not the whole log ([boundTranscript]).
		ActivityTranscriptElidedLines: result.TranscriptElidedLines,
		ActivityTranscriptElidedBytes: result.TranscriptElidedBytes,
		InputTokens:                   result.InputTokens,
		OutputTokens:                  result.OutputTokens,
		TotalTokens:                   result.InputTokens + result.OutputTokens,
		CacheReadTokens:               result.CacheReadTokens,
		CacheWriteTokens:              result.CacheWriteTokens,
		// The launch's model turns as its harness reported them: the calls every
		// spend figure counts this record as (tokens.LaunchCalls).
		HarnessTurns:  result.Turns,
		WorkItem:      run.WorkItem,
		Harness:       run.Harness,
		SandboxID:     run.SandboxID,
		CostUSD:       result.CostUSD,
		DeliveredRefs: result.DeliveredRefs,
		// How many more it reported than listed ([Result.DeliveredRefsElided]).
		DeliveredRefsElided: result.DeliveredRefsElided,
		// AND ITS PUSHES, on the launch's own record: an answer's resume collects
		// nothing, so a parked job's pushes are here or nowhere ([Result.Pushed]).
		PushedRefs:       PushedRecords(result.Pushed),
		PushedRefsElided: result.PushedElided,
		PushedRefsUnread: result.PushedUnread,
		ConversationKey:  run.ConversationKey,
		// THE SESSION THE LAUNCH RAN, as its script recorded it (not the one it was
		// asked to continue, when it fell back to a new one), the harness's
		// release, and what the read took of it to keep or why none, so a later
		// launch knows whether it may continue it. The upload and record are made
		// off the read, from the moment its take spooled the session.
		HarnessSession: session,
		HarnessVersion: release,
		// AND WHICH BRIEF the launch handed it, whether the run continued the
		// session it was asked to and, where not, why its script started its own
		// with the fresh brief (the started phase record carries the fresh brief,
		// which a continuing run never read). THE REASON IS HELD TO ITS BOUND
		// HERE, redacted then cut ([BoundFellBack]): it is a line of a box file the
		// run's own agent can write as long as the engine reads whole.
		Brief:                   string(facts.Brief),
		HarnessSessionContinued: result.Continued,
		SessionNotContinued:     finalNotContinued(run, facts, result),
	}
	// The window the run was pinned to, as its runner pins the model its launch
	// recorded ([windowPinner]), none for a runner that pins none.
	rec.ContextWindowTokens = pinned
	if kept := result.Kept; kept != nil {
		rec.SessionTakenBytes = kept.Bytes
		// AND WHY IT WAS NOT KEPT, at the executor log's bound for the reason: it
		// quotes the take's refusal, which can quote the box's archive.
		rec.SessionNotKept = boundReason(kept.NotKept, executorkeep.UnkeptReasonMaxBytes)
		rec.SessionNotKeptLoss = kept.Loss
	}
	rec.StartedAt = facts.StartedAt.UTC()
	if took := collected.Sub(facts.StartedAt); took > 0 {
		rec.DurationMS = int(took / time.Millisecond)
	}
	switch {
	case run.Stop != nil:
		// ENDED BY A PERSON, NOT FAILED ([Coordinator.StopSeat]): marked failed
		// it would list with the runs that broke. Who stopped it is the note.
		rec.Notes = "stopped by " + run.Stop.By + " before it finished"
	case run.Lost != nil && !run.asked():
		// LOST, NOT FAILED: its host cut it off mid-step ([JobLoss]), saying
		// nothing about the run, and its round is carried on.
		rec.Notes = lostNote(*run.Lost)
	case run.asked():
		// STOPPED, NOT FAILED: parked, to be resumed by its answer. The question
		// is the note, redacted where the ask was recorded.
		rec.Notes = "stopped to ask: " + run.Question
	case !result.Success:
		rec.Failed = true
		rec.Error = redact.Secrets(result.Error)
		if rec.Error == "" {
			rec.Error = "the executor's launch did not succeed and gave no reason"
		}
		rec.ErrorKind = types.ExecutorErrorLaunchFailed
		switch {
		case result.TurnCapExceeded:
			// FIRST: whatever else ended the run, nothing held it to its cap.
			rec.ErrorKind = types.ExecutorErrorTurnCapExceeded
		case ranOutOfContext(run, result):
			rec.ErrorKind = types.ExecutorErrorContextExhausted
		case requestRefused(run, result):
			rec.ErrorKind = types.ExecutorErrorRequestRefused
		case result.OutOfMemory && Placement(run.Placement).OwnsMemory():
			// A direct box's kills are the engine host's domain's, recorded
			// below and never the run's ending.
			rec.ErrorKind = types.ExecutorErrorOutOfMemory
		case result.RoundCapped:
			rec.ErrorKind = types.ExecutorErrorMaxTurns
		}
	}
	// THE KERNEL'S KILLS FOR MEMORY, on any ending, so a run that survived a
	// killed command is evidence of its box's size too.
	rec.MemoryKills, rec.MemoryBytes = result.MemoryKills, result.MemoryBytes
	return rec
}
