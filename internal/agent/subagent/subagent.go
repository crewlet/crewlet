// Package subagent runs the ephemeral workers a turn's Execute phase spawns.
//
// A sub-agent is one tool loop with a parent-written prompt, a parent-chosen
// slice of the parent's own tools, and hard caps on rounds, wall-clock and
// tokens. It is a LEAF: it cannot spawn further sub-agents and it cannot
// contact colleagues, which is the only thing bounding the depth of the whole
// construct — there is no depth counter anywhere, because a leaf needs none.
//
// Four properties are load-bearing. Each is enforced in code here, never in
// the prompt, because a prompt is a request and this is a boundary:
//
//   - THE GRANT IS A SECURITY BOUNDARY. A child's reachable set is the
//     parent's own tools, minus the engine-control denylist, minus anything
//     whose annotations say it writes to a shared surface. Both halves of
//     "reachable" are covered — what the parent NAMED and what the child can
//     DISCOVER — because the child's discovery runs against the filtered
//     snapshot, not the parent's. Discovery over the parent's catalogue is
//     how a child would otherwise promote a tool nobody granted it.
//
//   - THE BUDGET SLICE IS THE BATCH'S, NOT EACH CHILD'S. One fraction of the
//     parent's remaining tokens is computed once and shared by every child in
//     a batch. The alternative — a fresh fraction per child — over-allocates
//     by N, which is the whole cost of a fan-out being invisible until the
//     org budget is gone.
//
//   - A CHILD'S FAILURE IS A RESULT, NOT AN ERROR. Timeout, budget exhaustion
//     and a panic all come back as a Result with Failed set and the partial
//     transcript intact. The trap is a timeout or budget path that returns
//     early: the phase-failure guard never sees it and the spawn publishes
//     NOTHING — the parent's Execute event shows a spawn_subagent
//     call whose sub-agent left no phase record, no partial transcript and no
//     reason it stopped. Here every path produces exactly one Result carrying
//     all three, so the caller's phase event cannot be missing.
//
//   - AN ERROR MEANS NOTHING RAN. Spawn and SpawnBatch return an error only
//     for a request or a wiring that could not start a child at all. That
//     split is what lets the tool wrapper answer the model with a refusal in
//     one case and with the worker's own words in the other.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
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

// ToolName is the name the Execute-phase model calls to spawn one.
//
// A constant because it is also the first entry of the denylist below: the
// tool that spawns a sub-agent is the tool a sub-agent must never see, and
// two spellings of that name is one recursion away from a fork bomb.
const ToolName = "spawn_subagent"

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
// planner reads. 500 runes holds two or three sentences — enough to say what
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

