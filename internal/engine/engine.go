package engine

import (
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
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
// hot reload landing mid-turn change the round cap between Plan and Execute —
// and then needs a context-local "pin" to paper over it. An epoch that
// is replaced rather than mutated makes that unrepresentable: an in-flight turn
// holds the one it started under until it ends.
type Company struct {
	Config *config.Company
	Org    *org.Organization
	Models *phase.Registry

	// Tools is the catalogue every seat's surface is cut from: the
	// builtins, plus the SHARED MCP servers, which one company-wide child
	// serves for everyone.
	//
	// It is not the whole surface. A `shared: false` server is a template
	// that gives each role its own child holding that role's credentials,
	// and two children of one template publish the same tool names — so
	// they cannot live in one registry without one shadowing the other and
	// every seat calling whichever won. Per-role tools go in seatTools.
	Tools *tools.Registry

	// seatTools is one registry per seat this NODE runs, built when the
	// seat is claimed and dropped when it is released.
	//
	// Only the seats this node holds: a fleet claims a slice of the
	// company each, and a node that spawned every seat's children would
	// run the whole company's MCP processes N times over. Absent means "no
	// per-role children here", and [Company.ToolsFor] then serves the
	// shared surface — which is the correct surface for a seat whose
	// company declares no per-role server at all.
	seatMu    sync.RWMutex
	seatTools map[string]*tools.Registry
}

// ToolsFor is the surface one seat's turns run against.
//
// The seat's own registry when this node has claimed it, and the shared one
// otherwise. Never nil: a seat with no per-role children still has the
// builtins, and returning nil here would fail the turn rather than run it with
// the tools it does have.
func (c *Company) ToolsFor(handle string) *tools.Registry {
	c.seatMu.RLock()
	defer c.seatMu.RUnlock()
	if reg, ok := c.seatTools[handle]; ok {
		return reg
	}
	return c.Tools
}

// setSeatTools installs a claimed seat's own surface.
func (c *Company) setSeatTools(handle string, reg *tools.Registry) {
	c.seatMu.Lock()
	defer c.seatMu.Unlock()
	if c.seatTools == nil {
		c.seatTools = make(map[string]*tools.Registry)
	}
	c.seatTools[handle] = reg
}

// dropSeatTools forgets a released seat's surface.
func (c *Company) dropSeatTools(handle string) {
	c.seatMu.Lock()
	defer c.seatMu.Unlock()
	delete(c.seatTools, handle)
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
	if err := c.Validate(); err != nil {
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
		switch {
		case a.Handle < b.Handle:
			return -1
		case a.Handle > b.Handle:
			return 1
		default:
			return 0
		}
	})
	return out
}

// RunnerFor builds the phase runner for one seat.
//
// Per turn, not cached. A runner holds the turn's task and its conversation
// history, both of which are per-turn facts; caching one per seat would carry
// the previous turn's ask into the next one.
func (c *Company) RunnerFor(handle string, in RunnerInput) (*runner.Runner, error) {
	role := c.Org.AgentSeatByHandle(handle)
	if role == nil {
		return nil, fmt.Errorf("engine: %q is not an agent seat in this company", handle)
	}
	te := c.Config.TurnEngine
	del := te.Delegation
	return runner.New(runner.Config{
		Seat:     prompts.Seat{Org: c.Org, Role: role},
		Registry: c.ToolsFor(handle),
		Models:   c.Models,
		Caps: runner.Caps{
			ExecutorRounds:  te.MaxToolRounds,
			ExecutorCeiling: te.ExecuteMaxToolRoundsCeiling,
			ExtensionStep:   te.ExtensionRoundStep,
			ExtensionOn:     te.ExtensionEnabled.Or(true),
		},
		Budget: in.Budget,
		Judge:  in.Judge,
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
		Onboarding: runner.Onboarding{
			Markers: in.Markers, Latch: in.Latch,
			Rounds:  te.OnboardingMaxToolRounds,
			Ceiling: te.OnboardingMaxToolRoundsCeiling,
		},
		Resume: in.Resume,
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
	// thin-trigger case — a pointer the turn-start search could not use,
	// re-searched between Plan and Execute on the plan summary — and with
	// one loop there is nothing between the phases to hang it on. The
	// executor asks instead, with search_knowledge, over the same seam.
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
}

// TurnSettings is the loop's pinned configuration for this epoch.
func (c *Company) TurnSettings() turn.Settings {
	te := c.Config.TurnEngine
	return turn.Settings{
		MaxIterations:        te.MaxIterations,
		DelegationDepthLimit: te.DelegationDepthLimit,
		SkipNames:            MetaToolNames(),
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
