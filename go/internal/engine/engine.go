package engine

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/tools"
)

// Company is one immutable configuration epoch.
//
// EVERYTHING A TURN READS COMES FROM ONE OF THESE, taken by value at the top
// of the turn. Python read each setting from a live cell on every access, so a
// hot reload landing mid-turn could change the round cap between Plan and
// Execute — and needed a context-local "pin" to paper over it. An epoch that
// is replaced rather than mutated makes that unrepresentable: an in-flight turn
// holds the one it started under until it ends.
type Company struct {
	Config *config.Company
	Org    *org.Organization
	Models *phase.Registry

	// Tools is the catalogue every seat's surfaces are cut from. One
	// registry per epoch rather than per seat: MCP instances are per-role
	// and register under distinct names, and a per-seat registry would
	// duplicate every builtin once per seat for nothing.
	Tools *tools.Registry
}

// NewCompany builds an epoch from a validated config.
//
// It does NOT reach the network. Building an epoch must be something a
// `validate` command can do, and a constructor that dialled a provider would
// make config validation depend on the vendor being up.
func NewCompany(c *config.Company) (*Company, error) {
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
	models, err := buildProviders(c)
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
	return runner.New(runner.Config{
		Seat:     prompts.Seat{Org: c.Org, Role: role},
		Registry: c.Tools,
		Models:   c.Models,
		Caps: runner.Caps{
			PlanRounds:     te.PlanMaxToolRounds,
			ExecuteRounds:  te.MaxToolRounds,
			ReviewRounds:   te.MaxToolRounds,
			PlanCeiling:    te.PlanMaxToolRoundsCeiling,
			ExecuteCeiling: te.ExecuteMaxToolRoundsCeiling,
			ExtensionStep:  te.ExtensionRoundStep,
			ExtensionOn:    te.ExtensionEnabled.Or(true),
		},
		Budget:       in.Budget,
		Judge:        in.Judge,
		Task:         in.Task,
		Conversation: in.Conversation,
		AlwaysOn:     te.ExecutorAlwaysOnTools,
		SkipNames:    MetaToolNames(),
	})
}

// RunnerInput is the per-turn half of a runner's configuration.
type RunnerInput struct {
	Task         string
	Conversation string

	// Budget is the shared token counter this turn charges. Nil is the
	// embedded single-node case, where no counter is shared with anyone.
	Budget toolloop.BudgetMeter

	// Judge decides round-cap extensions. Nil sends every exhaustion
	// straight to the rescue path.
	Judge extension.Judge
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
