// Package subagent runs the workers a turn's executor delegates to.
//
// A worker is one tool loop with a persona, a slice of the parent's own
// tools, a declared answer shape and hard caps on rounds, wall-clock and
// tokens. It is a LEAF: it cannot delegate further and it cannot contact
// colleagues, which is the only thing bounding the depth of the whole
// construct — there is no depth counter anywhere, because a leaf needs none.
//
// Three files, three concerns: this one is the boundary and the loop,
// workflow.go is the task graph, result.go is how a worker answers.
//
// Five properties are load-bearing. Each is enforced in code, never in the
// prompt, because a prompt is a request and these are boundaries:
//
//   - THE GRANT IS A SECURITY BOUNDARY. A worker's reachable set is the
//     parent's own tools, minus the engine-control denylist, minus anything
//     whose annotations say it writes to a shared surface. Both halves of
//     "reachable" are covered — what was NAMED and what the worker can
//     DISCOVER — because discovery runs against the filtered snapshot, not
//     the parent's. Discovery over the parent's catalogue is how a worker
//     would otherwise promote a tool nobody granted it. A CONFIG TEMPLATE
//     CHANGES NOTHING HERE: `workers:` names tools, and every name still
//     passes this filter, so Tier B config can never be an escalation path.
//
//   - THE BUDGET SLICE IS THE CALL'S, NOT EACH WORKER'S. One fraction of the
//     parent's remaining tokens is computed once and shared by every task in
//     the call. The alternative — a fresh fraction per task — over-allocates
//     by N, which is the whole cost of a fan-out being invisible until the
//     org budget is gone.
//
//   - A WORKER'S FAILURE IS A RESULT, NOT AN ERROR. Timeout, budget
//     exhaustion, a skipped dependency and a panic all come back as a Result
//     with a status and the partial transcript intact. The trap is a path
//     that returns early: the phase-failure guard never sees it and the call
//     publishes NOTHING — the parent's execute event shows a delegate call
//     whose worker left no phase record, no partial transcript and no reason
//     it stopped. Here every path produces exactly one Result carrying all
//     three, so the caller's phase event cannot be missing.
//
//   - AN ERROR MEANS NOTHING RAN. Run returns an error only for a request or
//     a wiring that could not start any task at all. That split is what lets
//     the tool answer the model with a refusal in one case and with the
//     workers' own answers in the other.
//
//   - AN ABSENT ANSWER IS NEVER SYNTHESISED. A worker that never submitted
//     reports `no_result` with its prose, and a task waiting on it is
//     skipped rather than fed a fabricated answer. See result.go.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tools"
)

var log = logging.Get("agent.subagent")

// ToolName is the name the executor calls to hand work to a worker.
//
// A constant because it is also the first entry of the denylist below: the
// tool that starts a worker is the tool a worker must never see, and two
// spellings of that name is one recursion away from a fork bomb.
const ToolName = "delegate"

// skillLoaderTool is the read-only prompt-fragment loader.
//
// Granted to every child that can reach it even when the parent did not name
// it, because the child's own prompt tells it to call this — the tool-skill
// catalogue rendered into the system prompt is a list of `key — summary`
// lines ending in "call load_tool_skill(key)". A child told to call a tool it
// was not given spends a round discovering that, every time.
//
// It cannot widen anything: it returns prompt text and touches nothing.
const skillLoaderTool = "load_tool_skill"

// The classified reasons a child stopped short. They ride the Result and
// become the phase event's error_kind, which is what a dashboard groups on —
// so they are wire strings, not an internal enum.
const (
	// KindTimeout — the child's own cap or the batch's expired.
	KindTimeout = "timeout"
	// KindBudget — the fractional slice refused a charge.
	KindBudget = "budget_exhausted"
	// KindPanic — something in the child's stack panicked and was
	// contained.
	KindPanic = "panic"
	// KindCancelled — the parent turn was torn down under it.
	KindCancelled = "cancelled"
	// KindFailed — anything else the loop returned: a provider that would
	// not answer, a surface that broke.
	KindFailed = "failed"
)

// ScopeSubagent is the budget scope a refused sub-agent charge names.
//
// It exists so the refusal cannot be read as the PARENT's budget running out.
// A bare boolean spend outcome cannot express it, and the loop then publishes
// a budget_exhausted event pointing at the seat's own cap — which sends an
// operator to raise a limit that was never reached.
// [toolloop.SpendOutcome] carries the scope, so the honest answer is simply a
// different scope name.
const ScopeSubagent = "subagent"

// errorLimit caps the failure text carried back to the model, in runes.
//
// A panic's message can arrive with a stack behind it and a provider error can
// carry a whole response body; either would blow past the tool result the
// parent reads. 500 runes holds two or three sentences — enough to say what
// stopped, never enough to bury the sibling results it is rendered next to.
const errorLimit = 500

