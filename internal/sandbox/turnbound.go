package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// # What bounds a collected job's turn
//
// A job that finished is collected and its turn resumed to review it. Until
// that resume runs, the run's row holds the seat: its other mail waits behind
// it. A resume that fails is handed back, and one that meets a condition that
// ends by waiting (a model's usage limit, a spent budget, a paused seat) is
// held until that condition clears. Either way the waiter announces the job
// again on its next poll, so the job comes round until its resume runs.
//
// Two bounds keep that from going on for ever, and both END THE RUN, announced
// on the board under a reason of its own, with the seat freed:
//
//   - THE TURN'S DEADLINE ([PendingRun.Deadline]), a scheduled fire's
//     wall-clock cap. The poll and the collection after it end a job still
//     working at it, and the turn loop refuses a round that starts past it,
//     but neither covered the time between: a finished job whose resume kept
//     failing or waiting was handed back and announced on every poll. On a
//     live company a health check with a ten-minute cap finished its job in
//     six minutes, met a network outage and then the model's weekly limit in
//     its review, and held its seat for twenty-seven hours, its next three
//     fires queued behind it. Past the
//     deadline a completion's resume gets the attempt it can have at once,
//     and anything that would make it wait or retry ends it instead
//     ([Coordinator.completeAtDeadline], [Coordinator.endAtDeadline]).
//   - THE RESUME GIVE-UP ([ResumeGiveUp], [MinResumeFailures]), for every
//     turn, capped or not. A resume that FAILS (not one that waits) is
//     counted onto the job's record and, once it has failed that often for
//     that long, the run is settled ([Coordinator.resumeGivenUp]). Uncounted,
//     each poll's announcement was a fresh delivery with a fresh budget, so a
//     resume that always failed was retried every poll for as long as the
//     engine ran.
//
// A WAIT ON A CONDITION is not bounded here for a turn with no deadline: the
// condition names when it lifts, or the event that lifts it, and the seat
// could not run other work through it meanwhile. An answer's resume is bounded
// by its own series ([MaxAnswerAttempts]) and the run's pause TTL.

// ResumeGiveUp is how long the resume of a finished job's turn may keep failing
// before the run is settled as [types.ExecutorFailureResumeFailing].
//
// HALF AN HOUR: long enough that a network outage or a provider's incident of
// the kind that clears in minutes does not cost a finished job its review, and
// short enough that a seat is not held for an afternoon by a turn that cannot
// be re-entered. The job's work is not lost with it: what it pushed is on its
// branch, and the ending says so.
const ResumeGiveUp = 30 * time.Minute

// MinResumeFailures is how many failed resumes must have happened before
// [ResumeGiveUp] can end a run, for the reason [MinConnectFailures] gives: a
// node asleep for an hour between two attempts must not give a job up on its
// second.
const MinResumeFailures = 3

// failure is what a hand-back counts onto the job's record: the instant a
// collection could not read its box ([Release.CollectFailedAt]) or a resume
// failed ([Release.ResumeFailedAt]). The zero value counts nothing.
type failure struct {
	collectFailed time.Time
	resumeFailed  time.Time
}

// pastDeadline reports whether the job ev announces belongs to a turn whose
// wall-clock cap has passed, read from the run's row. A row that cannot be read,
// or that holds another job, is not past anything: the ordinary path answers
// it, and the next announcement asks again.
func (c *Coordinator) pastDeadline(ctx context.Context, ev types.ExecutorFinished) bool {
	run, found, err := c.pending.Get(ctx, ev.TurnID)
	if err != nil {
		log.WarnContext(ctx, "sandbox_deadline_unread", "turn_id", ev.TurnID, "error", err.Error())
		return false
	}
	return found && run.LaunchID == ev.LaunchID && run.PastDeadline(c.now())
}

