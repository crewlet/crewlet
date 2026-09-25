package engine

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/tools"
)

// Company is one immutable configuration epoch.
//
// EVERYTHING A TURN READS COMES FROM ONE OF THESE, taken by value at the top
// of the turn. Reading each setting from a live cell on every access lets a
// hot reload landing mid-turn change the round cap between the executor and
// the reviewer, and then needs a context-local "pin" to paper over it. An
// epoch that is replaced rather than mutated makes that unrepresentable: an
// in-flight turn holds the one it started under until it ends.
type Company struct {
	Config *config.Company
	Org    *org.Organization

	// Models is the company's providers.llm, built. NIL WHEN IT HAS NONE,
	// which is a valid company rather than a broken one: an org chart
	// written before its credentials exist validates, applies and places
	// its seats. What it cannot do is think, so the dispatcher holds every
	// delivery on the seat's inbox while this is nil (see
	// [Engine.conditionsFor]) and the apply that brings a provider releases
	// them. The nil registry answers every method as a company with no
	// models ([phase.ErrNoProviders]), so a consumer that forgets to ask
	// is refused rather than crashed.
	Models *phase.Registry

	// Tools is the catalogue every seat's surface is cut from: the
	// builtins, plus the SHARED MCP servers, which one company-wide child
	// serves for everyone.
	//
	// It is not the whole surface. A `shared: false` server is a template
	// that gives each role its own child holding that role's credentials,
	// and two children of one template publish the same tool names — so
	// they cannot live in one registry without one shadowing the other and
	// every seat calling whichever won.
	//
	// A seat this node has CLAIMED runs against a clone of this with its
	// own children filed in, and that clone lives on the [Engine] rather
	// than here: it belongs to the seat's LEASE, and an epoch is replaced
	// wholesale by every apply. See the seatTools field in run.go.
	Tools *tools.Registry
}

// NewCompany builds an epoch from a validated config.
//
// It does NOT reach the network. Building an epoch must be something a
// `validate` command can do, and a constructor that dialled a provider would
// make config validation depend on the vendor being up.
func NewCompany(c *config.Company) (*Company, error) {
	// ENV-ONLY, which is what makes `crewlet validate` work on a laptop:
	// the secret store lives in a database this path must not need.
	return NewCompanyWith(c, config.EnvOnly())
}

// NewCompanyWith is [NewCompany] over a caller-supplied ${VAR} resolver.
//
// THE ONE SEAM THE SECRET STORE PLUGS INTO. A running node passes a chain
// with the store in front of the environment, so a rotated secret wins over
// a stale `.env` that was exported into the process months ago — which is
// the whole point of having a store, and is a rule that has to hold for
// EVERY value, not just the ones whose call site remembered.
func NewCompanyWith(c *config.Company, env *config.Resolver) (*Company, error) {
	if env == nil {
		env = config.EnvOnly()
	}
	return newCompany(c, env)
}

func newCompany(c *config.Company, env *config.Resolver) (*Company, error) {
	if c == nil {
		return nil, fmt.Errorf("engine: no company config")
	}
	// VALIDATED HERE, not merely assumed. ParseCompany validates, but a
	// Company is an exported struct an embedder can build directly — and an
	// epoch assembled from an invalid one is a company that boots and then
	// fails at its first turn, which is the worst place to learn it.
	//
	// It is also what lets everything below rely on the invariants instead
	// of re-checking them: a validated role always yields a handle, so the
	// seat walk needs no empty-handle guard.
	//
	// The RUNNABLE rules only. An epoch is built from stored revisions, and
	// one that breaks an admission rule added after it was stored still runs
	// as it always did; refusing to build it would take a working company
	// down on upgrade. A submitted document met the admission rules at the
	// door it came through. See [config.Company.ValidateRunnable].
	if err := c.ValidateRunnable(); err != nil {
		return nil, fmt.Errorf("engine: invalid company config: %w", err)
	}
	organization, err := c.Organization()
	if err != nil {
		return nil, fmt.Errorf("engine: organization: %w", err)
	}
	models, err := buildProviders(c, env)
	if err != nil {
		return nil, err
	}
	return &Company{
		Config: c,
		Org:    organization,
		Models: models,
		Tools:  tools.NewRegistry(),
	}, nil
}