// controlDenylist is the first-party engine-control surface a sub-agent never
// gets, whatever the parent names.
//
// These are Crewlet's OWN tools, so naming them is not a tool-stack
// dependency — it encodes the runtime invariants. Writes to external shared
// surfaces are deliberately NOT listed by name: that would couple the engine
// to one stack and silently pass every other. They are denied by annotation;
// see [mcp.WritesToSharedSurface].
var controlDenylist = map[string]struct{}{
	// A sub-agent that can spawn sub-agents has no depth bound at all.
	ToolName: {},

	// Launching a detached coding run is engine control keyed to the
	// PARENT: the pending row carries the parent's turn id and the
	// completion pauses the parent seat's inbox. A sub-agent's loop cannot
	// suspend, so the parent turn would finish normally, never persist an
	// execute state, and the seat would stay deaf for the whole coding run
	// with nothing to resume into. It carries no annotations either, so the
	// shared-write filter does not catch it.
	"run_sandbox": {},

	// The PARENT's discovery pair. Both close over the parent's surface, so
	// a child that inherited them would activate tools onto the surface of
	// the phase that spawned it — the one escalation the grant exists to
	// stop. The child gets its own pair, bound to its own surface, from
	// Config.Discovery.
	"activate_tool":         {},
	"list_mcp_server_tools": {},

	// Cross-agent communication. A short-lived worker's half-formed
	// conclusions must not land on a teammate's desk under the parent's
	// name. The last three are not engine tools today; they are denied
	// defensively in case an extension registers them.
	"a2a_ask":             {},
	"request_a2a_channel": {},
	"send_a2a_message":    {},
	"close_a2a_channel":   {},
}

// Denied reports whether a tool name is on the engine-control denylist.
//
// Exported because it is half of the security boundary and a caller
// assembling a parent surface should be able to ask the same question this
// package answers, rather than keeping a second list of names.
func Denied(name string) bool { _, ok := controlDenylist[name]; return ok }

// Limits are the runtime caps one delegate call runs under. They come from
// the company config's turn_engine.delegation block, which validates every
// one of them and carries the reasoning for each number.
type Limits struct {
	// MaxTurns caps a worker's tool rounds. A request above it is
	// CLAMPED, never refused: the request is a hint about how much work
	// the task needs, and refusing a hint costs the whole call.
	MaxTurns int

	// MaxTasksPerCall bounds one call's graph. Unlike MaxTurns this IS a
	// refusal, because a call with too many tasks is a misunderstanding of
	// the tool rather than an over-estimate: clamping it would silently
	// drop tasks the parent is about to read answers for.
	MaxTasksPerCall int

	// TaskTimeout bounds one worker.
	TaskTimeout time.Duration

	// CallTimeout bounds the whole call, waves included. Both apply — the
	// per-task cap stops one straggler, this one stops eight of them each
	// finishing just inside their own cap.
	CallTimeout time.Duration

	// MaxParallel caps concurrency across the call. Tasks beyond it run as
	// earlier ones finish.
	MaxParallel int

	// BudgetFraction is the share of the parent's REMAINING tokens one
	// call may consume — the total across every task, not each.
	BudgetFraction float64

	// MinTokensPerTask floors each task's share. A call whose total
	// divided by its task count falls below this is refused UP FRONT,
	// rather than started with every worker too poor to finish: N workers
	// that each die mid-round have spent the whole slice and produced
	// nothing, which is strictly worse than not starting.
	MinTokensPerTask int
}

// validate refuses a zero rather than defaulting it.
//
// Every field here is a cap, and a Go zero is not "unset" — it is the most
// destructive possible setting (no rounds, an expired deadline, no
// concurrency). The config layer applies the shipped defaults and validates
// exactly these bounds, so reaching this error means a caller assembled
// Limits by hand and left a field out; saying which field is the whole value.
func (l Limits) validate() error {
	var errs []error
	if l.MaxTurns < 1 {
		errs = append(errs, fmt.Errorf("limits: MaxTurns must be at least 1, got %d", l.MaxTurns))
	}
	if l.MaxTasksPerCall < 1 {
		errs = append(errs, fmt.Errorf("limits: MaxTasksPerCall must be at least 1, got %d",
			l.MaxTasksPerCall))
	}
	if l.TaskTimeout <= 0 {
		errs = append(errs, fmt.Errorf("limits: TaskTimeout must be positive, got %v", l.TaskTimeout))
	}
	if l.CallTimeout <= 0 {
		errs = append(errs, fmt.Errorf("limits: CallTimeout must be positive, got %v", l.CallTimeout))
	}
	if l.MaxParallel < 1 {
		errs = append(errs, fmt.Errorf("limits: MaxParallel must be at least 1, got %d", l.MaxParallel))
	}
	if l.BudgetFraction <= 0 || l.BudgetFraction > 1 {
		errs = append(errs, fmt.Errorf(
			"limits: BudgetFraction must be in (0, 1], got %v — it is a SHARE of the "+
				"parent's remaining budget, not a token count", l.BudgetFraction))
	}
	if l.MinTokensPerTask < 0 {
		errs = append(errs, fmt.Errorf("limits: MinTokensPerTask must not be negative, got %d",
			l.MinTokensPerTask))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("subagent: %w", err)
	}
	return nil
}

