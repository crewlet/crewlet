package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// resumeAndSettle re-enters the parked turn, then settles the box; seen is what
// was signalled before the turn's admission was asked
// ([Coordinator.signalsSeen]). A turn that relaunched leaves the box to its new
// job; otherwise the ending is decided and the box torn down, the record going
// with the ending's last step, here or by this node's retry
// ([Coordinator.finishEnding]). It reports what it left the delivery (still
// owed, gone, or spent), since only one of those is a retry. A recorded answer
// the run ends without goes back to the seat through the row
// ([Coordinator.letGoAnswer]); a reply a turn took ([ResumeRequest.Begin]) is
// spent. See the package doc, "Resuming and settling".
func (c *Coordinator) resumeAndSettle(adm admitted, run PendingRun,
	answer string, success bool, trigger *events.Event, outcome runOutcome, seen map[signal]uint64,
) (AnswerDisposition, error) {
	// UNDER THE ADMISSION, the only way here ([admitted]).
	ctx := adm.ctx
	// WHAT THIS ATTEMPT SAYS OF A WAIT FOR THE OPERATOR: a failure only an
	// operator clears counts the run in [Coordinator.ResumesBlocked] once its
	// hand-back lands; every other outcome takes it out as the attempt returns.
	blockedOn := ""
	defer func() {
		if blockedOn == "" {
			c.unblock(run.TurnID)
		}
	}()
	if len(run.ExecuteState) == 0 {
		// No parked turn to resume, so the run cannot be judged. Unreachable
		// through any live path ([StatusLaunching] until the parked state is
		// written, and launching runs are not claimable), and kept as the
		// assertion that the state does its job: a claimed run without one is a
		// row this build did not write, failed as no_execute_state, not resumed
		// into nothing.
		log.WarnContext(ctx, "sandbox_resume_no_execute_state",
			"turn_id", run.TurnID, "claimed_from", run.ClaimedFrom,
			"detail", "the row carried no parked turn state; the turn "+
				"cannot be resumed and the run is failed")
		if !c.settleFailed(ctx, run, types.ExecutorFailureNoExecuteState,
			"the run record carried none of its turn's parked state, so the turn that "+
				"started it cannot be continued") {
			return AnswerNotMine, errRunMovedOn
		}
		// TERMINALLY GONE, so nothing is owed the delivery: requeueing would circle
		// a deleted run, acking would swallow a person's message for a turn that no
		// longer exists. A recorded reply is not lost: the ending handed it back
		// through the row.
		return AnswerNotMine, nil
	}
	// THE SEAT STAYS HELD THROUGH THE RESUMED TURN. The claim holds the row in
	// [StatusResumed], a [Holding] status, counted so while the row stands; every
	// branch below recounts from the store after the turn, and a row outliving the
	// turn by the end it owes the executor log is counted until its retry deletes
	// it, the seat's mail held behind the end too. Giving the count back before the
	// resumer ran lifted the inbox hold for the whole turn and ran the next mail as
	// a second turn beside it (seathold.go).
	//
	// WHETHER THE TURN BEGAN, reported by the resumer calling Begin
	// ([ResumeRequest.Begin]); atomic since it need not call on this goroutine.
	var began atomic.Bool

	// A PANIC ENDS THE CLAIM, as an abandoned resume does, before going up. The
	// queue's handler recovers it, so a left claim kept the row `resumed`, holding
	// the seat with no mail until a restart or move. Ended on [ErrResumeAbandoned]'s
	// terms: announced as lost where the turn never began (a recorded reply goes
	// back through the row), settled silently where it did, since it may have
	// acted and a retry must not re-enter it.
	//
	// AND THE TURN'S FRAME RUNS until the resumer returns, which an ending decided
	// meanwhile waits on before deciding the turn's end ([Coordinator.framing]);
	// done once whichever way it returns.
	frameDone := sync.OnceFunc(func() { c.frameEnds(run.TurnID) })
	defer func(ctx context.Context) {
		if p := recover(); p != nil {
			frameDone()
			var note *failureNote
			if !began.Load() {
				note = &failureNote{reason: types.ExecutorFailureResumeBroken, detail: resumeBrokenDetail}
			}
			// DETACHED, bounded: the panic may be the cancellation's own and a
			// dead context writes nothing.
			settle, cancel := c.detachedRecord(ctx)
			defer cancel()
			c.settleAbandoned(settle, run, note)
			panic(p)
		}
	}(ctx)
	begin := func(ctx context.Context, carry ResumeCarry) error {
		switch {
		case claimedAnswer(run):
			// THE TURN TAKES THE ANSWER on the row, or does not run, under the
			// CLAIMANT's lease ([PendingStore.ClaimForResume]).
			taken, takeErr := c.pending.TakeAnswer(ctx, run.TurnID, run.LaunchID, fenceOf(run))
			if takeErr != nil {
				return fmt.Errorf("sandbox: recording that run %s's turn took its answer: %w",
					run.TurnID, takeErr)
			}
			if !taken {
				return fmt.Errorf("sandbox: run %s is no longer the claim that holds its answer: "+
					"the answer was let go of, or the seat's next holder fenced the row", run.TurnID)
			}
			// AND ITS DELIVERY IS SPENT AT THE TAKE, on every route ([CoordinatorOptions.Spent]):
			// the inline attempt holds it unacknowledged through the whole turn, and a
			// node stopping mid-turn leaves it for the next holder, who finds the run
			// reaped and nothing to recognise it by; spent only after the turn, it ran
			// as a second turn. NOT A REFUSAL'S, which carries no copy of its reply: the
			// inbox's delivery is the only one, and a turn that broke before reaching
			// anybody gives the take back, so it is spent once the turn that took it
			// keeps it ([Coordinator.spendRefused]).
			c.spend(ctx, run.AgentHandle, *run.Answer)
		case claimedWaitOver(run):
			// THE TURN TAKES THE ENGINE'S CARRYING ON, as an answer's: a claim
			// stopping before it is revived to waiting, one after it is used.
			taken, takeErr := c.pending.TakeWaitOver(ctx, run.TurnID, run.LaunchID, fenceOf(run))
			if takeErr != nil {
				return fmt.Errorf("sandbox: recording that run %s's turn carried it on: %w",
					run.TurnID, takeErr)
			}
			if !taken {
				return fmt.Errorf("sandbox: run %s is no longer the claim that carries it on: the seat's "+
					"next holder fenced the row", run.TurnID)
			}
		default:
			c.markResumeBegun(ctx, run, carry)
		}
		began.Store(true)
		return nil
	}

	// STRAIGHT TO THE RESUMER ([NewCoordinator] refuses to build without it): "this
	// node cannot resume this run" is its own answer ([ErrResumeUnavailable]), down
	// the failure path. AND THE RUN'S CALLS, read from its log here, after the box
	// was stopped or read, missing only a call still running when the bridge session
	// ended (internal/api/mcpbridge, "An opening ends without waiting for its
	// calls"). Never "no calls" for an unreadable log: that is a failed resume. Read
	// INSIDE the frame so a concurrent ending waits ([Coordinator.framing]). WITH THE
	// CALL A PERSON APPROVED that the log is still owed ([BridgeLog.withHeld]), which
	// a later attempt finds on the row alone.
	c.frameBegins(run.TurnID)
	calls, err := c.pending.BridgeLog(ctx, run)
	if err == nil {
		// WHAT THE READ SAW, before the held call is added
		// ([ResumeRequest.BridgedRead]).
		read := calls.Read()
		calls = calls.withHeld(run)
		// AND THE TURN'S LEADS, same frame ([Coordinator.leadsAt]), never "none"
		// for a part it could not read.
		leads := c.leadsAt(ctx, run)
		err = c.resume.Resume(ctx, ResumeRequest{
			Run: run, Bridged: calls, BridgedRead: read, Leads: leads, Answer: answer, Success: success,
			Trigger:       trigger,
			DeliveredRefs: outcome.DeliveredRefs, DeliveredRefsElided: outcome.DeliveredRefsElided,
			Pushed: outcome.Pushed, PushedElided: outcome.PushedElided, PushedUnread: outcome.PushedUnread,
			InputTokens: outcome.InputTokens, OutputTokens: outcome.OutputTokens,
			CacheReadTokens: outcome.CacheReadTokens, CacheWriteTokens: outcome.CacheWriteTokens,
			Engine: outcome.Engine, Ended: outcome.Ended, Asked: outcome.Asked, Lost: outcome.Lost,
			Begin: begin,
		})
	}
	frameDone()
	if err != nil {
		if errors.Is(err, ErrResumeAbandoned) && !began.Load() {
			// BROKEN BEFORE ITS TURN BEGAN (a panic re-entering the conversation,
			// say): not retried, as every abandoned resume (same bytes, same
			// defect), but nothing ran, so a recorded reply goes back through the
			// row. AND ANNOUNCED, like every run lost without its turn: no turn
			// published a completion, so the ending is the only account (else the
			// turn parked on it read as parked for good).
			note := abandonedNote(err)
			log.ErrorContext(ctx, "sandbox_resume_abandoned_before_turn",
				"turn_id", run.TurnID, "reason", note.reason, "error", err.Error(),
				"detail", "the resume could not begin the turn it would continue; the run "+
					"is ended and announced rather than retried, and a person's reply that "+
					"drove the resume goes back to the seat rather than being spent")
			if !c.settleAbandoned(ctx, run, note) {
				// NOT THIS NODE'S TO END after all: the run moved on (the next
				// holder fenced and revived it), and its fate, and a reply's, is
				// that holder's.
				return AnswerNotMine, errRunMovedOn
			}
			return AnswerNotMine, nil
		}
		if errors.Is(err, ErrResumeAbandoned) {
			// THE CLAIM IS NEVER GIVEN BACK: a retry could not cure any way a resumer
			// marks a turn so ([ErrResumeAbandoned]); it would re-enter the same
			// parked turn and repeat its writes, hit the same panic or refusal, run
			// past a person's stop, or find no run to collect. The run is SETTLED
			// instead, so a redelivery finds no record to claim (left claimed,
			// nothing settles it, since recovery runs only on a seat handoff, and the
			// busy seat would park every message beside a box nobody collects). The
			// busy count is RECOUNTED, not re-marked: the turn may have relaunched
			// before breaking, counting a job this settle ended. Nothing is
			// announced: the turn did resume and published its own failed
			// completion, and the reply it took was spent at the take.
			log.ErrorContext(ctx, "sandbox_resume_abandoned",
				"turn_id", run.TurnID, "error", err.Error(),
				"detail", "the run is settled rather than un-claimed, so the completion is "+
					"not redelivered into a conversation a retry must not re-enter")
			c.settleAbandoned(ctx, run, nil)
			// THE TURN RAN and must not run again, so what drove it is SPENT (the one
			// branch deliberately keeping the claim): the ordinary route would run
			// a second turn on a message already taken up, a refusal it took
			// included ([Coordinator.spendRefused]).
			c.spendRefused(ctx, run)
			return AnswerConsumed, nil
		}
		// PAST THE TURN'S DEADLINE NOTHING WAITS AND NOTHING IS RETRIED: a
		// completion's resume that failed or met a waiting condition after the
		// wall-clock cap is the turn's last, the run ended and the seat freed
		// ([Coordinator.endAtDeadline]). Handed back, a health check whose
		// ten-minute cap passed during a model's weekly limit was re-announced on
		// every poll for twenty-seven hours with the seat's next three fires queued
		// behind it. An answer's resume is bounded by its own series
		// ([MaxAnswerAttempts]).
		if !resumedFromWait(run) && run.PastDeadline(c.now()) {
			c.endAtDeadline(ctx, run, err.Error(), outcome.DeliveredRefs, outcome.DeliveredRefsElided)
			return AnswerConsumed, nil
		}
		// A RESUME ITS SEAT'S CONDITIONS REFUSED IS HELD, not retried
		// ([ErrResumeHeld]), A COMPLETION'S only: an answer's resume is its own
		// series, asking the seat's conditions before every attempt
		// ([Admission]), reverted below as the wait it is ([waitingRefusal]),
		// uncharged.
		var held *HeldError
		if errors.As(err, &held) && !resumedFromWait(run) {
			// The claim's count moves with the hand-back once its release lands
			// ([Coordinator.unclaimOwing]). seen is what was signalled before
			// admission, not at return: a signal after the refusal found no hold to
			// lift and would leave the job waiting for the hold's instant. See the
			// package doc, "Holding a refused resume".
			hold, unclaimErr := c.unclaimHeld(ctx, run, held.Refusal)
			if unclaimErr != nil {
				//nolint:nilerr // The run was ended in its place, as on
				// the retry's path below: nothing is left to come back to.
				return AnswerNotMine, nil
			}
			// THE COMPLETION IS SPENT: the hold is on the row and the poll announces
			// again when it lifts; where no poll can (the clock on its box's host
			// running) the caller hands the delivery back (renew.go). AND KEPT HERE,
			// unheld, for a re-fire at the hold's instant and the lifting signal,
			// since the poll reads through the host's store, whose silence is
			// unbounded. The release numbered its hold next after the claimed row's
			// ([CoordStore.ReleaseClaim]).
			c.keepHeldOnRow(run, trigger, hold, run.Holds+1, seen, false)
			return AnswerConsumed, nil
		}
		// Un-claim so a retry can win the flip again, else the redelivered
		// completion is refused and the conversation lost in resumed. A resume
		// refused on a named condition is a wait, said as one, handed back unheld
		// too: only this node knows its models are at their limit, so it is held
		// here until the refusal's instant ([Coordinator.keepRefused]), never on
		// the row, which would silence every waiter of the next holder. See the
		// package doc, "Holding a refused resume".
		refusal, refused := waitingRefusal(err)
		var failed failure
		if refused {
			log.InfoContext(ctx, "sandbox_resume_refused",
				"turn_id", run.TurnID, "revert_to", claimedFrom(run),
				"condition", string(refusal.Condition), "reason", refusal.Reason,
				"detail", "the resumed turn met a condition that ends by waiting; the run is "+
					"reverted and resumed once the condition clears")
		} else {
			log.ErrorContext(ctx, "sandbox_resume_failed",
				"turn_id", run.TurnID, "revert_to", claimedFrom(run),
				"attempt", run.Launch.ResumeFailures+1, "error", err.Error())
			// COUNTED ONCE THE HAND-BACK BELOW LANDS, not as the failure is met or
			// the attempt returns: a hand-back that did not land ended the run in
			// its place or found it moved on, and counted either side of that, an
			// over run held the alarm until its seat moved ([blockedResume]).
			blockedOn = c.blockedBy(run, err)
			failed.blocked = blockedOn
		}
		// A FAILURE IS COUNTED AND BOUNDED; a wait is not, nor a node's own
		// failure or one another node can get past ([Coordinator.resumeGivenUp]).
		// A completion's only, for the deadline's reason.
		if !refused && !claimedAnswer(run) && c.runsOwnFailure(ctx, run, err) {
			now := c.now()
			if c.resumeGivenUp(ctx, run, now, err) {
				return AnswerConsumed, nil
			}
			failed.resumeFailed = now
		}
		// AND THE WINDOW AN ANSWER'S RELAUNCH WAS REFUSED BELOW ITS FLOOR IN, onto
		// the job's record ([Release.RelaunchRefusedIn]): the answer's wait is its
		// series' in this node's memory, but its next attempt's relaunch (here or
		// on the next holder) reads the row and waits for its brief's room no
		// longer than that window.
		if unclaimErr := c.unclaimAt(ctx, run, resumeUnrevertedDetail, failed, nil,
			refusal.Window); unclaimErr != nil {
			//nolint:nilerr // Deliberate, and the same answer the
			// acted-and-broke branch above gives: the run has been settled, so
			// nothing is sent back for a retry to claim and nothing is owed the
			// delivery (ended and announced, a recorded reply gone back by its row).
			return AnswerNotMine, nil
		}
		// THE RUN IS BACK WHERE THE CLAIM FOUND IT (on the answer route, awaiting
		// this reply), so the delivery is still owed and comes back, not worked as
		// an ordinary message.
		return AnswerDeferred, err
	}
	// THE TURN RAN, so a refusal it took is kept and the inbox's delivery is spent
	// now ([Coordinator.spendRefused]): the run was told, and a copy is a reply it
	// already has.
	c.spendRefused(ctx, run)

	latest, err := c.current(ctx, run)
	if err != nil {
		// THE ACTED BRANCH'S ANSWER, for its reason: the resume is over, so a row
		// left claimed is picked up by nothing; the store decides whether this is
		// still the claimed run ([Coordinator.settleClaimed]).
		c.settleClaimed(ctx, run, err, nil)
		c.syncSeat(ctx, run.AgentHandle)
		// THE TURN RAN; only the settle after it could not be decided. What drove
		// it is spent.
		return AnswerConsumed, nil
	}
	if latest.LaunchID != run.LaunchID && slices.Contains(Active, latest.Status) {
		// The resumed turn RELAUNCHED its executor: a new job owns the row and
		// held the seat as it opened. The box this run was collected from is the
		// new job's too or already gone (the relaunch continued in it, or reclaimed
		// it through its own cell, [Coordinator.Launch]), so nothing of this run's
		// is left to reclaim.
		//
		// TOLD BY ITS LAUNCH, since its status can be any live one by now
		// (launching, running, claimed by its own completion, parked on its own
		// question). Reading for running alone tore a second launch's box down
		// under it; running or launching tore down a next job's question checkout
		// and ended the run under it. An already-over relaunch (one that could not
		// start) settles like the rest.
		log.InfoContext(ctx, "sandbox_relaunched_in_turn",
			"turn_id", run.TurnID, "status", latest.Status,
			"sandbox_id", latest.SandboxID, "continued", latest.SandboxID == run.SandboxID)
		// RECOUNTED, the store under the counts: the claim and relaunch are one
		// row, counted once as the claim the relaunch opened on (countwrite.go), so
		// the recount moves nothing unless the job moved since.
		c.syncSeat(ctx, run.AgentHandle)
		return AnswerConsumed, nil
	}
	c.finish(ctx, latest, ending{fence: fenceOf(run)})
	// RECOUNTED, not assumed free. Where the ending owed nothing more the store has
	// no record of the run, so the recount keeps only the seat's other live runs and
	// the inbox hold lifts once the resumed turn is over. Where the record OWES THE
	// TURN'S END, the ending is decided here and finished by this node's retry,
	// armed at once on its own goroutine ([errAskAway]): a capture still being kept
	// and the log's write after it are not this frame's to wait on. The record
	// outlives the resume, its ending keeping the inbox held
	// ([Coordinator.oweEndingAfter]), until the retry has deleted it, recounted and
	// settled ([Coordinator.retryOwed]). That retry may be under way or done as
	// this recount reads: a delete and every recount written move the seat
	// ([Coordinator.moved]), so an older listing is read again.
	c.syncSeat(ctx, run.AgentHandle)
	// The ordinary ending: the turn came back and the run is finished.
	return AnswerConsumed, nil
}

