package sandbox

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A CLAIM'S HAND-BACK THE COORDINATION STORE DID NOT ANSWER IS KEPT OWED.
//
// A claim whose work could not finish is handed back for its signal's retry
// ([Coordinator.unclaimAt]): a collection that could not read its box or finish
// after it ([Coordinator.collectFailed]), a park whose question could not be
// announced or recorded ([Coordinator.park]), and a resume that failed or that
// its seat's conditions held ([Coordinator.resumeAndSettle]). A hand-back that
// cannot be written ends the run ([Coordinator.unclaim]): a row left in the
// claim is picked up by nothing. But a release the store did not answer is no
// refusal. Its request found no store to answer it, or its write was sent and
// the answer lost, so it may have landed ([coord.ErrUnavailable]); and a store
// that does not answer the release is very often the same silence the claim
// was handed back for ([ErrStoreSilent], a park's record, a resume's read of
// its log): a leader election, a member restarting, a quorum lost for seconds.
// Ended there, the run was announced claim_unreverted, "its result is lost",
// while its box held the result, its question or its turn, and the store
// answered again a moment later.
//
// So this node KEEPS IT ([Coordinator.oweRelease]). The claim stays its own
// until the store answers, the release is asked again no further apart than
// the hand-back's spacing ([answerRetryCeiling]), sooner where an answer's own
// backoff arms its series sooner ([Coordinator.scheduleOwedLocked]), with the
// seat's mail held behind it, as a revival or a give-back the store did not
// confirm is ([Coordinator.oweRevival], [Coordinator.giveBackClaim]), and this
// node never turns it into an ending for the store's silence. UNCOUNTED: the release's own
// failure says nothing about the box, and a release that counts a failed
// collection or resume counts it once, when it lands. A release the store
// REFUSED (a definite answer no retry changes: a row this build cannot decode
// or encode, a value past what its key holds) still ends the run as
// [Coordinator.unclaim] says, and a seat that moves before the release lands
// has the claim reaped by its next holder, as a claim whose node stopped is
// ([Coordinator.reapTail]).
//
// THE SEAT COUNTS THE RUN AS THE CLAIM IT STILL IS until a release lands, and
// the landing moves that count to the set the release returns the run to, ONCE,
// in the step that takes the release's answer and drops it from the releases
// out ([Coordinator.unclaimOwing]), or from the claim that won on the run it
// put back ([Coordinator.claimCompletion]): a run owed its release is in the
// claim on the store's row, which holds the seat. Given back before the release
// instead, as a resume's hand-back once gave it, a release owed for minutes left
// the seat counted free beside a claim that would come back to it, and a
// recount meanwhile counted the run again, so the landing counted it twice.
//
// THE RELEASE IS ONE MORE WRITE IN ITS RUN'S ENTRY (countwrite.go), recorded
// with the release, before it is sent, from the claim to the status it returns
// the run to ([claimRelease.write]), and out for as long as the release is out
// or owed. So a recount counts the run where the moves made so far leave it,
// the claim, whatever the store's row says by then ([Coordinator.syncSeat]):
// one that listed the row after the release landed and was written before its
// answer was taken counted the run where the release put it, and the answer's
// move counted it there again, an answer's release taking the seat's holding
// count down twice. And a write recorded behind it on the same run moves after
// it, in the order the two were recorded: a park's question can land with its
// answer lost, the park moving nothing and handing its claim back, and a reply
// then answers that question while the release is owed. Read apart from the
// run's other writes, the release counted the run as the claim whatever was
// out behind it, and the record of that reply took the run out of a question
// set it was never counted in: a seat holding another question read it as
// closed, and that question's answer ran as an unrelated turn.
//
// # No claim of the job is taken on this node while its release is out
//
// A release names its claim by the job, the status and the lease, and a claim
// this node takes of the same job again carries all three. A release that
// landed though its answer was lost puts the run back where the claim found
// it, where the claim's retry takes it again; the release asked again then
// would give back that claim while its work is still going on, and the claim
// after it would collect and resume the job a second time beside the first. So
// the release is RECORDED AS OUT BEFORE IT IS SENT, and kept recorded while it
// is owed ([Coordinator.releases]), and no claim of the job is taken here while
// it is.
//   - A COMPLETION'S CLAIM is refused ([Coordinator.claimCompletion],
//     [errReleaseOwed]): the completion comes back on the queue's backoff, as
//     one whose claim the store does not take does, and the waiter announces
//     the job again once the release has put the run back in running.
//   - AN ANSWER'S CLAIM is left to the series that owes the release
//     ([Coordinator.leaveToRelease]), which makes the release again before
//     anything else and goes on to the answer only once it is no longer out
//     ([Coordinator.retryOwed]). The release is the answer's own where its
//     resume failed, and a park's where the store recorded the question though
//     it did not answer the write: a reply then answers the question while the
//     park's release is still owed, and the attempt the reply makes where it
//     is recorded ([Coordinator.attemptOwed]) used to claim the answer at once.
//     That claim was the one the release names, so the release made again gave
//     it back to running under the turn it had resumed, the answer with it.
//     And settled once its release landed, as the series of a completion's
//     release still is, the series left the answer on its run with nothing on
//     this node to resume it.
//
// A CLAIM ALREADY ON ITS WAY when the release went out is not refused, and
// cannot be. The node's admission orders two claims of a job only where both
// are admitted turns, one at a time on the seat, and a claim made at the
// turn's deadline asks for none ([Coordinator.completeAtDeadline]), beside the
// control topic's deliveries and the completions re-fired off their own
// goroutines ([Coordinator.Readmit]): such a claim can pass the refusal before
// the release is recorded and reach the store after it has landed. It wins
// only then, so ITS WIN IS PROOF THE RELEASE LANDED, whatever the release's own
// answer said: the release is settled there and never made again, and a
// release owed is not made again while a claim of its job is still on its way
// ([Coordinator.claiming]), which decides first.

