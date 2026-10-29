// Package sandbox is the executor runtime: every agent seat's executor is its
// coding CLI's own run, inside a box placed in one of the catalogue's cells.
//
// The shape that matters is that a run is DETACHED. The launch starts it, the
// turn SUSPENDS, and the engine RESUMES the same turn with the run's report
// when the run completes — possibly minutes later, possibly in a different
// process after a restart, possibly on a different node. Nothing
// here parks a goroutine on a running job: a goroutine cannot survive a
// restart, and a coding agent legitimately runs for longer than a deployment
// window.
//
// A run that stops to ask a person something is that same shape one step
// further: the seat goes FREE while the question waits, and the answer is
// matched back to the run by the CONVERSATION it was asked in rather than by
// the inbox batch the question arrived in. A person answers where they are
// talking, and the engine's own chat prompt routinely puts their reply in a
// finer partition than the question was asked from — so a match on the batch
// lost the answer outright. The rule is [ConversationRef.Answers], and
// [ConversationRef.Best] chooses between the runs it admits.
//
// AND BY WHEN IT WAS WRITTEN, against when the question was asked
// ([PendingRun.AskedAt]): the first reply posted after the question is the
// answer, and it is RECORDED on the run ([StatusAnswered]) before anything is
// done with it. Resuming with it is then this package's job, retried on its
// own schedule with the seat's inbox held behind it, never the inbox's — a
// person's message handed back to the broker returns behind their next one,
// which then took the question. See answerowed.go.
//
// AND IT REACHES THE SEAT ONCE, whatever becomes of the run. A recorded reply
// is either TAKEN by a resumed turn — written on the row at the last moment
// before the turn runs ([RecordedAnswer.TakenAt], [ResumeRequest.Begin]) — or
// LET GO back to the seat the way it came, through an outbox on the row
// ([PendingRun.HandBack]) — a chat reply to the seat's inbox, an answer by turn
// to its control topic ([handBackSubject]): after its attempts are spent, by an
// ending no turn reached, or by the seat's next holder reaping a claim whose
// node stopped before its turn took it ([Coordinator.RecoverSeat]) once the claims of that
// answer have died too often or for too long. Short of that, such a claim is
// REVIVED — the answer given back to the run and the run resumed with it
// ([PendingStore.ReviveAnswer], [Coordinator.revivable]) — because a claim that
// never reached its turn used nothing. The take is exclusive with both, in the
// store's own compare-and-set: an ending is DECIDED on the row before any of
// it is done ([RecordedEnding]), no turn takes an answer from a row whose
// ending is decided ([ErrRunEnding]) or from a revived run, which holds no
// claim, and a take that landed first is seen by the decision or the revival
// and the reply goes with the run as used — whichever lands first wins,
// whatever any lease says. And no ending deletes a row still
// holding a reply it owes the seat ([ErrAnswerOwed]). The reply's delivery is
// recorded as worked when the reply leaves the run, either way
// ([CoordinatorOptions.Spent]).
//
// AND A LOST RUN IS ANNOUNCED ONCE: the announcement goes out before the record
// is deleted, under the identity its decided ending recorded, so whichever node
// finishes the ending publishes the same event ([Coordinator.announceEnding]).
// So is a STOPPED one — a person's pause with `stop_running` ends the seat's
// launching and running runs at once ([Coordinator.StopSeat]) — as a stop and
// never as a loss, with who stopped it recorded in the deciding write
// ([PendingRun.Stop]), its spend read off the box and charged before the box
// goes, and the turn parked on it closed as stopped ([CoordinatorOptions.CloseStopped]).
//
// The reap fences the row to its lease before it revives or lets go of
// anything — and the revival stamps that lease on the row itself — and a take
// under an older lease — or under none — is refused, which holds because every
// claim stamps the CLAIMANT's lease on the row ([PendingStore.ClaimForResume])
// and every launch the lease that launched it ([CoordinatorOptions.Lease]).
//
// EVERY SUBJECT A RUN IS REACHED ON IS NAMED BY THE ROW'S SEAT ID — its
// completion and start on the seat's control subject, an answer by turn and a
// reply handed back on its inbox — through one parse of the id the row carries
// as text ([controlSubject], [inboxSubject]), because a seat's mailbox is named
// by its id, derived from its immutable handle (ADR-0013). The handle the row
// carries beside it is a label and this node's bookkeeping key, never an
// address: the coordinator's per-seat counts, holds and retries are keyed by it,
// and whatever maps one of them onto a lease or a mailbox maps the handle to
// the id first. An answer by turn is stamped with that same id
// ([AnswerDesk.Deliver]) and dropped by any seat whose run does not carry it
// ([Coordinator.AnswerByTurn]).
//
// WHO ANSWERED is recorded as iam.ActorFor makes it of a principal — the
// author, its kind and the credential it came through ([AnswerWords]) —
// on the row and on every announcement of what became of the answer, never as a
// seat beside an actor.
//
// AND THE ANSWER IS SEALED: its words and who gave them are on the row only
// inside a seal under the fleet keyring, bound to the run's turn, the question
// it answers and its deliveries, like every other instruction channel
// (ADR-0031), and every reader opens it before anything of it is used. An answer
// whose seal does not open is never handed to a turn or attributed to anybody.
// See answerseal.go.
//
// AND A BOX IN A `direct` OR `container` CELL IS TOUCHED ONLY ON ITS LAUNCHING
// NODE'S HOST ([PendingRun.LaunchNode], ADR-0065): that node's own waiter polls,
// keeps alive, reaps and reclaims it, a seat's holder elsewhere asks the host to
// read it ([BoxReader]), and a launching node whose presence stays lapsed for
// the box timeout has its running jobs lost with it. See boxhost.go.
//
// AND A JOB LOST WITH ITS HOST IS CARRIED ON, NEVER COLLECTED: one its
// engine's restart killed — which the box tells from one that crashed by the
// engine process that last saw it running ([JobLost], local_witness.go) — one
// whose box is gone, and one whose node left. The waiter that drives the box
// records the loss on the row ([PendingRun.Lost]), and the seat's holder reads
// what the job spent where its box is left and resumes the turn to relaunch the
// same round, at most [MaxRoundLosses] times a round. See lost.go.
//
// See docs/concepts/code-sandbox.md.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/logging"
)