// Limits are the runtime caps one spawn runs under. They come from the
// company config's turn_engine block, which validates every one of them.
type Limits struct {
	// MaxTurns caps a child's tool rounds. A parent asking for more is
	// CLAMPED, never refused: the request is a hint about how much work the
	// task needs, and refusing a hint costs the whole spawn.
	MaxTurns int

	// Timeout bounds one child.
	Timeout time.Duration

	// BatchTimeout bounds a whole batch. Both apply to a batched child —
	// the per-child cap stops one straggler, this one stops twenty of them
	// each finishing just inside their own cap.
	BatchTimeout time.Duration

	// MaxParallel caps concurrency within one batch. Children beyond it run
	// as earlier ones finish.
	MaxParallel int

	// BudgetFraction is the share of the parent's REMAINING tokens one
	// spawn may consume — for a batch, the total across all children.
	BudgetFraction float64

	// MinPerChildTokens floors the per-child slice. A batch whose total
	// divided by its child count falls below this is refused UP FRONT,
	// rather than started with every child too poor to finish: N children
	// that each die mid-round have spent the whole slice and produced
	// nothing, which is strictly worse than not starting.
	MinPerChildTokens int
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
	if l.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("limits: Timeout must be positive, got %v", l.Timeout))
	}
	if l.BatchTimeout <= 0 {
		errs = append(errs, fmt.Errorf("limits: BatchTimeout must be positive, got %v", l.BatchTimeout))
	}
	if l.MaxParallel < 1 {
		errs = append(errs, fmt.Errorf("limits: MaxParallel must be at least 1, got %d", l.MaxParallel))
	}
	if l.BudgetFraction <= 0 || l.BudgetFraction > 1 {
		errs = append(errs, fmt.Errorf(
			"limits: BudgetFraction must be in (0, 1], got %v — it is a SHARE of the "+
				"parent's remaining budget, not a token count", l.BudgetFraction))
	}
	if l.MinPerChildTokens < 0 {
		errs = append(errs, fmt.Errorf("limits: MinPerChildTokens must not be negative, got %d",
			l.MinPerChildTokens))
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

	// Models resolves the seat's per-phase provider chain. Sub-agents run
	// on [phase.Subagent] — the llm_subagent keys — which is what lets an
	// operator put fan-out work on a cheap model without touching the
	// model the seat itself thinks on.
	Models *phase.Registry

	// Universe is the PARENT phase's tool snapshot: everything a name could
	// resolve to. Parent is what the parent may itself call. The child's
	// reachable set is derived from BOTH — the snapshot alone is the whole
	// registry, and granting from that would hand a child tools the parent
	// never had.
	Universe tools.Snapshot
	Parent   []string

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

	// Skills renders the tool-skill catalogue into the child's prompt. Nil
	// keeps the prompt free of skill scaffolding.
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

// Request is one single-child spawn.
type Request struct {
	// TaskPrompt is seeded as the child's first user message.
	TaskPrompt string
	// SystemPrompt is the parent's task-specific prompt. The runtime
	// preamble is appended to it.
	SystemPrompt string
	// ToolNames is the allowlist the parent is asking to grant.
	ToolNames []string
	// MaxTurns is the parent's requested round cap. Zero means "the
	// runtime's" and anything above it is clamped down.
	MaxTurns int
}

func (r Request) validate() error {
	switch {
	case strings.TrimSpace(r.TaskPrompt) == "":
		return errors.New("subagent: task_prompt is required")
	case strings.TrimSpace(r.SystemPrompt) == "":
		return errors.New("subagent: system_prompt is required")
	}
	return nil
}

// Task is one child of a batch. It carries its own prompts and round cap; the
// allowlist is the BATCH's, applied uniformly.
type Task struct {
	TaskPrompt   string
	SystemPrompt string
	MaxTurns     int
}

// BatchRequest is a fan-out spawn.
//
// One allowlist for every child, deliberately. Heterogeneous capabilities per
// child in one call means one tool result the planner cannot reason about —
// it would have to remember which index got which grant. Two calls express
// that without ambiguity.
type BatchRequest struct {
	ToolNames []string
	Tasks     []Task
}

// Result is one child's record: everything the caller's phase event needs and
// everything the parent's model is told.
//
// It is produced on EVERY path a child can take, including the ones that
// never reached a model. A caller can publish from it unconditionally.
type Result struct {
	// Text is the child's answer, or its partial transcript when it was
	// cut off.
	Text string

	Rounds       int
	InputTokens  int
	OutputTokens int

	// Model is what actually served the calls; ProviderKey is the config
	// key its chain was resolved under.
	Model       string
	ProviderKey string

	// ToolsAvailable is what the child could call when it finished,
	// activations included. Rejected is what the parent asked for and did
	// not get, in request order.
	ToolsAvailable []string
	Rejected       []string

	Executions []toolloop.Execution

	// SystemPrompt and UserPrompt are what the child was actually sent.
	SystemPrompt string
	UserPrompt   string

	// Failed marks a child that did not finish. TimedOut narrows that to
	// the wall-clock cases, which is the one class a planner can usefully
	// retry with fewer children.
	Failed    bool
	TimedOut  bool
	Error     string
	ErrorKind string
}

// Tokens is the child's total spend.
func (r Result) Tokens() int { return r.InputTokens + r.OutputTokens }

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
	errChildDeadline = errors.New("sub-agent exceeded its wall-clock cap")
	errBatchDeadline = errors.New("the batch exceeded its wall-clock cap")
)

