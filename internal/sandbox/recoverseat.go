package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
)

// reclaimBox kills a run's box under killCtx and closes the run's credentials,
// logging under ctx. The error is a failed kill, the one outcome a retry can
// change; a run with no box, or a box whose placement is no longer configured,
// reports nil (nothing later could reach it).
func (c *Coordinator) reclaimBox(ctx, killCtx context.Context, run PendingRun) error {
	// BEFORE THE BOX CHECK: a launch that failed at create still opened a bridge
	// session nothing else closes.
	if c.ended != nil {
		c.ended(run.TurnID)
	}
	if run.SandboxID == "" {
		return nil
	}
	if !c.hosts(run) {
		// ANOTHER NODE'S HOST: killed by id here, the local backend would find no
		// such box and report done, leaving the job (a stopped one too) running.
		// Its host's waiter reclaims it once the row no longer names it, and is
		// told to look now (boxhost.go).
		log.InfoContext(ctx, "sandbox_reclaim_left_to_host", "turn_id", run.TurnID,
			"sandbox_id", run.SandboxID, "node", run.LaunchNode,
			"detail", "the box is on the host of the node that launched it, which reclaims it")
		c.tellHost(ctx, run)
		return nil
	}
	provider, err := c.mgr().Provider(Placement(run.Placement))
	if err != nil {
		// The record names a cell no longer configured: the box is unreachable, so
		// the caller still settles the record (open, it would hold the seat busy
		// forever over a box no configured cell reaches).
		log.WarnContext(ctx, "sandbox_teardown_no_backend",
			"turn_id", run.TurnID, "placement", run.Placement, "error", err.Error())
		return nil
	}
	if err := provider.Kill(killCtx, run.SandboxID); err != nil {
		log.WarnContext(ctx, "sandbox_teardown_failed",
			"turn_id", run.TurnID, "sandbox_id", run.SandboxID, "error", err.Error())
		return fmt.Errorf("sandbox: reclaiming box %s of run %s: %w", run.SandboxID, run.TurnID, err)
	}
	return nil
}

// RecoverSeat re-attaches to a seat's still-active runs as this node claims it.
// inbox is the inbox attachment every hold from here is taken for
// (seathold.go); attach, where not nil, attaches the seat's control topic once
// the recovery gate is closed (recoverygate.go). While the pass runs, every
// claim of the seat's runs here is refused ([ErrSeatRecovering]). Running jobs
// re-mark the seat held and questions are counted, each fenced to this lease
// ([Coordinator.fenceRun]); resumed and launching tails are reaped, which is
// safe only here ([Coordinator.reapTail]), except a dead claim of an untaken
// answer, which is revived ([Coordinator.revivable]). Each run is counted once
// ([Coordinator.takingOver]). See the package doc, "Recovering a seat".
func (c *Coordinator) RecoverSeat(ctx context.Context, handle, owner string, epoch int64,
	inbox queue.Attachment, attach func(context.Context) error) error {
	// THE SEAT'S RECOVERY GATE CLOSED FIRST, before anything can deliver: no claim
	// of its runs on this node until the pass is done, claims this node's attempts
	// already hold left to them (recoverygate.go).
	c.beginRecovery(handle)
	var (
		takeovers []*countWrite
		tally     recovery
		seen      map[signal]uint64
	)
	// EVERY TAKEOVER IS SETTLED ON EVERY WAY OUT, a panic included: one left out
	// holds every later move on its run (countwrite.go). THEN THE GATE OPENS and
	// what it refused is carried (completions re-fired, answers' next attempts
	// armed, [Coordinator.endRecovery]). ONLY THEN ARE OWED REFUSALS RE-ARMED
	// ([Coordinator.rearmOwed]) and held resumes kept ([Coordinator.keepHeldRows]),
	// so a re-fire's claim moves a finished-with run and neither starts a second
	// re-fire of a job the gate's carry re-fired ([Coordinator.carriedLocked]).
	defer func() {
		for _, w := range takeovers {
			c.settleWrite(w)
		}
		c.endRecovery(ctx, handle)
		c.rearmOwed(ctx, tally.owed)
		c.keepHeldRows(ctx, tally.held, seen)
	}()
	// THE CONTROL TOPIC INSIDE THE GATE: a completion published while the seat was
	// not consumed here is held by the subscription, delivered from now, and
	// refused by the gate until the pass is done.
	if attach != nil {
		if err := attach(ctx); err != nil {
			return fmt.Errorf("sandbox: recovering %s: %w", handle, err)
		}
	}
	// THE ATTACHMENT FIRST, before anything counted here can ask for the seat's
	// hold: every hold is taken for it.
	c.attachInbox(handle, inbox)
	// WHAT WAS SIGNALLED BEFORE THE RUNS ARE READ, for the keep of each held resume
	// ([Coordinator.keepHeldRows]).
	seen = c.signalsSeen()
	active, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		return fmt.Errorf("sandbox: recovering %s: %w", handle, err)
	}
	takeovers = c.takingOver(ctx, handle, active)
	if len(active) == 0 {
		return nil
	}
	for i, run := range active {
		c.takeoverAnswered(takeovers[i], c.recoverRun(ctx, run, owner, epoch, &tally, true))
	}
	log.InfoContext(ctx, "sandbox_seat_recovered",
		"seat", handle, "epoch", epoch, "running", tally.running,
		"parked", tally.parked, "abandoned", tally.abandoned, "finished", tally.finished,
		"revived", tally.revived, "owed", len(tally.owed), "held", len(tally.held), "active", len(active))
	return nil
}