// log is the package's own component, bound for the backend-NEUTRAL runtime:
// the coordinator, the waiter, the manager, launch, setup, mcprender and otel.
//
// Each backend binds its own (localLog, e2bLog) so a line names the box it
// came from. This var used to live in local.go bound to "sandbox.local", which
// stamped every backend-neutral event — and every remote-box event — as the
// local backend, so filtering logs by component hid remote runs exactly where
// an operator would look for them. A file added here logs as plain "sandbox"
// unless it is backend-specific, which is the safe default: an over-general
// component is findable, a wrong one is not.
var log = logging.Get("sandbox")

// DefaultHome is where a box that says nothing else keeps its artefacts.
//
// Every run artefact — the result, the done marker, the ask signal, the
// findings — lives under <home>/.crewlet. It is a per-SANDBOX property rather
// than a constant, which it was for as long as a remote VM was the only
// backend: many local boxes share one filesystem, and a shared home would have
// every run reading its neighbour's done marker.
//
// It is [hostbox.ContainerHome], declared there beside [hostbox.InHome],
// which judges a container box's paths against it — for both local boxes and
// for config validation, all of which already import that leaf package: the
// remote templates' one account has it as its home, and a container box
// mounts its own there so a path reads the same in both.
const DefaultHome = hostbox.ContainerHome

// ExecResult is one shell command's outcome inside a box.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Limits are the caps handed to a coding agent.
//
// Zero means UNSET for each field, and the runner falls back to the agent's own
// default. Note what is not here: a run deadline. The engine imposes no
// wall-clock limit on a coding job — it runs as long as it needs, and
// completion is detected by tracking the job rather than by a clock. There WAS
// a TimeoutSec here, read by nobody, contradicting the paragraph above it.
//
// MaxTurns comes from the resolved [Spec] — providers.sandbox.default_max_turns
// overlaid with role.sandbox.max_turns — and is the only engine-side bound on
// a runaway coding agent, since a job is deliberately never stopped on a clock.
//
// MAXBUDGETUSD IS SET BY NOBODY, deliberately. The fleet's own token meter
// already post-charges a collected run against the seat's budget, and a second
// cap denominated in the operator's dollars would fight it: two ceilings on one
// spend, disagreeing, with the CLI's the one that silently wins.
type Limits struct {
	MaxTurns     int
	MaxBudgetUSD float64
}

