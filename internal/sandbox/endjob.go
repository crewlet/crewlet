package sandbox

import (
	"context"
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
// SIGTERM would otherwise hold the run open for ever.
const JobStopGrace = 5 * time.Second

// JobKillWait is the most [Sandbox.KillJob] waits for a job to be gone: SIGTERM
// and [JobStopGrace] for every process of it to go, then SIGKILL and
// [JobStopGrace] more for the kernel to take them — the shape a direct box's
// teardown has always had (Close) — beside the command that carries it into a
// container or a VM.
const JobKillWait = 2 * JobStopGrace