// recovery counts what a seat's recovery pass found, for its one log line:
// finished is the endings the last holder decided and this one finished, revived
// the dead claims whose answer went back to its run, owed the running jobs whose
// record owes a refused completion its re-fire ([LaunchRecord.RefusalOwed]), and
// held the running jobs whose resume their row holds ([PendingRun.HeldFor]); the
// pass re-arms and keeps those once done.
type recovery struct {
	running, parked, abandoned, finished, revived int
	owed, held                                    []PendingRun
}

// rearmOwed owes again here the completion of each of runs' jobs whose record
// says a holder acknowledged a refused delivery ([LaunchRecord.RefusalOwed]): read
// off the row since that holder's memory did not survive a restart or move, and
// offered to this node's gate at once, never kept on the recorded condition alone
// (this holder may never see its signal). Refused, it is kept for this holder's
// condition ([Coordinator.keepRefused]) with a backstop re-fire
// ([Coordinator.keepLocked]). A job already carried is left
// ([Coordinator.carriedLocked]). See the package doc, "Re-arming owed refusals and
// holds".
func (c *Coordinator) rearmOwed(ctx context.Context, runs []PendingRun) {
	if len(runs) == 0 {
		return
	}
	var rearmed []PendingRun
	c.mu.Lock()
	if !c.closed {
		for _, run := range runs {
			if c.carriedLocked(run.TurnID) {
				continue
			}
			ev, trigger := announcementOf(run)
			kept := refusedCompletion{cond: run.Launch.RefusalOwed, ev: ev, trigger: trigger}
			c.keepLocked(kept)
			c.refireLocked(kept)
			rearmed = append(rearmed, run)
		}
	}
	c.mu.Unlock()
	for _, run := range rearmed {
		log.InfoContext(ctx, "sandbox_completion_refusal_rearmed",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "agent", run.AgentHandle,
			"condition", string(run.Launch.RefusalOwed),
			"since", run.Launch.RefusalOwedSince.UTC().Format(time.RFC3339),
			"detail", "a holder of the seat acknowledged a refused delivery of the job's completion and owed it "+
				"its re-fire; this holder owes it now, asks its own gate at once, and keeps it for its "+
				"condition's signal")
	}
}