// Config is everything one parent turn's spawns need. Built once per Execute
// phase and shared by every spawn in it, including every child of a batch.
type Config struct {
	// Seat is the parent. Its role picks the provider chain and names the
	// event's actor.
	Seat prompts.Seat

	// Models resolves the seat's per-phase provider chain. Workers run on
	// [phase.Subagent] — the llm_subagent keys — which is what lets an
	// operator put fan-out work on a cheap model without touching the
	// model the seat itself thinks on. A template or a task naming its own
	// `model` overrides that with one configured key and no fallback
	// chain: an explicit choice that quietly ran somewhere else would be
	// worse than a refusal.
	Models *phase.Registry

	// Universe is the PARENT phase's tool snapshot: everything a name could
	// resolve to. Parent is what the parent may itself call. The child's
	// reachable set is derived from BOTH — the snapshot alone is the whole
	// registry, and granting from that would hand a child tools the parent
	// never had.
	Universe tools.Snapshot

	// Parent is what the parent may itself call, read through a getter for
	// the same reason Discovery is: the parent's ACTIVE list is live. An
	// executor that discovers a tool mid-phase and activates it has widened
	// what it may call, and a child spawned afterwards should inherit that
	// — with a frozen slice the tool is silently rejected by Permit's
	// second filter and reported to the model as refused, which reads as a
	// permission decision nobody made. Nil, or a nil result, means the
	// parent may call nothing and every request is refused.
	Parent func() []string

	// Discovery builds the child's own discovery meta-tools, bound to the
	// child's surface through a getter.
	//
	// A getter because there is a real cycle: activate must mutate the same
	// Surface the loop reads its definitions from, so the tools cannot
	// exist before the surface, and the surface cannot resolve them until
	// they are in its snapshot. Nil leaves the child frozen to what it was
	// granted, which is the right shape for a caller that does not want a
	// worker widening itself at all.
	Discovery func(surface func() *tools.Surface) []tools.Callable

	// Workers are the templates this seat may delegate to, already
	// narrowed to what its role can see. A task naming one that is not
	// here is refused with the visible names listed — the seat's own
	// visibility is the boundary, and a refusal that named the whole
	// company's library would be telling the model about workers it
	// cannot have.
	Workers map[string]config.Worker

	// Skills renders the tool-skill catalogue into the worker's prompt.
	// Nil keeps the prompt free of skill scaffolding.
	Skills prompts.SkillCatalogue

	// Budget is the parent turn's shared token counter. Nil disables
	// charging entirely, which is the embedded single-node case.
	Budget toolloop.BudgetMeter

	// ParentRemaining is the parent seat's remaining token allowance, read
	// once when the phase started. ZERO MEANS UNCAPPED: a seat with no
	// per-agent budget imposes no fractional cap on its children either,
	// exactly as the parent itself runs uncapped. It is a value rather than
	// a live read because the fraction must be one number for a whole
	// batch — re-reading it per child would hand later children a share of
	// a total their siblings had already spent from.
	ParentRemaining int

	Limits Limits

	// Publisher receives the one SubagentBatched event a batch emits. Nil
	// skips it; publishing is telemetry and must never fail a spawn.
	Publisher queue.Publisher

	// Trace is the parent turn's trace context, so a child's event hangs
	// under the turn that spawned it.
	Trace events.TraceContext

	// Turn binds the child's surface to the parent turn.
	//
	// Without it every seat-bound tool in the grant fails at call time
	// rather than at grant time: lookup_colleague answers "no organization
	// is in scope", use_skill answers "can only be called during a turn".
	// A child handed tools that always fail is worse than a child that was
	// not granted them — it spends rounds discovering the refusal.
	//
	// Nil keeps the unbound behaviour, which is what a caller with no seat
	// in scope (a test, a tool exercised directly) actually has.
	Turn *turnctx.Turn

	// Guard builds the child's load-before-use gate FROM THE CHILD'S OWN
	// finished surface, so a child cannot reach by being spawned a tool
	// its parent would have had to load a skill for.
	//
	// A factory rather than a value, for the reason Discovery is a getter:
	// what a guard enforces is derived from an ACTIVE LIST, and the
	// child's does not exist until its surface does. Handing it the
	// parent's would gate the child on the parent's tools — a different
	// set, arrived at for different reasons.
	//
	// Nil leaves the child ungated, which is correct for a company with no
	// required skills.
	Guard func(*tools.Surface) tools.Guard

	// Telemetry receives every child's Result, once, on every path a child
	// can end on.
	//
	// The package produces a Result for every outcome specifically so the
	// caller's phase event cannot be missing — and the tool below is that
	// caller. Without this hook a spawn is invisible: its tokens are
	// charged, its model call happened, and nothing in the event store or
	// the dashboard says a sub-agent ran at all.
	Telemetry func(ctx context.Context, res Result)
}