// leadsAt is the turn's leads beside run's row as its resume reads them
// ([PendingStore.Leads]): where the store cannot give them, every lead the row
// counts is said as not kept, never none ([PendingRun.LeadsAppended]), and the
// read marked unread ([TurnLeads.Unread]), so the relaunch's job hands the
// marked leads on by number for a later read to tell. A resume is not refused for
// it, unlike an unreadable log: a lead told again is one the session may already
// show, and the brief says how many it cannot tell.
func (c *Coordinator) leadsAt(ctx context.Context, run PendingRun) TurnLeads {
	leads, err := c.pending.Leads(ctx, run)
	if err != nil {
		log.WarnContext(ctx, "sandbox_leads_part_unread", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"not_kept", run.LeadsAppended, "error", err.Error(),
			"detail", "every lead the run's record counts is told as not kept by the relaunch this resume "+
				"builds, rather than told again; the job that relaunch opens marks the ones this job's "+
				"record marks, so a later resume that reads them tells them")
		return TurnLeads{Cut: run.LeadsAppended, Unread: true}
	}
	return leads
}

// markResumeBegun records on a claim of a collected job what the resuming turn
// does with the job's round ([LaunchRecord.ResumeCarry]), as it begins
// ([ResumeRequest.Begin]), under the claimant's lease. An unsaid carry is none.
//
// BEST EFFORT, AND BOUNDED ([resumeCarryRecordBudget] plus the row's crossing,
// [Coordinator.rowContext]): nothing the turn does reads it, only readers of
// whether a relaunch would continue the job's session, so an untaken record is
// logged and read as not said, undecided either way.
func (c *Coordinator) markResumeBegun(ctx context.Context, run PendingRun, carry ResumeCarry) {
	if !carry.Valid() {
		return
	}
	ctx, cancel := c.rowContext(ctx, resumeCarryRecordBudget)
	defer cancel()
	if _, err := c.pending.MarkResumeBegun(ctx, run.TurnID, run.LaunchID, fenceOf(run), carry); err != nil {
		log.WarnContext(ctx, "sandbox_resume_carry_unrecorded", "turn_id", run.TurnID,
			"carry", string(carry), "error", err.Error(),
			"detail", "what the resumed turn does with its job's round was not recorded on the run; while "+
				"the turn runs, whether a relaunch continues the job's session reads as not said")
	}
}