// keepHeldRows keeps here the completion of each of runs' jobs whose resume a
// holder handed back held on the row ([PendingRun.HeldFor]), as that holder kept
// it ([Coordinator.keepHeldOnRow]): re-fired at the hold's instant (at once if
// past) or its condition's signal, never before, since the claim refuses a held
// row. A signal seen since `seen` lifts the hold and re-fires at once. A job
// already carried is left, asked under the keep's lock ([Coordinator.carriedLocked]).
// See the package doc, "Re-arming owed refusals and holds".
func (c *Coordinator) keepHeldRows(ctx context.Context, runs []PendingRun, seen map[signal]uint64) {
	var kept []PendingRun
	for _, run := range runs {
		if c.keepHeldOnRow(run, nil, Refusal{Condition: run.HeldFor, Until: run.HeldUntil}, run.Holds, seen,
			true) {
			kept = append(kept, run)
		}
	}
	for _, run := range kept {
		log.InfoContext(ctx, "sandbox_completion_hold_kept",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "agent", run.AgentHandle,
			"condition", string(run.HeldFor), "until", run.HeldUntil.UTC().Format(time.RFC3339),
			"detail", "a holder of the seat handed the job's resume back held on its row; this holder keeps "+
				"its completion for the hold's instant, and lifts the hold and re-fires it on its condition's signal")
	}
}

// reaped is what [Coordinator.reapTail] did with a tail the seat's last holder
// left.
type reaped int

const (
	// reapLeft: nothing, a newer lease owns the row or it is gone.
	reapLeft reaped = iota
	// reapMoved: not abandoned after all (given back before this node's fence
	// landed); the row is reported as it stands.
	reapMoved
	// reapRevived: a dead claim's answer was given back to its run, the answered
	// row reported, owed its resume.
	reapRevived
	// reapEnded: the tail was ended, its ending this call's.
	reapEnded
	// reapOwed: a revival that did not land, kept for this node's retry with the
	// seat's mail held ([Coordinator.oweRevival]).
	reapOwed
	// reapLive: refused, the claim being this node's own live attempt's
	// ([errReapLive]): left to it and counted as the claim holding the seat.
	reapLive
)

// errReapLive is the invariant a reap refuses on: a claim this node's own live
// attempt holds is never an abandoned tail. The recovery gate keeps them apart
// (recoverygate.go), so meeting one means the gate failed; the reap still refuses,
// since reaping it ended a turn under the review still running it.
var errReapLive = errors.New("sandbox: invariant broken: a reap reached a claim this node's own " +
	"live attempt holds, which the seat's recovery gate keeps from every pass")