// Spawn runs one sub-agent and returns its record.
//
// The error is reserved for a request or a wiring that could not start a
// child at all; every way a started child can end is a Result.
func Spawn(ctx context.Context, cfg Config, req Request) (Result, error) {
	if err := cfg.validate(); err != nil {
		return Result{}, err
	}
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	provider, key, err := resolveProvider(cfg)
	if err != nil {
		return Result{}, err
	}

	meter := cfg.Budget
	if cap := slice(cfg.ParentRemaining, cfg.Limits.BudgetFraction); cap > 0 && meter != nil {
		meter = newSliceMeter(meter, cap)
	}

	childCtx, cancel := context.WithTimeoutCause(ctx, cfg.Limits.Timeout, errChildDeadline)
	defer cancel()
	return run(childCtx, cfg, provider, key, meter, req), nil
}

// BatchRefusedError is a batch that never started because its budget slice
// could not give every child a workable share.
//
// One error for the batch rather than one synthetic failure per child, because
// the refusal is a property of the BATCH. N copies of one sentence reads to a planner as N independent
// failures, and the obvious reaction to that is to retry them individually,
// which is exactly the thing the floor exists to prevent.
type BatchRefusedError struct {
	// Slice is the total the batch would have shared, MinPerChild the floor
	// each child needed, and Tasks how many were asked for.
	Slice       int
	MinPerChild int
	Tasks       int
}

func (e *BatchRefusedError) Error() string {
	return fmt.Sprintf(
		"subagent: batch refused: the budget slice is %d tokens, which cannot give "+
			"%d children the %d each they need to finish — spawn fewer children "+
			"or wait for budget",
		e.Slice, e.Tasks, e.MinPerChild)
}

// SpawnBatch fans out children concurrently and returns their records in
// INPUT ORDER.
//
// Order matters more than it looks: the parent's model wrote the tasks as a
// list and reads the answers as one, so a result slice ordered by completion
// would silently re-pair every answer with the wrong question.
func SpawnBatch(ctx context.Context, cfg Config, req BatchRequest) ([]Result, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if len(req.Tasks) == 0 {
		return nil, errors.New("subagent: a batch needs at least one task")
	}
	for i, t := range req.Tasks {
		if err := (Request{TaskPrompt: t.TaskPrompt, SystemPrompt: t.SystemPrompt}).validate(); err != nil {
			return nil, fmt.Errorf("subagent: tasks[%d]: %w", i, err)
		}
	}
	provider, key, err := resolveProvider(cfg)
	if err != nil {
		return nil, err
	}

	// ONE slice for the whole batch, computed once. Every child charges the
	// same meter, so they compete: whoever spends first leaves less for the
	// rest. That is deliberate — the alternative, a fresh fraction per
	// child, hands out N times the fraction the operator configured.
	meter := cfg.Budget
	total := slice(cfg.ParentRemaining, cfg.Limits.BudgetFraction)
	if total > 0 && meter != nil {
		if floor := cfg.Limits.MinPerChildTokens; floor > 0 && total < floor*len(req.Tasks) {
			return nil, &BatchRefusedError{Slice: total, MinPerChild: floor, Tasks: len(req.Tasks)}
		}
		meter = newSliceMeter(meter, total)
	}

	batchCtx, cancelBatch := context.WithTimeoutCause(ctx, cfg.Limits.BatchTimeout, errBatchDeadline)
	defer cancelBatch()

	results := make([]Result, len(req.Tasks))
	sem := make(chan struct{}, cfg.Limits.MaxParallel)
	var wg sync.WaitGroup
	for i, task := range req.Tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-batchCtx.Done():
				// Never got a worker: the deadline landed while this child
				// was queued behind MaxParallel. Said explicitly, because
				// "never started" is the one failure a planner can retry
				// unchanged — unlike a child that burned its budget.
				results[i] = neverStarted(batchCtx)
				return
			}
			if batchCtx.Err() != nil {
				// The turn came free only BECAUSE the deadline killed the
				// child ahead, so both select cases were ready and the
				// choice between them was a coin flip. Re-checking after
				// the acquire is what makes "never started" a fact rather
				// than a scheduling accident — without it the same batch
				// reports the same child two different ways on two runs.
				results[i] = neverStarted(batchCtx)
				return
			}
			childCtx, cancel := context.WithTimeoutCause(batchCtx, cfg.Limits.Timeout, errChildDeadline)
			defer cancel()
			results[i] = run(childCtx, cfg, provider, key, meter, Request{
				TaskPrompt:   task.TaskPrompt,
				SystemPrompt: task.SystemPrompt,
				ToolNames:    req.ToolNames,
				MaxTurns:     task.MaxTurns,
			})
		}()
	}

	// WAIT for every child, deadline or not.
	//
	// Returning at the batch deadline with children still running would be
	// a data race on the result slots and a goroutine leak the caller has
	// no handle on. The deadline is enforced by CANCELLING their contexts,
	// which is what actually stops the work; waiting only costs the time a
	// provider takes to notice. Abandoning the wait is only safe in a
	// runtime where a cancelled task stops at its next suspension point. A
	// goroutine has no such point, and its writes would land in a slice the
	// caller already returned.
	wg.Wait()

	publishBatched(ctx, cfg, results)
	return results, nil
}