// parentNames is the parent's callable set, nil-safe.
func (c Config) parentNames() []string {
	if c.Parent == nil {
		return nil
	}
	return c.Parent()
}

func (c Config) validate() error {
	var errs []error
	if c.Models == nil {
		errs = append(errs, errors.New("no provider registry"))
	}
	if c.Seat.Role == nil {
		errs = append(errs, errors.New("no parent role"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("subagent: %w", err)
	}
	return c.Limits.validate()
}

// Grant is the resolved reachable set for one child.
type Grant struct {
	// Universe is the SAFE snapshot: everything the child could ever
	// resolve, discovery included. Nothing outside it is reachable, because
	// [tools.Surface] resolves against its snapshot and refuses to activate
	// a name the snapshot lacks.
	Universe tools.Snapshot

	// Active is what the child is offered up front.
	Active []string

	// Rejected is what the parent asked for and did not get, deduplicated,
	// in request order.
	Rejected []string
}

// Permit resolves a child's grant.
//
// Exported because it is the security boundary, and a boundary a test cannot
// interrogate directly is one that gets asserted through six layers of loop
// and eventually not at all.
//
// Three filters, in the order that makes the rejection message honest:
//
//  1. on the engine-control denylist — the parent may hold it, the child
//     never does;
//  2. not something the PARENT can call — a child must not reach past its
//     spawner, and the parent's own surface is already the resolved
//     availability for this turn;
//  3. annotated as a write to a shared surface — a sub-agent posting to a
//     channel or commenting on an issue writes under the parent's identity,
//     onto a transcript a human reads.
//
// The same three filters build the discovery universe, so a child cannot walk
// around the grant by discovering what it was denied.
func Permit(universe tools.Snapshot, parent, requested []string) Grant {
	parentSet := make(map[string]bool, len(parent))
	for _, name := range parent {
		parentSet[name] = true
	}

	safe := tools.Snapshot{}
	reachable := make(map[string]bool)
	for _, e := range universe.Entries() {
		if !childMayReach(e, parentSet) {
			continue
		}
		next, err := safe.With(e)
		if err != nil {
			// Skipping one entry rather than failing the spawn: a snapshot
			// that somehow carries a duplicate or unnamed tool must not
			// cost the child every OTHER tool it was granted.
			log.Warn("subagent_catalogue_entry_skipped", "tool", e.Name(), "error", err)
			continue
		}
		safe = next
		reachable[e.Name()] = true
	}

	var active, rejected []string
	seen := make(map[string]bool, len(requested))
	for _, name := range requested {
		if seen[name] {
			// A repeat is not a second rejection and not a second grant.
			// Two ToolDefs with one name is a request some vendors refuse
			// outright, and reporting the same refusal twice reads to a
			// model as two different problems.
			continue
		}
		seen[name] = true
		if reachable[name] {
			active = append(active, name)
			continue
		}
		rejected = append(rejected, name)
	}

	// The skill loader rides along unrequested when the child can reach it;
	// see the constant.
	if reachable[skillLoaderTool] && !seen[skillLoaderTool] {
		active = append(active, skillLoaderTool)
	}

	return Grant{Universe: safe, Active: active, Rejected: rejected}
}

// childMayReach is the three-filter test, in one place so the grant and the
// discovery catalogue cannot answer it differently.
func childMayReach(e tools.Entry, parent map[string]bool) bool {
	switch {
	case Denied(e.Name()):
		return false
	case !parent[e.Name()]:
		return false
	case mcp.WritesToSharedSurface(e.Annotations):
		return false
	}
	return true
}

// The deadline causes. Attached with [context.WithTimeoutCause] so a child
// that stopped can say WHICH clock ran out rather than reporting a generic
// DeadlineExceeded that a provider's own internal HTTP timeout produces just
// as readily.
var (
	errTaskDeadline = errors.New("the worker exceeded its wall-clock cap")
	errCallDeadline = errors.New("the delegate call exceeded its wall-clock cap")
)

// Run plans one delegate call and executes its graph.
//
// The error is reserved for a request or a wiring that could not start ANY
// task — a malformed graph, an unknown worker, a budget slice too thin to
// share. Every way a started task can end is a Result, and the results come
// back in the order the parent wrote them.
func Run(ctx context.Context, cfg Config, req Request) ([]Result, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	tasks, err := plan(req, cfg.Workers, cfg.Limits)
	if err != nil {
		return nil, err
	}

	// ONE slice for the whole call, computed once. Every worker charges
	// the same meter, so they compete: whoever spends first leaves less
	// for the rest. That is deliberate — the alternative, a fresh fraction
	// per task, hands out N times the fraction the operator configured.
	meter := cfg.Budget
	total := slice(cfg.ParentRemaining, cfg.Limits.BudgetFraction)
	if total > 0 && meter != nil {
		if floor := cfg.Limits.MinTokensPerTask; floor > 0 && total < floor*len(tasks) {
			return nil, &RefusedError{Slice: total, MinPerTask: floor, Tasks: len(tasks)}
		}
		meter = newSliceMeter(meter, total)
	}

	callCtx, cancelCall := context.WithTimeoutCause(ctx, cfg.Limits.CallTimeout, errCallDeadline)
	defer cancelCall()

	results := runGraph(callCtx, tasks, cfg.Limits.MaxParallel,
		func(taskCtx context.Context, r resolved, deps []Result) Result {
			provider, key, err := resolveProvider(cfg, r.model)
			if err != nil {
				// A model key that does not resolve is this TASK's
				// failure, not the call's: its siblings are running on
				// keys that do, and refusing the whole call would throw
				// away work that is already in flight.
				return Result{
					ID: r.ID, Worker: r.Worker, Status: StatusFailed,
					Error: ledger.Elide(err.Error(), errorLimit),
				}
			}
			childCtx, cancel := context.WithTimeoutCause(taskCtx, cfg.Limits.TaskTimeout, errTaskDeadline)
			defer cancel()
			return run(childCtx, cfg, provider, key, meter, r, deps)
		})

	publishCall(ctx, cfg, tasks, results)
	return results, nil
}

// run drives one worker to completion and never returns anything but a
// Result.
func run(ctx context.Context, cfg Config, provider llm.Provider, key string,
	meter toolloop.BudgetMeter, task resolved, deps []Result,
) (res Result) {
	res.ID, res.Worker, res.ProviderKey = task.ID, task.Worker, key

	// TELEMETRY ON EVERY PATH, including the panic the frame below
	// contains. Deferred FIRST so it runs LAST: the recovery below writes
	// the failure onto res, and a hook registered before it would report a
	// result the panic had not yet been folded into.
	//
	// The package produces a Result for every outcome precisely so a
	// caller's phase event cannot be missing, and without this a delegate
	// call is invisible — its tokens charged, its model call made, and
	// nothing in the event store saying a worker ran.
	if cfg.Telemetry != nil {
		defer func() { cfg.Telemetry(ctx, res) }()
	}

	// A PANIC IS CONTAINED HERE.
	//
	// Workers run concurrently, so a panicking goroutine takes the whole
	// PROCESS down: one malformed MCP schema would stop a fleet node
	// mid-turn. Even alone it would unwind through the parent's own tool
	// call and kill a turn that may be forty minutes of work. Neither is a
	// proportionate answer to one worker falling over, and the parent is
	// told exactly what happened either way.
	//
	// The stack is logged, not swallowed: a contained panic nobody can see
	// is a bug that never gets fixed.
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		log.ErrorContext(ctx, "subagent_panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		res.Status = StatusFailed
		res.Error = ledger.Elide(fmt.Sprintf("worker panicked: %v", r), errorLimit)
	}()

	grant := Permit(cfg.Universe, cfg.parentNames(), task.tools)
	res.Rejected = grant.Rejected
	if len(grant.Rejected) > 0 {
		log.InfoContext(ctx, "subagent_tools_rejected", "rejected", grant.Rejected,
			"task", task.ID, "worker", task.Worker, "role", cfg.Seat.Role.Name)
	}

	// THE SUBMISSION TOOL IS ALWAYS ON THE SURFACE and never subject to
	// the grant: it is how the worker answers, so a filter that could
	// remove it would leave a worker able to work and unable to report.
	submit := newSubmitTool(task.output)
	universe, err := grant.Universe.With(tools.Entry{Tool: submit, Origin: tools.OriginBuiltin})
	if err != nil {
		// A granted tool already publishes this name. The worker keeps its
		// tools and loses the ability to answer structurally, which is a
		// no_result rather than a failure — but it is a CONFIG problem, so
		// it is logged loudly with the name that collided.
		log.ErrorContext(ctx, "subagent_submit_tool_shadowed", "tool", SubmitTool, "error", err)
		universe = grant.Universe
		submit = nil
	}
	active := slices.Clone(grant.Active)
	if submit != nil {
		active = append(active, SubmitTool)
	}

	var surface *tools.Surface
	if cfg.Discovery != nil {
		for _, meta := range cfg.Discovery(func() *tools.Surface { return surface }) {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			next, err := universe.With(tools.Entry{Tool: meta, Origin: tools.OriginBuiltin})
			if err != nil {
				// A meta-tool colliding with a granted name would shadow the
				// real tool: the model's call would reach the meta-tool while
				// the catalogue described the other. Drop the meta-tool — the
				// worker loses discovery, not correctness.
				log.WarnContext(ctx, "subagent_meta_tool_skipped", "tool", meta.Name(), "error", err)
				continue
			}
			universe = next
			active = append(active, meta.Name())
		}
	}
	surface = tools.NewSurface(phase.Subagent.String(), universe, active)
	// BOUND to the parent turn, or every seat-scoped tool in the grant
	// fails at call time — see Config.Turn.
	if cfg.Turn != nil {
		surface = surface.ForTurn(cfg.Turn)
	}
	if cfg.Guard != nil {
		// AFTER the surface exists and from that surface, so what the
		// guard enforces and what the worker's catalogue showed cannot
		// disagree.
		if guard := cfg.Guard(surface); guard != nil {
			surface = surface.WithGuard(guard)
		}
	}

	// The catalogue is rendered only for a worker that can act on it.
	// Listing tools to one with no activate_tool is an invitation to call
	// a name it was never offered, which costs a round and produces a
	// refusal.
	var catalogue string
	if cfg.Discovery != nil {
		catalogue = renderCatalogue(grant.Universe)
	}
	res.SystemPrompt = prompts.BuildSubagent(cfg.Seat, prompts.SubagentInput{
		ParentSystemPrompt: task.systemPrompt,
		// Both the offered set AND the discoverable catalogue, so a skill
		// covering a tool the worker may later activate is in the prompt
		// before the first call rather than after the guard blocks it.
		AvailableTools: union(surface.Active(), grant.Universe.Names()),
		ToolCatalogue:  catalogue,
		Submits:        submit != nil,
		Skills:         cfg.Skills,
	})
	res.UserPrompt = withDependencies(task.Prompt, deps)

	progress := &toolloop.Progress{}
	loop, err := toolloop.Run(ctx, toolloop.Config{
		Provider:  provider,
		Surface:   surface,
		MaxRounds: task.maxTurns,
		Budget:    meter,
		Progress:  progress,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: res.SystemPrompt},
			{Role: llm.RoleUser, Content: res.UserPrompt},
		},
		// THE SUBMISSION ENDS THE LOOP. Without this a worker that has
		// answered keeps its remaining rounds and spends them narrating
		// what it just submitted — on the parent's budget, for output
		// nobody reads.
		TerminateAfter: terminators(submit),
		// No AllowSuspend: a worker's conversation is never persisted, so
		// a suspended one could never be resumed — the tool loop turns
		// the attempt into an ordinary tool failure it can react to.
	})
	res.ToolsAvailable = surface.Active()

	if err == nil {
		res.Text = loop.Text
		res.Rounds = loop.RoundsUsed
		res.InputTokens, res.OutputTokens = loop.InputTokens, loop.OutputTokens
		res.Model = loop.Model
		res.Executions = loop.Executions
		res.Narration = loop.Narration
		res.Status, res.Output = submitted(submit)
		if res.Status == StatusNoResult {
			log.WarnContext(ctx, "subagent_never_submitted", "task", task.ID,
				"worker", task.Worker, "rounds", res.Rounds)
		}
		return res
	}

	// The failure paths all report the PARTIAL state. A worker that spent
	// nine rounds and then hit its cap did nine rounds of work the parent
	// paid for; reporting zeros throws away both the transcript and the
	// only evidence of what it cost.
	partial := progress.Snapshot()
	res.Text = partial.Text
	res.Rounds = partial.RoundsUsed
	res.InputTokens, res.OutputTokens = partial.InputTokens, partial.OutputTokens
	res.Model = partial.Model
	res.Executions = partial.Executions
	res.Narration = partial.Narration

	kind, reason := stopReason(ctx)
	res.Status, res.Error = classify(kind, reason, err)

	// A SUBMISSION SURVIVES THE FAILURE THAT FOLLOWED IT. A worker that
	// answered and then spent a round it did not have has still answered,
	// and discarding that would make the parent re-run finished work.
	if status, out := submitted(submit); status == StatusOK {
		res.Status, res.Output, res.Error = StatusOK, out, ""
	}

	switch res.Status {
	case StatusTimedOut:
		log.WarnContext(ctx, "subagent_timed_out", "role", cfg.Seat.Role.Name,
			"task", task.ID, "rounds", res.Rounds, "tokens", res.Tokens(), "reason", reason)
	case StatusBudget:
		log.WarnContext(ctx, "subagent_budget_exhausted", "role", cfg.Seat.Role.Name,
			"task", task.ID, "tokens", res.Tokens())
	}
	return res
}

