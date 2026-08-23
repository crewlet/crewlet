package engine

import (
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
// published, never mutated (rewrite/decisions/404), so each new one gets a new
// registry — and a node that equipped only its first epoch would serve a
// company whose agents lost every builtin at the first config change, with
// nothing failing.

// equip registers the node-backed builtins into an epoch.
//
// A failure here fails the APPLY. The alternative — log it and serve the epoch
// anyway — publishes a company whose agents cannot look up a colleague or
// recall their own work, which looks from the outside like a model that has
// stopped trying rather than a node that is missing half its tool surface.
func (e *Engine) equip(c *Company) error {
	if c == nil {
		return fmt.Errorf("engine: cannot equip a nil epoch")
	}
	deps := builtin.Deps{A2A: e.a2aFor(c), Sandbox: e.sandboxLauncher()}
	if db := e.backends.Store; db != nil {
		skills := learning.NewSkills(db)
		deps.Skills = skills
		deps.Refinable = skills
		deps.Episodes = learning.NewEpisodes(db)
		deps.Diary = learning.NewDiary(db)
		deps.Onboarding = learning.NewOnboarding(db)
	}
	if _, err := builtin.Register(c.Tools, deps); err != nil {
		return err
	}
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
// Nil when the node has no store: the channel table is what makes an ask
// durable, and an in-memory one would open channels that vanish on restart
// while telling the asking agent its question was delivered.
func (e *Engine) a2aFor(c *Company) builtin.Asker {
	if e.backends == nil || e.backends.Store == nil || e.backends.Queue == nil {
		return nil
	}
	svc, err := a2a.New(a2a.NewSQLStore(e.backends.Store), e.backends.Queue,
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