// recoverRun takes over one run of a seat this node just acquired and reports the
// status the seat counts it in ([Coordinator.takeoverAnswered]): none for a run
// ended, reaped, left to a newer lease or gone, or a dead claim whose revival is
// kept owed. again is whether a run that moved under this recovery's fence
// ([Coordinator.fenceClaim], [Coordinator.fenceRun]) is taken over once more as
// what it moved to (once, the fence then holds the row). See the package doc,
// "Recovering a seat".
func (c *Coordinator) recoverRun(ctx context.Context, run PendingRun, owner string, epoch int64,
	tally *recovery, again bool,
) (counted string) {
	if run.Ending != nil {
		// AN ENDING THE LAST HOLDER DECIDED AND DID NOT FINISH is this holder's to
		// finish on the recorded terms (reply, box, one announcement under its own
		// identity), never a second ending of its own.
		if decided, err := c.endRecord(ctx, run, ending{
			fence: Fence{Owner: owner, Epoch: epoch}, whileIn: Active, launch: EveryLaunch,
		}); err != nil {
			log.WarnContext(ctx, "sandbox_ending_kept", "turn_id", run.TurnID, "error", err.Error(),
				"detail", "the ending the seat's last holder decided could not be finished yet; "+
					"this node finishes it, and the seat's mail waits behind it")
		} else if decided != nil {
			tally.finished++
		}
		return ""
	}
	switch run.Status {
	case StatusLaunching, StatusResumed:
		moved, did := c.reapTail(ctx, run, owner, epoch)
		switch did {
		case reapRevived:
			// THE ANSWER IS THE RUN'S AGAIN, owed its resume: taken over as the
			// answered run it now is, like one the last holder never resumed.
			tally.revived++
			return c.recoverRun(ctx, *moved, owner, epoch, tally, false)
		case reapMoved:
			if again {
				return c.recoverRun(ctx, *moved, owner, epoch, tally, false)
			}
		case reapEnded:
			tally.abandoned++
		case reapLeft, reapOwed:
			// Another lease's, gone, or a revival this node retries.
		case reapLive:
			// THIS NODE'S OWN LIVE CLAIM, left to its attempt and counted as the
			// claim holding the seat.
			return run.Status
		}
		return ""
	}
	// FENCED FIRST, TAKEN OVER AS THE FENCE FOUND IT ([Coordinator.fenceRun]): an
	// ending decided after the listing is finished here, a moved run taken over as
	// what it moved to.
	latest, served, gone := c.fenceRun(ctx, run, owner, epoch)
	switch {
	case gone:
		return ""
	case latest.Ending != nil || (again && latest.Status != run.Status):
		return c.recoverRun(ctx, latest, owner, epoch, tally, false)
	}
	run = latest
	switch run.Status {
	case StatusRunning:
		counted = run.Status
		tally.running++
		// A RESUME ITS ROW HOLDS is kept for the hold's instant once the pass is
		// done ([Coordinator.keepHeldRows]), whatever else is owed (an earlier
		// re-fire claims nothing). Else an acknowledged owed refusal is re-armed
		// ([Coordinator.rearmOwed]), a since-lifted hold included, which owed its
		// completion in the release that wrote it ([Release.Hold]).
		switch {
		case run.HeldFor != "":
			tally.held = append(tally.held, run)
		case run.Launch.RefusalOwed != "":
			tally.owed = append(tally.owed, run)
		}
		// WHAT THE BOX CALLS IS SERVED HERE NOW ([CoordinatorOptions.Recovered])
		// unless a newer lease owns the run; an unanswered fence is no such
		// evidence, and what is served acts only while this node holds the seat,
		// just granted.
		if c.recovered != nil && served {
			c.recovered(ctx, run)
		}
	case StatusAnswered:
		// AN ANSWER THE PREVIOUS OWNER RECORDED AND NEVER RESUMED WITH (it
		// stopped or the seat moved between record and resume): on the row, so
		// this node's to drive, the person not asked to resend, the seat's mail
		// waiting behind it. Scheduled, not run: this is the seat's preparation
		// and the mailbox is not open yet.
		counted = run.Status
		c.queueOwed(ctx, run)
		tally.parked++
	case StatusAwaiting, StatusReseed:
		if c.settledUnresumable(ctx, run, Fence{Owner: owner, Epoch: epoch}) {
			tally.abandoned++
			return ""
		}
		// NOTHING IS DONE TO THE RUN (its answer moves it) but the new owner must
		// know the question is open, or the answer arrives at a seat believing
		// nothing waits and runs as an unrelated turn. AND IT IS FENCED TO THIS
		// NODE'S LEASE like every run the seat holds, covering both old-holder cases:
		// one that has NOT noticed writes under its old lease, outranked
		// ([PendingStore.RecordAnswer], [PendingStore.ClaimForResume]); one that HAS
		// writes nothing ([ErrSeatNotHeld]) and the store refuses a record or claim
		// under no lease on an owned row. Left at the old holder's, a node with no seat
		// could answer and claim the run and a parked run stayed at the zero epoch.
		counted = run.Status
		tally.parked++
	}
	// AND A REPLY A DECLINE OWES THE SEAT, whatever the run did since: the
	// decline's write landed and its node stopped before the copies were
	// published (or confirmed). Owed to the seat, so this holder publishes before
	// the mailbox opens, the copies waiting with the seat's mail. A run reaped
	// above publishes what it carries before its record goes
	// ([Coordinator.endRecord]); an answered run's owed resume publishes first
	// ([Coordinator.retryOwed]).
	if len(run.HandBack) > 0 &&
		slices.Contains([]string{StatusRunning, StatusAwaiting, StatusReseed}, run.Status) {
		c.queueOwed(ctx, run)
	}
	return counted
}