// neverStarted is the record for a child the batch deadline reached before it
// got a worker.
func neverStarted(ctx context.Context) Result {
	kind, reason := stopReason(ctx)
	if kind == "" {
		// The select lost a race it should not be able to lose: Done fired
		// with no cause. Report it rather than returning a zero Result that
		// reads as a child which ran and said nothing.
		kind, reason = KindFailed, "the batch stopped before this child started"
	}
	return Result{
		Failed:    true,
		TimedOut:  kind == KindTimeout,
		ErrorKind: kind,
		Error:     "never started: " + reason,
	}
}

// run drives one child to completion and never returns anything but a Result.
func run(ctx context.Context, cfg Config, provider llm.Provider, key string,
	meter toolloop.BudgetMeter, req Request,
) (res Result) {
	res.ProviderKey = key

	// A PANIC IS CONTAINED HERE, on both the single and the batched path.
	//
	// Batched, the case is not arguable: a panicking goroutine takes the
	// whole PROCESS down, so one malformed MCP schema would stop a fleet
	// node mid-turn. Single, it would unwind through the parent's Execute
	// tool call and kill a turn that may be forty minutes of work. Neither
	// is a proportionate answer to one worker falling over, and the parent
	// is told exactly what happened either way.
	//
	// The stack is logged, not swallowed: a contained panic nobody can see
	// is a bug that never gets fixed.
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		log.ErrorContext(ctx, "subagent_panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		res.Failed = true
		res.ErrorKind = KindPanic
		res.Error = ledger.Elide(fmt.Sprintf("sub-agent panicked: %v", r), errorLimit)
	}()

	grant := Permit(cfg.Universe, cfg.Parent, req.ToolNames)
	res.Rejected = grant.Rejected
	if len(grant.Rejected) > 0 {
		log.InfoContext(ctx, "subagent_tools_rejected", "rejected", grant.Rejected,
			"role", cfg.Seat.Role.Name)
	}

	universe := grant.Universe
	active := slices.Clone(grant.Active)
	var surface *tools.Surface
	if cfg.Discovery != nil {
		for _, meta := range cfg.Discovery(func() *tools.Surface { return surface }) {
			next, err := universe.With(tools.Entry{Tool: meta, Origin: tools.OriginBuiltin})
			if err != nil {
				// A meta-tool colliding with a granted name would shadow the
				// real tool: the model's call would reach the meta-tool while
				// the catalogue described the other. Drop the meta-tool — the
				// child loses discovery, not correctness.
				log.WarnContext(ctx, "subagent_meta_tool_skipped", "tool", meta.Name(), "error", err)
				continue
			}
			universe = next
			active = append(active, meta.Name())
		}
	}
	surface = tools.NewSurface(phase.Subagent.String(), universe, active)

	// The catalogue is rendered only for a child that can act on it.
	// Listing tools to a worker with no activate_tool is an invitation to
	// call a name it was never offered, which costs a round and produces a
	// refusal.
	var catalogue string
	if cfg.Discovery != nil {
		catalogue = renderCatalogue(grant.Universe)
	}
	res.SystemPrompt = prompts.BuildSubagent(cfg.Seat, prompts.SubagentInput{
		ParentSystemPrompt: req.SystemPrompt,
		// Both the offered set AND the discoverable catalogue, so a skill
		// covering a tool the child may later activate is in the prompt
		// before the first call rather than after the guard blocks it.
		AvailableTools: union(surface.Active(), grant.Universe.Names()),
		ToolCatalogue:  catalogue,
		Skills:         cfg.Skills,
	})
	res.UserPrompt = req.TaskPrompt

	progress := &toolloop.Progress{}
	loop, err := toolloop.Run(ctx, toolloop.Config{
		Provider:  provider,
		Surface:   surface,
		MaxRounds: clampTurns(req.MaxTurns, cfg.Limits.MaxTurns),
		Budget:    meter,
		Progress:  progress,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: res.SystemPrompt},
			{Role: llm.RoleUser, Content: req.TaskPrompt},
		},
		// No AllowSuspend: a sub-agent's conversation is never persisted,
		// so a suspended one could never be resumed — the tool loop turns
		// the attempt into an ordinary tool failure the child can react to.
	})
	res.ToolsAvailable = surface.Active()

	if err == nil {
		res.Text = loop.Text
		res.Rounds = loop.RoundsUsed
		res.InputTokens, res.OutputTokens = loop.InputTokens, loop.OutputTokens
		res.Model = loop.Model
		res.Executions = loop.Executions
		return res
	}

	// The failure paths all report the PARTIAL state. A child that spent
	// nine rounds and then hit its cap did nine rounds of work the parent
	// paid for; reporting zeros throws away both the transcript and the
	// only evidence of what it cost.
	partial := progress.Snapshot()
	res.Text = partial.Text
	res.Rounds = partial.RoundsUsed
	res.InputTokens, res.OutputTokens = partial.InputTokens, partial.OutputTokens
	res.Model = partial.Model
	res.Executions = partial.Executions
	res.Failed = true

	switch kind, reason := stopReason(ctx); {
	case kind == KindTimeout:
		res.TimedOut = true
		res.ErrorKind = KindTimeout
		res.Error = reason
		log.WarnContext(ctx, "subagent_timed_out", "role", cfg.Seat.Role.Name,
			"rounds", res.Rounds, "tokens", res.Tokens(), "reason", reason)
	case kind != "":
		res.ErrorKind = kind
		res.Error = reason
	case errors.Is(err, toolloop.ErrBudgetExhausted):
		res.ErrorKind = KindFailed
		res.Error = ledger.Elide(err.Error(), errorLimit)
		var be *toolloop.BudgetError
		if errors.As(err, &be) && be.Scope == ScopeSubagent {
			// The child's own slice, not the seat's cap. Reported as its own
			// kind so nobody goes looking for a company budget that never
			// ran out.
			res.ErrorKind = KindBudget
			log.WarnContext(ctx, "subagent_budget_exhausted", "role", cfg.Seat.Role.Name,
				"used", be.Used, "slice", be.Limit)
		}
	default:
		res.ErrorKind = KindFailed
		res.Error = ledger.Elide(err.Error(), errorLimit)
	}
	return res
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
	case errors.Is(cause, errChildDeadline):
		return KindTimeout, errChildDeadline.Error()
	case errors.Is(cause, errBatchDeadline):
		return KindTimeout, errBatchDeadline.Error()
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
func resolveProvider(cfg Config) (llm.Provider, string, error) {
	members, err := cfg.Models.Chain(cfg.Seat.Role, phase.Subagent)
	if err != nil {
		return nil, "", fmt.Errorf("subagent: %w", err)
	}
	c, err := chain.New(members, chain.Options{})
	if err != nil {
		return nil, "", fmt.Errorf("subagent: %w", err)
	}
	return c, members[0].Key, nil
}