// Seats returns the company's agent seats, for the placement sweep.
//
// AGENT seats only. A human seat is addressable and never spawned, so
// including one would make the fleet try to claim a lease for something no
// node can run — and then report the company permanently under capacity.
func (c *Company) Seats() []placement.Seat {
	// NIL IS NO SEATS, and the sweep asks on every tick: an unconfigured
	// node has an empty seat set rather than an unanswerable question, and
	// converges the moment its first epoch arrives.
	if c == nil {
		return nil
	}
	var out []placement.Seat
	for role := range c.Org.AllRoles() {
		// Through the predicate, not a comparison against KindAgent: an
		// UNSET kind is an agent, and spelling the rule out here would
		// have excluded every role that did not name its kind — which is
		// most of them, and produces a company with no seats at all.
		if !role.IsAgent() {
			continue
		}
		// No empty-handle guard: validation refuses any role whose name
		// yields no handle ("the name yields no handle, so set one
		// explicitly"), and NewCompany validates. Probed, not assumed —
		// "!!!", "---" and "日本" are all refused at parse.
		out = append(out, placement.Seat{Handle: role.Handle(), Placement: role.Placement})
	}
	// Sorted, because this feeds the placement math and the sweep compares
	// its own answer across ticks. An org walk's order is stable today but
	// is not a property the org model promises, and a fleet that reshuffled
	// its eligibility list every tick would churn seats for no reason.
	slices.SortFunc(out, func(a, b placement.Seat) int {
		return cmp.Compare(a.Handle, b.Handle)
	})
	return out
}

// RunnerFor builds the phase runner for one seat, against the tool surface
// this node gives it.
//
// Per turn, not cached. A runner holds the turn's task and its conversation
// history, both of which are per-turn facts; caching one per seat would carry
// the previous turn's ask into the next one.
//
// reg is an explicit PARAMETER rather than something the epoch looks up,
// because the seat's own surface is a fact about this node's leases and not
// about the configuration: only the node that claimed the seat has its
// per-role children. Pass [Engine.seatRegistry]; nil falls back to the
// epoch's shared surface, which is the correct answer for a seat that
// declares no per-role server and for any caller that holds no lease.
func (c *Company) RunnerFor(handle string, reg *tools.Registry, in RunnerInput) (*runner.Runner, error) {
	role := c.Org.AgentSeatByHandle(handle)
	if role == nil {
		return nil, fmt.Errorf("engine: %q is not an agent seat in this company", handle)
	}
	if c.Models == nil {
		// Refused HERE, naming the seat and the fix, rather than by the
		// runner's own nil check, which reads as a wiring fault. The
		// dispatcher never gets this far for a company with no models, so
		// what reaches it is a path that bypasses the inbox: a coding run
		// resuming after a revision removed every provider.
		return nil, fmt.Errorf("engine: seat %q cannot take a turn: %w", handle, phase.ErrNoProviders)
	}
	if reg == nil {
		reg = c.Tools
	}
	// REFUSED HERE TOO, for the reason [turn.Run] refuses it: this field
	// arms submit_work's own citation check, and a caller that omits it
	// does not get a weaker check, it gets none — `no_action` accepted on a
	// turn somebody is waiting on, and a citation naming any surface at all.
	// The resume path omitted it while the dispatch path did not, so the two
	// halves of one contract disagreed with nothing to say so.
	if !in.Reply.Valid() {
		return nil, fmt.Errorf(
			"engine: RunnerInput.Reply is %q for %q, which is not one of %q, %q "+
				"or %q — derive it from the trigger (ReplyFor) or carry it off the "+
				"pending run's row, because the submission check reads it",
			in.Reply.Kind, handle, turn.ReplyNone, turn.ReplyTool, turn.ReplyEngine)
	}
	te := c.Config.TurnEngine
	del := te.Delegation
	return runner.New(runner.Config{
		Seat:     prompts.Seat{Org: c.Org, Role: role},
		Registry: reg,
		Models:   c.Models,
		Caps: runner.Caps{
			ExecutorRounds:  te.MaxToolRounds,
			ExecutorCeiling: te.ExecuteMaxToolRoundsCeiling,
			ExtensionStep:   te.ExtensionRoundStep,
			ExtensionOn:     te.ExtensionEnabled.Or(true),
		},
		Budget: in.Budget,
		Judge:  in.Judge,
		// Threaded from the caller rather than resolved here, because
		// whether this node holds the seat is a fact about its leases and
		// not about the configuration — the same reason reg is a
		// parameter.
		Fence: in.Fence,
		// The company's own delegation caps AND the seat's visible worker
		// templates, from the SAME pinned epoch as the round caps above,
		// so a revision landing mid-turn cannot move a cap a call is
		// judged against or add a worker to a graph that is already
		// planned.
		Subagent: &runner.SubagentConfig{
			Limits: subagent.Limits{
				MaxTurns:         del.MaxTurns,
				MaxTasksPerCall:  del.MaxTasksPerCall,
				TaskTimeout:      seconds(del.TaskTimeoutSeconds),
				CallTimeout:      seconds(del.CallTimeoutSeconds),
				MaxParallel:      del.MaxParallel,
				BudgetFraction:   del.BudgetFraction,
				MinTokensPerTask: del.MinTokensPerTask,
			},
			// CLONED, because the live config cell is replaced wholesale
			// by an apply and a turn holding the old map would otherwise
			// be reading a schema the next apply is free to mutate.
			Workers:   config.CloneWorkers(c.Config.WorkersFor(handle)),
			Remaining: in.Remaining,
		},
		Task:         in.Task,
		Context:      in.Context,
		Reply:        in.Reply,
		Conversation: in.Conversation,
		Skills:       in.Skills,
		SkipNames:    MetaToolNames(),
		Publisher:    in.Publisher,
		Turn:         in.Turn,
		// The working indicator's phase hook, threaded per turn like the
		// publisher beside it: which turn's indicator a phase moves is the
		// caller's answer, not the epoch's.
		OnPhase: in.OnPhase,
		Onboarding: runner.Onboarding{
			Markers: in.Markers, Latch: in.Latch,
			Rounds:  te.OnboardingMaxToolRounds,
			Ceiling: te.OnboardingMaxToolRoundsCeiling,
		},
		Resume: in.Resume,
		// The executor's RUNTIME: nil for the native tool loop, non-nil
		// for a coding CLI in agent mode. Supplied by the caller rather
		// than resolved here, because building it needs the engine and
		// the turn — and there is one helper behind both call sites, so
		// a turn cannot change runtime by being resumed.
		AgentRun: in.AgentRun,
		// The turn's note box, opened by the caller because the caller is
		// what files it where a person's note can find it.
		Steer: in.Steer,
	})
}

