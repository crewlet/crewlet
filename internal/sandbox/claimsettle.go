package sandbox

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// current is a claimed run as its record stands after the resumed turn returned.
// The LATEST, since the box to settle is the one it names now (a re-seeded run
// provisioned a fresh box; a relaunch has a new job). A gone record is answered
// with the snapshot, whose settle reclaims a box that may remain.
//
// THREE ANSWERS, NOT TWO: "here", "gone" and "the store could not be read" are
// three facts and the last must not read as either other. As a bool, callers read
// false as "settle nothing", leaving the row in [StatusResumed], which nothing
// recovers ([Coordinator.unclaim]); the error goes to [Coordinator.settleClaimed].
func (c *Coordinator) current(ctx context.Context, run PendingRun) (PendingRun, error) {
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	if err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: reading run %s to settle it: %w", run.TurnID, err)
	}
	if !found {
		return run, nil
	}
	return latest, nil
}

// claimedOnly is the status a claim's own ending is licensed for: the run is
// ended only while it is still the claim — see [Coordinator.endClaim].
var claimedOnly = []string{StatusResumed}

// endClaim ends a run while it is still the claim this call took, for an ending
// decided without reading the row (a failed settle read, [Coordinator.settleClaimed];
// a refused release, [Coordinator.unclaim]); it reports whether it ended the run
// and the error where the ending is kept ([errEndingOwed]). The condition moves
// to the store, so a relaunched run or a landed release is declined. Decided
// first, reclaimed second, the reclaim riding the recorded ending
// ([RecordedEnding.Reclaim]). See the package doc, "Ending a claim".
func (c *Coordinator) endClaim(ctx context.Context, run PendingRun, e ending) (bool, error) {
	// DETACHED, like every teardown: reached from failure paths and a drain-cancelled
	// handler, so the context is often dead, and a no-op settle is the stranded
	// row again. Each write gets the row's crossing time besides ([detachedEnding]).
	e.fence, e.whileIn, e.launch, e.reclaim = fenceOf(run), claimedOnly, run.LaunchID, true
	endCtx, done := detachedEnding(ctx)
	defer done()
	decided, err := c.endRecord(endCtx, run, e)
	return decided != nil, err
}

// settleClaimed ends a run whose record could not be read, WHILE IT IS STILL
// CLAIMED ([Coordinator.endClaim]). SILENCE IS NOT AN ANSWER HERE: both callers
// just took a suspended turn from the engine, and the row left is the claim itself
// (no poll reads a [StatusResumed] row, no redelivery re-claims it, no answer matches
// it, no reaper expires it), a turn destroyed in silence beside a held paused box,
// which [Coordinator.unclaim] ends rather than accept. The one hazard the read
// guarded (killing a job the turn RELAUNCHED in this box) the claim's license
// declines. note is what the ending announces, nil where the turn announced itself.
// Reports whether the ending is this node's, as [Coordinator.finish] does.
func (c *Coordinator) settleClaimed(ctx context.Context, run PendingRun, cause error, note *failureNote) bool {
	ended, err := c.endClaim(ctx, run, ending{note: note})
	switch {
	case err != nil:
		log.ErrorContext(ctx, "sandbox_claimed_settle_kept",
			"turn_id", run.TurnID, "error", err.Error(), "cause", cause.Error(),
			"detail", "the run's record could neither be read nor ended; its ending is kept and "+
				"this node finishes it, its box included, once the store answers — or the seat's "+
				"next recovery pass reaps it, if the seat moves first")
	case !ended:
		// Not ours: the run moved on (a relaunch before the turn broke) or
		// somebody ended it; that party owns the box and the record.
		log.InfoContext(ctx, "sandbox_claimed_settle_declined",
			"turn_id", run.TurnID, "cause", cause.Error(),
			"detail", "the run is no longer the claim this settle took, so it is left to "+
				"whoever moved it")
	default:
		log.WarnContext(ctx, "sandbox_claimed_settled_unread",
			"turn_id", run.TurnID, "error", cause.Error(),
			"detail", "the run's record could not be read after the resume, so it was ended "+
				"while it was still the claim rather than left in it, and its box reclaimed")
	}
	return ended || err != nil
}

// unclaim hands a claimed tail back to the exact status the claim took it from,
// only while it is still this call's claim ([Release]), counting the run into the
// set that status belongs to. It takes a context of its own, since the failure it
// undoes is often the cancellation. A hand-back the store refused ends the run
// while still the claim ([Coordinator.endClaim]); one it did not answer is kept
// owed (claimrelease.go). No stop is reported on success. Returns nil where the
// run is still in a retry's hands, the release's error where it was ended. See
// the package doc, "Handing a claim back".
func (c *Coordinator) unclaim(ctx context.Context, run PendingRun, detail string) error {
	return c.unclaimAt(ctx, run, detail, failure{}, nil, BudgetWindow{})
}