// errReleaseOwed is a completion refused because this node is still giving
// back, or still owes, the release of the claim it took on the same job
// ([Coordinator.oweRelease]).
var errReleaseOwed = errors.New("sandbox: this node still owes the release of the job's last claim")

// claimRelease is a claim's hand-back: the run as claimed, the release that
// gives the claim back, and the sentence the run's ending reaches an operator's
// board with where the store refuses the release.
type claimRelease struct {
	run     PendingRun
	release Release
	detail  string

	// write is the release's move in its run's entry (countwrite.go):
	// recorded with the release, from the claim to the status it returns the
	// run to, and answered in the step that drops the release from the
	// releases out, as landed where it did. Under mu.
	write *countWrite
}

// releaseFor is the release that gives run's claim back: what the claim
// recorded on its way rides back with it (its charge, the launch's own record,
// whether its collection read the box, and whether it collected the job),
// failed is what the release counts onto the job's record, and hold, where
// set, holds the run's resume back ([Release.Hold]).
func releaseFor(run PendingRun, failed failure, hold *Refusal) Release {
	return Release{
		Launch: run.LaunchID, To: claimedFrom(run), Charged: run.Charged, CompanyCharged: run.CompanyCharged,
		Published: run.Launch.Published, CollectFailedAt: failed.collectFailed, ResumeFailedAt: failed.resumeFailed,
		Collected: run.Collected, CollectedAt: run.Launch.CollectedAt, BoxAnswered: run.BoxAnswered,
		Fence: fenceOf(run), Hold: hold,
	}
}

