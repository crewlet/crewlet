package sandbox

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// A COLLECTION'S HAND-BACK THE COORDINATION STORE DID NOT ANSWER IS KEPT OWED.
//
// A collection that could not finish hands its claim back for the
// completion's retry ([Coordinator.collectFailed]), and a hand-back that
// cannot be written ends the run ([Coordinator.unclaimAt]): a row left in the
// claim is picked up by nothing. But a release the store did not answer is no
// refusal. Its request found no store to answer it, or its write was sent and
// the answer lost, so it may have landed ([coord.ErrUnavailable]); and a store
// that does not answer the release is very often the same silence the
// collection was handed back for ([ErrStoreSilent]): a leader election, a
// member restarting, a quorum lost for seconds. Ended there, the run was
// announced claim_unreverted, "its result is lost", while its box held the
// result and the store answered again a moment later.
//
// So this node KEEPS IT ([Coordinator.oweRelease]). The claim stays its own
// until the store answers, the release is asked again on the hand-back's
// spacing ([answerRetryCeiling]) with the seat's mail held behind it, as a
// revival or a give-back the store did not confirm is
// ([Coordinator.oweRevival], [Coordinator.giveBackClaim]), and this node never
// turns it into an ending for the store's silence. UNCOUNTED: the release's own
// failure says nothing about the box, and a release that counts a failed
// collection counts it once, when it lands. A release the store REFUSED (a
// definite answer no retry changes: a row this build cannot decode or encode, a
// value past what its key holds) still ends the run as [Coordinator.unclaimAt]
// says, and a seat that moves before the release lands has the claim reaped by
// its next holder, as a claim whose node stopped is ([Coordinator.reapTail]).
//
// # Nothing of the job is claimed on this node while its release is owed
//
// A release names its claim by the job, the status and the lease, and a claim
// this node takes of the same job again carries all three. A release that
// landed though its answer was lost puts the run back in running, where the
// completion's retry claims it again; the release asked again then would give
// back that claim while its collection is still working, and the claim after
// it would collect and resume the job a second time beside the first. So a
// completion of the job is refused here while its release is owed
// ([Coordinator.OnCompleted], [errReleaseOwed]) and comes back on the queue's
// backoff, as one whose claim the store does not take does, and the waiter
// announces the job again once the release has put the run back in running.
// Nothing can claim it between the release and the record that it is owed: a
// completion is claimed only by the seat's holder, and the seat's control
// topic hands this node its deliveries one at a time.

// errReleaseOwed is a completion refused because this node still owes the
// release of the claim it took on the same job ([Coordinator.oweRelease]).
var errReleaseOwed = errors.New("sandbox: this node still owes the release of the job's last claim")

// claimRelease is a claim's hand-back: the run as claimed, the release that
// gives the claim back, and the sentence the run's ending reaches an operator's
// board with where the store refuses the release.
type claimRelease struct {
	run     PendingRun
	release Release
	detail  string
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

// unclaimCollection hands back the claim a collection could not finish, failed
// being what the release counts onto the job's record, and keeps a release the
// store did not answer owed rather than ending the run for it (see the top of
// this file). It reports as [Coordinator.unclaimAt] does: nil where the claim
// went back, is owed, or had moved on, and the release's own error where the
// run was ended in its place.
func (c *Coordinator) unclaimCollection(ctx context.Context, run PendingRun, failed failure) error {
	return c.unclaimOwing(ctx, claimRelease{
		run: run, release: releaseFor(run, failed, nil), detail: collectUnrevertedDetail,
	})
}

// unclaimOwing makes cr's release, for a collection's hand-back and for each
// retry of one this node owes: a release the store did not answer is owed
// again ([Coordinator.oweRelease]), and any other answer settles what was owed
// and is acted on as [Coordinator.unclaimAt] acts on it.
//
// A CONTEXT OF ITS OWN, as [Coordinator.unclaimAt]'s is, because the failure it
// hands back is often the cancellation itself.
func (c *Coordinator) unclaimOwing(ctx context.Context, cr claimRelease) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	released, err := c.pending.ReleaseClaim(ctx, cr.run.TurnID, cr.release)
	if errors.Is(err, coord.ErrUnavailable) {
		c.oweRelease(ctx, cr, err)
		return nil
	}
	if c.releaseDone(cr.run.TurnID) && err == nil && released {
		log.InfoContext(ctx, "sandbox_claim_reverted",
			"turn_id", cr.run.TurnID, "launch_id", cr.run.LaunchID, "revert_to", cr.release.To,
			"detail", "the claim's hand-back the coordination store did not answer has landed; the run "+
				"is back where the claim found it, for its completion's retry")
	}
	// THE SEAT STILL COUNTS THE RUN: every collection hands back a claim its
	// seat counts as it was, since running and resumed both hold it.
	return c.releaseAnswered(ctx, cr, true, released, err)
}

// oweRelease keeps a claim's release the store did not answer for this node's
// retry ([Coordinator.retryOwed]) on the hand-back's spacing, with the seat's
// mail held behind it; if the seat moves first, its next holder's recovery
// pass reaps the claim. See the top of this file.
func (c *Coordinator) oweRelease(ctx context.Context, cr claimRelease, cause error) {
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
	r.release = &cr
	c.mu.Unlock()
	c.owe(ctx, run.AgentHandle, run.TurnID)
	c.scheduleOwed(run.TurnID, answerRetryCeiling)
}

// releaseDone forgets a claim's release this node owed, once the store has
// answered it, and reports whether one was owed.
func (c *Coordinator) releaseDone(turnID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.retries[turnID]
	if r == nil || r.release == nil {
		return false
	}
	r.release = nil
	return true
}

// releasing reports whether this node still owes the release of the claim it
// took on turnID's job launch.
func (c *Coordinator) releasing(turnID, launch string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.retries[turnID]
	return r != nil && r.release != nil && r.release.run.LaunchID == launch
}
