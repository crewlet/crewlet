package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
)

// A run whose job was lost with the host it ran on, and the round carried on. A
// job cut off by its engine's restart, its box root, a departed node
// ([Waiter.goneAfter]), or a host cut off from the store for the host grace
// ([Coordinator.loseWithHost]) neither finished nor failed. Its round
// is carried on ([ResumeRequest.Lost]), in the same box where it is left, its
// spend read and charged once; at most [MaxRoundLosses] times, since a round
// whose jobs keep being lost takes its host down. See the package doc, "Lost
// jobs".

// MaxRoundLosses is how many of one round's jobs may be lost with their hosts
// and the round still carried on: the next loss ends the turn.
//
// TWO, from what a loss is. One is routine: a deploy, a node drained, a host
// patched and rebooted while a long run worked. Two in one round is a run long
// enough to see a deploy and its rollback, or a drain and the node's return,
// and still plausible on a busy fleet. A third is no longer the fleet moving
// under the run: it is the run bringing its host down, and each further
// relaunch costs every other seat on that node its work in progress too.
const MaxRoundLosses = 2

// KeptAfterLoss reports whether a round whose job was lost for the losses-th
// time — counting that loss — is carried on from the turn's kept session of the
// job lost: on its first loss only. A round lost again is a run that may take
// its host down from the state it holds, so its relaunch starts a session of
// its own rather than replay that state toward the round's limit. The one rule
// the runner's relaunch keeps to (internal/agent/runner's Lost) and every
// reader of whether a relaunch would continue a session reads
// ([ReadContinuityOf]).
func KeptAfterLoss(losses int) bool { return losses <= 1 }

// LossCause is what a lost job was lost to — see [JobLoss].
type LossCause string

const (
	// LossEngineRestarted is a job on the engine host that went with the
	// engine process that last saw it running ([JobLost]): a container or
	// systemd restart, or a host reboot. Its box is still there.
	LossEngineRestarted LossCause = "engine_restarted"
	// LossBoxGone is a running job whose box is gone from where it ran: a
	// box root that did not outlive its node, or a container removed under it.
	LossBoxGone LossCause = "box_gone"
	// LossLaunchNodeGone is a running job whose box was on the host of a node
	// that left the fleet and did not come back within the host grace
	// ([Waiter.goneAfter]).
	LossLaunchNodeGone LossCause = "launch_node_gone"
	// LossLaunchNodeStoreSilent is a job whose box is on the host of a node
	// that answered every collection of it, for the host grace, that its
	// coordination store did not answer its read of the run's row, while the
	// collecting node's store answered ([hostUnsettled]): its box is there,
	// but only that node can read it, and it could not. Recorded by the
	// collection that found it so, on its own claim ([Coordinator.loseWithHost]).
	// A capture that node was keeping stays begun under its held taker lease,
	// so the relaunch reads the turn's row for it ([Coordinator.ContinuedCapture]).
	// A build that does not know it reads it as a cause it does not know,
	// which leaves no box to read and is said in general terms.
	LossLaunchNodeStoreSilent LossCause = "launch_node_store_silent"
)

// LossCauses are the four.
var LossCauses = []LossCause{LossEngineRestarted, LossBoxGone, LossLaunchNodeGone, LossLaunchNodeStoreSilent}

// Valid reports whether this build knows c.
func (c LossCause) Valid() bool { return slices.Contains(LossCauses, c) }

// Why is what happened to a job lost to c, as a clause that follows "when" —
// for the log, the job's record and the run that carries its round on. A cause
// this build does not know is said in the most general terms.
func (c LossCause) Why() string {
	switch c {
	case LossEngineRestarted:
		return "the engine it ran under restarted, which killed it"
	case LossBoxGone:
		return "its box was lost from the host it ran on"
	case LossLaunchNodeGone:
		return "the node it ran on left the fleet for good, taking its box with it"
	case LossLaunchNodeStoreSilent:
		return "the node it ran on could not reach the coordination store for the host grace, " +
			"which left its box unreadable"
	}
	return "it was lost with the host it ran on"
}

// leavesBox reports whether a job lost to c left its box where it was, so what
// it did up to then can still be read off it. A box only a node cut off from
// the coordination store can read ([LossLaunchNodeStoreSilent]) is still where
// it was, but nothing reads it.
func (c LossCause) leavesBox() bool { return c == LossEngineRestarted }

