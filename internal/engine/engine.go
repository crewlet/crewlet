package engine

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// Company is one immutable configuration epoch.
//
// Everything a turn reads comes from one, taken by value at the top of the
// turn. An epoch is replaced, never mutated, so a hot reload landing mid-turn
// cannot change the round cap between the executor and the reviewer: an
// in-flight turn holds the one it started under until it ends.
type Company struct {
	Config *config.Company
	Org    *org.Organization

	// Models is the company's providers.llm, built. NIL WHEN IT HAS NONE, a
	// valid company: a chart written before its credentials exist applies
	// and places its seats, and the dispatcher holds every delivery on the
	// seat's inbox until an apply brings a provider ([Engine.conditionsFor]).
	// The nil registry answers as a company with no models
	// ([phase.ErrNoProviders]), so a consumer that forgets to ask is refused
	// rather than crashed.
	Models *phase.Registry

	// Helpers are the entries a helper (Ask Crewlet, the Advisor) runs on,
	// each built again as a helper's run ([buildHelpers]), by key. Nil where
	// no helper runs on any.
	Helpers map[string]llm.Provider
	// Searchers are Ask Crewlet's search runs of the entries its model runs
	// on ([buildSearchers]), by key; an entry whose harness has none is absent.
	Searchers map[string]*harness.Provider

	// contacts resolves the `${VAR}` contact ids in the prompts' roster
	// through the chain this epoch was built with (the secret store, then the
	// handed environment), so a prompt reads an id as notification routing
	// does. Never the process environment behind the chain's back.
	contacts org.EnvLookup

	// Tools is the catalogue every seat's surface is cut from: the builtins
	// plus the SHARED MCP servers.
	//
	// It is not the whole surface: a `shared: false` server gives each role
	// its own child holding that role's credentials, and two children of one
	// template publish the same tool names, so in one registry one would
	// shadow the other for every seat. A seat this node has CLAIMED
	// runs against a clone with its own children filed in, kept on the
	// [Engine] because it belongs to the seat's lease and an epoch is
	// replaced by every apply (the seatTools field in run.go).
	Tools *tools.Registry

	// AskTool is `ask_and_wait`, the tool an executor's run asks a person
	// through and parks on (ADR-0068): offered on that run's surface alone
	// ([runner.Config.Ask]), since no other phase has a run to park. Built
	// with the builtins ([Engine.equip]); nil on an epoch built without an
	// engine.
	AskTool runner.RunAsk

	// CheckoutTool is `code_checkout`, an executor's run's checkout of a
	// Gitlet repository (ADR-0093), on that run's surface alone
	// ([runner.Config.Checkout]) for AskTool's reason: it grants to the
	// running job's row. Built with the builtins where the epoch's code tools
	// serve ([Engine.equip]); nil on an epoch whose company hosts no code.
	CheckoutTool tools.Callable

	// RunsAround is what an executor's run sees the runs in flight around it
	// with (ADR-0176): the read its launch's brief lists, and `list_runs`,
	// `message_run` and `wait_for_run` on its surface ([runner.Config.Runs]).
	// Built with the builtins ([Engine.equip]); nil on an epoch built without
	// an engine.
	RunsAround runner.RunsAround
	// HoldTool records an executor's run's call to a tool at Needs approval
	// as the question it parks on (ADR-0077), on that run's surface alone
	// ([runner.Config.Hold]). Nil without an engine or the fleet keyring's
	// cipher, whose runs are refused every such call.
	HoldTool tools.Holder

	// ActivatedAt is the instant this epoch's revision was activated (the
	// activation pointer's [coord.Activation.At], or at boot the row's
	// [store.Revision.ServedAt]), in UTC, stamped once at build
	// ([stampActivation]). Zero for a company no activation named (one handed
	// to [New] in code); nothing derived from the configuration may be
	// written from a zero instant.
	//
	// Every row derived from the configuration is stamped with it
	// ([configplane.ActivationStamp]) so an older configuration applied late
	// on another node cannot walk a newer one back. Never the applying node's
	// clock: every node applies one activation separately, at each reconcile
	// tick and boot. See [Engine.applyChart].
	ActivatedAt time.Time

	// codeInForce is Gitlet's settings (limits, deleted retention) IN FORCE
	// at this epoch, stamped at install ([Engine.installEpoch],
	// [carriedCode]): its own `code` block where it sets `code.backend:
	// gitlet`, else the newest earlier hosting epoch's, which a committer
	// and a retention duty running from it keep deciding with until a
	// restart. Nil when never installed or with no hosting epoch before it
	// ([Engine.codeInForce]).
	codeInForce *config.Code

	// roster is this epoch's chart as Gitlet reads a seat's grant
	// ([codeRoster]), built at its first question and stamped at install
	// ([Engine.installEpoch], [rosterOf]). Nil when never installed, which
	// [Engine.codeRoster] answers as no chart: code reads refuse so another
	// node is asked, never treating the seat as standing nowhere. A function,
	// so an epoch copied whole copies no lock.
	roster func() codeRoster
	// catalogue is the sandbox catalogue this epoch was built with
	// ([buildSandbox]): cells, runners, engine-wide setup steps and the
	// default turn cap. Nil where the company reaches no cell.
	//
	// PINNED WITH THE EPOCH: every launch of its turns places by it, never by
	// the node's live manager, which could resolve a pinned entry to a cell
	// this revision never configured or refuse a run_in it just dropped. The
	// coordinator keeps the live manager, whose newest catalogue carries every
	// cell a run already out may be in ([sandbox.Coordinator.SetManager]).
	catalogue *sandbox.Manager
}