// SettleParked ends the seat's parked runs no build will resume
// ([CoordinatorOptions.Unresumable]) and reports whether one still holds its box
// paused for its answer: the one parked state that keeps the seat from other work.
func (c *Coordinator) SettleParked(ctx context.Context, handle string) (pausing bool, err error) {
	fence, held := c.leaseOf(handle)
	if !held {
		return false, notHeld(ctx, handle, "", "settling its parked runs")
	}
	runs, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		return false, fmt.Errorf("sandbox: listing seat %s's parked runs: %w", handle, err)
	}
	settled := false
	for _, run := range runs {
		if !run.WaitsOn(run.LaunchID) {
			continue
		}
		if c.settledUnresumable(ctx, run, fence) {
			settled = true
			continue
		}
		pausing = pausing || run.Status == StatusAwaiting
	}
	if settled {
		c.syncSeat(ctx, handle)
	}
	return pausing, nil
}

// settledUnresumable ends a parked run no build will resume
// ([CoordinatorOptions.Unresumable]) while it is still parked, and reports
// whether it is over: ended here, kept for this node's retry, or ended by another.
func (c *Coordinator) settledUnresumable(ctx context.Context, run PendingRun, fence Fence) bool {
	if c.unresumable == nil {
		return false
	}
	u, err := c.unresumable(ctx, run)
	if err != nil {
		log.WarnContext(ctx, "sandbox_parked_run_unjudged", "turn_id", run.TurnID,
			"agent", run.AgentHandle, "error", err.Error(),
			"detail", "whether any build can resume this parked run could not be read, so it "+
				"waits for its answer as before and is judged again at its next resume")
		return false
	}
	if u == nil {
		return false
	}
	decided, err := c.endRecord(ctx, run, ending{
		fence: fence, whileIn: Awaiting, launch: run.LaunchID, reclaim: true, note: u.note(),
	})
	if err != nil {
		log.WarnContext(ctx, "sandbox_unresumable_run_kept", "turn_id", run.TurnID,
			"reason", u.Reason, "error", err.Error(),
			"detail", "the parked run no build can resume is not ended yet; this node "+
				"finishes the ending, and the seat's mail waits behind it")
		return true
	}
	if decided != nil {
		log.InfoContext(ctx, "sandbox_unresumable_run_ended", "turn_id", run.TurnID,
			"agent", run.AgentHandle, "reason", u.Reason, "status", run.Status)
		return true
	}
	// NOT ENDED UNDER THIS LICENSE: the run moved on since the listing (an answer
	// recorded, say), whose resume judges it again.
	return false
}

// fenceRun moves a run this recovery takes over (running or parked) to this node's
// lease and reads it again, for [Coordinator.recoverRun]. It reports the run as the
// fence found it, whether its box's calls may be served here (false for a newer
// lease's), and whether it is gone. READ AGAIN, BECAUSE THE LISTING IS NOT THE ROW THE
// FENCE FENCED: an ending the last holder decides between listing and fence (a
// person's stop) lands under its old lease and, after the fence, its retry is
// outranked ([PendingStore.DecideEnding]), so the fenced row alone tells it; taken
// over as listed, a stopped job ran on and the seat stayed held. A RUN GONE BEFORE
// THE FENCE is not taken over either (as listed it was counted as holding with
// nothing to recount), so a fence that does not land reads too. A FENCE OR READ THAT
// FAILS LEAVES THE RUN AS LISTED, as the reap does ([Coordinator.fenceClaim]): an
// ending decided then waits for the next holder.
func (c *Coordinator) fenceRun(ctx context.Context, run PendingRun, owner string, epoch int64,
) (latest PendingRun, served, gone bool) {
	won, err := c.pending.ClaimOwnership(ctx, run.TurnID, owner, epoch)
	switch {
	case err != nil:
		log.WarnContext(ctx, "sandbox_ownership_claim_failed",
			"turn_id", run.TurnID, "error", err.Error(),
			"detail", "the run could not be fenced to this node's lease; it is taken over as listed")
		return run, true, false
	case !won:
		// NOT FENCED: gone since the listing (not taken over), or a NEWER LEASE owns
		// it (taken over as listed, its box's calls not served here, as is a run
		// this read cannot tell about).
		if _, found, readErr := c.pending.Get(ctx, run.TurnID); readErr == nil && !found {
			return PendingRun{}, false, true
		}
		return run, false, false
	}
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		log.WarnContext(ctx, "sandbox_recovery_read_failed",
			"turn_id", run.TurnID, "error", err.Error(),
			"detail", "the fenced run could not be read again; it is taken over as listed")
		run.Owner, run.OwnerEpoch = owner, epoch
		return run, true, false
	case !found:
		// FINISHED SINCE THE LISTING, by whoever ended it.
		return PendingRun{}, false, true
	}
	return latest, true, false
}