// JobLoss is the verdict that a running job was lost with its host — see
// [PendingRun.Lost].
type JobLoss struct {
	Cause LossCause `json:"cause"`
	// Node is the node whose host the job's box was on
	// ([PendingRun.LaunchNode]): the one node whose verdict about the box
	// counts, recorded only on a row naming the same node.
	Node string `json:"node,omitempty"`
	// At is when the verdict was reached, on its finder's clock.
	At time.Time `json:"at"`
}

// LostRun is what a resume carries a lost job's round on with — see
// [ResumeRequest.Lost].
type LostRun struct {
	Cause LossCause
	// Why is [LossCause.Why] of Cause.
	Why string
	// BoxKept is whether the lost job's box is still where it ran, so a
	// relaunch that continues in it finds the job's working tree as the job
	// left it.
	BoxKept bool
	// Losses is how many of the round's jobs have been lost, this one
	// included, and Limit how many a round is carried on after
	// ([MaxRoundLosses]).
	Losses, Limit int
}

// carryOnLost is a completion's claim of a lost job carried on: its spend read
// off its box where left, charged and published once, and the turn resumed to
// carry the round on ([LostRun]), or parked on a question the job asked. The
// read is asked again like any collection ([Coordinator.collectFailed]); a gone
// box or one past the give-up is carried on without it, and a store's silence
// is not, unless the host's alone for the host grace. One verdict per failure
// ([collectVerdict]). It resumes nothing itself: its caller under the node's
// admission does (ADR-0063); a nil carry with nil error is settled already. See
// the package doc, "Lost jobs".
func (c *Coordinator) carryOnLost(ctx context.Context, run PendingRun) (*lostCarry, error) {
	loss := *run.Lost
	var (
		result Result
		read   bool
	)
	if loss.Cause.leavesBox() {
		boxed, _, err := c.boxResult(ctx, run, BoxReadCollect)
		// ONE VERDICT ON A READ THAT FAILED, at one reading of the clock, and
		// the hand-back below acts on that verdict rather than judging the
		// failure again ([collectVerdict]).
		var verdict collectVerdict
		if err != nil {
			verdict = c.collectGivenUp(run, err, c.now())
		}
		switch {
		case err == nil:
			// THE BOX ANSWERED, which ends the run of failed reads behind
			// this one whatever a later step does: a drain the store does
			// not answer, or that this node cuts short, hands the claim
			// back with that run ended ([Release.BoxAnswered]).
			result, read = boxed, true
			run.BoxAnswered = true
		case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, ErrBoxHostSilent), !verdict.givenUp:
			return nil, c.collectFailedAt(ctx, run, err, verdict)
		default:
			log.WarnContext(ctx, "sandbox_lost_run_unread", "turn_id", run.TurnID,
				"launch_id", run.LaunchID, "error", err.Error(),
				"detail", "what the lost job did could not be read off its box, so its spend "+
					"is not charged; its round is carried on all the same")
		}
	}
	// ITS CALLS STILL IN FLIGHT, waited for on the run's log before the round
	// is carried on from what the job did ([Coordinator.drainCalls]): a lost
	// job's process may be gone, which its runner's lease says, and a call it
	// left running elsewhere is recorded, with what it cost, before the resume
	// reads it. A wait this node cancelled hands the claim back uncounted.
	drained, err := c.drainCalls(ctx, run)
	if err != nil {
		return nil, c.collectFailed(ctx, run, err)
	}
	if drained {
		run = c.rereadSpend(ctx, run)
	}
	// WHAT IT PUSHED, read whether or not its box answered: the push record
	// is Gitlet's, never the box's, so a job lost with its box still
	// has its pushes on the record, and its report says what each became
	// rather than that nothing was recorded ([Coordinator.readPushes]). A
	// lost job did not finish, so no change it asked to open is opened. Held
	// to the record's bound with the rest of the result ([Coordinator.fitText]).
	result, err = c.readPushes(ctx, run, cutOff(result, loss.Cause))
	if err != nil {
		return nil, c.collectFailed(ctx, run, err)
	}
	result = c.fitText(ctx, run, result)
	var outcome runOutcome
	if read {
		// THE BOX ANSWERED, which ends any run of failed reads behind this
		// one, and the record rides the claim from here as a collected
		// job's does ([Coordinator.completeAdmitted]).
		run.Collected = true
		run.Charged, run.CompanyCharged = c.charge(ctx, run, result)
		run.Launch = c.publishCollected(ctx, run, result)
		if run.asked() {
			// IT ASKED BEFORE IT WENT: the question was written and its harness
			// was on its way out, so the run is parked on it as a job that
			// finished asking is, and the answer carries it on.
			return nil, c.park(ctx, run, result)
		}
		outcome = runOutcome{
			DeliveredRefs: result.DeliveredRefs, DeliveredRefsElided: result.DeliveredRefsElided,
			InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
			CacheReadTokens: result.CacheReadTokens, CacheWriteTokens: result.CacheWriteTokens,
		}
	}
	outcome.Pushed, outcome.PushedElided, outcome.PushedUnread = result.Pushed, result.PushedElided,
		result.PushedUnread
	outcome.Engine = run.Launch.Bridged.Plus(EngineSpend{Aux: result.Condensed})
	outcome.Lost = &LostRun{
		Cause: loss.Cause, Why: loss.Cause.Why(),
		BoxKept: loss.Cause.leavesBox() && run.SandboxID != "",
		Losses:  run.RoundLosses + 1, Limit: MaxRoundLosses,
	}
	line := []any{"turn_id", run.TurnID,
		"agent", run.AgentHandle, "launch_id", run.LaunchID, "cause", string(loss.Cause),
		"node", loss.Node, "round_losses", outcome.Lost.Losses, "read", read}
	line = append(line, lossSpans(run, loss.At)...)
	log.WarnContext(ctx, "sandbox_run_lost_carried_on", append(line,
		"detail", "the run's job was lost before it finished: "+loss.Cause.Why()+
			"; its turn resumes to carry the round on")...)
	// THE REPORT, for a round its job had already submitted: the turn
	// collects that round from its calls rather than carrying it on, and
	// files this as the run's account of itself ([ResumeRequest.Lost]) —
	// cut off, as the result above was, whether or not its box was read.
	return &lostCarry{run: run, report: runReport(run, result), outcome: outcome}, nil
}

