package sandbox

import (
	"context"
	"errors"
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
// again on its schedule (reannounce.go), so the job comes round until its
// resume runs.
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
//     ([Coordinator.completeAtDeadline], [Coordinator.endAtDeadline]). A
//     resume its seat's budget held on the run's row, which the waiter passes
//     over and the claim refuses until the hold lifts, is held no later than
//     the deadline ([Coordinator.unclaimHeld]): held until the budget's window
//     turned, a day, a week or a month on, nothing reached it to end it.
//   - THE RESUME GIVE-UP ([ResumeGiveUp], [MinResumeFailures]), for every
//     turn, capped or not. A resume that FAILS for a reason of the run's own
//     (not one that waits, not one the node or its infrastructure failed, and
//     not one another node or build can get past while a live node may) is
//     counted onto the job's record and, once it has failed that often for
//     that long, the run is settled ([Coordinator.resumeGivenUp]).
//     Uncounted, each poll's announcement was a fresh delivery with a fresh
//     budget, so a resume that always failed was retried every poll for as
//     long as the engine ran.
//
// A WAIT ON A CONDITION is not bounded here for a turn with no deadline: the
// condition names when it lifts, or the event that lifts it, and the seat
// could not run other work through it meanwhile. An answer's resume is bounded
// by its own series ([MaxAnswerAttempts]) and the run's pause TTL.
//
// NOR IS A CONDITION ONLY AN OPERATOR CLEARS. A resume that fails on this
// node's disk (full, mounted read-only, or failing to read or write), on its
// limit of open files, or on the fleet's nodes holding different keys or
// running different builds is the node's or its infrastructure's
// ([CoordinatorOptions.NodesFailure]), so it counts nothing toward the
// give-up, and no wait clears it: the run and its seat wait for the operator,
// the job's completion coming round on each announcement, and a turn with a
// deadline still ends at it. Ending the run would throw away the review of a
// job that finished, for a condition the next resume after the operator's fix
// gets past. What the operator sees, condition by condition:
//
//   - every one: the `executor_resume_blocked` alarm, raised by every data
//     node from the status record of the node holding the seat, a satellite's
//     included, while one of its runs waits so ([Coordinator.ResumesBlocked],
//     [CoordinatorOptions.OperatorsCondition]), naming the seat waiting
//     longest, the node holding it, the condition's kind and for how long,
//     and cleared once a resume gets past it;
//   - every one: each attempt's `sandbox_resume_failed` line, at error, its
//     `error` naming the condition (its `attempt` does not rise for these:
//     it numbers only the run's own failures): the system's or the store
//     driver's own words for a disk or the open files (such as "no space
//     left on device", "database is full", "read-only file system",
//     "input/output error", "too many open files"),
//     the estate's "finish the upgrade" for builds that differ, and a note or
//     a run's record this node could not verify for keys that differ;
//   - a full disk that is the volume holding this node's databases: the
//     `volume_low` alarm besides, raised while the volume has less free
//     space than its next restore, vacuum or snapshot needs.

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
// failed ([Release.ResumeFailedAt]); and, in this node's memory rather than on
// the record, the kind of a condition only an operator clears that the resume
// failed on, which counts the run in [Coordinator.ResumesBlocked] once the
// hand-back lands ([blockedResume]). The zero value counts nothing.
type failure struct {
	collectFailed time.Time
	resumeFailed  time.Time
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

// completeAtDeadline ends the run of a completion its node would not resume now,
// because its turn's deadline has passed: claimed, collected, charged and
// recorded as every completion is ([Coordinator.collectClaimed]), and then
// settled instead of resumed ([Coordinator.endAtDeadline]).
//
// THE CONDITION THAT REFUSED IT IS NOT WAITED FOR. Before the deadline a
// completion the gate refuses, or one this node holds off after its resume met
// a limit, waits for the condition to clear, because the turn still has time
// to review its job. After it, the wait is time the turn does not have, spent
// holding the seat. Asked by an announcement of the job, or, for one this node
// holds off itself where no waiter announces it, by that hold's own backstop
// ([Coordinator.recheckHeld]), so a deadline that passes while nothing
// announces the job still ends the run within the backstop.
//
// No turn starts here, which is why no admission is asked: the claim, the
// collection and the ending are the run's, and none of them is a turn. But the
// claim is a claim like any other, and meets the seat's recovery gate: a pass
// of the seat under way on this node refuses it, and its road carries it past
// the pass (recoverygate.go). Asking no admission, it was the one claim the
// acquisition's own refusal of a seat still establishing never kept out of
// that seat's recovery.
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
// [MinResumeFailures] times across [ResumeGiveUp], counting the failure being
// handled, and reports whether it did. The run's other failures are on its
// job's record ([LaunchRecord.ResumeFailures]), so the count holds across
// nodes and restarts.
//
// ONLY THE RUN'S OWN FAILURES COUNT ([Coordinator.runsOwnFailure]), and its
// caller asks before it asks this. A resume the node or its infrastructure
// failed (a coordination store that did not answer the read of where the
// turn's writes are minted, a full disk, a stop that cut it short) is handed
// back as any failure is, and comes round again when the poll announces the
// job, but it is no evidence that the turn cannot be re-entered: counted, an
// outage of half an hour ended every run whose job finished during it, the
// work it reviewed thrown out with the run, though the next attempt after the
// outage would have resumed it.
//
// NOR DOES A RESUME ANOTHER NODE OR BUILD CAN MAKE ([ErrResumeElsewhere]),
// while a live node may make it: a parked state a newer build wrote, met by an
// older node still holding the seat through a roll, is a failure of where the
// completion went, and the run is resumed once a node that reads it holds the
// seat. Once no live node reads the state's version, as after a rollback, it
// is counted like the run's own, so the seat is not held for ever by a state
// nobody running can read; and a reply kind this build does not read, which
// is no part of that version, once no live node runs another build.
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

// blockedResume is a completion whose last resume on this node failed on a
// condition only an operator clears ([CoordinatorOptions.OperatorsCondition]):
// the seat it holds, the condition's kind, and when the first such failure in
// a row was met.
//
// COUNTED ONCE THE ATTEMPT'S HAND-BACK LANDS, never as the failure is met:
// handBack is the release that gives the claim back while it is out (or owed,
// which this node makes again until it lands), and counted whether a release
// of it landed. A run waits for the operator only once it is back where its
// claim found it, the completion coming round again on it; one whose
// hand-back did not land was ended in its place, or had moved on, its ending
// decided by another path while the resume ran (a person's stop of the run
// the resumed turn relaunched, say), and counted as the failure was met, after
// that ending had taken it out ([Coordinator.runSettled]), it held the alarm
// up until its seat moved. Recorded with the release, in the step that records
// it ([Coordinator.handingBackLocked]), and decided in the step that takes its
// answer ([Coordinator.blockedAnsweredLocked]), so an ending that lands
// between the release's landing on the store and that step takes the entry
// out before it is counted. An entry counted by an earlier attempt stays
// counted while the next one's release is out.
//
// AND ONLY WHILE THIS NODE HOLDS THE SEAT UNDER fence, the lease the attempt's
// claim was taken with ([fenceOf]). The seat's release takes the run out
// ([Coordinator.ReleaseSeat]), but nothing cuts a resume's frame short there
// (the node's admission ends a turn at a drain's cut alone), and a frame still
// running here met its failure after the release, its hand-back landing on a
// row no newer holder had fenced yet: counted, a run of a seat this node no
// longer held stood in its status record, raised fleet-wide whatever the seat's
// next holder made of it, until the node restarted or the run came back to it.
// So [Coordinator.ResumesBlocked] counts a run only where the lease seam answers
// that this node holds the seat under fence ([CoordinatorOptions.Lease]), and
// lets go of one it no longer does; and an entry an earlier holding left is no
// start for a later one's ([Coordinator.blockedHandBackLocked]).
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
// and whose last resume here failed on a condition only an operator clears.
//
// THE GIVE-UP NEVER ENDS SUCH A RUN ([Coordinator.resumeGivenUp]): the failure
// is the node's, so the run and its seat wait for the operator, the job's
// completion coming round on each announcement and failing again until the
// condition is cleared. Each attempt's `sandbox_resume_failed` line says why,
// but a line repeated on every announcement is no signal anyone is paged by,
// so the wait is raised as the executor_resume_blocked alarm while it lasts:
// the node says this in its status record, and every data node raises it from
// there (internal/engine's executorReading), since the node holding the seat
// may be a satellite, which evaluates no alarm table of its own. FROM THIS
// NODE'S OWN MEMORY of its attempts, which is exact for the seats it holds:
// counted once the failed attempt's hand-back lands, and only while this node
// holds the seat under the lease that attempt's claim was taken with
// ([blockedResume]); a resume of the run that gets past the condition, or
// fails for anything else, takes it out, and so does a hand-back that does not
// land, the run's settling ([Coordinator.runSettled]) and the seat's release,
// whose next holder says it again from its own attempts. A restart forgets it
// until the run's next attempt fails again.
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
