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
// See docs/concepts/code-sandbox.md.
package sandbox

import (
	"context"

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
// There are deliberately NO CPU, MEMORY OR DISK FIELDS. A box's resources are a
// property of its TEMPLATE, fixed when the template is built, and the create
// APIs accept no resource arguments. Sizing is done by pointing Template at one
// built with the resources you want, not by a per-run field that would silently
// do nothing.
type Spec struct {
	// CodingAgent names the runner: "claude-code", "opencode", …
	CodingAgent string

	// Template is the provider's image or template id. Empty takes the
	// provider's default.
	Template string

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

	InputTokens  int
	OutputTokens int
	CostUSD      float64
	SessionID    string

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

	WriteFile(ctx context.Context, path string, content []byte) error
	ReadFile(ctx context.Context, path string) ([]byte, error)

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

// ExecOptions are the per-command knobs both exec shapes take.
type ExecOptions struct {
	Env        map[string]string
	Cwd        string
	TimeoutSec float64
}

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
	MCPServers map[string]map[string]any
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
