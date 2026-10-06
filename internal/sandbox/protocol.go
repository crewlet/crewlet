// Package sandbox is the code-work runtime: a sandbox-enabled seat runs its
// executor phase as a coding agent inside an isolated box instead of the native
// tool loop.
//
// The shape that matters is that a coding run is DETACHED. The tool starts it,
// the Execute loop SUSPENDS, and the engine RESUMES the same loop with the
// result spliced in when the run completes — possibly minutes later, possibly
// in a different process after a restart, possibly on a different node. Nothing
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
// lost the answer outright. The rule, and what it does with a row parked
// before a conversation identity was written, is [ConversationRef.Answers].
//
// See docs/concepts/code-sandbox.md.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

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
const DefaultHome = "/home/user"

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
	// role.sandbox.run_in over providers.sandbox.default_run_in, and carried
	// here rather than chosen by the caller so the launch, the pending row
	// and the reconnect all name the same cell.
	Placement Placement

	// CodingAgent names the runner: "claude-code", "opencode", …
	CodingAgent string

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

	// Env is the run environment: LLM credentials, the generic agent
	// identity (CREWLET_AGENT_*), the setup steps' env, and role.sandbox.env
	// — which is where an operator DECLARES an external token. The engine
	// never names a tool-specific variable itself.
	Env map[string]string

	// CredentialFiles is a subscription-CLI login: a path relative to the
	// box home mapped to an absolute path on the ENGINE HOST.
	//
	// Each provider decides what to do with it, and they decide differently
	// on purpose. A local box seeds the files in and writes a refreshed one
	// back; a REMOTE one ignores them, because they carry a refresh token
	// whose rotation is shared fleet state, and pushing that onto somebody
	// else's VM is a materially larger trust step than the scoped headless
	// token the run env already exports.
	CredentialFiles map[string]string
}

// AgentLLM is the seat's resolved model and endpoint, for a coding agent that
// must configure its own provider rather than read credentials from the env.
//
// Some agents resolve a bare "<provider>/<model>" against a catalogue AND the
// vendor's default endpoint — so a custom gateway plus an unlisted model id
// either fails to resolve or silently hits the wrong host. A runner uses this
// to declare a provider with an explicit base URL and the exact model,
// bypassing both. An agent that reads its credentials from the environment
// ignores it.
//
// THE API KEY IS NOT HERE. It rides the run env, and a written config
// references it by variable name, so the secret is never duplicated into a
// config file inside the box.
type AgentLLM struct {
	Model string

	// ProviderType is the model FAMILY to address. For an API entry that is
	// the providers.llm type, which already names the family; for a
	// subscription entry every provider shares one type, so it carries the
	// CLI profile's VENDOR instead — otherwise a Claude subscription's
	// "sonnet" would be addressed as "openai/sonnet".
	ProviderType string

	// BaseURL is the endpoint. Empty means the vendor default, and so no
	// custom provider declaration at all.
	BaseURL string
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

	// NeedsInput means the agent asked a question and stopped. Question and
	// AskTo say what it asked and who should answer: "requester", "team",
	// "manager", or a name.
	NeedsInput bool
	Question   string
	AskTo      string

	// DeliveredRefs are the branches and pull requests the run produced —
	// what the delivery gate judges a coding turn on.
	DeliveredRefs []string
	ChangedFiles  []string
	Commands      []string

	Error string

	// Transcript is the agent's streamed activity log — tool calls, shell
	// commands, todos — captured from its output. It is the observability
	// surface for an agent that emits no telemetry of its own. Tail-capped
	// here, redacted at publish.
	Transcript string
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

	// JobRunning reports whether the background job whose handle
	// StartBackground returned is still running.
	//
	// ASKED OF THE BOX, because only the backend knows what its handle
	// names. Inside a remote VM or a container the handle is a pid in the
	// box's own process namespace, where nothing but the box's own work
	// can hold it. On the engine host it is a host pid, which the kernel
	// hands to any process once the job is gone: a probe that took the pid
	// on trust would read a stranger as the job and hold a dead run open
	// until the stranger exits. An error means the box could not be asked,
	// which is not proof of either answer.
	JobRunning(ctx context.Context, commandID string) (bool, error)

	WriteFile(ctx context.Context, path string, content []byte) error

	// ReadFile reads a file the engine means to read WHOLE — a report, a
	// question, a marker, a result line — and REFUSES one past
	// [MaxFileBytes] ([ErrFileTooLarge]) rather than returning its first
	// part, because a clipped report reads as a finished one. Empty on
	// missing: a poll for a marker that is not written yet is not an error.
	//
	// NEVER FOR A MACHINE STREAM. A coding agent's event log and its stderr
	// grow with the run and have no size a whole read could honestly refuse
	// at — the engine keeps a bounded share of them in the end — so they
	// are read with [Sandbox.OpenFile] or [Sandbox.ReadTail] instead.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// OpenFile streams a file front to back, for a MACHINE STREAM decoded
	// once in bounded memory: a coding agent's event log, read at
	// collection a line at a time. Nothing is refused for its size, since
	// the reader holds one piece at a time and decides itself what to keep.
	// A missing file is a reader that yields nothing, as ReadFile's empty
	// answer is; the caller closes it.
	OpenFile(ctx context.Context, path string) (io.ReadCloser, error)

	// ReadTail reads at most n bytes from the END of a file, with the
	// file's whole size — for a question about a stream's end: has the job
	// printed its terminal event, what is it doing now, what did it say last
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
	// on connect, which is why the reaper must not use it.
	Connect(ctx context.Context, sandboxID string) (Sandbox, error)

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

	// Poll reports whether the background job has finished.
	Poll(ctx context.Context, box Sandbox, handle RunHandle) (bool, error)

	// Collect reads the finished job's result out of the box.
	Collect(ctx context.Context, box Sandbox, handle RunHandle) (Result, error)

	// Peek reads what a job has said about itself SO FAR, without ending,
	// pausing or otherwise touching it — the live output a person watching
	// a run is shown ([Output]). Safe on a job in any state: one that has
	// finished says so, and one that has said nothing yet answers an empty
	// output rather than an error.
	Peek(ctx context.Context, box Sandbox, handle RunHandle) (Output, error)
}

// RunRequest is one coding run's inputs.
type RunRequest struct {
	Brief  string
	Env    map[string]string
	Limits Limits

	// LLM is the seat's model and endpoint, for a runner that configures its
	// own provider. Nil leaves the agent to read the env.
	LLM *AgentLLM

	// MCPServers is the scoped MCP surface, keyed by server name.
	// SERVER-LEVEL SCOPING ONLY — there is no per-tool allowlist, because
	// the agent inside the box negotiates its own tool list with the server
	// and an allowlist the engine could not enforce would be a claim rather
	// than a control.
	MCPServers map[string]MCPServer
}

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
		return r.Poll(ctx, box, handle)
	}); err != nil {
		return Result{}, err
	}
	return r.Collect(ctx, box, handle)
}

// WaitFunc blocks until a predicate holds or the context ends. Injected so the
// inline shape's polling cadence is the caller's choice — a test wants none.
type WaitFunc func(ctx context.Context, done func(context.Context) (bool, error)) error

// probeByKill answers [Sandbox.JobRunning] for a box whose handle is a pid in
// the box's own process namespace, by running `kill -0` inside it.
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