// publishBatched emits the one fan-out summary event, best effort.
//
// Telemetry must never fail a spawn: the children have already run and their
// results are the parent's answer, so a broker that refuses this event must
// not turn a finished batch into a failed tool call.
func publishBatched(ctx context.Context, cfg Config, results []Result) {
	if cfg.Publisher == nil {
		return
	}
	var successes, tokens int
	for _, r := range results {
		if !r.Failed {
			successes++
		}
		tokens += r.Tokens()
	}
	ev := events.New(types.SubagentBatched{
		ParentHandle: cfg.Seat.Role.Handle(),
		TaskCount:    len(results),
		Successes:    successes,
		Failures:     len(results) - successes,
		TotalTokens:  tokens,
	}, cfg.Trace)
	// The payload carries no role, so the envelope's source is the only
	// attribution this event has — without it every fan-out in the company
	// renders as "system".
	ev.Source = cfg.Seat.Role.Name
	if err := cfg.Publisher.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "subagent_batched_publish_failed", "error", err)
	}
}

// renderCatalogue is the slim catalogue a discovery-capable child is shown:
// first-party tools by name and one-line description, MCP servers by name
// only.
//
// Servers are named rather than expanded for the same reason the planner's
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
		if desc := firstLine(e.Tool.Description()); desc != "" {
			lines = append(lines, "- "+e.Name()+": "+desc)
			continue
		}
		lines = append(lines, "- "+e.Name())
	}
	for _, server := range servers {
		lines = append(lines, "- MCP server `"+server+"` (use the discovery tools to list its tools)")
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
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

// --- the LLM-callable tool -------------------------------------------------

// Tool is `spawn_subagent`, bound to ONE parent turn.
//
// Bound rather than registered globally: everything a spawn needs — the
// parent's surface, its remaining budget, its trace — is per-turn. The
// alternative is a global registration that reaches those values by hanging a
// map off the tool-call context and validating its shape at every call. A
// closure over the turn's Config makes that whole class of "engine config
// missing or malformed" failure unrepresentable.
type Tool struct{ cfg Config }

var _ tools.Callable = (*Tool)(nil)

// NewTool binds the tool to a turn's config.
func NewTool(cfg Config) *Tool { return &Tool{cfg: cfg} }

// Name is the tool's name in the registry.
func (t *Tool) Name() string { return ToolName }

// Description is what the model is told this tool does.
func (t *Tool) Description() string {
	return "Spawn an ephemeral sub-agent to do a narrowly-scoped task on your " +
		"behalf (for example: web research, code execution, large-doc " +
		"summarisation). The sub-agent runs with ONLY the tools you name in " +
		"`tool_names` (subset of your own tools, minus spawn_subagent itself " +
		"and all colleague-surface tools), returns a concise text answer, and " +
		"cannot itself spawn further sub-agents or contact colleagues. Use this " +
		"when the task is a bounded capability call rather than work that " +
		"belongs with a colleague."
}

// Parameters is the tool's JSON Schema.
func (t *Tool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_prompt": map[string]any{
				"type": "string",
				"description": "The concrete task the sub-agent should complete. " +
					"Seeded as its first user message. For a single sub-agent. " +
					"Mutually exclusive with `tasks`.",
			},
			"system_prompt": map[string]any{
				"type": "string",
				"description": "Task-specific system prompt for the sub-agent. The " +
					"runtime appends the mandatory preamble (no further sub-agents, " +
					"no colleague contact, return a concise final answer) " +
					"automatically.",
			},
			"tool_names": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Names of tools the sub-agent(s) may call. Must be a " +
					"subset of your own tool list; `spawn_subagent`, `a2a_ask`, and " +
					"outbound write tools to shared surfaces (channel post, issue " +
					"comment, pull request) are rejected regardless. The SAME " +
					"allowlist applies to all children in a batched call.",
			},
			"max_turns": map[string]any{
				"type": "integer",
				"description": "Optional cap on tool-call rounds for a single " +
					"sub-agent. Capped at the runtime's configured maximum; asking " +
					"for more is silently clamped. Ignored when `tasks` is provided " +
					"— use the per-task `max_turns` instead.",
			},
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_prompt":   map[string]any{"type": "string"},
						"system_prompt": map[string]any{"type": "string"},
						"max_turns":     map[string]any{"type": "integer"},
					},
					"required": []any{"task_prompt", "system_prompt"},
				},
				"description": "Fan out N sub-agents in parallel. Use for independent " +
					"workstreams that can run concurrently (parallel research, batch " +
					"reads, multi-source summarisation). Each entry needs its own " +
					"`task_prompt` and `system_prompt`; the batch shares one " +
					"`tool_names` allowlist. The whole batch shares one budget slice " +
					"— greedy children can starve siblings. Results are returned in " +
					"input order. Mutually exclusive with the single-shape fields " +
					"above (`task_prompt` / `system_prompt` / `max_turns`).",
			},
		},
	}
}