// Spec is what it takes to mint and drive one box.
//
// The REPO IS NOT HERE. Which repository to work in is task context the executor
// puts in the brief, and the coding agent clones it with the token the config
// injects — a spec field for it would make one repo per role a structural fact
// when it is a per-turn one.
//
// There are deliberately NO CPU, MEMORY OR DISK FIELDS, and no TEMPLATE either.
// A remote box's resources are a property of its template, fixed when the
// template is built, and the create APIs accept no resource arguments; a local
// box is sized by providers.sandbox.local.run_args. Both are configured on the
// BACKEND, once, so a spec field for either would be a second answer to a
// question the catalogue has already settled — and the last one, Template, was
// set by nobody while looking exactly like a wired knob.
type Spec struct {
	// Placement is WHICH configured backend runs this box. Resolved from
	// the run_in of the providers.llm entry the launch picked, over
	// providers.sandbox.default_run_in, and carried here rather than chosen
	// by the caller so the launch, the pending row and the reconnect all
	// name the same cell.
	Placement Placement

	// CodingAgent names the runner: "claude-code", "opencode", …
	CodingAgent string

	// TurnID is the turn whose run the box is made for. A box on the
	// engine's host records it in its own directory, which is how that
	// host's waiter tells a box no run names any more from one a run is
	// using, and reclaims it ([HostBox]); a remote box has no use for it.
	// Set by [Coordinator.Launch], never by a caller.
	TurnID string

	// TimeoutSec is NOT a run deadline: it is the box's initial TTL, which
	// the waiter refreshes every tick, so a running job is never killed by
	// the clock. It is the ORPHAN-RECLAIM GRACE — how long a box outlives
	// an engine that has stopped heart-beating.
	TimeoutSec float64

	// PauseTTLSec bounds a PAUSED box: how long its snapshot is held before
	// the reaper reclaims it. 0 means never pause — always re-seed from the
	// pushed branch instead.
	PauseTTLSec float64

	// MaxTurns caps the agentic rounds a coding run may take. 0 is
	// uncapped, and is the default.
	//
	// Resolved here rather than passed alongside, because it is settled the
	// same way TimeoutSec and PauseTTLSec are — a provider default with a
	// per-seat override — and a second resolution path for the same shape
	// of knob is how the two come to disagree. [Launch] turns it into the
	// runner's [Limits].
	MaxTurns int

	// Env is the run environment: the LLM entry's env and sign-in, the
	// generic agent identity (CREWLET_AGENT_*), the setup steps' env, and
	// role.sandbox.env — which is where an operator DECLARES an external
	// token. The engine never names a tool-specific variable itself.
	Env map[string]string

	// CredentialFiles is a subscription-CLI login: a path relative to the
	// box home mapped to an absolute path on the ENGINE HOST.
	//
	// Each provider decides what to do with it, and they decide differently
	// on purpose. A local box seeds the files in and writes one its run
	// refreshed back — only over the very login it was seeded from, so an
	// idle box or a stale refresh never reverts a newer one; a REMOTE one
	// ignores them, because they carry a refresh token
	// whose rotation is shared fleet state, and pushing that onto somebody
	// else's VM is a materially larger trust step than the scoped headless
	// token the run env already exports.
	CredentialFiles map[string]string
}

// RunHandle points at a detached job.
//
// PERSISTED, which is the whole point: the command id and pid go into the
// pending-run row so a later turn — or a fresh engine after a restart — can
// reconnect to a job that is still running and collect its result.
type RunHandle struct {
	CommandID string
	PID       int
	SessionID string
}