// reapTail ends a tail the last holder abandoned (a launching row or a claim) and
// announces it; or, for a claim whose node stopped before its turn took the person's
// answer, REVIVES it ([PendingStore.ReviveAnswer], fenced to this lease in one write)
// and reports the row for takeover as the answered run. A recorded answer's delivery
// was spent at the record, so only the row brings it to the run: a claim that never
// took it ([RecordedAnswer.TakenAt]) never used it; reaping lost the run it was for.
// ONLY WHILE IT CAN BE RESUMED ([Coordinator.revivable]); past that the run is reaped
// and the answer let go to the seat. A turn that took it and died used it: reaped as
// spent ([CoordinatorOptions.Spent]). TAKE AND REVIVAL ARE EXCLUSIVE IN THE STORE
// ([ErrAnswerTaken]). An unconfirmed revival is RETRIED ([Coordinator.oweRevival]),
// never a reason to end the run. FENCED FIRST and READ AGAIN
// ([PendingStore.ClaimOwnership]); an unfenceable row is reaped or revived as listed.
func (c *Coordinator) reapTail(ctx context.Context, run PendingRun, owner string, epoch int64,
) (*PendingRun, reaped) {
	fence := Fence{Owner: owner, Epoch: epoch}
	if run.Status == StatusResumed && c.liveHere(run.TurnID) {
		// NOT A TAIL ANYBODY ABANDONED: this node's own attempt holds the claim
		// ([errReapLive]).
		log.ErrorContext(ctx, "sandbox_reap_refused_live_claim",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "launch_id", run.LaunchID,
			"error", errReapLive.Error(),
			"detail", "the claim is left to this node's attempt that holds it; the seat's recovery gate "+
				"should have kept the pass and the claim apart, and did not")
		return nil, reapLive
	}
	if run.Status == StatusResumed {
		fenced, still, decided := c.fenceClaim(ctx, run, owner, epoch)
		if !decided {
			return nil, reapLeft
		}
		if still == nil {
			return &fenced, reapMoved
		}
		run = *still
	}
	detail := "the node that owned this seat stopped mid-run, so its turn cannot be continued by the " +
		"seat's new owner"
	if moved, did, handled := c.reviveWaitOver(ctx, run, fence); handled {
		return moved, did
	}
	if run.Status == StatusResumed && run.Answer != nil && !run.Answer.Taken() {
		why := c.revivable(run)
		if why == "" {
			answered, took, err := c.reviveAnswer(ctx, run, fence)
			switch {
			case err == nil && answered != nil:
				return answered, reapRevived
			case err == nil:
				// THE CLAIM MOVED between read and revival (given back or ended):
				// taken over as what it is now.
				return c.rereadClaim(ctx, run, fence)
			case took != nil:
				// A TURN TOOK THE ANSWER FIRST: used, so the run is reaped as spent.
				run = *took
			default:
				c.oweRevival(ctx, run, fence, err)
				return nil, reapOwed
			}
		} else {
			log.WarnContext(ctx, "sandbox_answer_not_revived",
				"turn_id", run.TurnID, "agent", run.AgentHandle, "reason", why,
				"lost_claims", run.Answer.LostClaims)
			detail = "the node holding this seat stopped while resuming the run with a person's " +
				"answer, and " + why + ", so the run is not resumed again"
		}
	}
	taken := run.Answer != nil && run.Answer.Taken()
	if taken {
		// THE TURN TOOK THE REPLY AND DIED WITH IT: spent, so the carrying delivery
		// (unacknowledged if taken inline, or kept for a refusal) is dropped when it
		// comes round to this holder.
		c.spendTaken(ctx, run)
	}
	// Fenced on the lease this node just took, so a record a newer owner claimed
	// is left to it, neither ended nor announced here. ANNOUNCED like the other
	// ways a run is lost, by the ending itself: a turn that died with the previous
	// owner must be visible, not inferred from a record quietly leaving the board.
	decided, ours := c.finish(ctx, run, ending{
		fence: fence,
		note:  &failureNote{reason: types.ExecutorFailureAbandoned, detail: detail},
	})
	if !ours {
		return nil, reapLeft
	}
	// LOGGED ONCE THE ENDING IS DECIDED: what an operator searches for (did the
	// answer go back to the seat?) is the decision's to settle
	// ([RecordedEnding.Returned]); before it a line could only say what the row held
	// (answer_held, answer_taken: a held untaken answer returns, a taken one is held
	// too, and one an earlier decline owes goes back as well). answer_handed_back is
	// the field to search; ending_decided false is an unconfirmed decision whose
	// retry decides it and whose announcement says whether it went back.
	log.WarnContext(ctx, "sandbox_abandoned_tail_reaped",
		"turn_id", run.TurnID, "agent", run.AgentHandle,
		"sandbox_id", run.SandboxID, "status", run.Status,
		"answer_held", run.Answer != nil, "answer_taken", taken,
		"answer_handed_back", decided != nil && decided.Returned,
		"ending_decided", decided != nil)
	return nil, reapEnded
}