// spawnArgs is the tool's argument shape. The struct tags ARE the schema
// above; decoding through JSON rather than field by field keeps one
// definition of the mapping instead of two that can disagree.
type spawnArgs struct {
	TaskPrompt   string     `json:"task_prompt"`
	SystemPrompt string     `json:"system_prompt"`
	ToolNames    []string   `json:"tool_names"`
	MaxTurns     turnCount  `json:"max_turns"`
	Tasks        []taskArgs `json:"tasks"`
}

type taskArgs struct {
	TaskPrompt   string    `json:"task_prompt"`
	SystemPrompt string    `json:"system_prompt"`
	MaxTurns     turnCount `json:"max_turns"`
}

// turnCount is a round cap as a model actually emits one.
//
// Tool arguments are model output, and models emit an integer as a bare
// number, a float and a quoted string more or less at random. Two shapes are
// REFUSED rather than coerced, because both have a silently wrong reading:
// `3.9` truncates to 3 and mis-caps the child, and `true` is a Go bool that
// numeric coercion would turn into 1 — a one-round sub-agent that can do
// nothing but answer.
type turnCount int

func (t *turnCount) UnmarshalJSON(b []byte) error {
	text := strings.TrimSpace(string(b))
	if text == "null" {
		return nil
	}
	// A quoted number is the common string form; anything else quoted falls
	// through to the numeric parse below and is refused there.
	if unquoted, err := strconv.Unquote(text); err == nil {
		text = strings.TrimSpace(unquoted)
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return fmt.Errorf("max_turns must be a whole number, got %s", string(b))
	}
	if n < 1 {
		return fmt.Errorf("max_turns must be at least 1, got %d", n)
	}
	*t = turnCount(n)
	return nil
}

