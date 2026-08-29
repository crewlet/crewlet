package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
)

// Equipping an epoch: putting this NODE's tools into it.
//
// NewCompany deliberately builds an epoch without them, because building one
// must be something `crewlet validate` can do on a laptop — a constructor that
// needed a database would make config validation depend on having one. So the
// epoch arrives with an empty registry and the engine fills it, here, with the
// tools its own backends can actually serve.
//
// Which is also why this runs on EVERY apply and not once at boot: an epoch is
// published, never mutated (decisions/404), so each new one gets a new
// registry — and a node that equipped only its first epoch would serve a
// company whose agents lost every builtin at the first config change, with
// nothing failing.

// equip registers the node-backed builtins into an epoch.
//
// A failure here fails the APPLY. The alternative — log it and serve the epoch
// anyway — publishes a company whose agents cannot look up a colleague or
// recall their own work, which looks from the outside like a model that has
// stopped trying rather than a node that is missing half its tool surface.
func (e *Engine) equip(ctx context.Context, c *Company) error {
	if c == nil {
		return fmt.Errorf("engine: cannot equip a nil epoch")
	}
	// THE COMPANY'S OWN NUMBERS, not the builtins' defaults. Each of these
	// was validated, schema'd and documented and read by nobody, so setting
	// one produced a revision and changed nothing an operator could observe.
	refinement := c.Config.Learning.SkillRefinement
	deps := builtin.Deps{
		A2A:               e.a2aFor(c),
		Sandbox:           e.sandboxLauncher(),
		Events:            e.telemetry(),
		EpisodeLimit:      c.Config.Learning.Episodic.RetrievalLimit,
		SkillBodyMax:      refinement.MaxBodyChars,
		SkillVersionsKept: refinement.MaxVersionsKept,
	}
	if db := e.backends.Store; db != nil {
		skills := learning.NewSkills(db)
		deps.Skills = skills
		deps.Refinable = skills
		deps.Episodes = learning.NewEpisodes(db)
		deps.Diary = learning.NewDiary(db)
		deps.Onboarding = learning.NewOnboarding(db)
	}
	// THE REGISTRY, NOT ITS CONTENT. load_tool_skill is registered
	// whenever a node HAS a registry, empty or not — because whether a
	// company has published skills is a fact about the knowledge base
	// that changes without an apply, and a tool that appeared and
	// disappeared with the sync would make the required-skill guard
	// unarmable exactly when the first skill lands.
	if e.skills != nil {
		deps.ToolSkills = e.skills
	}
	if _, err := builtin.Register(c.Tools, deps); err != nil {
		return err
	}
	// THE SHARED MCP SERVERS, into the same registry and straight after
	// the builtins, because this is the surface every seat's is cloned
	// from. Per-role children are NOT here: they belong to a seat's lease
	// rather than to the epoch, and this node holds only some of the
	// company's seats — see mcp.go.
	//
	// A server that will not start does not fail the apply. It costs that
	// server's tools; refusing the epoch over it would take a working
	// company down because one vendor's binary was missing.
	e.startSharedServers(ctx, c)
	// The operator's ${var} map is CONFIG, so it is refreshed per epoch —
	// unlike the skills themselves, which come from the knowledge base and
	// outlive one. A variable a revision removed then surfaces here rather
	// than on that skill's next edit, which might be never.
	e.refreshSkillVariables(c)
	e.auditSkills(c)

	// THE EMBEDDER IS BUILT AT THE APPLY, which is what makes a width
	// change fail where somebody is watching rather than weeks later at
	// the first recall. A company with none configured gets nil, and every
	// consumer treats that as "no similarity search" rather than a fault.
	embedder, err := e.buildEmbedder(c)
	if err != nil {
		return err
	}
	e.embeddings.Store(&embedder)
	// THE FLEET'S CREDENTIAL LEDGER, onto the pools this epoch just built.
	// Local and infallible — it stores a handle — which is why it can sit
	// after the one step here that can fail: an epoch that is refused never
	// reaches this line, and one that is not must never be published with
	// pools that publish nothing. See cooldowns.go.
	e.shareCooldowns(c)
	return nil
}

// sandboxLauncher is the run_sandbox tool's seam, or nil where no seat can run
// code.
//
// Nil OMITS the tool rather than registering a broken one: a model shown a
// tool that always fails learns to distrust the whole catalogue and burns a
// round finding out each time — and a seat that planned around a box it will
// never get delivers nothing while looking like it tried.
func (e *Engine) sandboxLauncher() builtin.SandboxLauncher {
	if e.sandboxCoordinator == nil || e.sandboxPending == nil {
		return nil
	}
	return &launcher{engine: e}
}

// a2aFor builds the agent-to-agent service for one epoch, or nil.
//
// Per EPOCH, because its directory answers "is this handle an agent seat" out
// of the org — and a service holding the previous epoch's org would refuse an
// ask to a seat the current revision added, or accept one to a seat it removed.
//
// Nil when the node has no FLEET store: a channel is the authorization record
// the ANSWERING seat's node reads, so it has to be somewhere both nodes can
// see. It used to be the node's own database, which is why a cross-node ask
// woke its target and then dropped the reply as "no such channel".
func (e *Engine) a2aFor(c *Company) builtin.Asker {
	if e.backends == nil || e.backends.Fleet == nil || e.backends.Queue == nil {
		return nil
	}
	svc, err := a2a.New(a2a.NewCoordStore(e.backends.Fleet), e.backends.Queue,
		a2a.Options{Directory: agentSeats{org: c.Org}})
	if err != nil {
		// Logged rather than returned: a company without agent-to-agent
		// messaging is a real deployment, and refusing to boot over an
		// optional surface would take the whole node down for it.
		log.Warn("a2a_unavailable", "error", err,
			"hint", "agents on this node cannot ask each other questions")
		return nil
	}
	return svc
}

// agentSeats answers the A2A directory out of one epoch's org.
//
// AGENT seats only, which is the whole question it exists to answer: a human
// seat is addressable and never spawned, so a channel opened to one is a
// channel no turn will ever answer.
type agentSeats struct{ org *org.Organization }

func (d agentSeats) IsAgentSeat(handle string) bool {
	return d.org != nil && d.org.AgentSeatByHandle(handle) != nil
}

// markers is the onboarding marker store, or nil on a node with none.
func (e *Engine) markers() runner.Markers {
	if e.backends == nil || e.backends.Store == nil {
		return nil
	}
	return learning.NewOnboarding(e.backends.Store)
}

// telemetry is where a builtin's own lifecycle events go, or nil.
//
// Nil on a node with no queue — `crewlet validate` builds a registry to check
// a config and publishes nothing — which the builtins read as "do not publish"
// rather than as a reason to fail a tool call.
func (e *Engine) telemetry() builtin.Telemetry {
	if e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	return e.backends.Queue
}