// unclaimOwing makes cr's release, for a claim's hand-back
// ([Coordinator.unclaimAt]) and for each retry of one this node owes: a
// release the store did not answer is owed again ([Coordinator.oweRelease]),
// and any other answer settles what was owed and is acted on as
// [Coordinator.releaseAnswered] acts on it. A release a claim of its job TOOK
// OVER while it was out, by winning on the run it put back, has landed, and
// nothing more is owed for it ([Coordinator.claimCompletion]); so has one the
// seat's release dropped, whose claim its next holder reaps.
//
// A CONTEXT OF ITS OWN, because the failure it hands back is often the
// cancellation itself.
func (c *Coordinator) unclaimOwing(ctx context.Context, cr *claimRelease) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	turnID := cr.run.TurnID
	released, err := c.pending.ReleaseClaim(ctx, turnID, cr.release)
	unanswered := errors.Is(err, coord.ErrUnavailable)
	moved := func() {}
	c.mu.Lock()
	out := c.releases[turnID] == cr
	owed := false
	if !unanswered {
		if out {
			delete(c.releases, turnID)
			// THE CLAIM'S COUNT MOVES WITH IT, now and not before, and
			// once: the run was the claim on the store's row until this
			// release landed, and a recount counted it as the claim while
			// the release was out ([Coordinator.syncSeat]), so it moves to
			// the set the release returned it to in the step that ends
			// that, as the release's write in the run's entry is answered
			// (countwrite.go). A completion's claim goes back to running,
			// which holds the seat as the claim did; an answer's to
			// answered, which holds nothing, its owed resume holding the
			// seat's mail. A release that did not land moves nothing, and
			// lets go whatever was recorded behind it on the run.
			moved = c.wroteCountLocked(cr.write, err == nil && released)
		}
		if r := c.retries[turnID]; r != nil && r.release == cr {
			r.release, owed = nil, true
		}
	}
	c.mu.Unlock()
	moved()
	switch {
	case unanswered && out:
		c.oweRelease(ctx, cr, err)
		return nil
	case unanswered:
		// TAKEN OVER while it was out: nothing is owed here.
		return nil
	}
	if owed && err == nil && released {
		log.InfoContext(ctx, "sandbox_claim_reverted",
			"turn_id", turnID, "launch_id", cr.run.LaunchID, "revert_to", cr.release.To,
			"detail", "the claim's hand-back the coordination store did not answer has landed; the run "+
				"is back where the claim found it, for its retry")
	}
	return c.releaseAnswered(ctx, *cr, released, err)
}

// oweRelease keeps a claim's release the store did not answer for this node's
// retry ([Coordinator.retryOwed]) on the hand-back's spacing, with the seat's
// mail held behind it; if the seat moves first, its next holder's recovery
// pass reaps the claim. See the top of this file.
func (c *Coordinator) oweRelease(ctx context.Context, cr *claimRelease, cause error) {
	run := cr.run
	log.WarnContext(ctx, "sandbox_claim_revert_owed",
		"turn_id", run.TurnID, "launch_id", run.LaunchID, "revert_to", cr.release.To, "error", cause.Error(),
		"detail", "the coordination store did not answer the claim's hand-back; this node keeps the claim, "+
			"gives it back once the store answers and never ends the run for the store's silence, and the "+
			"seat's mail waits behind it")
	c.mu.Lock()
	r := c.retries[run.TurnID]
	if r == nil {
		r = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID, first: c.now(),
			window: answerWindow(run)}
		c.retries[run.TurnID] = r
	}
	r.release = cr
	c.mu.Unlock()
	c.owe(ctx, run.AgentHandle, run.TurnID)
	c.scheduleOwed(run.TurnID, answerRetryCeiling)
}

// releasing reports whether this node is giving back, or owes, the release of
// the claim it took on turnID's job launch.
func (c *Coordinator) releasing(turnID, launch string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.releasingLocked(turnID, launch)
}

// releasingLocked is [Coordinator.releasing] for a caller holding c.mu.
func (c *Coordinator) releasingLocked(turnID, launch string) bool {
	cr := c.releases[turnID]
	return cr != nil && cr.run.LaunchID == launch
}