// Call runs the spawn the model asked for.
func (t *Tool) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	_, batched := args["tasks"]

	var parsed spawnArgs
	if err := remarshal(args, &parsed); err != nil {
		return failed(err.Error()), nil
	}

	// Mutual exclusion is checked here rather than expressed as a oneOf in
	// the schema: tool-call serialisers routinely flatten a oneOf into a
	// hybrid payload carrying both branches, and the engine then has to
	// guess which one the model meant.
	single := parsed.TaskPrompt != "" || parsed.SystemPrompt != ""
	if single && batched {
		return failed("spawn_subagent: pass either `task_prompt` + `system_prompt` " +
			"(single) or `tasks` (batched), not both"), nil
	}

	if batched {
		if len(parsed.Tasks) == 0 {
			return failed("spawn_subagent: `tasks` must not be empty"), nil
		}
		req := BatchRequest{ToolNames: parsed.ToolNames}
		for _, task := range parsed.Tasks {
			req.Tasks = append(req.Tasks, Task{
				TaskPrompt:   task.TaskPrompt,
				SystemPrompt: task.SystemPrompt,
				MaxTurns:     int(task.MaxTurns),
			})
		}
		results, err := SpawnBatch(ctx, t.cfg, req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return tools.Result{}, ctxErr
			}
			return failed(err.Error()), nil
		}
		// The tool itself SUCCEEDED even when children failed: per-child
		// errors ride inside the payload so the planner can pick out which
		// sub-tasks need a retry. Marking the whole call failed would tell
		// it to throw away the siblings that worked.
		return tools.Result{Output: renderBatch(results)}, nil
	}

	result, err := Spawn(ctx, t.cfg, Request{
		TaskPrompt:   parsed.TaskPrompt,
		SystemPrompt: parsed.SystemPrompt,
		ToolNames:    parsed.ToolNames,
		MaxTurns:     int(parsed.MaxTurns),
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return tools.Result{}, ctxErr
		}
		return failed(err.Error()), nil
	}
	if result.ErrorKind == KindCancelled {
		// The turn is being torn down. Reporting this to the model is noise
		// on a conversation that is about to stop; the loop's own context
		// check is what ends it.
		return tools.Result{}, context.Cause(ctx)
	}
	if result.Failed {
		// The partial text goes back WITH the reason. A child that spent
		// eight rounds before its cap usually produced most of an answer,
		// and discarding it makes the parent re-run the whole task.
		out := "sub-agent " + result.ErrorKind + ": " + result.Error
		if strings.TrimSpace(result.Text) != "" {
			out += "\n\nPartial output:\n" + result.Text
		}
		return failed(out), nil
	}
	return tools.Result{Output: result.Text}, nil
}