// completeAtDeadline ends the run of a completion its node would not resume now,
// because its turn's deadline has passed: claimed, collected, charged and
// recorded as every completion is ([Coordinator.collectClaimed]), and then
// settled instead of resumed ([Coordinator.endAtDeadline]).
//
// THE CONDITION THAT REFUSED IT IS NOT WAITED FOR. Before the deadline a
// completion the gate refuses, or one this node holds off after its resume met
// a limit, waits for the condition to clear, because the turn still has time
// to review its job. After it, the wait is time the turn does not have, spent
// holding the seat.
//
// No turn starts here, which is why no admission is asked: the claim, the
// collection and the ending are the run's, and none of them is a turn.
func (c *Coordinator) completeAtDeadline(ctx context.Context, ev types.ExecutorFinished, fence Fence,
	why string,
) error {
	// A CLAIM OF THE JOB LIKE ANY OTHER, held to what this node still owes
	// the job's last claim ([Coordinator.claimCompletion]).
	run, won, err := c.claimCompletion(ctx, ev, fence)
	if err != nil {
		return fmt.Errorf("sandbox: claiming %s to end it at its deadline: %w", ev.TurnID, err)
	}
	if !won {
		log.InfoContext(ctx, "sandbox_completion_already_claimed",
			"turn_id", ev.TurnID, "launch_id", ev.LaunchID)
		return nil
	}
	c.forgetRefused(ev.TurnID)
	var result Result
	if run.Lost == nil {
		// A JOB THIS COLLECTION FINDS LOST with the host its box is on
		// ([Coordinator.loseWithHost]) is ended here as a job the row held
		// lost is: unread, at its deadline.
		var collected bool
		if run, result, collected, err = c.collectClaimed(ctx, run); !collected && run.Lost == nil {
			return err
		}
	}
	c.endAtDeadline(ctx, run, why, result.DeliveredRefs, result.DeliveredRefsElided)
	return nil
}

// endAtDeadline settles a claimed run whose turn's deadline passed while its
// resume could not run, and raises the alarm: an Error line, and the run's
// ending announced on the board under [types.ExecutorFailureDeadline].
//
// WHAT THE JOB DID IS KEPT WITH THE TURN, never thrown away with the wait. Its
// collection already published the launch's own record (`executor_collected`:
// the harness's account, its transcript and its spend), which the turn's page
// in Live shows; the ending names what the job delivered (delivered, and how
// many more it reported beyond them), so the board says where a person or the
// seat's next turn picks the work up. The box goes as every finished job's box
// goes at the end of its turn.
func (c *Coordinator) endAtDeadline(ctx context.Context, run PendingRun, why string,
	delivered []string, elided int,
) {
	log.ErrorContext(ctx, "sandbox_run_ended_at_deadline",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "launch_id", run.LaunchID,
		"deadline", run.Deadline.UTC().Format(time.RFC3339),
		"past_s", c.now().Sub(run.Deadline).Seconds(), "cause", why,
		"delivered", delivered, "delivered_elided", elided,
		"detail", "the turn's wall-clock cap passed while the turn that reviews its executor's "+
			"job could not run; the run is ended and its seat freed rather than held until "+
			"the cause clears")
	c.settleFailed(ctx, run, types.ExecutorFailureDeadline, deadlineDetail(run.Deadline, delivered, elided))
}

// deadlineDetail is the sentence a run ended at its turn's deadline reaches the
// board with: when the cap passed, that the job's work is kept, and what it
// delivered.
func deadlineDetail(deadline time.Time, delivered []string, elided int) string {
	detail := fmt.Sprintf("the turn passed its wall-clock cap at %s while the review of its "+
		"executor's finished work could not run, so the turn was ended rather than left waiting; "+
		"the executor's own account of the work is on the turn", deadline.UTC().Format(time.RFC3339))
	if len(delivered) == 0 {
		return detail + ", and what it pushed, if anything, is on its branch"
	}
	detail += ", and it delivered " + strings.Join(delivered, ", ")
	if elided > 0 {
		detail += fmt.Sprintf(" and %d more", elided)
	}
	return detail
}

// resumeGivenUp settles a claimed run whose turn's resume has now failed
// [MinResumeFailures] times across [ResumeGiveUp], counting the failure being
// handled, and reports whether it did. The run's other failures are on its
// job's record ([LaunchRecord.ResumeFailures]), so the count holds across
// nodes and restarts.
func (c *Coordinator) resumeGivenUp(ctx context.Context, run PendingRun, now time.Time, cause error) bool {
	attempts := run.Launch.ResumeFailures + 1
	since := run.Launch.ResumeFailingSince
	if since.IsZero() {
		since = now
	}
	if attempts < MinResumeFailures || now.Sub(since) < ResumeGiveUp {
		return false
	}
	log.ErrorContext(ctx, "sandbox_resume_given_up",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "launch_id", run.LaunchID,
		"attempts", attempts, "failing_for_s", now.Sub(since).Seconds(),
		"detail", "the executor's job finished but its turn could not be resumed for the resume "+
			"give-up window; the run is ended and its seat freed")
	note := &failureNote{reason: types.ExecutorFailureResumeFailing, detail: "the executor's job finished, but the " +
		"turn it belongs to failed every attempt to continue from it for half an hour, so the turn was ended; " +
		"the work it pushed, if any, is on its branch"}
	if c.failureKind != nil {
		note.errorKind = c.failureKind(cause)
	}
	c.settleNoted(ctx, run, note)
	return true
}