// submitted reads the worker's answer off its submission tool.
//
// NOTHING IS SYNTHESISED when none arrived: `no_result` is the honest status,
// and the worker's prose rides the Result beside it. See result.go.
func submitted(submit *structured.Tool[resultPayload]) (Status, map[string]any) {
	if submit == nil {
		return StatusNoResult, nil
	}
	payload, ok := submit.Value()
	if !ok {
		return StatusNoResult, nil
	}
	return StatusOK, payload.Fields
}

// terminators is the tool-name list that ends a worker's loop.
func terminators(submit *structured.Tool[resultPayload]) []string {
	if submit == nil {
		return nil
	}
	return []string{SubmitTool}
}

// stopReason classifies a context that ended, and returns ("", "") for one
// that did not.
//
// It reads the CAUSE rather than matching on DeadlineExceeded, because a
// provider's own HTTP client returns that error for its own reasons — and
// reporting somebody else's timeout as the sub-agent's cap sends an operator
// to raise a limit that was never reached.
func stopReason(ctx context.Context) (kind, reason string) {
	switch cause := context.Cause(ctx); {
	case cause == nil:
		return "", ""
	case errors.Is(cause, errTaskDeadline):
		return KindTimeout, errTaskDeadline.Error()
	case errors.Is(cause, errCallDeadline):
		return KindTimeout, errCallDeadline.Error()
	default:
		// The parent turn was torn down. NOT a timeout: nothing exceeded a
		// cap, and a planner told "timed out" would helpfully retry with a
		// smaller task against an engine that is shutting down.
		return KindCancelled, ledger.Elide(cause.Error(), errorLimit)
	}
}