// Result is one coding run's outcome.
type Result struct {
	Text    string
	Success bool

	// InputTokens is the run's WHOLE prompt count, its cached share
	// included, and CacheReadTokens / CacheWriteTokens are the share of it
	// the provider's prompt cache served and stored — the same convention
	// every engine provider reports on its completion, so a coding run's
	// tokens fold into the spend rollup beside the phases the engine ran
	// itself. A breakdown, never an addition: the total is input + output.
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	CostUSD          float64
	SessionID        string

	// Turns is how many model turns the run's CLI says its own loop took —
	// each one a provider call the engine neither made nor saw — and zero
	// where the CLI reported none. It is what the run's phase record counts
	// as its calls (tokens.PhaseCalls): a run whose CLI said nothing counts
	// as one, the one call the engine knows it asked for.
	//
	// AS REPORTED, never estimated: Claude Code states it on its result
	// line (`num_turns`); OpenCode's stream does not carry a count a
	// reader can trust — 2.0 prints a step's `step_start` for some steps
	// of a run and not others — so its runs report none.
	Turns int

	// DeliveredRefs are the branches and pull or merge requests the run
	// reported delivering, in its report's order: the `Delivered:` lines it
	// wrote, or the pull-request URLs its prose holds where it named none.
	// They are DISPLAY and a reminder — the run's phase record and the run's
	// report its reviewer reads carry them — and nothing judges a turn on
	// them: whether a turn reached anybody is the engine's own record of the
	// calls it made ([github.com/crewlet/crewlet/internal/agent/turn]).
	DeliveredRefs []string

	// DeliveredRefsElided is how many more refs the run reported than its
	// record lists ([MaxDeliveredRefBytes]). Set by the coordinator.
	DeliveredRefsElided int

	Error string

	// Event is how the CLI said its run ended IN FIELDS, beside the words of
	// Text and Error: its own terminal event, verbatim — Claude Code's
	// result line, whose `api_error_status` names a spent plan however the
	// sentence beside it is worded. It is what an executor's ending is
	// classified by first, as a call's is (cliagent.Provider.RunEnded), and
	// nothing else reads it. Set on a run that did not succeed, by a CLI that
	// prints such an event; empty otherwise, and wherever the line is past
	// [MaxRunEventBytes] ([boundBox]).
	Event string

	// Transcript is the agent's activity log — what it said, each tool it
	// called and the first line of what it ran — rebuilt from its own
	// output, or its stderr where the CLI streams nothing better. It is the
	// observability surface for an agent that emits no telemetry of its
	// own. A runner returns it WHOLE and redacted; the coordinator holds it
	// to the record's bound ([Coordinator.fitResult]) and redacts it again
	// at publish.
	Transcript string

	// TranscriptElidedLines and TranscriptElidedBytes are how much of the
	// transcript's middle the record's bound left out: whole lines, and
	// every byte not kept. Zero for a transcript kept whole. Set by the
	// coordinator, never by a runner.
	TranscriptElidedLines int
	TranscriptElidedBytes int

	// Condensed is what condensing this result's report, failure or
	// question to the record's bounds cost the seat's auxiliary model
	// ([Coordinator.fitResult]) — a failed rewrite included, since it was
	// still paid for. Set by the coordinator, never by a runner; the segment
	// that resumes from this collection pays it ([ResumeRequest.Engine]).
	Condensed AuxTokens
}

// usageFloored is r with every count of what the run spent made non-negative,
// and whether any had to be.
//
// A RUN REPORTS ITS OWN USAGE, parsed out of the coding CLI's last line of
// output — and that line is printed inside a box the run's own agent can run
// any command in. A negative count there is a REFUND: the charge would
// subtract it from the seat's budget counter, and the phase record from every
// rollup that sums it, so a run could buy itself headroom by printing a
// number. Nothing a run spends is negative, so a negative count is read as
// nothing spent, and the caller says so. Floored where every collected result
// enters the coordinator, so every backend's parser is covered by one rule.
func (r Result) usageFloored() (Result, bool) {
	floored := false
	floor := func(n *int) {
		if *n < 0 {
			*n, floored = 0, true
		}
	}
	floor(&r.InputTokens)
	floor(&r.OutputTokens)
	floor(&r.CacheReadTokens)
	floor(&r.CacheWriteTokens)
	floor(&r.Turns)
	if r.CostUSD < 0 {
		r.CostUSD, floored = 0, true
	}
	return r, floored
}

