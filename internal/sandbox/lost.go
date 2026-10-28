package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/redact"
)

// A run whose job was LOST with the host it ran on, and the round carried on.
//
// A JOB CAN GO WITHOUT ENDING. A `direct` box's job dies with its engine's
// restart ([JobState]), a box root that did not outlive its node takes the job
// with it, a VM is reclaimed while the engine is away past the box timeout, and
// a node that leaves the fleet for good takes every local box it held
// ([Waiter.goneAfter]). None of those is a run that finished, or one that
// failed: the job was cut off mid-step by something outside it, with nothing
// written to say how far it got. Collected, its partial output was judged as
// the round's result — a run that "did not succeed" for reasons it never had —
// and where its box was gone the run ended the turn, which the park had
// already acknowledged, so nothing would ever start it again.
//
// SO A LOST JOB'S ROUND IS CARRIED ON, as a parked run's is once its question
// is answered ([ResumeRequest.Lost], [ResumeRequest.Asked]): the turn resumes
// and relaunches its executor for the same round, its calls so far kept on the
// row ([PendingRun.Continues]), in the box the lost job used where that box is
// still there and this node may continue in it — its working tree as the job
// left it — and in a fresh one from whatever the job pushed otherwise. What the
// job spent up to then is read off its box where the box is left, charged and
// published as its own record, exactly once, as a finished job's is.
//
// NOT FOREVER. A round whose jobs keep being lost is a run that takes its own
// host down — it exhausts the memory its container is held to, say, which on a
// Kubernetes node with cgroup v2 kills every process in the container, the
// engine included — and every relaunch takes the node's other seats down with
// it again. So a round is carried on after at most [MaxRoundLosses] losses, and
// the next ends its executor phase, and so its turn, saying why.

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

// LossCause is what a lost job was lost to — see [JobLoss].
type LossCause string

const (
	// LossEngineRestarted is a job on the engine host that went with the
	// engine process that last saw it running ([JobLost]): a container or
	// systemd restart, or a host reboot. Its box is still there.
	LossEngineRestarted LossCause = "engine_restarted"
	// LossBoxGone is a running job whose box is gone from where it ran: a
	// box root that did not outlive its node, or a VM its provider reclaimed.
	LossBoxGone LossCause = "box_gone"
	// LossLaunchNodeGone is a running job whose box was on the host of a node
	// that left the fleet and did not come back within the box timeout
	// ([Waiter.goneAfter]).
	LossLaunchNodeGone LossCause = "launch_node_gone"
)

// LossCauses are the three.
var LossCauses = []LossCause{LossEngineRestarted, LossBoxGone, LossLaunchNodeGone}

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
	}
	return "it was lost with the host it ran on"
}

// leavesBox reports whether a job lost to c left its box where it was, so what
// it did up to then can still be read off it.
func (c LossCause) leavesBox() bool { return c == LossEngineRestarted }

// JobLoss is the verdict that a running job was lost with its host — see
// [PendingRun.Lost].
type JobLoss struct {
	Cause LossCause `json:"cause"`
	// Node is the node whose host the job's box was on, "" for a box every
	// node reaches: the one node whose verdict about a local box counts.
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

// carryOnLost is a completion's claim of a lost job carried on: what the job
// did up to then read off its box where its box is left, charged and published
// once, and the turn resumed to carry the job's round on ([LostRun]) — or, where
// the job had asked a person something before it went, parked on that question
// as a finished job is.
//
// THE READ IS ASKED AGAIN like any collection ([Coordinator.collectFailed]): a
// box that did not answer for a moment hands the claim back for the completion's
// retry, so the job's spend is not lost to one transport blip. One that is gone
// by now, or has not answered for the give-up window, is no reason to hold the
// round any longer: it is carried on without the read, and the line says the
// job's spend went with its box.
func (c *Coordinator) carryOnLost(adm admitted, run PendingRun, trigger *events.Event) error {
	ctx := adm.ctx
	loss := *run.Lost
	var (
		result Result
		read   bool
	)
	if loss.Cause.leavesBox() {
		boxed, _, err := c.boxResult(ctx, run, BoxReadCollect)
		switch {
		case err == nil:
			result, read = c.fitText(ctx, run, cutOff(boxed, loss.Cause)), true
		case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, ErrBoxHostSilent),
			!c.collectGivenUp(run, err, c.now()):
			return c.collectFailed(ctx, run, err)
		default:
			log.WarnContext(ctx, "sandbox_lost_run_unread", "turn_id", run.TurnID,
				"launch_id", run.LaunchID, "error", err.Error(),
				"detail", "what the lost job did could not be read off its box, so its spend "+
					"is not charged; its round is carried on all the same")
		}
	}
	var outcome runOutcome
	if read {
		// THE BOX ANSWERED, which ends any run of failed reads behind this
		// one, and the record rides the claim from here as a collected
		// job's does ([Coordinator.completeAdmitted]).
		run.Collected = true
		run.Charged, run.CompanyCharged = c.charge(ctx, run, result)
		run.Launch = c.publishPhase(ctx, run, result)
		if result.NeedsInput {
			// IT ASKED BEFORE IT WENT: the question was written and its CLI
			// was on its way out, so the run is parked on it as a job that
			// finished asking is, and the answer carries it on.
			return c.park(ctx, run, result)
		}
		outcome = runOutcome{
			DeliveredRefs: result.DeliveredRefs, DeliveredRefsElided: result.DeliveredRefsElided,
			InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
			CacheReadTokens: result.CacheReadTokens, CacheWriteTokens: result.CacheWriteTokens,
		}
	}
	outcome.Engine = run.Launch.Bridged.Plus(EngineSpend{Aux: result.Condensed})
	outcome.Lost = &LostRun{
		Cause: loss.Cause, Why: loss.Cause.Why(),
		BoxKept: loss.Cause.leavesBox() && run.SandboxID != "",
		Losses:  run.RoundLosses + 1, Limit: MaxRoundLosses,
	}
	log.WarnContext(ctx, "sandbox_run_lost_carried_on", "turn_id", run.TurnID,
		"agent", run.AgentHandle, "launch_id", run.LaunchID, "cause", string(loss.Cause),
		"node", loss.Node, "round_losses", outcome.Lost.Losses, "read", read,
		"detail", "the run's job was lost before it finished: "+loss.Cause.Why()+
			"; its turn resumes to carry the round on")
	// THE REPORT, for a round its job had already submitted: the turn
	// collects that round from its calls rather than carrying it on, and
	// files this as the run's account of itself ([ResumeRequest.Lost]).
	if !read {
		result = cutOff(Result{}, loss.Cause)
	}
	_, err := c.resumeAndSettle(adm, run, runReport(result), false, trigger, outcome)
	return err
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

// collectGivenUp reports whether a collection that failed with cause, at now,
// is past asking again: its box is gone, or it has failed for
// [MinConnectFailures] attempts spanning [ConnectGiveUp] since the last
// collection that read the box — the waiter's own bound on an unreachable box.
// See [Coordinator.collectFailed].
func (c *Coordinator) collectGivenUp(run PendingRun, cause error, now time.Time) bool {
	if errors.Is(cause, ErrBoxGone) {
		return true
	}
	since := run.Launch.CollectFailingSince
	if since.IsZero() {
		since = now
	}
	return run.Launch.CollectFailures+1 >= MinConnectFailures && now.Sub(since) >= ConnectGiveUp
}

// lostNote is the job record's account of a lost job — see [runPhase].
func lostNote(loss JobLoss) string {
	return fmt.Sprintf("cut off before it finished, when %s", loss.Cause.Why())
}