// MaxAnswerRevivals is how many claims of one recorded answer may die before their
// turn took it and still be given back to the run ([Coordinator.reapTail]); the
// next is reaped and the answer handed to the seat.
//
// A DYING CLAIM IS A NODE THAT STOPPED between claim and turn (deploy, crash, moved
// lease), a sub-second window: one loss is bad luck, three is a resume killing its
// node before its turn (an allocation the panic guard cannot catch), each revival
// handing it to the next holder with the seat's mail held. Three bounds that to
// three handoffs (minutes at the 45 s lease TTL) while a run meeting two deploys
// resumes. Counted on the row ([RecordedAnswer.LostClaims]) since each node's own
// attempt count ([MaxAnswerAttempts]) resets with the event counted.
const MaxAnswerRevivals = 3

// revivable reports why a dead claim's recorded answer is NOT given back to its
// run, or "" where it is.
//
// TWO BOUNDS, kept on the row since a stopping node resets its counts: claims died
// ([MaxAnswerRevivals]), and time since the first against the run's awaiting window
// ([answerWindow], `pause_ttl_seconds`), past which a resume dying with its node is
// not worth waiting out. NOT THE BOX (the claim never touched it; a gone box
// re-seeds from the pushed branch as any answered run does), NOR THE PARKED STATE
// (a claim is only taken on a run having it; the resume's own refusal,
// [types.ExecutorFailureNoExecuteState], hands the answer back anyway).
func (c *Coordinator) revivable(run PendingRun) string {
	return c.revivableAfter(run, "this answer", run.Answer.LostClaims, run.Answer.FirstLostAt)
}

// revivableAfter is [Coordinator.revivable] on a claim's own counts: lost claims
// of what (the claim, said as the reason says it) died before their turn took
// it, the first at firstLost.
func (c *Coordinator) revivableAfter(run PendingRun, what string, lost int, firstLost time.Time) string {
	if lost >= MaxAnswerRevivals {
		return fmt.Sprintf("%d claims of %s have now died before their turn took it", lost+1, what)
	}
	if window := answerWindow(run); window > 0 && !firstLost.IsZero() && c.now().Sub(firstLost) >= window {
		return fmt.Sprintf("%s has been owed its resume for %s since the first claim of it "+
			"died, past the run's pause_ttl_seconds", what, c.now().Sub(firstLost).Round(time.Second))
	}
	return ""
}