// Sandbox is one live, isolated execution environment.
type Sandbox interface {
	// ID is the provider's handle for this box, and what a reconnect needs.
	ID() string

	// Home is the absolute path this run's artefacts live under. A property
	// rather than a constant — see [DefaultHome].
	Home() string

	Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)

	// StartBackground launches a detached process and returns its handle
	// immediately. The job keeps running after the caller's turn ends and
	// writes its result to a file a later collect reads.
	StartBackground(ctx context.Context, cmd string, opts ExecOptions) (string, error)

	// Job reports what is known of the background job whose handle
	// StartBackground returned: still running, ended, or lost with the
	// engine process that last saw it running ([JobState]).
	//
	// ASKED OF THE BOX, because only the backend knows what its handle
	// names. Inside a remote VM or a container the handle is a pid in the
	// box's own process namespace, where nothing but the box's own work
	// can hold it. On the engine host it is a host pid, which the kernel
	// hands to any process once the job is gone: a probe that took the pid
	// on trust would read a stranger as the job and hold a dead run open
	// until the stranger exits. And only a box on the engine host can lose
	// its job with the engine: a remote box answers running or ended. An
	// error means the box could not be asked, which is not proof of any
	// answer.
	Job(ctx context.Context, commandID string) (JobState, error)

	// WriteFile puts content at path whole, by REPLACING the file there: the
	// content is staged under a fresh name beside the path and renamed over
	// it, so a process in the box reading the file meanwhile — a git
	// credential helper reading the token file the completion poll keeps
	// current under a running job — finds the old file or the new one,
	// never an empty or part-written one. A write that fails removes what it
	// staged, or says it could not, where a remote box stopped answering.
	// The new file has the mode the backend writes every file with,
	// whatever the replaced one had.
	//
	// A backend that writes the box from the engine host replaces only a
	// regular file where it lies, or puts one where nothing is, and refuses
	// anything else at the path with [ErrNotRegularFile], leaving it as the
	// box made it: the rename writes through no link, and a named pipe is
	// never opened to be waited on. A remote box is a machine of its own, so
	// what is at the path there is replaced, a link included.
	WriteFile(ctx context.Context, path string, content []byte) error

	// ReadFile reads a file the engine means to read WHOLE — a report, a
	// question, a marker, a result line — and REFUSES one past
	// [MaxFileBytes] ([ErrFileTooLarge], as a [*FileTooLargeError] naming the
	// cap it refused at) rather than returning its first part, because a
	// clipped report reads as a finished one. Empty on missing: a poll for a
	// marker that is not written yet is not an error.
	//
	// NEVER FOR A MACHINE STREAM. A coding agent's event log and its stderr
	// grow with the run and have no size a whole read could honestly refuse
	// at — the engine keeps a bounded share of them in the end — so they
	// are read with [Sandbox.OpenFile] or [Sandbox.ReadTail] instead.
	//
	// A path that names something other than a regular file is refused with
	// [ErrNotRegularFile] by a backend that reads the box from the engine
	// host, for all three reads, because opening one there could block.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// OpenFile streams a file front to back, for a MACHINE STREAM decoded
	// once in bounded memory: a coding agent's event log, read at
	// collection a line at a time. Nothing is refused for its size, since
	// the reader holds one piece at a time and decides itself what to keep.
	// A missing file is a reader that yields nothing, as ReadFile's empty
	// answer is; the caller closes it.
	OpenFile(ctx context.Context, path string) (io.ReadCloser, error)

	// ReadTail reads at most n bytes from the END of a file, with the
	// file's whole size — for a question about a stream's end: what is the
	// job doing now, how far has its stream grown, what did it say last
	// before it failed. A whole read answers each of those at the cost of
	// the whole stream, every poll, for as long as the run lasts. Empty on
	// missing, as the others are.
	ReadTail(ctx context.Context, path string, n int) (FileTail, error)

	// SetTimeout resets the box's wall-clock TTL to seconds from now.
	//
	// THE KEEPALIVE. A provider reclaims a box that many seconds after the
	// TTL was last set, and the waiter calls this every poll tick — so a
	// running box is bounded only by how long the engine can go WITHOUT a
	// heartbeat, never by a fixed run deadline. A provider with no settable
	// TTL no-ops.
	SetTimeout(ctx context.Context, seconds float64) error

	// Pause snapshots the box for a later resume, holding a run blocked on
	// a clarification with exact conversational continuity. A provider
	// without snapshots no-ops, and the engine re-seeds from the pushed
	// branch instead.
	Pause(ctx context.Context) error

	Close(ctx context.Context) error
}

// FileTail is the end of a file, read by [Sandbox.ReadTail].
type FileTail struct {
	// Data is the file's last bytes — all of it when the file is no longer
	// than what was asked for. It begins wherever the byte count put it, so
	// a reader of lines drops a partial first one ([FileTail.Lines]).
	Data []byte

	// Size is the whole file's size as the read found it, so a caller can
	// say how much came before Data rather than present the end as the
	// whole.
	Size int64
}

// Whole reports whether Data is the entire file.
func (t FileTail) Whole() bool { return int64(len(t.Data)) >= t.Size }

// Before is how many bytes of the file came before Data and were not read.
func (t FileTail) Before() int64 { return max(t.Size-int64(len(t.Data)), 0) }

