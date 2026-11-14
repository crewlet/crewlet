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
	// for it — a script of an older build, one not yet at its first line, a
	// record that is not the protocol's — so nothing was asked, and the read
	// kills the job instead.
	EndUnheard EndAsk = "unheard"
	// EndGone is a pid the box says is no process of the job
	// ([ErrNotJobProcess]), so nothing was asked, and the read kills the job
	// instead.
	EndGone EndAsk = "gone"
)

// EndAsks are the three.
var EndAsks = []EndAsk{EndAsked, EndUnheard, EndGone}

// Valid reports whether this build knows a.
func (a EndAsk) Valid() bool { return slices.Contains(EndAsks, a) }

// JobEnder is a [Runner] whose job's script ends its job on request
// (internal/sandbox/executor's Runner): it ends its harness, packs its session
// as its own exit does, and records that it was ended.
//
// OPTIONAL, as [SessionTaker] is: a runner whose scripts take no request has
// every job of its asked nothing, and killed ([Sandbox.KillJob]).
//
// THE FILE IS THE RUNNER'S AND THE SIGNAL THE BACKEND'S: the runner reads the
// pid the script recorded where its own paths put it, and the box signals it
// as only the box can ([Sandbox.EndJob]). The wait for the job to be over, and
// the kill past it, are the read's ([Coordinator.endJob]).
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
// SIGTERM and SIGKILL: a direct box's job group at its teardown, and a process
// a run's script stops itself — its harness, an OpenCode run's server and its
// login exporter (internal/sandbox/executor).
//
// ONE FIGURE, because it is one decision: how long a process told to go is
// given to go before it is made to. Five seconds is the grace the engine host
// gives a harness it terminates (internal/providers/llm/harness's termGrace),
// enough for a Node runtime — Claude Code is one, and so is OpenCode's — to run
// its exit handlers and close its sockets. Both go well inside it: Claude Code
// 2.1.310, told to stop mid-tool with a stdio MCP server connected, measured
// going in under a fifth of a second, its tool's processes and its MCP server
// with it; and OpenCode 2.0.32's server, the slowest process a run's script
// stops, in a tenth of a second, and in three with a stdio MCP server still
// connecting, which it stops itself. A process past it is wedged rather than
// slow, and a run's done marker waits on its script: a process that ignored
// SIGTERM would otherwise hold the run open for ever. OpenCode's run client on
// 2.2.7 and 2.3.0 is such a process at an end request: it begins its shutdown
// at the SIGTERM and never finishes it, so every end of such a run spends the
// whole grace and kills it (measured; on 2.0.32 and 2.1.4 the client is gone
// within a fifth of a second), and the end is recorded as killed.
const JobStopGrace = 5 * time.Second

// JobKillWait is the most [Sandbox.KillJob] waits for a job to be gone: SIGTERM
// and [JobStopGrace] for every process of it to go, then SIGKILL and
// [JobStopGrace] more for the kernel to take them — the shape a direct box's
// teardown has always had (Close) — beside the command that carries it into a
// container or a VM.
const JobKillWait = 2 * JobStopGrace

// JobEndWait is the most the read that ends a stopped, lost or capped job waits,
// once its script was asked ([EndAsked]), for the job to be over before it kills
// it ([Coordinator.endJob]).
//
// TWO MINUTES AND FIFTEEN SECONDS, which is everything the ended script does on
// its way out, each at its own bound: the harness's grace ([JobStopGrace]); the
// pack, at [SessionPackWait] — its minute for tar and gzip over a session at its
// bound, and the minute it keeps for a harness to come up, which an OpenCode
// script's export through its own running server does not spend and its `api`
// calls do; and the script's own stops of its login exporter and its server,
// a grace each. A job not over by then has a pack that is stuck, and is killed
// — its session then packed by the read. An OpenCode script's calls to its own
// server, its export among them, are each bounded, and the calls on an end
// request's way out add up to less than this wait (internal/sandbox/executor's
// openCodeEndPath), so none of them is what the kill finds wedged.
// NOTHING ELSE: the pages of its session's message list an OpenCode script asks
// for the measure of its session's newest request (internal/sandbox/executor's
// openCodeContext) are never asked on an end request, whose round no relaunch
// continues in place, so they spend none of this wait.
const JobEndWait = SessionPackWait + 3*JobStopGrace

// JobEndBudget is the most an end adds to the read it is made in: the wait for
// the job to be over, and the kill past it ([JobKillWait]).
const JobEndBudget = JobEndWait + JobKillWait