// unclaimHeld hands back a claim whose resume the seat's conditions refused,
// holding its resume until the refusal lifts ([Release.Hold]) and counting the
// window its relaunch was refused in ([Release.RelaunchRefusedIn]). A refusal
// only an event lifts is held until [answerWaitBackstop], or a turn's deadline if
// no later ([PendingRun.Deadline]), since nothing reaches a held row to end it.
// Returns the hold the completion is kept for ([Coordinator.keepHeldOnRow]). See
// the package doc, "Holding a refused resume".
func (c *Coordinator) unclaimHeld(ctx context.Context, run PendingRun, refusal Refusal) (Refusal, error) {
	if refusal.Until.IsZero() {
		refusal.Until = c.now().Add(answerWaitBackstop)
	}
	if !run.Deadline.IsZero() && run.Deadline.Before(refusal.Until) {
		refusal.Until = run.Deadline
	}
	log.InfoContext(ctx, "sandbox_resume_held",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "condition", string(refusal.Condition),
		"reason", refusal.Reason, "until", refusal.Until.UTC().Format(time.RFC3339),
		"detail", "the job stays collected in its paused box and its seat held behind it; "+
			"its completion is announced again when the refusal lifts")
	return refusal, c.unclaimAt(ctx, run, resumeUnrevertedDetail, failure{}, &refusal, refusal.Window)
}

// unclaimAt is [Coordinator.unclaim] for a claim whose collection or following
// resume failed: failed is what the release counts onto the job's record; detail
// is the sentence an ending reaches an operator's board with if the store refuses
// the release; hold holds the resume back ([Release.Hold]); refusedIn is the
// window the relaunch was refused below its floor in ([Release.RelaunchRefusedIn]).
// Every hand-back is recorded as out first by [Coordinator.handingBackLocked], so
// no claim of the job starts meanwhile ([Coordinator.claimCompletion]), and made
// by [Coordinator.unclaimOwing]; an unanswered one is kept owed (claimrelease.go).
func (c *Coordinator) unclaimAt(ctx context.Context, run PendingRun, detail string, failed failure,
	hold *Refusal, refusedIn BudgetWindow,
) error {
	// AND WHAT THE CLAIM LEARNED OF THE BOX HOST'S STORE goes onto the job's
	// record, so a restart or next holder reads it before acknowledging a
	// delivery no clock of its own covers (renew.go).
	silent, answered := c.hostStoreSeen(ctx, run, failed)
	c.mu.Lock()
	cr, moved := c.handingBackLocked(run, detail, failed, hold)
	cr.release.HostStoreSilentSince, cr.release.HostStoreAnswered = silent, answered
	cr.release.RelaunchRefusedIn = refusedIn
	c.mu.Unlock()
	moved()
	return c.unclaimOwing(ctx, cr)
}

// handingBackLocked records the release that hands run's claim back among the
// releases out, and its write in the run's entry, in one step under mu, returning
// it for [Coordinator.unclaimOwing] to make after unlock. A recount counts the run
// as the claim while the release is out; a caller whose own write failed records
// the release in the step taking that write's answer ([Coordinator.park]). The
// release it replaces (an earlier job's, still owed) is answered first as not
// landed, or it would hold every later move behind it. See the package doc,
// "Handing a claim back".
func (c *Coordinator) handingBackLocked(run PendingRun, detail string, failed failure, hold *Refusal,
) (*claimRelease, func()) {
	cr := &claimRelease{run: run, release: releaseFor(run, failed, hold), detail: detail}
	// THE RELEASE WHOSE LANDING COUNTS THE RUN AS WAITING FOR THE OPERATOR
	// ([blockedResume]) is recorded before the one it replaces is answered, so an
	// earlier-counted run keeps its instant.
	if failed.blocked != "" {
		c.blockedHandBackLocked(cr, failed.blocked)
	}
	moved := func() {}
	if earlier := c.releases[run.TurnID]; earlier != nil {
		moved = c.wroteCountLocked(earlier.write, false)
		c.blockedAnsweredLocked(earlier, false)
	}
	cr.write = c.writingCountLocked(run, StatusResumed, cr.release.To)
	c.releases[run.TurnID] = cr
	return cr, moved
}

