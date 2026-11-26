package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// What bounds a collected job's turn: its row holds the seat until the resume
// runs. Two bounds end the run with the seat freed: the turn's deadline
// ([PendingRun.Deadline]), past which a resume gets one attempt
// ([Coordinator.completeAtDeadline]), and the resume give-up ([ResumeGiveUp],
// [MinResumeFailures]) for failures of the run's own. A wait on a condition is
// not bounded here; nor is a condition only an operator clears
// ([CoordinatorOptions.NodesFailure]), raised as `executor_resume_blocked`. See
// the package doc, "What bounds a collected job's turn".

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
// failed ([Release.ResumeFailedAt]), on the clock of node on
// ([Release.FailedOn]); and, in this node's memory rather than on
// the record, the kind of a condition only an operator clears that the resume
// failed on, which counts the run in [Coordinator.ResumesBlocked] once the
// hand-back lands ([blockedResume]). The zero value counts nothing.
type failure struct {
	collectFailed time.Time
	resumeFailed  time.Time
	on            string
	blocked       string
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

// completeAtDeadline ends the run of a completion its node would not resume now
// because its turn's deadline has passed: claimed, collected, charged and
// recorded as every completion is ([Coordinator.collectClaimed]), then settled
// ([Coordinator.endAtDeadline]). The refusing condition is not waited for,
// since the turn has no time left; asked by an announcement, or by a held
// completion's backstop ([Coordinator.recheckHeld]). No turn starts, so no
// admission is asked, but its claim meets the seat's recovery gate
// (recoverygate.go), and a read of its box made here holds one of the node's
// collections ([Coordinator.boxResult]). See the package doc, "What bounds a
// collected job's turn".
func (c *Coordinator) completeAtDeadline(ctx context.Context, ev types.ExecutorFinished, fence Fence,
	why string,
) error {
	// A CLAIM OF THE JOB LIKE ANY OTHER, held to what this node still owes
	// the job's last claim and to the seat's recovery gate
	// ([Coordinator.claimCompletion]): asking no admission is not asking no
	// gate, and a pass of the seat under way here refuses it, its caller's
	// road carrying it past the pass (recoverygate.go).
	run, won, done, err := c.claimCompletion(ctx, ev, fence)
	if done != nil {
		defer done()
	}
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
// [MinResumeFailures] times across [ResumeGiveUp], counting this failure, and
// reports whether it did; the count is on the job's record
// ([LaunchRecord.ResumeFailures]) across nodes and restarts, timed on the row
// (rowclock.go). Only the run's own failures count ([Coordinator.runsOwnFailure]):
// not the node's or its infrastructure's, and not [ErrResumeElsewhere] while a
// live node may resume it. See the package doc, "What bounds a collected job's
// turn".
func (c *Coordinator) resumeGivenUp(ctx context.Context, run PendingRun, now time.Time, cause error) bool {
	attempts := run.Launch.ResumeFailures + 1
	since := run.Launch.ResumeFailingSince
	if since.IsZero() {
		since = now
	}
	if !c.resumeGiveUpDue(run, now) {
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

// resumeGiveUpDue reports whether run's resume, failing once more at now, has
// failed [MinResumeFailures] times across [ResumeGiveUp] by this node's clock
// (rowclock.go).
func (c *Coordinator) resumeGiveUpDue(run PendingRun, now time.Time) bool {
	return run.Launch.ResumeFailures+1 >= MinResumeFailures &&
		run.Launch.resumeWait().lasted(c.node, now, ResumeGiveUp)
}

// runsOwnFailure reports whether err, the failure of run's resume, is the
// run's own, which the give-up counts ([Coordinator.resumeGivenUp]), rather
// than the node's or its infrastructure's ([CoordinatorOptions.NodesFailure]),
// or one another node or build can get past while a live node may
// ([ErrResumeElsewhere], [CoordinatorOptions.ResumableElsewhere]). A fleet
// that cannot be read counts nothing: the store that did not answer is the
// node's, and the next failure asks again.
func (c *Coordinator) runsOwnFailure(ctx context.Context, run PendingRun, err error) bool {
	if c.nodesFailure != nil && c.nodesFailure(ctx, err) {
		return false
	}
	if !errors.Is(err, ErrResumeElsewhere) || c.resumableElsewhere == nil {
		return true
	}
	elsewhere, readErr := c.resumableElsewhere(ctx, run)
	if readErr != nil {
		log.WarnContext(ctx, "sandbox_resume_elsewhere_unread",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "error", readErr.Error(),
			"detail", "whether a live node can resume the run could not be read, so this "+
				"failure is not counted toward the resume give-up; the next one asks again")
		return false
	}
	return !elsewhere
}

// blockedResume is a completion whose last resume here failed on a condition
// only an operator clears ([CoordinatorOptions.OperatorsCondition]): its seat,
// the condition's kind, and when the first such failure in a row was met.
// handBack is the release giving the claim back. Counted once that hand-back
// lands, recorded and decided in the release's steps
// ([Coordinator.handingBackLocked], [Coordinator.blockedAnsweredLocked]), and
// only while this node holds the seat under fence ([fenceOf]). See the package
// doc, "Resumes blocked on an operator".
type blockedResume struct {
	agent    string
	kind     string
	since    time.Time
	fence    Fence
	handBack *claimRelease
	counted  bool
}

// ResumesBlocked is what this node says of the runs of seats it holds that
// wait for an operator ([Coordinator.ResumesBlocked]): how many, and the one
// waiting longest.
type ResumesBlocked struct {
	// Runs is how many runs of seats this node holds finished their job and
	// failed their last resume here on a condition only an operator clears.
	Runs int
	// Seat is the seat of the run waiting longest, Kind the condition its
	// last resume failed on, and Since when the first such failure in a row
	// was met, on this coordinator's clock: an instant rather than how long,
	// which a reader measures on its own. Zero where Runs is.
	Seat  string
	Kind  string
	Since time.Time
}

// ResumesBlocked reports the runs of seats this node holds whose job finished
// and whose last resume here failed on a condition only an operator clears. The
// give-up never ends such a run, so the wait is raised as the
// executor_resume_blocked alarm from the node's status record by every data
// node (internal/engine's executorReading). From this node's memory, exact for
// its seats: counted once the hand-back lands, under the claim's lease
// ([blockedResume]); a restart forgets it until the next failure. See the
// package doc, "Resumes blocked on an operator".
func (c *Coordinator) ResumesBlocked() ResumesBlocked {
	c.mu.Lock()
	counted := make(map[string]blockedResume, len(c.blocked))
	for turnID, b := range c.blocked {
		if b.counted {
			counted[turnID] = b
		}
	}
	c.mu.Unlock()
	var out ResumesBlocked
	var oldest blockedResume
	var gone map[string]Fence
	for turnID, b := range counted {
		// ASKED OUTSIDE mu, as every write this coordinator makes on a
		// seat's behalf asks it: the seat host answers under a lock of its
		// own.
		if fence, held := c.leaseOf(b.agent); !held || (fence.Fenced() && fence != b.fence) {
			if gone == nil {
				gone = map[string]Fence{}
			}
			gone[turnID] = b.fence
			continue
		}
		out.Runs++
		if out.Runs == 1 || b.since.Before(oldest.since) ||
			(b.since.Equal(oldest.since) && b.agent < oldest.agent) {
			oldest = b
		}
	}
	if len(gone) > 0 {
		// LET GO OF, unless an attempt under another lease recorded the run
		// since: that holding says its own.
		c.mu.Lock()
		for turnID, fence := range gone {
			if b, ok := c.blocked[turnID]; ok && b.fence == fence {
				delete(c.blocked, turnID)
			}
		}
		c.mu.Unlock()
	}
	if out.Runs > 0 {
		out.Seat, out.Kind, out.Since = oldest.agent, oldest.kind, oldest.since
	}
	return out
}

// blockedBy is the kind of run's resume failure err where only an operator
// clears it and the run is a completion's, whose give-up never ends it;
// "" otherwise. An answer's resume is bounded by its own series
// ([MaxAnswerAttempts]) and is not counted.
func (c *Coordinator) blockedBy(run PendingRun, err error) string {
	if c.operatorsCondition == nil || claimedAnswer(run) {
		return ""
	}
	if kind, ok := c.operatorsCondition(err); ok {
		return kind
	}
	return ""
}

// unblock takes turnID's run out of [Coordinator.ResumesBlocked]: its latest
// resume did not fail on a condition only an operator clears.
func (c *Coordinator) unblock(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.blocked, turnID)
}

// blockedHandBackLocked records cr, the hand-back of a claim whose resume
// failed on kind, a condition only an operator clears, as the release whose
// landing counts its run in [Coordinator.ResumesBlocked] ([blockedResume]). A
// run an earlier attempt counted keeps the instant of its first such failure in
// a row, and stays counted while cr is out; but not one an earlier holding of
// the seat counted, under another lease, which a frame of that holding left
// behind it: the seat's next holder, this node again included, says it from
// its own attempts. Under mu.
func (c *Coordinator) blockedHandBackLocked(cr *claimRelease, kind string) {
	run := cr.run
	fence := fenceOf(run)
	b, ok := c.blocked[run.TurnID]
	if !ok || b.fence != fence {
		b = blockedResume{agent: run.AgentHandle, fence: fence, since: c.now()}
	}
	b.kind, b.handBack = kind, cr
	if c.blocked == nil {
		c.blocked = map[string]blockedResume{}
	}
	c.blocked[run.TurnID] = b
}

// blockedAnsweredLocked acts on cr's answer, landed or not, for the run it
// would count in [Coordinator.ResumesBlocked], where cr is still the hand-back
// that counts it: a release that landed counts it, and one that did not takes it
// out. Under mu, in the step that takes the release's answer.
func (c *Coordinator) blockedAnsweredLocked(cr *claimRelease, landed bool) {
	turnID := cr.run.TurnID
	b, ok := c.blocked[turnID]
	if !ok || b.handBack != cr {
		return
	}
	if !landed {
		delete(c.blocked, turnID)
		return
	}
	b.handBack, b.counted = nil, true
	c.blocked[turnID] = b
}