// clampTurns applies the runtime cap to a parent's request.
//
// Clamped, never refused: the request is the parent's estimate of how much
// work the task needs, and an over-estimate is not an error worth losing the
// whole spawn over. A request of zero means "unspecified" and takes the cap.
func clampTurns(requested, cap int) int {
	if requested < 1 {
		return cap
	}
	return min(requested, cap)
}

// slice is the absolute token ceiling a spawn gets.
//
// Zero means NO CEILING, and it is the answer for a parent with no per-seat
// budget: a seat that runs uncapped does not get to impose a cap on its
// children out of nowhere — there is no number to take a fraction of.
func slice(remaining int, fraction float64) int {
	if remaining <= 0 || fraction <= 0 || fraction > 1 {
		return 0
	}
	// At least one token, so a fraction that rounds to zero still produces a
	// ceiling rather than silently reading as "uncapped" — the two are
	// opposite answers and they must not share an encoding.
	return max(1, int(float64(remaining)*fraction))
}

// sliceMeter caps what a spawn — or a whole batch of them — may charge,
// on top of whatever the parent's counter already enforces.
type sliceMeter struct {
	inner toolloop.BudgetMeter
	cap   int

	mu   sync.Mutex
	used int
}

func newSliceMeter(inner toolloop.BudgetMeter, cap int) *sliceMeter {
	return &sliceMeter{inner: inner, cap: cap}
}