// resumeCarryRecordBudget bounds the write recording what a resumed turn does
// with its job's round ([Coordinator.markResumeBegun]), beside the row's crossing
// time ([RowBudget]): FIVE SECONDS, like the session-start write
// (internal/engine's sessionStartRecordBudget): one compare-and-swap of one row
// a healthy store answers in milliseconds, and a store silent for five is in an
// outage this write waits out no better. On the resume's path before its turn
// runs, so a slow store holds the turn back by no more than that plus the row's
// bytes crossing (none in the store's process; up to thirteen minutes across the
// slowest allowed link for a row at its bound). The five alone cut a megabyte
// row while its bytes still crossed and the carry went unrecorded
// (crewlet/crewlet#1076).
const resumeCarryRecordBudget = 5 * time.Second

// claimedAnswer reports whether a claimed run carries a RECORDED answer its
// claim was taken for: the chat route's, which lives on the row, not a delivery.
func claimedAnswer(run PendingRun) bool {
	return run.ClaimedFrom == StatusAnswered && run.Answer != nil
}

// resumedFromWait reports whether a claimed run is carried on from waiting, by
// its recorded answer or by the engine ([claimedWaitOver]), never a completion:
// a deadline or a budget's hold is not a completion's to end or hold for it.
func resumedFromWait(run PendingRun) bool { return claimedAnswer(run) || claimedWaitOver(run) }