// childReport is one child's line in a batched tool result.
type childReport struct {
	Index         int      `json:"index"`
	Text          string   `json:"text"`
	TurnsUsed     int      `json:"turns_used"`
	TokensUsed    int      `json:"tokens_used"`
	RejectedTools []string `json:"rejected_tools,omitempty"`
	TimedOut      bool     `json:"timed_out"`
	Error         string   `json:"error,omitempty"`
	Model         string   `json:"model,omitempty"`
}

// renderBatch is the batched tool's output: JSON, so the planner can read the
// per-child split rather than a prose blob it has to re-parse.
func renderBatch(results []Result) string {
	reports := make([]childReport, 0, len(results))
	for i, r := range results {
		reports = append(reports, childReport{
			Index: i, Text: r.Text, TurnsUsed: r.Rounds, TokensUsed: r.Tokens(),
			RejectedTools: r.Rejected, TimedOut: r.TimedOut, Error: r.Error, Model: r.Model,
		})
	}
	blob, err := json.Marshal(map[string]any{"results": reports})
	if err != nil {
		// Nothing in childReport can refuse to marshal, but returning an
		// empty string here would hand the planner silence for a batch that
		// ran and cost tokens.
		log.Error("subagent_batch_render_failed", "error", err)
		return fmt.Sprintf("%d sub-agents ran; their results could not be rendered: %v",
			len(results), err)
	}
	return string(blob)
}

// remarshal moves a decoded argument map into a typed struct, via JSON
// because the arguments already came off the wire as JSON and the struct tags
// are the schema.
func remarshal(args map[string]any, into any) error {
	blob, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("spawn_subagent: arguments could not be re-encoded: %w", err)
	}
	if err := json.Unmarshal(blob, into); err != nil {
		return fmt.Errorf("spawn_subagent: arguments do not match the schema: %w", err)
	}
	return nil
}

// failed is a tool refusal the model reads and can act on.
func failed(msg string) tools.Result { return tools.Result{Output: msg, Failed: true} }