// NewCompany builds an epoch from a validated config.
//
// It does NOT reach the network, so `crewlet validate` does not depend on a
// vendor being up.
func NewCompany(c *config.Company) (*Company, error) {
	// Env-only: the secret store lives in a database `validate` must not need.
	return NewCompanyWith(c, config.EnvOnly())
}

// NewCompanyWith is [NewCompany] over a caller-supplied ${VAR} resolver.
//
// The one seam the secret store plugs into: a running node passes a chain
// with the store in front of the environment, so a rotated secret wins over a
// stale exported `.env` for every value.
func NewCompanyWith(c *config.Company, env *config.Resolver) (*Company, error) {
	if env == nil {
		env = config.EnvOnly()
	}
	return newCompany(c, env, nil, nil)
}

// processEUID is the user this engine runs as, the one fact
// [config.Company.ValidateHost] needs. A variable so a test judges a root
// engine without being one, and a suite run as root does not change every
// case that builds an epoch.
var processEUID = os.Geteuid

// newCompany builds an epoch whose harness entries keep their sit-outs on
// plans, the node's registry ([harness.Plans]), and whose LLM fleets balance
// on ledger, the node's memory ([fleet.Ledger]); nil for an epoch nothing runs.
func newCompany(c *config.Company, env *config.Resolver, plans *harness.Plans,
	ledger *fleet.Ledger) (*Company, error) {
	if c == nil {
		return nil, fmt.Errorf("engine: no company config")
	}
	// VALIDATED HERE: a Company can be built directly, bypassing
	// ParseCompany, and an invalid epoch boots then fails at its first turn.
	// Everything below relies on it (a validated role always yields a
	// handle). The RUNNABLE rules only: a stored revision a newer peer
	// admitted under rules this build lacks still runs, or the fleet's epoch
	// splits; admission rules ran at the door ([config.Company.ValidateRunnable]).
	if err := c.ValidateRunnable(); err != nil {
		return nil, fmt.Errorf("engine: invalid company config: %w", err)
	}
	if err := c.ValidateHost(processEUID()); err != nil {
		return nil, fmt.Errorf("engine: this node cannot launch the company config: %w", err)
	}
	organization, err := c.Organization()
	if err != nil {
		return nil, fmt.Errorf("engine: organization: %w", err)
	}
	models, err := buildProviders(c, env, plans, ledger)
	if err != nil {
		return nil, err
	}
	helpers, err := buildHelpers(c, env, plans)
	if err != nil {
		return nil, err
	}
	searchers, err := buildSearchers(c, env, plans)
	if err != nil {
		return nil, err
	}
	// Described before anything is filed, so every MCP tool here and in each
	// seat's clone carries its server's description into the prompt.
	registry := tools.NewRegistry()
	registry.DescribeServers(serverDescriptions(c))
	return &Company{
		Config:    c,
		Org:       organization,
		Models:    models,
		Helpers:   helpers,
		Searchers: searchers,
		Tools:     registry,
		contacts:  env.LookupOK,
	}, nil
}