// Lines is Data without a first line the window began inside, and how many
// bytes that partial line held. A tail that is the whole file begins at a
// line, so nothing is dropped from it; one that began mid-file begins
// wherever the byte count landed, and the bytes before its first line break
// are the end of a line nobody can read whole from here.
func (t FileTail) Lines() ([]byte, int) {
	if t.Whole() {
		return t.Data, 0
	}
	i := bytes.IndexByte(t.Data, '\n')
	if i < 0 {
		return nil, len(t.Data)
	}
	return t.Data[i+1:], i + 1
}

// tailOf is the end of a file held in memory, in the shape every backend's
// [Sandbox.ReadTail] answers.
func tailOf(content []byte, n int) FileTail {
	n = max(n, 0)
	if len(content) <= n {
		return FileTail{Data: content, Size: int64(len(content))}
	}
	return FileTail{Data: content[len(content)-n:], Size: int64(len(content))}
}

// ExecOptions are the per-command knobs both exec shapes take.
type ExecOptions struct {
	Env        map[string]string
	Cwd        string
	TimeoutSec float64
}

// JobState is what is known of a detached job ([Sandbox.Job], [Runner.Poll]).
//
// THREE ANSWERS, because "not running" is two facts that a run is carried
// forward from differently. A job that ENDED finished, or died while an engine
// that had seen it running was still there to see it go: what it left is the
// round's result, a crash included, and it is collected. A job that was LOST
// went with the engine process that last saw it running, before any engine
// could see it end: a `direct` box's process tree is the engine host's, and
// the restart that replaces the engine — its container restarted, its systemd
// unit stopped with every process in its control group, its host rebooted —
// kills the job in the same instant, mid-step, with nothing written to say so.
// What it left is not a result, and its round is carried on rather than
// collected ([PendingRun.Lost]).
type JobState string

const (
	// JobRunning is a job still working — or hung, which nothing tells apart
	// from working without a clock the engine deliberately does not impose.
	JobRunning JobState = "running"
	// JobEnded is a job that is over and was seen to end: it wrote its done
	// marker, or its process went while an engine that had watched it run was
	// still there.
	JobEnded JobState = "ended"
	// JobLost is a job that is over and that nothing alive saw end: it went
	// with the engine process that last saw it running, or after it, before
	// the engine that replaced it could look.
	JobLost JobState = "lost"
)

// JobStates are the three.
var JobStates = []JobState{JobRunning, JobEnded, JobLost}

// Valid reports whether this build knows s.
func (s JobState) Valid() bool { return slices.Contains(JobStates, s) }

// Over reports whether the job is no longer running, whichever way it went.
func (s JobState) Over() bool { return s == JobEnded || s == JobLost }

// ErrBoxGone is a [Provider.Connect] that found the box DEFINITIVELY not
// there — reclaimed by its provider, reaped, its directory removed — as
// against one that merely could not be reached. The difference decides
// whether asking again can help: a collection that cannot reach a box is
// retried, and one whose box is gone is settled at once.
var ErrBoxGone = errors.New("sandbox: the box is gone")

// boxGone is a backend's own sentence saying a box no longer exists, which is
// also [ErrBoxGone] — the sentence is what a person reads, the sentinel what
// a caller acts on.
type boxGone struct{ err error }

func (g boxGone) Error() string   { return g.err.Error() }
func (g boxGone) Unwrap() []error { return []error{g.err, ErrBoxGone} }

// ErrBoxPaused is a [Provider.Attach] that found the box PAUSED where its
// backend cannot read a paused box without waking it — an E2B snapshot, which
// has no envd to answer, or a paused container, which takes no exec. The box
// is there; reading it would mean resuming it, which is the one thing the
// caller asked not to do.
var ErrBoxPaused = errors.New("sandbox: the box is paused")

// boxPaused is a backend's own sentence saying a box is paused, which is also
// [ErrBoxPaused].
type boxPaused struct{ err error }

func (p boxPaused) Error() string   { return p.err.Error() }
func (p boxPaused) Unwrap() []error { return []error{p.err, ErrBoxPaused} }