// jobEndPoll is how often the read asks whether an ended job is over: the
// runner's own poll, a marker read and, in a container or a VM, one exec of a
// tenth to half a second. An end takes seconds, so a second costs at most a
// second of it and a handful of execs.
const jobEndPoll = time.Second

// endJob ENDS a stopped or lost job, or one still working past its turn's cap,
// before its session is taken, on its box's host, as the first step of the
// read that takes it ([Coordinator.readBox] for a stop's and for a collection
// past the cap, [Coordinator.readLost]), and reports whether the job is over
// when it is done.
//
// A job that is already over has strays at most — a process its harness left
// running in its group — which the kill sweeps. Any other is ASKED to end
// ([JobEnder]): its script ends its harness and packs the session as its own
// exit does (internal/sandbox/executor's end.go), so what the read then takes
// is the job's own end, the step in flight settled, and FINAL. The read waits
// for that, at most [JobEndWait], polling every [jobEndPoll], and then KILLS
// what is left of the job ([Sandbox.KillJob]) — a sweep after a clean end, the
// escalation past the wait, and at once where nothing could be asked: a script
// of an older build or one not yet at its first line ([EndUnheard]), a pid that
// is no process of the job ([EndGone]), a box that could not be asked.
//
// A JOB STILL RUNNING AFTER ALL THAT — a box whose kill could not reach it, a
// container that can name no process group of the job's own and so signalled
// nothing of it ([ErrNoJobGroup]), a poll that cannot say — is taken as it
// stands, not final, and its reclaim ends it — a capped job's once its turn's
// ending reclaims the box its collection paused; the log says so.
//
// THE KILL OUTLIVES THE READ: it runs under a context of its own ([detached]),
// since the stop, the loss or the cap decided the job's end, and a read cut
// short — its node draining, its asker gone — must not leave the job running on
// the strength of the read alone. Such a read is said as cut short ([whyCutShort]),
// never as a job that outlived its end, and reads nothing after the kill: its
// context is gone, so it takes no session, and the ending's next read finds
// the job killed.
//
// WORKING, where set, is told the moment the end has SEEN THE JOB STILL WORKING
// — its poll answered that it runs, or its script heard the request
// ([EndAsked]) — before the wait and before anything is killed, once or more:
// what a collection past its turn's cap records as its verdict
// ([Coordinator.readBox]), since a kill can take the job's script before the
// script records the request, and leave nothing else that says the job was
// working. A stop's and a loss's read pass none: their end is decided on the
// run's record already.
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
		// THE SWEEP'S ERROR IS NO VERDICT on the job, whose end its poll
		// already read: [Coordinator.killJob] says it, and strays it could
		// not reach go with the box's reclaim.
		_ = c.killJob(ctx, run, box, "")
		return true
	}
	if pollErr == nil && working != nil {
		// SEEN STILL WORKING, and said so before anything is asked of it.
		working()
	}
	ask, err := EndUnheard, error(nil)
	if ender, ok := runner.(JobEnder); ok {
		ask, err = ender.AskEnd(ctx, box, handle)
	}
	if err == nil && ask == EndAsked && working != nil {
		// ITS SCRIPT HEARD THE REQUEST, so the job was still working however
		// its poll answered: said so before the wait, and before a read cut
		// short kills what is left of it.
		working()
	}
	began := time.Now()
	var why string
	switch {
	case ctx.Err() != nil:
		// THE READ ENDED as it asked, which is no failure of the box's: an
		// ask under a dead context fails on that alone.
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
// ended before the job did — its node draining, its asker no longer waiting —
// which says nothing about the job: never "outlived_its_end", a wait run out on
// a stuck pack or a wedged call, which is the one to look into.
const whyCutShort = "read_cut_short"

// awaitEnd waits for an asked job to be over, at most [Coordinator.endWait]:
// whether it was, and whether the read's own context ended first — cut short,
// which is no wait that ran out.
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
// context of its own ([detached]) — see [Coordinator.endJob] — why being the
// reason the end gave for it, "" for the sweep after the job's own end; and
// answers the kill's error, which the read's poll weighs.
//
// A BOX THAT CAN NAME NO GROUP OF THE JOB'S OWN signalled nothing
// ([ErrNoJobGroup]) and is said at info, apart from a box that could not be
// asked: it is what such a box always answers — after a clean end as much as
// past the wait — and what is left of the job then runs until the box's
// reclaim, which the poll after the kill says where it matters.
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