// leaveToRelease reports whether an answer's attempt at run must leave its claim
// to the series that owes the release of the job's last claim, because this
// node is still giving that release back or owes it; and keeps that series armed
// where it is, so it makes the release first and then comes back to the answer.
// See the top of this file.
//
// ARMED ON THE HAND-BACK'S SPACING, and only where nothing arms it yet: an owed
// release's series is armed already ([Coordinator.oweRelease]), and one still on
// its way is armed again once it is owed. One that lands or is refused instead
// is followed by this series' read of the row, which finds what became of the
// answer. Armed at once, a series that came back here while the release was
// still on its way would have read the row and come back again, as fast as the
// store answered.
func (c *Coordinator) leaveToRelease(ctx context.Context, run PendingRun) bool {
	c.mu.Lock()
	if !c.releasingLocked(run.TurnID, run.LaunchID) {
		c.mu.Unlock()
		return false
	}
	r := c.retries[run.TurnID]
	if r == nil {
		r = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID, first: c.now(),
			window: answerWindow(run)}
		c.retries[run.TurnID] = r
	}
	if r.stop == nil {
		c.scheduleOwedLocked(run.TurnID, answerRetryCeiling)
	}
	c.mu.Unlock()
	log.InfoContext(ctx, "sandbox_answer_release_owed",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "launch_id", run.LaunchID,
		"detail", "the answer is recorded on a run whose last claim this node is still giving back, or owes "+
			"the release of; the series that makes that release claims the answer once the release is no "+
			"longer out")
	c.owe(ctx, run.AgentHandle, run.TurnID)
	return true
}

// claimCompletion claims ev's job for this node's collection of it, as
// [PendingStore.ClaimForResume] does, unless this node is giving back or owes
// the release of the job's last claim: refused then, with [errReleaseOwed],
// and the completion comes back for its retry. See the top of this file.
//
// THE REFUSAL AND THE COUNT OF CLAIMS ON THEIR WAY ARE ONE STEP, so a claim
// either finds the release recorded, or is counted before the release can be
// made again ([Coordinator.retryOwed]). A claim that WINS while the release is
// still recorded is proof the release landed: the release is settled, never
// made again, and the seat's mail it held is let go.
func (c *Coordinator) claimCompletion(ctx context.Context, ev types.ExecutorFinished, fence Fence,
) (PendingRun, bool, error) {
	turnID, launch := ev.TurnID, ev.LaunchID
	c.mu.Lock()
	if c.releasingLocked(turnID, launch) {
		c.mu.Unlock()
		log.InfoContext(ctx, "sandbox_completion_release_owed",
			"turn_id", turnID, "agent", ev.AgentHandle, "launch_id", launch,
			"detail", "this node is still giving back the job's last claim, or owes its release, which the "+
				"coordination store did not answer; the job is claimed again once that release lands")
		return PendingRun{}, false, errReleaseOwed
	}
	c.claiming[turnID]++
	c.mu.Unlock()
	run, won, err := c.pending.ClaimForResume(ctx, turnID, CompletionTail(launch), fence)
	c.mu.Lock()
	if c.claiming[turnID]--; c.claiming[turnID] <= 0 {
		delete(c.claiming, turnID)
	}
	landed, owed := false, false
	moved := func() {}
	if cr := c.releases[turnID]; won && cr != nil && cr.run.LaunchID == launch {
		delete(c.releases, turnID)
		landed = true
		// ITS COUNT MOVES ONCE, here, as the release's own answer would have
		// moved it ([Coordinator.unclaimOwing]): its write answered as
		// landed, back to running, which the claim that won took it out of
		// again (countwrite.go).
		moved = c.wroteCountLocked(cr.write, true)
		if r := c.retries[turnID]; r != nil && r.release == cr {
			r.release, owed = nil, true
		}
	}
	c.mu.Unlock()
	moved()
	if landed {
		log.InfoContext(ctx, "sandbox_claim_reverted",
			"turn_id", turnID, "launch_id", launch,
			"detail", "a claim of the job that was on its way when its last claim's hand-back went out won "+
				"on the run that hand-back put back, so the hand-back landed; it is not made again")
	}
	if owed {
		c.settleOwed(ctx, ev.AgentHandle, turnID)
	}
	return run, won, err
}