// Seats returns the company's agent seats, for the placement sweep.
//
// AGENT seats only: a human seat is never spawned, and claiming a lease for
// it would report the company permanently under capacity.
func (c *Company) Seats() []placement.Seat {
	// Nil is no seats: the sweep asks every tick, and an unconfigured node
	// converges the moment its first epoch arrives.
	if c == nil {
		return nil
	}
	var out []placement.Seat
	for role := range c.Org.AllRoles() {
		// The predicate, not a KindAgent comparison: an UNSET kind is an
		// agent, and most roles name none.
		if !role.IsAgent() {
			continue
		}
		// No empty-handle guard: NewCompany validates, and validation
		// refuses a name that yields no handle.
		//
		// The id rides with the seat: the host and the mailbox sweep build
		// durable names from it where the company view is gone. A seat with
		// no id is still listed, so the host refuses it loudly rather than
		// it vanishing from the placement arithmetic.
		id, _ := c.Org.AgentIDFor(role)
		out = append(out, placement.Seat{
			ID: id, Handle: role.Handle(), Placement: role.Placement})
	}
	// Sorted: the sweep compares its answer across ticks, and the org walk's
	// order is not promised, so an unsorted list could churn seats.
	slices.SortFunc(out, func(a, b placement.Seat) int {
		return cmp.Compare(a.Handle, b.Handle)
	})
	return out
}