// reviveAnswer gives a dead claim back to its recorded answer under this node's
// lease ([PendingStore.ReviveAnswer]). It reports the run as revived; or, where a
// turn took the answer first, the row as it stands, to be reaped as spent; or
// neither, for a claim that moved under it; or the store's failure.
func (c *Coordinator) reviveAnswer(ctx context.Context, run PendingRun, fence Fence,
) (revived, took *PendingRun, err error) {
	written, won, err := c.pending.ReviveAnswer(ctx, run.TurnID, Revival{
		Launch: run.LaunchID, Answer: run.Answer.EventIDs, Fence: fence,
		// A CLAIM WHOSE NODE STOPPED, so counted ([MaxAnswerRevivals]).
		Lost: true,
	})
	switch {
	case errors.Is(err, ErrAnswerTaken):
		return nil, &written, err
	case err != nil:
		return nil, nil, err
	case !won:
		return nil, nil, nil
	}
	log.InfoContext(ctx, "sandbox_answer_revived",
		"turn_id", written.TurnID, "agent", written.AgentHandle,
		"lost_claims", written.Answer.LostClaims, "first_lost_at", written.Answer.FirstLostAt,
		"detail", "the node holding this seat stopped before the resumed turn took the person's "+
			"answer; the answer is given back to the run, and this node resumes it")
	return &written, nil, nil
}

// rereadClaim is a dead claim's row read again after its revival was refused (it
// moved under the reap) and reported as it is now: given back or ended, to be taken
// over as that; still the claim or unreadable, for the revival to be retried
// ([Coordinator.oweRevival]); or nothing where a newer lease owns it or it is gone.
func (c *Coordinator) rereadClaim(ctx context.Context, run PendingRun, fence Fence) (*PendingRun, reaped) {
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		c.oweRevival(ctx, run, fence, fmt.Errorf("sandbox: reading run %s again after its "+
			"revival was refused: %w", run.TurnID, err))
		return nil, reapOwed
	case !found, outranked(latest, fence):
		return nil, reapLeft
	case latest.Status == StatusResumed && latest.Ending == nil:
		c.oweRevival(ctx, latest, fence, fmt.Errorf("sandbox: run %s's claim changed under its "+
			"revival", run.TurnID))
		return nil, reapOwed
	}
	return &latest, reapMoved
}

// fenceClaim moves a claimed row to this node's lease and reads it again, for
// [Coordinator.reapTail]. It reports the row to reap (still; nil where it is no
// longer a claim, fenced then being the row as it stands) and whether the reap
// should go on (false for a row a newer lease owns or that is gone). A FENCE OR
// READ THAT FAILS LEAVES THE ROW AS LISTED and the reap goes on: the ending's own
// reads decide what the row holds, so an untaken reply is handed back anyway.
func (c *Coordinator) fenceClaim(ctx context.Context, run PendingRun, owner string, epoch int64,
) (fenced PendingRun, still *PendingRun, decided bool) {
	won, err := c.pending.ClaimOwnership(ctx, run.TurnID, owner, epoch)
	if err != nil {
		log.WarnContext(ctx, "sandbox_ownership_claim_failed",
			"turn_id", run.TurnID, "error", err.Error(),
			"detail", "the claim could not be fenced before its reap; it is reaped as listed, "+
				"and a reply on it no turn took is handed back")
		return PendingRun{}, &run, true
	}
	if !won {
		// A NEWER LEASE owns the run: its reap, not ours.
		return PendingRun{}, nil, false
	}
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		log.WarnContext(ctx, "sandbox_reap_read_failed",
			"turn_id", run.TurnID, "error", err.Error(),
			"detail", "the fenced claim could not be read again; it is reaped as listed, and "+
				"a reply on it no turn took is handed back")
		run.Owner, run.OwnerEpoch = owner, epoch
		return PendingRun{}, &run, true
	case !found:
		return PendingRun{}, nil, false
	case latest.Status != StatusResumed && latest.Status != StatusLaunching:
		return latest, nil, true
	}
	return PendingRun{}, &latest, true
}