// lossSpans is what the line that carries a lost job's round on says of the
// time the loss cost, in whole seconds to at, when the loss was found
// ([JobLoss.At]): how long the job ran (ran_s, from [LaunchRecord.StartedAt])
// and since the newest kept capture of its own launch (since_save_s,
// [launchKept]), the work a relaunch continuing it does again. Each is left
// out where its instant is unknown. Three clocks (the finder's, the
// launcher's, the taker's) make each an estimate within twice
// objstore.AssumedClockSkew, which the clock_skew alarm holds; a figure below
// zero, those clocks' doing or the loss's own final take, is zero.
func lossSpans(run PendingRun, at time.Time) []any {
	if at.IsZero() {
		return nil
	}
	var spans []any
	if start := run.Launch.StartedAt; !start.IsZero() {
		spans = append(spans, "ran_s", max(0, int(at.Sub(start).Seconds())))
	}
	if kept := launchKept(run); kept != nil && !kept.At.IsZero() {
		spans = append(spans, "since_save_s", max(0, int(at.Sub(kept.At).Seconds())))
	}
	return spans
}

// lostCarry is what a lost job's round is carried on with: the claimed run as
// the read left it, the job's report and the outcome its resume settles.
type lostCarry struct {
	run     PendingRun
	report  string
	outcome runOutcome
}

// cutOff is what a lost job left, marked as cut off: never a success, whatever
// it had written — it did not get to say it was done — and with why beside
// whatever it had reported.
func cutOff(result Result, cause LossCause) Result {
	why := "the run was cut off before it finished, when " + cause.Why()
	if result.Error != "" {
		why += ". It had reported: " + result.Error
	}
	result.Success, result.Error = false, redact.Secrets(why)
	return result
}

// collectVerdict is what one collection attempt decided about a failure of its
// read: whether it is past asking again ([Coordinator.collectGivenUp]), and,
// where the box's host answered that its store did not answer it, for how long
// ([hostUnsettled]). Decided once per attempt, at one reading of the clock, and
// handed to every later step acting on that failure: judged twice, a bound
// crossed between a lost job's carry and its hand-back ended the run as
// unreachable just after the carry found its read too young to skip.
type collectVerdict struct {
	// at is the reading of the clock the attempt was judged at.
	at time.Time
	// givenUp is whether the collection is past asking again.
	givenUp bool
	// unsettled is whether the failure is the box's host's own answer that its
	// store did not answer it, timed by the clock on that host, and
	// unsettledFor how long that clock had run at at.
	unsettled    bool
	unsettledFor time.Duration
}