// Spend reserves against the slice, then charges the real counter.
//
// The reservation is taken BEFORE the inner charge and under the lock,
// because that charge can block: two children of one batch would otherwise
// both test against the same `used` snapshot, both pass, and both spend —
// overshooting the slice by a whole child. Reserving first makes the check
// and the increment one operation, which is the same rule the shared counter
// itself follows.
func (m *sliceMeter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	m.mu.Lock()
	if m.used+tokens > m.cap {
		used := m.used
		m.mu.Unlock()
		return toolloop.SpendOutcome{
			OK: false, Scope: ScopeSubagent, Used: used, Limit: m.cap,
		}, nil
	}
	m.used += tokens
	m.mu.Unlock()

	outcome, err := m.inner.Spend(ctx, tokens)
	if err != nil {
		// The reservation STAYS. An unreachable counter does not say whether
		// the charge landed, so releasing it would let a sibling spend
		// tokens that may already be billed. The round is aborted either
		// way — this only decides what the siblings still running see.
		return outcome, err
	}
	if !outcome.OK {
		// A refusal is definite: nothing was charged, so the reservation
		// goes back. Keeping it would shrink the slice for every sibling
		// over a charge that never happened.
		m.mu.Lock()
		m.used -= tokens
		m.mu.Unlock()
	}
	return outcome, nil
}