// releaseAnswered acts on the store's answer to cr's release as
// [Coordinator.unclaimAt] reports it: a failed release ends the run while still
// the claim, one that moved on is left to what moved it, and a landed one has
// already moved the seat's count in the step taking its answer
// ([Coordinator.unclaimOwing]).
func (c *Coordinator) releaseAnswered(ctx context.Context, cr claimRelease, released bool, err error) error {
	run, to := cr.run, cr.release.To
	switch {
	case err != nil:
		log.ErrorContext(ctx, "sandbox_claim_revert_failed",
			"turn_id", run.TurnID, "revert_to", to, "error", err.Error(),
			"detail", "the claim could not be handed back, so nothing would ever pick "+
				"this run up; it is ended and announced instead, unless the hand-back "+
				"landed after all")
		ended, endErr := c.endClaim(ctx, run, ending{
			unused: claimedAnswer(run),
			note:   &failureNote{reason: types.ExecutorFailureClaimStranded, detail: cr.detail},
		})
		// AUTHORITATIVE, not assumed: the ending and this call each moved things
		// without knowing what the other found.
		c.syncSeat(ctx, run.AgentHandle)
		switch {
		case ended:
			return fmt.Errorf("sandbox: handing back the claim on %s (to %s): %w", run.TurnID, to, err)
		case endErr != nil:
			// THE ENDING IS KEPT, and whether the hand-back landed is open: this
			// node finishes it or finds the run handed back and leaves it to its
			// retry. The run is not known gone, so the caller takes the retry's path.
			log.WarnContext(ctx, "sandbox_claim_revert_undecided",
				"turn_id", run.TurnID, "revert_to", to, "error", endErr.Error(),
				"detail", "the claim could not be handed back and the run could not be ended "+
					"either; this node keeps the ending and finishes it, or leaves the run to "+
					"its retry if the hand-back landed")
		default:
			log.WarnContext(ctx, "sandbox_claim_revert_landed",
				"turn_id", run.TurnID, "revert_to", to,
				"detail", "the run is no longer the claim: the hand-back that reported a "+
					"failure landed, or the run moved on, so it is left to its retry")
		}
		return nil
	case !released:
		log.WarnContext(ctx, "sandbox_claim_moved_on",
			"turn_id", run.TurnID, "launch_id", run.LaunchID,
			"detail", "the run no longer holds this claim, so nothing was handed back: "+
				"the resumed turn launched another job, the seat's next owner reaped it, "+
				"or its ending let its answer go")
		c.syncSeat(ctx, run.AgentHandle)
	}
	// LANDED: the claim's count moved with it in the step that settled it
	// ([Coordinator.unclaimOwing]).
	return nil
}

// claimedFrom is the status a claimed run held before its claim. A claim always
// records it; running is the only status a completion takes a tail out of, so a
// row without the record goes back to it.
func claimedFrom(run PendingRun) string {
	if run.ClaimedFrom == "" {
		return StatusRunning
	}
	return run.ClaimedFrom
}

// settleFailed ends a run that failed: reaps the box, finishes the run, frees the
// seat, and SAYS SO. Every step is attempted whatever the earlier ones did, since
// none failing may leave a seat parked on an over run. This path destroys a turn
// (record deleted, box gone, completion acked) and once published nothing, an
// identical silence to the seat, dashboard and requester. Announced by the ending
// itself ([Coordinator.endRecord]), only for the ending decided on the run: a newer
// lease's, or one somebody else ended or DECIDED first, is that party's to explain
// (a second announcement would name a reason the run did not end for). The engine
// is told on the same gate ([CoordinatorOptions.Stopped]); a recorded reply no turn
// took goes back to the seat ([ErrAnswerOwed]). Reports whether the ending is this
// node's ([Coordinator.finish]).
func (c *Coordinator) settleFailed(ctx context.Context, run PendingRun, reason, detail string) bool {
	return c.settleNoted(ctx, run, &failureNote{reason: reason, detail: detail})
}

// settleNoted is [Coordinator.settleFailed] over a whole announcement. The run
// leaves the set it was counted in (resumed or launching) as its delete lands, the
// answer moving it once ([ending.counted]); counted out afterwards, a recount
// could free a seat whose other run still worked. A run no longer this node's has
// no delete to move it, so the seat is recounted from the store. See the package
// doc, "Seat counts".
func (c *Coordinator) settleNoted(ctx context.Context, run PendingRun, note *failureNote) bool {
	_, ours := c.finish(ctx, run, ending{fence: fenceOf(run), note: note, counted: true})
	if !ours {
		c.syncSeat(ctx, run.AgentHandle)
	}
	return ours
}

// The four sentences a stranded claim reaches an operator's board with, one per
// caller because what an operator does differs: an unannounced question was seen
// by nobody; an announced but unrecorded one may be answered and never matched;
// an unrevertable resume or collection is finished work in a box. All name the
// coordination store, since an unhandable claim brought each here.
const (
	parkUnannouncedDetail = "the executor stopped to ask a person a question, but neither the " +
		"question could be announced nor its own claim given back to the " +
		"coordination store, so nobody was asked and the turn cannot be continued; the " +
		"work it pushed, if any, is on its branch"

	parkUnrecordedDetail = "the executor stopped to ask a person a question, but neither the " +
		"question nor its own claim could be written to the coordination store, so " +
		"the turn cannot be continued and nobody can answer it; the work it pushed, if " +
		"any, is on its branch"

	resumeUnrevertedDetail = "the executor's launch finished but the turn it belongs to could " +
		"not be re-entered, and the executor's claim could not be given back to the coordination " +
		"store for another attempt, so the turn cannot be continued; the work it pushed, " +
		"if any, is on its branch"

	collectUnrevertedDetail = "the executor's launch finished but its box could not be read " +
		"back, and the executor's claim could not be given back to the coordination store for another " +
		"attempt, so its result is lost; the work it pushed, if any, is on its branch"
)

// resumeBrokenDetail is the sentence a run whose resume broke before its turn
// began reaches an operator's board with: nothing ran and a retry would reach the
// same defect, so the run is ended.
const resumeBrokenDetail = "the turn this executor's launch belongs to could not be re-entered — " +
	"resuming it broke before the turn began, and another attempt would reach the same " +
	"defect — so the turn cannot be continued; the work it pushed, if any, is on its branch"