// RunnerFor builds the phase runner for one seat, against the tool surface
// this node gives it.
//
// Per turn, not cached: a runner holds the turn's task and history, and a
// cached one would carry the previous turn's ask into the next.
//
// reg is a parameter because the seat's surface is a fact about this node's
// leases, not the configuration: only the claiming node has its per-role
// children. Pass [Engine.seatRegistry]; nil falls back to the epoch's shared
// surface as the seat holds it ([grantedSurface]), right for a seat with no
// per-role server and for a caller holding no lease.
func (c *Company) RunnerFor(ctx context.Context, handle string, reg *tools.Registry, in RunnerInput) (_ *runner.Runner, err error) {
	defer origin.Stamp(&err)
	role := c.Org.AgentSeatByHandle(handle)
	if role == nil {
		return nil, fmt.Errorf("engine: %q is not an agent seat in this company", handle)
	}
	if c.Models == nil {
		// Refused here, naming the seat, rather than by the runner's nil
		// check, which reads as a wiring fault. Only a path that bypasses the
		// inbox reaches this: a resume after a revision removed every provider.
		return nil, fmt.Errorf("engine: seat %q cannot take a turn: %w", handle, phase.ErrNoProviders)
	}
	if reg == nil {
		reg = grantedSurface(c, role)
	}
	// Refused here too, as [turn.Run] refuses it: Reply arms submit_work's
	// citation check, and omitting it disables the check entirely
	// (`no_action` accepted on a turn somebody waits on), so the dispatch and
	// resume paths must both carry it.
	if !in.Reply.Valid() {
		return nil, fmt.Errorf(
			"engine: RunnerInput.Reply is %q for %q, which is not one of %q, %q "+
				"or %q — derive it from the trigger (ReplyFor) or carry it off the "+
				"pending run's row, because the submission check reads it",
			in.Reply.Kind, handle, turn.ReplyNone, turn.ReplyTool, turn.ReplyEngine)
	}
	// A pickup's turn carries the read of its answer, for the same reason:
	// without it the submission checks hold the turn to nothing on its item
	// (ADR-0122).
	if in.Reply.Item != "" && in.PickupAnswer == nil {
		return nil, fmt.Errorf("engine: RunnerInput.Reply names item %q for %q and no "+
			"PickupAnswer reads it: build it with Engine.pickupAnswerFor", in.Reply.Item, handle)
	}
	te := c.Config.TurnEngine
	del := te.Delegation
	return runner.New(runner.Config{
		// The turn's own reading of who may be reached, so the prompt's
		// roster leaves out exactly whom its tools do, and the chain its
		// ${VAR} contact ids resolve through.
		Seat: prompts.Seat{Org: c.Org, Role: role, Env: c.contacts,
			Withheld: in.Turn.Context.WithholdsContacts,
			Chatlet:  c.Config.ChatBackendFor() == config.ChatChatlet},
		Registry: reg,
		// What this seat's surfaces leave out, every call of which it would
		// be refused: builtins the authority table refuses before any object
		// is named (health tools without a health grant), and code writes
		// where its code tier writes no repository ([Company.withheldFrom]).
		Withheld: c.withheldFrom(ctx, handle, reg),
		Models:   c.Models,
		// The process's, threaded per turn: the session directory is not
		// configuration.
		Sessions: in.Sessions,
		Metrics:  in.Metrics,
		// The onboarding pass's extension policy, the only engine loop a cap
		// is extended in: the executor's rounds are its harness's own and the
		// reviewer's budget is structural.
		Caps: runner.Caps{
			ExtensionStep: te.ExtensionRoundStep,
			ExtensionOn:   te.ExtensionEnabled.Or(true),
		},
		Budget: in.Budget,
		Judge:  in.Judge,
		// Bound to the turn's attribution, so every rewrite the runner, its
		// ledgers and its workers ask for is this turn's cost.
		Compact: in.Compact.For(role, in.Turn.Context.Aux()),
		// From the caller, for reg's reason: a fact about this node's leases.
		Fence: in.Fence,
		// Delegation caps and worker templates from the same pinned epoch,
		// so a revision mid-turn cannot move a cap or add a worker to a
		// planned graph.
		Subagent: &runner.SubagentConfig{
			Limits: subagent.Limits{
				MaxTurns:         del.MaxTurns,
				MaxTasksPerCall:  del.MaxTasksPerCall,
				TaskTimeout:      seconds(del.TaskTimeoutSeconds),
				CallTimeout:      seconds(del.CallTimeoutSeconds),
				MaxParallel:      del.MaxParallel,
				BudgetFraction:   del.BudgetFraction,
				MinTokensPerTask: del.PerTaskFloor(),
			},
			// Cloned, so the turn never reads a map the next apply may
			// mutate.
			Workers:   config.CloneWorkers(c.Config.WorkersFor(handle)),
			Remaining: in.Remaining,
		},
		Task:         in.Task,
		Context:      in.Context,
		Reply:        in.Reply,
		PickupAnswer: in.PickupAnswer,
		Endings:      in.Endings,
		// Rendered at the dispatch and frozen with the endings (capability.go).
		Access:            in.Access,
		CapabilityChanges: in.CapabilityChanges,
		Skills:            in.Skills,
		SkipNames:         toolname.Discovery(),
		Publisher:         in.Publisher,
		Turn:              in.Turn,
		OnPhase:           in.OnPhase,
		Onboarding: runner.Onboarding{
			Markers: in.Markers, Latch: in.Latch,
			Rounds:  te.OnboardingRounds(),
			Ceiling: te.OnboardingMaxToolRoundsCeiling,
		},
		Resume: in.Resume,
		// From the caller: building it needs the engine and the turn, and
		// one helper serves dispatch and resume alike ([Engine.agentRunFor]).
		Launcher: in.Launcher,
		// Built with the epoch's builtins, so the run's evidence is checked
		// against the tracker and knowledge base a work item's decision is.
		Ask:      c.AskTool,
		Checkout: c.CheckoutTool,
		Hold:     c.HoldTool,
		Runs:     c.RunsAround,
		// Opened by the caller, which files it where a person's note finds it.
		Steer: in.Steer,
	})
}

