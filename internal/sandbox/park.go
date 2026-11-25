package sandbox

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// park announces the question, records it, and settles the box per the pause policy.
// EVERY STEP THAT CAN FAIL RUNS UNDER THE CLAIM, a failure handing it back: once
// the row says awaiting no redelivery reaches the question again, so announce first,
// record second, and either failing gives the claim back for the retry or, where it
// cannot, ENDS the run (a row nothing picks up is a turn destroyed in silence,
// [Coordinator.unclaim]). THE ONE STOP THAT IS NOT AN ENDING, so reported here as
// well as in [Coordinator.endRecord]; a park whose write failed reports nothing.
// The seat is freed only once the question is on the row (a person can take days;
// earlier, a reply would run as a new turn). This is the wait PauseTTL exists for:
// the box stays paused, TTL ticking, unless zero, when it is torn down and the run
// parks straight into reseed (the answer resumes either way).
func (c *Coordinator) park(ctx context.Context, run PendingRun, result Result) error {
	// THE ANCHOR, taken BEFORE the announcement: nobody can answer before this
	// instant, so an earlier reply is not its answer ([PendingRun.AskedAt]).
	asked := c.now()
	announcement := types.ExecutorAsked{
		Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		TurnID: run.TurnID, WorkKey: run.WorkKey, SandboxID: run.SandboxID,
		Question: run.Question, QuestionContext: run.QuestionContext, Decision: run.Decision,
		Audience: run.Audience,
		// THE IDENTITY, like the launch announcement: this event is display and
		// the durable thread is what a feed reader means by the conversation.
		ConversationKey: run.ConversationKey,
		// The item recorded at launch, so the question shows against its work.
		WorkItem: run.WorkItem,
		// THE QUESTION'S OWN JOB and any call it holds, so a live surface offers
		// the answer the engine takes for it.
		LaunchID: run.LaunchID,
		HeldCall: run.HeldCall.Ref(),
	}
	ev := events.New(announcement, events.TraceContext{
		TraceID: run.TraceID, ParentSpanID: run.SpanID,
	})
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(announcement.EventType()), ev); err != nil {
		if unclaimErr := c.unclaim(ctx, run, parkUnannouncedDetail); unclaimErr != nil {
			//nolint:nilerr // Deliberate, as at the record below: the
			// claim did not go back, so the run has been ended and a
			// redelivered completion would find no record to claim.
			return nil
		}
		return fmt.Errorf("sandbox: announcing the question %s asked: %w", run.TurnID, err)
	}

	// BEING WRITTEN FROM HERE: a recount counts the run as the claim it still is
	// until the write's answer moves it (countwrite.go).
	write := c.writingCount(run, StatusResumed, StatusAwaiting)
	defer c.settleWrite(write)
	err := c.pending.MarkAwaiting(ctx, run.TurnID, Clarification{
		InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
		CacheReadTokens: result.CacheReadTokens, CacheWriteTokens: result.CacheWriteTokens,
		// What condensing this collection cost, paid by the same resume.
		Condensed: result.Condensed,
		// WHO IT IS PUT TO, resolved now and written with the question: the label
		// is the harness's words, and "what is waiting on me" was unanswerable
		// from the row ([PendingRun.AudienceHandles]).
		Answerers: c.audience.ResolveAudience(run, run.Audience),
		AskedAt:   asked,
	})
	// Freed and opened as one move: between two writes the seat would read idle
	// and the answer would run as an unrelated turn. Made in the step ending the
	// write's counting as the claim, so a recount cannot count the question
	// twice. A failed write records its claim's hand-back in this step too, so the
	// release sits behind it in the run's entry (countwrite.go), with what the
	// claim learned of the box host's store ([Coordinator.unclaimAt]). See the
	// package doc, "Parking a question".
	var (
		silent   time.Time
		answered bool
	)
	if err != nil {
		silent, answered = c.hostStoreSeen(ctx, run, failure{})
	}
	c.mu.Lock()
	var handBack *claimRelease
	movedBack := func() {}
	if err != nil {
		handBack, movedBack = c.handingBackLocked(run, parkUnrecordedDetail, failure{}, nil)
		handBack.release.HostStoreSilentSince, handBack.release.HostStoreAnswered = silent, answered
	}
	moved := c.wroteCountLocked(write, err == nil)
	c.mu.Unlock()
	movedBack()
	moved()
	if err != nil {
		// THE WAIT DID NOT LAND: the run is not parked and the row is still in
		// the claim. Handed back, the poll fires again, the paused box is
		// collected a second time and the park retried; where it cannot be, the
		// run is ended, since nothing picks up a row left claimed.
		log.ErrorContext(ctx, "sandbox_park_write_failed",
			"turn_id", run.TurnID, "error", err.Error(),
			"detail", "the question could not be recorded, so the run is not parked; its "+
				"claim is given back for the completion to be retried, or the run is "+
				"ended where it cannot be")
		if unclaimErr := c.unclaimOwing(ctx, handBack); unclaimErr != nil {
			//nolint:nilerr // Deliberate: the claim was not handed back,
			// so the run has been SETTLED and a redelivered completion
			// would retry against no record. Both failures are logged and
			// the loss is announced.
			return nil
		}
		// THE STORE'S SILENCE ([ErrStoreSilent]), like every step a collection
		// cannot go on without: the park follows a read the box's host answered,
		// so no clock runs on it and nothing renews its delivery, which is handed
		// back as this node's failure, never the message's (internal/engine's
		// controlOutcome), or an outage would send it to the poison.
		return fmt.Errorf("sandbox: parking %s: %w", run.TurnID, storeSilent(ctx, err))
	}

	// THE AGENT HAS STOPPED, durably: what the engine holds up while a turn works
	// comes down here ([CoordinatorOptions.Stopped]).
	c.reportStopped(ctx, run)
	if c.parked != nil {
		c.parked(ctx, run)
	}

	if run.PauseTTLSeconds == 0 {
		c.teardown(ctx, run)
		if err := c.pending.SetStatus(ctx, run.TurnID, StatusReseed, fenceOf(run)); err != nil {
			log.WarnContext(ctx, "sandbox_reseed_mark_failed", "turn_id", run.TurnID, "error", err.Error())
		}
	}
	log.InfoContext(ctx, "sandbox_run_awaiting_clarification",
		"turn_id", run.TurnID, "audience", run.Audience)
	return nil
}