// Provider mints sandboxes. Configured under providers.sandbox and swapped
// wholesale on an apply, mirroring the LLM providers beside it.
type Provider interface {
	// Kind names the backend, for logs and the operator surface.
	Kind() string

	Create(ctx context.Context, spec Spec) (Sandbox, error)

	// Connect reattaches to an existing box by id, live or paused.
	//
	// The detached lifecycle rests on this: the completion turn — possibly
	// in a fresh engine after a restart — reattaches to the box that ran the
	// job, collects its result and tears it down. A PAUSED box auto-resumes
	// on connect, which is why the reaper must not use it, and why neither
	// may a reader ([Provider.Attach]).
	Connect(ctx context.Context, sandboxID string) (Sandbox, error)

	// Attach reattaches to an existing box by id WITHOUT RESUMING IT, for a
	// caller that only reads: a peek at a running job's output, and the
	// completion poll.
	//
	// THE READ MUST CHANGE NOTHING, and through Connect it could not: a
	// reader that took a running record, then reached the box after the
	// collection had read it and paused it, woke the box back up — and on
	// E2B, where nothing renews a box once its record stops running, the
	// woken box was killed at its TTL and the parked run lost the snapshot
	// it had been paused to keep. A box this backend cannot read while it
	// is paused is refused with [ErrBoxPaused]; one whose files the engine
	// host reads directly is handed back as it is, paused or not. A box
	// that is gone is [ErrBoxGone], as for Connect. Nothing about the box's
	// timer or its state moves.
	Attach(ctx context.Context, sandboxID string) (Sandbox, error)

	// Kill terminates a box by id WITHOUT resuming it.
	//
	// The primitive the pause reaper needs. Connect auto-resumes, so
	// reclaiming a paused snapshot through it would boot the VM back up
	// purely to kill it — paying for a resume to pay for a shutdown.
	// Best-effort: a box that is already gone is not an error.
	Kill(ctx context.Context, sandboxID string) error
}

// Runner runs one coding agent inside a box against a brief.
//
// TWO SHAPES, ONE RUNNER: the inline Run (start, block, result — for tests and
// short jobs) and the detached Start/Poll/Collect triple the engine drives
// across turns. Run is Start followed by Collect on the same handle.
type Runner interface {
	Name() string

	// Install puts the agent in the box. Separate from Create because a
	// reused box already has it, and re-installing on every turn is the
	// slowest thing in a coding turn's critical path.
	Install(ctx context.Context, box Sandbox) error

	Start(ctx context.Context, box Sandbox, req RunRequest) (RunHandle, error)

	// Poll reports what is known of the background job: running, ended —
	// finished, or seen to die — or lost with the engine that last saw it
	// running ([JobState]).
	Poll(ctx context.Context, box Sandbox, handle RunHandle) (JobState, error)

	// Collect reads the finished job's result out of the box.
	Collect(ctx context.Context, box Sandbox, handle RunHandle) (Result, error)

	// Follow begins a LIVE READING of what a job says about itself, for a
	// person watching the run ([LiveReading]): advanced a read at a time,
	// each read adding what the job has written since, settled and
	// redacted, without ending, pausing or otherwise touching the job. Safe
	// on a job in any state: one that has finished says so, and one that
	// has said nothing yet reads as nothing rather than as an error.
	Follow(handle RunHandle) LiveReading
}

// RunRequest is one coding run's inputs.
//
// NEVER PERSISTED: a run is started once, by the node that launched it, and
// everything a later collection or resume needs is on the run's row and in the
// box. So a field added here changes no record a peer of another build reads.
type RunRequest struct {
	// System is the run's standing instructions, which the runner hands its
	// CLI on the channel that ADDS to the CLI's own system prompt rather than
	// replacing it: the vendor's prompt is what teaches the harness this run
	// exists to use. A runner adds its own standing instruction to it — the
	// report the run writes before it stops, at the box's path for it.
	System string

	// Brief is the task: the prompt the CLI runs, and nothing standing.
	Brief string

	// Resume is the CLI's own session the run CONTINUES, as an earlier run in
	// the same box reported it ([Result.SessionID]): the conversation, its
	// tool results and its working files are where that run left them, and
	// Brief is its next message — Claude Code's `--resume <id>`, OpenCode's
	// `--session <id>`. A seat's job sets it where the job before it in this
	// box parked on a person's question and the answer has come; the
	// assistant's deep work, where its run parked on a proposal and the
	// approval has come. Empty starts a new session.
	//
	// THE CLI'S SESSION AND NOT A SUMMARY OF IT, because the run that asked
	// knew what it had read, tried and decided, and a fresh conversation
	// told only the question and the answer starts that work again. It holds
	// only in the box the session lives in, which is why a seat's launch
	// sets it only where the job continues in the asking job's box
	// ([LaunchRequest.Continue]). A runner whose CLI cannot continue a
	// session refuses a run that asks it to, rather than start a new
	// conversation its caller believes is the old one.
	Resume string

	Env    map[string]string
	Limits Limits

	// Model is the model the run works under, written in the grammar of the
	// CLI the runner drives: the seat's providers.llm entry's model exactly
	// as its runs on the engine host hand it to that same CLI, which is the
	// only CLI the engine lets reach this runner. Empty leaves the agent's
	// own choice.
	//
	// NO ENDPOINT AND NO KEY: an entry talks to its CLI's own vendor, and its
	// sign-in rides the run env, so nothing secret is written into a file
	// inside the box.
	Model string

	// MCPServers is the scoped MCP surface, keyed by server name.
	// SERVER-LEVEL SCOPING ONLY — there is no per-tool allowlist, because
	// the agent inside the box negotiates its own tool list with the server
	// and an allowlist the engine could not enforce would be a claim rather
	// than a control.
	MCPServers map[string]MCPServer

	// Steer is how the run's CLI reaches a person's notes to its turn while
	// it works — see [SteerWiring]. The zero value takes no note inside the
	// run: its notes wait for whatever reads the turn next, which is what a
	// run with no tool bridge — the assistant's deep work — has. NEVER
	// PERSISTED: it names the job's own endpoint, and a resumed turn's next
	// launch is handed its own.
	Steer SteerWiring
}