// settleAbandoned ends a run whose resume was abandoned, from its record, or
// while it is still the claim this resume took where that cannot be read
// ([Coordinator.settleClaimed]), and recounts the seat. A recorded reply no turn
// took goes back to the seat ([ErrAnswerOwed]); one the turn took is spent.
// SETTLED EVEN WHEN THE RECORD CANNOT BE READ: settling nothing left the row in
// the claim, the one state nothing recovers from.
//
// note is what the ending announces: nil for a resume whose turn ran (it
// published its own failed completion), the loss for one whose turn never began
// ([resumeBrokenDetail]). Reports whether the ending is this node's
// ([Coordinator.finish]); false is a run that moved on (the next holder fenced
// and revived it), whose fate this node must not announce.
func (c *Coordinator) settleAbandoned(ctx context.Context, run PendingRun, note *failureNote) bool {
	defer c.syncSeat(ctx, run.AgentHandle)
	settle, readErr := c.current(ctx, run)
	if readErr != nil {
		return c.settleClaimed(ctx, run, readErr, note)
	}
	_, ours := c.finish(ctx, settle, ending{fence: fenceOf(run), note: note})
	return ours
}

// spend is [CoordinatorOptions.Spent] for one recorded answer: its deliveries,
// as the answer carries them.
//
// BEFORE AN ANSWER IS LET GO OF, this makes its copies the reply: the original
// coming round after is a reply already handed back. A REFUSAL CARRIES NO
// DELIVERY, so letting one go spends nothing, which sends its reply back the way
// it came: the inbox's delivery comes round as the ordinary message
// ([AnswerRefused]). Spent by id once a turn keeps it ([Coordinator.spendRefused]).
func (c *Coordinator) spend(ctx context.Context, handle string, answer RecordedAnswer) {
	if c.spent == nil {
		return
	}
	if evs := answer.decodedEvents(); len(evs) > 0 {
		c.spent(ctx, handle, evs)
	}
}

