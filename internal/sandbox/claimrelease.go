package sandbox

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A claim's hand-back the coordination store did not answer is kept owed
// ([Coordinator.oweRelease]), never turned into an ending: its write may have
// landed, and the silence is often the one the claim was handed back for. It is
// asked again within [answerRetryCeiling], uncounted, the seat's mail held; a
// release the store refused still ends the run ([Coordinator.unclaim]). The
// seat counts the run as the claim until a release lands, the release being one
// more write in its run's entry (countwrite.go). No claim of the job is taken
// here while its release is out ([errReleaseOwed]); a claim already on its way
// that wins proves the release landed; a later job's release takes an earlier
// one's place. See the package doc, "Owed claim releases".

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
		FailedOn:  failed.on,
		Collected: run.Collected, CollectedAt: run.Launch.CollectedAt, BoxAnswered: run.BoxAnswered,
		HarnessEnd: run.BoxHarnessEnd, Fence: fenceOf(run), Hold: hold,
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
// cancellation itself, as a teardown's write of a run's row takes
// ([Coordinator.detachedRecord]): the teardown's grace and the row's crossing.
func (c *Coordinator) unclaimOwing(ctx context.Context, cr *claimRelease) error {
	ctx, cancel := c.detachedRecord(ctx)
	defer cancel()
	turnID := cr.run.TurnID
	// A RELEASE WHOSE ANSWER IS NEVER TAKEN, its store call panicking, is
	// dropped on the way out: left recorded, it refused every claim of its
	// job and kept its run counted as the claim, and nothing would make it
	// again (countwrite.go).
	answered := false
	defer func() {
		if !answered {
			c.dropRelease(cr)
		}
	}()
	released, err := c.pending.ReleaseClaim(ctx, turnID, cr.release)
	answered = true
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
		// AND THE RUN WAITS FOR THE OPERATOR once it is back where the
		// claim found it, in the same step ([blockedResume]).
		c.blockedAnsweredLocked(cr, err == nil && released)
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

// dropRelease drops cr from the releases out, where it still is, with its write
// in its run's entry answered as one that did not land: a release whose answer
// was never taken ([Coordinator.unclaimOwing]).
func (c *Coordinator) dropRelease(cr *claimRelease) {
	moved := func() {}
	c.mu.Lock()
	if c.releases[cr.run.TurnID] == cr {
		delete(c.releases, cr.run.TurnID)
		moved = c.wroteCountLocked(cr.write, false)
	}
	c.blockedAnsweredLocked(cr, false)
	c.mu.Unlock()
	moved()
}

// oweRelease keeps a claim's release the store did not answer for this node's
// retry ([Coordinator.retryOwed]) on the hand-back's spacing, with the seat's
// mail held behind it; if the seat moves first, its next holder's recovery pass
// reaps the claim. Only while it is still out, asked in the one step that
// records the release on its series, owes the seat's run and arms the series: a
// claim on its way can take it over first ([Coordinator.claimCompletion]), and
// a seat's release drops it ([Coordinator.releaseOwed]). See the package doc,
// "Owed claim releases".
func (c *Coordinator) oweRelease(ctx context.Context, cr *claimRelease, cause error) {
	run := cr.run
	c.mu.Lock()
	if c.releases[run.TurnID] != cr {
		c.mu.Unlock()
		return
	}
	r := c.retries[run.TurnID]
	if r == nil {
		r = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID, first: c.now(),
			window: answerWindow(run)}
		c.retries[run.TurnID] = r
	}
	r.release = cr
	c.oweLocked(run.AgentHandle, run.TurnID)
	c.scheduleOwedLocked(run.TurnID, answerRetryCeiling)
	c.mu.Unlock()
	c.reconcileHold(ctx, run.AgentHandle)
	log.WarnContext(ctx, "sandbox_claim_revert_owed",
		"turn_id", run.TurnID, "launch_id", run.LaunchID, "revert_to", cr.release.To, "error", cause.Error(),
		"detail", "the coordination store did not answer the claim's hand-back; this node keeps the claim, "+
			"gives it back once the store answers and never ends the run for the store's silence, and the "+
			"seat's mail waits behind it")
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

// leaveToRelease reports whether an answer's attempt at run must leave its
// claim to the series that owes the release of the job's last claim, because
// this node is still giving that release back or owes it, and keeps that series
// armed, so it makes the release first and then comes back to the answer. Armed
// on the hand-back's spacing, only where nothing arms it yet, so a series does
// not spin while the release is on its way; owed in the step that finds the
// release out and records the series, so a seat's release cannot fall between.
// See the package doc, "Owed claim releases".
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
	c.oweLocked(run.AgentHandle, run.TurnID)
	if r.stop == nil {
		c.scheduleOwedLocked(run.TurnID, answerRetryCeiling)
	}
	c.mu.Unlock()
	c.reconcileHold(ctx, run.AgentHandle)
	log.InfoContext(ctx, "sandbox_answer_release_owed",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "launch_id", run.LaunchID,
		"detail", "the answer is recorded on a run whose last claim this node is still giving back, or owes "+
			"the release of; the series that makes that release claims the answer once the release is no "+
			"longer out")
	return true
}

