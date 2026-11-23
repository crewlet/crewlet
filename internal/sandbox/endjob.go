package sandbox

import (
	"context"
	"errors"
	"slices"
	"time"
)

// JobEnd is how a job was ended on request, as its script recorded it the
// moment its harness was over ([Result.JobEnd]).
type JobEnd string

const (
	// JobEndAsked is a harness told to stop that went within its grace.
	JobEndAsked JobEnd = "asked"
	// JobEndKilled is a harness that outlived its grace and was killed.
	JobEndKilled JobEnd = "killed"
	// JobEndBefore is a request that came before the harness started, which
	// started nothing.
	JobEndBefore JobEnd = "before"
)

// JobEnds are the three.
var JobEnds = []JobEnd{JobEndAsked, JobEndKilled, JobEndBefore}

// Valid reports whether this build knows e.
func (e JobEnd) Valid() bool { return slices.Contains(JobEnds, e) }

// EndAsk is what asking a job to end came to ([JobEnder]).
type EndAsk string

const (
	// EndAsked is a request delivered to the job's script, which ends its
	// job: the read waits for it to be over.
	EndAsked EndAsk = "asked"
	// EndUnheard is a job whose box holds no end protocol this build speaks
	// for it ([JobEnder.AskEnd]): nothing was asked, and the read kills it.
	EndUnheard EndAsk = "unheard"
	// EndGone is a pid the box says is no process of the job
	// ([ErrNotJobProcess]): nothing was asked, and the read kills it.
	EndGone EndAsk = "gone"
)

// EndAsks are the three.
var EndAsks = []EndAsk{EndAsked, EndUnheard, EndGone}

// Valid reports whether this build knows a.
func (a EndAsk) Valid() bool { return slices.Contains(EndAsks, a) }

// JobEnder is a [Runner] whose job's script ends its job on request
// (internal/sandbox/executor's Runner): it ends its harness, packs its session
// as its own exit does, and records that it was ended. Optional, as
// [SessionTaker] is: a runner without it has every job killed
// ([Sandbox.KillJob]).
//
// The runner reads the pid file, since its paths place it; only the box can
// signal it ([Sandbox.EndJob]). The wait and the kill past it are the read's
// ([Coordinator.endJob]).
type JobEnder interface {
	// AskEnd reads the end protocol the job's script recorded in box and sends
	// its script the request ([Sandbox.EndJob]): [EndAsked]; [EndUnheard] where
	// the box holds no protocol this build speaks for its job (an older
	// build's script, one not yet at its first line, a record that is not `1
	// <pid>` with pid > 1); [EndGone] where that pid is no process of the job.
	// An error is a box that could not be asked.
	AskEnd(ctx context.Context, box Sandbox, handle RunHandle) (EndAsk, error)
}

// JobStopGrace is how long one process of a run's job is given between
// SIGTERM and SIGKILL: a direct box's job group at teardown, and each process
// a run's script stops itself (internal/sandbox/executor). One figure for one
// decision; five seconds matches the engine host's harness grace
// (internal/providers/llm/harness's termGrace), enough for a Node runtime's
// exit handlers, and Claude Code and OpenCode measured well inside it. A
// process past it is wedged, and a run's done marker waits on its script, so
// one ignoring SIGTERM would hold the run open for ever. OpenCode's run client
// on 2.3.2 is gone within 0.2 s of its SIGTERM, before or after its session
// is interrupted (measured).
const JobStopGrace = 5 * time.Second

// JobKillWait is the most [Sandbox.KillJob] waits for a job to be gone: SIGTERM
// and [JobStopGrace] for every process of it to go, then SIGKILL and
// [JobStopGrace] more for the kernel to take them, as a direct box's Close
// does.
const JobKillWait = 2 * JobStopGrace

// JobEndWait is the most the read that ends a stopped, lost or capped job waits,
// once its script was asked ([EndAsked]), for the job to be over before it kills
// it ([Coordinator.endJob]).
//
// It is the ended script's way out, each step at its own bound: the harness's
// grace ([JobStopGrace]), the pack ([SessionPackWait]), and the script's stops
// of its login exporter and its server, a grace each. A job not over by then
// has a stuck pack, and is killed and its session packed by the read. An
// OpenCode script's calls on an end request's way out are each bounded and sum
// to less than this (internal/sandbox/executor's openCodeEndPath); its
// openCodeContext pages are never asked on an end request.
const JobEndWait = SessionPackWait + 3*JobStopGrace

// JobEndBudget is the most an end adds to the read it is made in: the wait for
// the job to be over, and the kill past it ([JobKillWait]).
const JobEndBudget = JobEndWait + JobKillWait

// jobEndPoll is how often the read asks whether an ended job is over: one
// marker read, in a container an exec of up to half a second. An end
// takes seconds, so a second costs at most a second and a handful of execs.
const jobEndPoll = time.Second