// spendRefused is [CoordinatorOptions.Spent] for the refusal run holds, once a turn
// that took it KEEPS it: the deliveries its reply was made of, by the ids it names
// ([AnswerRefusal.EventIDs]), so the inbox's delivery is dropped when it comes
// round instead of running as a turn. KEPT means the turn returned having run,
// broke after it began (never retried), or died holding it ([Coordinator.reapTail]);
// never the take alone, which a turn breaking before reaching anybody gives back
// (the refusal may then be let go, sending the delivery back as an ordinary
// message; spent at the take it was lost). BY ID ALONE, all a refusal keeps; an
// unparseable id is skipped.
func (c *Coordinator) spendRefused(ctx context.Context, run PendingRun) {
	refusal, refused := run.Refused()
	if !refused || c.spent == nil {
		return
	}
	evs := make([]*events.Event, 0, len(refusal.EventIDs))
	for _, id := range refusal.EventIDs {
		if parsed, err := uuid.Parse(id); err == nil {
			evs = append(evs, &events.Event{ID: parsed})
		}
	}
	if len(evs) > 0 {
		c.spent(ctx, run.AgentHandle, evs)
	}
}

// spendTaken is what a turn that TOOK run's answer and died holding it spends:
// the answer's deliveries, or the refusal's ([Coordinator.spendRefused]).
func (c *Coordinator) spendTaken(ctx context.Context, run PendingRun) {
	if run.Answer == nil {
		return
	}
	c.spend(ctx, run.AgentHandle, *run.Answer)
	c.spendRefused(ctx, run)
}