// lostWithHost reports whether the verdict is that only the box's host has kept
// it unread, for the host grace: a collection of a job not yet lost records
// the job LOST with that host and carries its round on ([Coordinator.loseWithHost]),
// and a lost job's round is carried on without its read ([Coordinator.carryOnLost]).
func (v collectVerdict) lostWithHost() bool { return v.givenUp && v.unsettled }

// collectGivenUp is the verdict on a collection that failed with cause, at now
// ([collectVerdict]): past asking again where its box is gone, or after
// [MinConnectFailures] attempts spanning [ConnectGiveUp] since the last read of
// the box, timed on the row (rowclock.go). Never for a read that did not start
// ([errReadUnstarted]) or a step the store did not answer ([ErrStoreSilent]),
// which say nothing of the box; but a host whose store alone is silent for the
// host grace is given up as a silent host is, its job lost with it
// ([collectVerdict.lostWithHost]). See [Coordinator.collectFailed] and the
// package doc, "Lost jobs".
func (c *Coordinator) collectGivenUp(run PendingRun, cause error, now time.Time) collectVerdict {
	verdict := collectVerdict{at: now}
	switch {
	case errors.Is(cause, ErrStoreSilent):
		verdict.unsettledFor, verdict.unsettled = c.hostUnsettledFor(run, cause, now)
		m := c.mgr()
		verdict.givenUp = verdict.unsettled && m != nil && verdict.unsettledFor >= m.HostGrace()
		return verdict
	case errors.Is(cause, ErrBoxGone):
		verdict.givenUp = true
		return verdict
	case errors.Is(cause, errReadUnstarted):
		return verdict
	}
	verdict.givenUp = run.Launch.CollectFailures+1 >= MinConnectFailures &&
		run.Launch.collectWait().lasted(c.node, now, ConnectGiveUp)
	return verdict
}

// loseWithHost records a job lost with the host its box is on, on the claim its
// collection holds ([LossLaunchNodeStoreSilent], [PendingStore.MarkClaimLost]),
// and answers the claim so, for its frame to carry the round on
// ([Coordinator.carryOnLost]) or end it at the turn's deadline. Recorded before
// anything is carried on, so the relaunch counts the loss ([MaxRoundLosses]); a
// verdict the store does not take is handed back uncounted, the clock kept. The
// clock on the host is kept past a verdict that lands, until the run settles
// ([hostUnsettled]). See the package doc, "Lost jobs".
func (c *Coordinator) loseWithHost(ctx context.Context, run PendingRun, cause error,
	verdict collectVerdict,
) (PendingRun, error) {
	loss := JobLoss{Cause: LossLaunchNodeStoreSilent, Node: run.LaunchNode, At: verdict.at.UTC()}
	marked, err := c.pending.MarkClaimLost(ctx, run.TurnID, run.LaunchID, loss, fenceOf(run))
	switch {
	case err != nil:
		log.WarnContext(ctx, "sandbox_collect_loss_unrecorded", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "node", run.LaunchNode, "error", err.Error(),
			"detail", "the run's job is lost with the node its box is on, which has not been able to read "+
				"the run's record for the host grace, but the coordination store did not record that; the "+
				"claim is handed back uncounted and the next collection records it")
		return run, c.collectFailed(ctx, run, storeSilent(ctx, err))
	case !marked:
		log.InfoContext(ctx, "sandbox_collect_claim_moved_on", "turn_id", run.TurnID,
			"launch_id", run.LaunchID,
			"detail", "the run no longer holds this claim, so its job is not recorded lost here: a newer "+
				"lease took the run, or its ending was decided, and that settles it")
		return run, nil
	}
	run.Lost = &loss
	log.WarnContext(ctx, "sandbox_collect_lost_with_host", "turn_id", run.TurnID,
		"agent", run.AgentHandle, "launch_id", run.LaunchID, "node", run.LaunchNode,
		"unsettled_for_s", verdict.unsettledFor.Seconds(), "error", cause.Error(),
		"detail", "the node whose host the run's box is on has answered every collection of it, for the box "+
			"timeout, that the coordination store did not answer its read of the run's record, so nothing can "+
			"read the box back; the job is recorded lost with that node and its round carried on in a box of "+
			"its own, as for a node that left the fleet")
	return run, nil
}

// lostNote is the launch record's account of a lost job — see
// [collectedRecord].
func lostNote(loss JobLoss) string {
	return fmt.Sprintf("cut off before it finished, when %s", loss.Cause.Why())
}