// RunnerInput is the per-turn half of a runner's configuration.
type RunnerInput struct {
	Task         string
	Conversation string

	// Skills is the company's tool-skill registry, threaded per turn like
	// everything else the runner reads: the registry itself outlives an
	// epoch, but which registry a turn runs against is the node's answer
	// at the moment the turn started.
	Skills *skills.Registry

	// Context is the turn's prefetched prompt blocks, judged against THIS
	// turn's trigger and frozen before the runner is built.
	//
	// There is no re-fetch seam beside it any more. One existed for the
	// thin-trigger case: a pointer the turn-start search could not use,
	// re-searched between the planning and acting phases on the plan the
	// first had written. With one phase deciding and acting there is
	// nothing between them to hang it on, and the executor asks instead,
	// with search_knowledge, over the same seam.
	Context prefetch.Blocks

	// Reply says who is waiting for this turn, derived from the trigger
	// before the turn starts. See [turn.Reply].
	Reply turn.Reply

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

	// Remaining reads the seat's token headroom for a sub-agent spawn.
	// Nil means the seat is uncapped, which is what a company with no
	// token budget already is — and is NOT the same as a read that failed,
	// which refuses the spawn rather than granting it no ceiling.
	Remaining runner.Remaining

	// Publisher receives the phase telemetry, and Turn identifies the turn
	// it belongs to. Nil publishes nothing — the right answer for a runner
	// a test drives directly, and the reason both are per-turn inputs
	// rather than epoch configuration.
	Publisher queue.Publisher
	Turn      runner.Turn

	// OnPhase moves the working indicator's wording as the turn changes
	// phase. Nil is a turn nobody is watching a composer for — see
	// [Engine.beginWorkingStatus], whose nil session's Phase is a no-op, so
	// this is wired unconditionally rather than branched on.
	OnPhase func(phase.Phase)

	// AgentRun runs this turn's executor as a coding CLI's own agentic
	// run. Nil is the native tool loop. Built by [Engine.agentRunFor],
	// which BOTH RunnerFor call sites use — a turn whose executor ran as
	// an agentic loop and came back to a native one would rebuild a
	// surface for a conversation that never existed.
	AgentRun runner.AgentLauncher

	// Markers and Latch drive the first-turn onboarding pass. Nil markers
	// disable it: without somewhere to mark, the pass would run every turn
	// forever. The latch is the PROCESS's, not the turn's — it is what
	// stops a transient marker-read failure re-onboarding a seat this
	// process has already seen marked.
	Markers runner.Markers
	Latch   *runner.Latch

	// Resume makes this runner's turn a RE-ENTRY into a suspended Execute
	// conversation rather than a fresh turn. Nil is the ordinary case.
	Resume *runner.Resume

	// Steer is the turn's box of notes from a person, opened by
	// [steerBox] from the SAME launcher as AgentRun above — a turn whose
	// executor is a coding CLI's own loop answers every note
	// `unsupported`. Nil is a turn nobody can steer: every test that
	// drives a runner directly.
	Steer *steer.Box
}

// TurnSettings is the loop's pinned configuration for this epoch.
//
// wallClock is the cap THIS turn's trigger carried, in seconds, zero for the
// triggers that carry none. It is a per-turn argument rather than a config
// field because it comes off the fire, not the epoch.
func (c *Company) TurnSettings(wallClock int) turn.Settings {
	te := c.Config.TurnEngine
	return turn.Settings{
		MaxIterations:        te.MaxIterations,
		DelegationDepthLimit: te.DelegationDepthLimit,
		SkipNames:            MetaToolNames(),
		MaxWallClock:         time.Duration(wallClock) * time.Second,
	}
}

// MetaToolNames are the tools the ledger filters out.
//
// A meta-tool is never a delivery, so in a record whose only job is "what
// already happened that matters" it is pure noise. Named here rather than in
// the ledger because the ledger imports nothing from crewlet, deliberately.
func MetaToolNames() []string {
	return []string{"activate_tool", "list_mcp_server_tools"}
}