// RunnerInput is the per-turn half of a runner's configuration.
type RunnerInput struct {
	// Task is the ask, and Endings how the conversation's earlier turns
	// ended, rendered at dispatch ([conversationRead]) and fixed for the
	// turn. Endings is empty on a resume, which thaws the frozen one.
	Task    string
	Endings string

	// Access is the seat's "## Your access" section and CapabilityChanges what
	// changed in it since the seat was last told, rendered at the dispatch
	// ([Engine.capabilityFor]) and frozen with Endings.
	Access            string
	CapabilityChanges string

	// Metrics is the node's recorder every surface of the turn counts into
	// ([runner.Config.Metrics]), the executor's bridged calls included:
	// [metrics.ToolPolicyRefusalLoops] and [metrics.ToolArgsOutsideSchema].
	// Nil counts nothing.
	Metrics *metrics.Recorder

	// Compact is the epoch's compactor; the runner binds it to the seat.
	// Nil rewrites nothing, and its readers carry a payload whole or name it
	// by size — never a cut.
	Compact *compact.Compactor

	// Skills is the company's tool-skill registry: it outlives an epoch, but
	// which one a turn runs against is the node's answer at turn start.
	Skills *skills.Registry

	// Context is the turn's prefetched prompt blocks, judged against THIS
	// turn's trigger and frozen before the runner is built. A thin trigger
	// gets no re-fetch: the executor asks with search_knowledge instead.
	Context prefetch.Blocks

	// Reply says who is waiting for this turn, derived from the trigger
	// before the turn starts. See [turn.Reply].
	Reply turn.Reply

	// PickupAnswer reads how the turn has left the item a work pickup
	// handed it ([Engine.pickupAnswerFor]); required where Reply names an
	// item, nil otherwise.
	PickupAnswer func(ctx context.Context) (types.PickupAnswer, error)

	// Budget is the shared token counter this turn charges. Nil is the
	// embedded single-node case, where no counter is shared with anyone.
	Budget toolloop.BudgetMeter

	// Judge decides round-cap extensions. Nil sends every exhaustion
	// straight to the rescue path.
	Judge extension.Judge

	// Fence stops the turn's tool loop the moment this node stops holding
	// the seat's grant, or a person's pause asks the running turn to stop.
	// Built by [Engine.seatFence]; nil is an open fence, which is every
	// test that drives a runner directly.
	Fence func() error

	// Remaining reads the seat's token headroom for a sub-agent spawn. Nil
	// means uncapped (no token budget), which is NOT a failed read: that
	// refuses the spawn.
	Remaining runner.Remaining

	// Publisher receives the phase telemetry, and Turn identifies the turn
	// it belongs to. Nil publishes nothing, as for a runner a test drives.
	Publisher queue.Publisher
	Turn      runner.Turn

	// Sessions is where the turn's phases serve their tools to their
	// harness: the engine's one [toolsession.Host] ([Engine.sessions]).
	// Required: a phase's tools reach its harness through nothing else, so
	// [runner.New] refuses a runner without it.
	Sessions *toolsession.Host

	// OnPhase moves the working indicator's wording as the turn changes
	// phase. Wired unconditionally: [Engine.beginWorkingStatus]'s nil
	// session's Phase is a no-op.
	OnPhase func(phase.Phase)

	// Launcher runs this turn's executor as its harness's own agentic run,
	// built by [Engine.agentRunFor] for both RunnerFor call sites. Required:
	// the runner refuses to be built without one.
	Launcher runner.AgentLauncher

	// Markers and Latch drive the first-turn onboarding pass. Nil markers
	// disable it, or it would run every turn. The latch is the PROCESS's, so
	// a transient marker-read failure cannot re-onboard a seat this process
	// has already seen marked.
	Markers runner.Markers
	Latch   *runner.Latch

	// Resume makes this runner's turn the COLLECTION of the executor run it
	// parked on rather than a fresh turn. Nil is the ordinary case.
	Resume *runner.Resume

	// Steer is where a person's notes to the turn wait: the first segment's
	// box, or the record of the run a resumed segment collects
	// ([Engine.runSteers]). Nil is a turn nobody can steer.
	Steer runner.SteerSource
}

// TurnSettings is the loop's pinned configuration for this epoch.
//
// deadline is the instant THIS turn's wall-clock cap ends ([wallClockDeadline]
// on the turn's own start, carried across its parks), zero where the trigger
// carries none: it comes off the fire, not the epoch.
func (c *Company) TurnSettings(deadline time.Time) turn.Settings {
	te := c.Config.TurnEngine
	return turn.Settings{
		MaxIterations:        te.MaxIterations,
		DelegationDepthLimit: te.DelegationDepthLimit,
		SkipNames:            toolname.Discovery(),
		Deadline:             deadline,
	}
}

// wallClockDeadline is the instant a turn begun at started stops starting
// rounds, for a fire's wall-clock cap of capSeconds — zero, no deadline, for
// the zero cap every other trigger carries. See [turn.Settings.Deadline].
func wallClockDeadline(started time.Time, capSeconds int) time.Time {
	if capSeconds <= 0 {
		return time.Time{}
	}
	return started.Add(time.Duration(capSeconds) * time.Second).UTC()
}