// claimDoneLocked counts a claim of turnID's job off the claims on their way
// ([Coordinator.claiming]). Under mu.
func (c *Coordinator) claimDoneLocked(turnID string) {
	if c.claiming[turnID]--; c.claiming[turnID] <= 0 {
		delete(c.claiming, turnID)
	}
}

// claimCompletion claims ev's job for this node's collection of it, as
// [PendingStore.ClaimForResume] does, unless this node is giving back or owes
// the release of the job's last claim ([errReleaseOwed]). Through the recovery
// gate first ([Coordinator.claimGateLocked], [ErrSeatRecovering]), counted as
// this node's live attempt until the caller calls done; done is nil only beside
// an error the gate or the refusal returned. Either failure is the store's
// silence ([ErrStoreSilent]). The refusal and the count of claims on their way
// are one step, and a claim that wins while the release is recorded settles it.
// See the package doc, "Owed claim releases".
func (c *Coordinator) claimCompletion(ctx context.Context, ev types.ExecutorFinished, fence Fence,
) (run PendingRun, won bool, done func(), err error) {
	turnID, launch := ev.TurnID, ev.LaunchID
	c.mu.Lock()
	done, err = c.claimGateLocked(ev.AgentHandle, turnID)
	if err != nil {
		c.mu.Unlock()
		log.InfoContext(ctx, "sandbox_completion_recovery_refused",
			"turn_id", turnID, "agent", ev.AgentHandle, "launch_id", launch,
			"detail", "the seat's recovery pass is under way on this node, which claims none of its runs "+
				"until the pass is done; the completion is carried past it")
		return PendingRun{}, false, nil, err
	}
	if c.releasingLocked(turnID, launch) {
		c.mu.Unlock()
		done()
		log.InfoContext(ctx, "sandbox_completion_release_owed",
			"turn_id", turnID, "agent", ev.AgentHandle, "launch_id", launch,
			"detail", "this node is still giving back the job's last claim, or owes its release, which the "+
				"coordination store did not answer; the job is claimed again once that release lands")
		return PendingRun{}, false, nil, storeSilent(ctx, errReleaseOwed)
	}
	c.claiming[turnID]++
	c.mu.Unlock()
	// AND UNCOUNTED ON EVERY WAY OUT, a panic in the claim included: a count
	// left up held an owed release of the job back for good, and a live
	// attempt left counted kept every later recovery of the seat off the run.
	answered := false
	defer func() {
		if !answered {
			c.mu.Lock()
			c.claimDoneLocked(turnID)
			c.mu.Unlock()
			done()
		}
	}()
	run, won, err = c.pending.ClaimForResume(ctx, turnID, CompletionTail(launch), fence)
	answered = true
	c.mu.Lock()
	c.claimDoneLocked(turnID)
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
		c.blockedAnsweredLocked(cr, true)
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
	return run, won, done, storeSilent(ctx, err)
}