// Used is what the slice has spent, for a caller reporting on a batch.
func (m *sliceMeter) Used() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used
}

// resolveProvider builds the seat's sub-agent chain.
//
// A chain even for one member: the wrapper is a pass-through there, and a
// one-member seat then fails, logs and reports identically to a three-member
// one.
func resolveProvider(cfg Config, key string) (llm.Provider, string, error) {
	var members []chain.Member
	if key != "" {
		// AN EXPLICIT KEY GETS NO FALLBACK CHAIN, and no silent
		// substitution. The seat's chain exists so a turn survives one
		// provider being down; a template or a task that named a model
		// named it for a reason, and quietly running somewhere else is
		// how an operator's cheap-model decision becomes a frontier bill
		// nobody can trace. An unknown key is refused, naming what is
		// configured.
		p, ok := cfg.Models.Provider(key)
		if !ok {
			return nil, "", fmt.Errorf(
				"subagent: model %q is not configured under providers.llm — configured: %s",
				key, strings.Join(cfg.Models.Keys(), ", "))
		}
		members = []chain.Member{{Key: key, Provider: p}}
	} else {
		resolved, err := cfg.Models.Chain(cfg.Seat.Role, phase.Subagent)
		if err != nil {
			return nil, "", fmt.Errorf("subagent: %w", err)
		}
		members = resolved
	}
	c, err := chain.New(members, chain.Options{})
	if err != nil {
		return nil, "", fmt.Errorf("subagent: %w", err)
	}
	return c, members[0].Key, nil
}

// publishCall emits the one fan-out summary event, best effort.
//
// Telemetry must never fail a call: the workers have already run and their
// results are the parent's answer, so a broker that refuses this event must
// not turn a finished call into a failed tool call.
func publishCall(ctx context.Context, cfg Config, tasks []resolved, results []Result) {
	if cfg.Publisher == nil {
		return
	}
	var successes, tokens int
	statuses := make(map[string]string, len(results))
	for _, r := range results {
		if r.Status.Succeeded() {
			successes++
		}
		tokens += r.Tokens()
		statuses[r.ID] = string(r.Status)
	}
	ev := events.New(types.SubagentBatched{
		ParentHandle: cfg.Seat.Role.Handle(),
		TaskCount:    len(results),
		Successes:    successes,
		Failures:     len(results) - successes,
		TotalTokens:  tokens,
		Graph:        graphOf(tasks),
		Statuses:     statuses,
	}, cfg.Trace)
	// The payload carries no role, so the envelope's source is the only
	// attribution this event has — without it every fan-out in the company
	// renders as "system".
	ev.Source = cfg.Seat.Role.Name
	if err := cfg.Publisher.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "subagent_batched_publish_failed", "error", err)
	}
}

// graphOf renders the shape the call ran, so a dashboard can draw it without
// the parent's tool arguments — which are not on any event.
func graphOf(tasks []resolved) []types.SubagentNode {
	out := make([]types.SubagentNode, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, types.SubagentNode{
			ID: t.ID, Worker: t.Worker, Wave: t.wave,
			After: slices.Clone(t.After),
		})
	}
	// INPUT ORDER, matching the results beside it: the two are read
	// together, and a graph in wave order beside results in input order
	// makes the reader do the pairing by hand.
	slices.SortStableFunc(out, func(a, b types.SubagentNode) int {
		return indexOfID(tasks, a.ID) - indexOfID(tasks, b.ID)
	})
	return out
}

func indexOfID(tasks []resolved, id string) int {
	for _, t := range tasks {
		if t.ID == id {
			return t.order
		}
	}
	return 0
}

// renderCatalogue is the slim catalogue a discovery-capable child is shown:
// first-party tools by name and one-line description, MCP servers by name
// only.
//
// Servers are named rather than expanded for the same reason the parent's
// catalogue names them — a real server publishes dozens of tools and a wall
// of them is what discovery exists to avoid.
func renderCatalogue(safe tools.Snapshot) string {
	var lines []string
	var servers []string
	for _, e := range safe.Entries() {
		if server, ok := e.FromMCP(); ok {
			if !slices.Contains(servers, server) {
				servers = append(servers, server)
			}
			continue
		}
		lines = append(lines, tools.CatalogueLine(e.Name(), e.Tool.Description()))
	}
	for _, server := range servers {
		lines = append(lines, "- MCP server `"+server+"` (use the discovery tools to list its tools)")
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// union merges two name lists, deduplicated and sorted.
//
// Sorted because it reaches a system prompt, and a prompt whose bytes move
// between two builds of one phase costs the full uncached rate for every
// remaining round.
func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, name := range b {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