// endJob ends a stopped or lost job, or one past its turn's cap, first in the
// read that takes its session ([Coordinator.readBox], [Coordinator.readLost]),
// and reports whether it is over. An over job's strays are swept; any other is
// asked to end ([JobEnder]) so its script packs a final session, and what is
// left is killed, detached ([Coordinator.killJob]), after [JobEndWait] or at
// once where nothing could be asked: a read cut short ([whyCutShort]) must not
// leave the job running, and takes no session. One still running after that is
// taken as it stands, not final, and its box's reclaim ends it.
//
// working (a capped collection's verdict; nil for a stop or loss) is told
// once or more when the end sees the job working (its poll, or [EndAsked])
// before any kill, which can take the script before it records the request.
func (c *Coordinator) endJob(ctx context.Context, run PendingRun, box Sandbox, runner Runner,
	working func()) bool {

	if run.CommandID == "" {
		// NO JOB WAS STARTED in the box — a launch stopped or lost before its
		// command was recorded — so there is nothing to end.
		return true
	}
	handle := run.Handle()
	state, pollErr := runner.Poll(ctx, box, handle)
	if pollErr == nil && state.Over() {
		// The sweep's error is no verdict: the poll already read the end, and
		// strays it missed go with the box's reclaim.
		_ = c.killJob(ctx, run, box, "")
		return true
	}
	if pollErr == nil && working != nil {
		// Said before anything is asked of it.
		working()
	}
	ask, err := EndUnheard, error(nil)
	if ender, ok := runner.(JobEnder); ok {
		ask, err = ender.AskEnd(ctx, box, handle)
	}
	if err == nil && ask == EndAsked && working != nil {
		// Its script heard the request, so it was working however its poll
		// answered: said before a cut-short read kills it.
		working()
	}
	began := time.Now()
	var why string
	switch {
	case ctx.Err() != nil:
		// The read ended as it asked, which is no failure of the box's; first,
		// since an ask under a dead context fails on that alone.
		why = whyCutShort
	case err != nil:
		why = "ask_failed"
		log.WarnContext(ctx, "sandbox_job_end_unasked", "turn_id", run.TurnID, "sandbox_id", run.SandboxID,
			"error", err.Error(), "detail", "the job could not be asked to end, so it is killed at once")
	case ask == EndUnheard:
		why = "script_unheard"
	case ask == EndGone:
		why = "not_the_jobs"
	default:
		switch over, cut := c.awaitEnd(ctx, box, runner, handle); {
		case over:
			log.InfoContext(ctx, "sandbox_job_ended", "turn_id", run.TurnID, "sandbox_id", run.SandboxID,
				"launch_id", run.LaunchID, "waited_seconds", time.Since(began).Seconds())
			_ = c.killJob(ctx, run, box, "")
			return true
		case cut:
			why = whyCutShort
		default:
			why = "outlived_its_end"
		}
	}
	switch why {
	case "outlived_its_end":
		log.WarnContext(ctx, "sandbox_job_killed", "turn_id", run.TurnID, "sandbox_id", run.SandboxID,
			"why", why, "waited_seconds", time.Since(began).Seconds())
	case whyCutShort:
		log.InfoContext(ctx, "sandbox_job_killed", "turn_id", run.TurnID, "sandbox_id", run.SandboxID,
			"why", why, "waited_seconds", time.Since(began).Seconds(), "error", context.Cause(ctx).Error())
	default:
		log.InfoContext(ctx, "sandbox_job_killed", "turn_id", run.TurnID, "sandbox_id", run.SandboxID,
			"why", why)
	}
	killErr := c.killJob(ctx, run, box, why)
	if why == whyCutShort {
		return false
	}
	state, err = runner.Poll(ctx, box, handle)
	if err == nil && state.Over() {
		return true
	}
	detail := "the job is still running after its end and its kill"
	switch {
	case err != nil:
		detail = "whether the job is over could not be read after its kill: " + err.Error()
	case killErr != nil:
		detail = "the job is still running, and its kill could not end it: " + killErr.Error()
	}
	log.WarnContext(ctx, "sandbox_job_unended", "turn_id", run.TurnID, "sandbox_id", run.SandboxID,
		"detail", detail+"; its session is taken as it stands, not final, and its box's reclaim ends it")
	return false
}

// whyCutShort is the kill's reason ([Coordinator.endJob]) where the read itself
// ended first (node draining, asker gone), which says nothing about the job;
// "outlived_its_end" (a stuck pack or wedged call) is the one to look into.
const whyCutShort = "read_cut_short"

// awaitEnd waits for an asked job to be over, at most [Coordinator.endWait]:
// whether it was, and whether the read's own context ended first (cut short).
func (c *Coordinator) awaitEnd(ctx context.Context, box Sandbox, runner Runner, handle RunHandle) (over, cut bool) {
	deadline := time.NewTimer(c.endWait)
	defer deadline.Stop()
	tick := time.NewTicker(c.endPoll)
	defer tick.Stop()
	for {
		if state, err := runner.Poll(ctx, box, handle); err == nil && state.Over() {
			return true, false
		}
		select {
		case <-ctx.Done():
			return false, true
		case <-deadline.C:
			return false, false
		case <-tick.C:
		}
	}
}

// killJob kills what is left of run's job in box ([Sandbox.KillJob]) under a
// [detached] context ([Coordinator.endJob]); why is the end's reason, "" for
// the sweep after a clean end. The error is for the read's poll to weigh.
//
// [ErrNoJobGroup] is logged at info: such a box always answers it, after a
// clean end too, and what is left runs until the box's reclaim.
func (c *Coordinator) killJob(ctx context.Context, run PendingRun, box Sandbox, why string) error {
	killCtx, cancel := detached(ctx)
	defer cancel()
	err := box.KillJob(killCtx, run.CommandID)
	switch {
	case err == nil:
	case errors.Is(err, ErrNoJobGroup):
		log.InfoContext(ctx, "sandbox_job_ungrouped", "turn_id", run.TurnID, "sandbox_id", run.SandboxID,
			"why", why, "detail", err.Error()+"; what is left of the job runs until its box is reclaimed")
	default:
		log.WarnContext(ctx, "sandbox_job_kill_failed", "turn_id", run.TurnID, "sandbox_id", run.SandboxID,
			"why", why, "error", err.Error())
	}
	return err
}