// SteerWiring is where a run's CLI asks for its turn's notes: one tool of the
// MCP server the run's tool bridge is ([RunRequest.MCPServers]), which a hook
// of the CLI's calls between its steps, and the URL of the bridge's notes
// route, which a CLI that admits notes from a plugin of its own polls. The
// engine fills it from the same names its bridge serves, so the box and the
// bridge cannot disagree about them (internal/api/mcpbridge).
type SteerWiring struct {
	// Server is the name the bridge is listed under in the run's MCP
	// configuration, and Tool the tool a hook calls on it — never listed,
	// so no model is offered it.
	Server string
	Tool   string

	// URL is the job's notes route: the bridge endpoint the box already
	// dials, with the route's suffix.
	URL string
}

// Set reports whether the wiring names anything a CLI could reach.
func (w SteerWiring) Set() bool { return w.Server != "" && w.Tool != "" && w.URL != "" }

// Run is the inline shape: start, wait, collect.
//
// A helper rather than an interface method, so a Runner implements three
// primitives instead of four and the two shapes cannot drift apart — the
// blocking one IS the detached one with a wait in the middle.
func Run(ctx context.Context, r Runner, box Sandbox, req RunRequest, wait WaitFunc) (Result, error) {
	handle, err := r.Start(ctx, box, req)
	if err != nil {
		return Result{}, err
	}
	if err := wait(ctx, func(ctx context.Context) (bool, error) {
		state, err := r.Poll(ctx, box, handle)
		return state.Over(), err
	}); err != nil {
		return Result{}, err
	}
	return r.Collect(ctx, box, handle)
}

// WaitFunc blocks until a predicate holds or the context ends. Injected so the
// inline shape's polling cadence is the caller's choice — a test wants none.
type WaitFunc func(ctx context.Context, done func(context.Context) (bool, error)) error

// probeByKill answers whether a job is running for a box whose handle is a pid
// in the box's own process namespace, by running `kill -0` inside it.
//
// The handle is parsed rather than interpolated: it is read back from the run's
// record in the coordination store, and a shell command is the wrong place to
// find out it was not a number.
func probeByKill(ctx context.Context, box Sandbox, commandID string) (bool, error) {
	pid, err := strconv.Atoi(commandID)
	if err != nil || pid <= 0 {
		return false, fmt.Errorf("sandbox: job handle %q is not a process id", commandID)
	}
	res, err := box.Exec(ctx, "kill -0 "+strconv.Itoa(pid)+" 2>/dev/null", ExecOptions{})
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

// remoteJob answers [Sandbox.Job] for a box whose job runs in a machine of its
// own, which the engine's restart does not reach — an E2B VM, and the fake that
// stands in for one: running while `kill -0` there says so ([probeByKill]),
// and ended otherwise. Such a job is never lost with the engine: it outlives
// every engine process, and whichever one finds it gone found it gone.
func remoteJob(ctx context.Context, box Sandbox, commandID string) (JobState, error) {
	running, err := probeByKill(ctx, box, commandID)
	switch {
	case err != nil:
		return "", err
	case running:
		return JobRunning, nil
	}
	return JobEnded, nil
}
