package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/knowledge"
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
// published, never mutated, so each new one gets a new
// registry — and a node that equipped only its first epoch would serve a
// company whose agents lost every builtin at the first config change, with
// nothing failing.
//
// # Two halves, because only one of them touches the node
//
// [Engine.equipEpoch] writes into the epoch it is handed and nowhere else: the
// embedding backend, the builtins, the credential pools' ledger. Everything it
// builds is unreachable until that epoch is installed, so a revision refused
// after it leaves nothing behind. [Engine.startSharedServers] is the other
// half: the shared MCP children are this NODE's processes, and reconciling
// them to a revision stops the ones it dropped. An apply therefore runs the
// first half while the revision can still be refused and the second only once
// nothing can refuse it (see [Engine.Apply]).

// equip puts this node's tools into an epoch: [Engine.equipEpoch], then the
// shared MCP servers filed into the same registry.
//
// BOOT'S COMPOSITION, and only boot's. Boot installs the epoch straight after
// this with no step between them that can fail, and a boot that fails later
// tears the children down with everything else it started, so at boot there
// is no refusal for a started child to outlive. An apply has refusals after
// its build, and runs the two halves apart.
func (e *Engine) equip(ctx context.Context, c *Company) error {
	if err := e.equipEpoch(c); err != nil {
		return err
	}
	e.startSharedServers(ctx, c)
	return nil
}

// equipEpoch registers the node-backed builtins and the embedding backend into
// an epoch, and touches nothing outside it.
//
// A failure here fails the APPLY. The alternative — log it and serve the epoch
// anyway — publishes a company whose agents cannot look up a colleague or
// recall their own work, which looks from the outside like a model that has
// stopped trying rather than a node that is missing half its tool surface.
func (e *Engine) equipEpoch(c *Company) error {
	if c == nil {
		return fmt.Errorf("engine: cannot equip a nil epoch")
	}
	// THE EMBEDDING BACKEND FIRST, onto the epoch being equipped rather than
	// beside it: the memory tools below write and recall with it, so they
	// embed with THIS revision's provider, and storing the epoch is what
	// publishes it — a reader that takes it from the epoch it pinned (the
	// embedding duty's tick, a search, a turn's prefetch) never meets a
	// revision an apply refused. Built at the apply, which is also what
	// makes a width change fail where somebody is watching rather than
	// weeks later at the first recall. A company with none configured gets
	// nil, and every consumer treats that as "no similarity search" rather
	// than a fault.
	vectors, err := e.buildEmbedder(c)
	if err != nil {
		return err
	}
	c.vectors = vectors
	// THE COMPANY'S OWN NUMBERS, not the builtins' defaults: each is a
	// validated, documented setting, and a builtin left on its own default
	// would make setting one a revision that changes nothing an operator
	// can observe.
	refinement := c.Config.Learning.SkillRefinement
	deps := builtin.Deps{
		A2A:               e.a2aFor(c),
		Sandbox:           e.sandboxLauncher(),
		Knowledge:         KnowledgeSearch(e, c),
		Events:            e.telemetry(),
		Recall:            e.prefetcher(c),
		EpisodeLimit:      c.Config.Learning.Episodic.RetrievalLimit,
		RefreshesPerTurn:  c.Config.Learning.PersonalMemory.MaxRefreshesPerTurn,
		SkillBodyMax:      refinement.MaxBodyBytes,
		SkillVersionsKept: refinement.MaxVersionsKept,
	}
	if db := e.backends.Store; db != nil {
		skills := learning.NewSkills(db)
		deps.Skills = skills
		if refinement.Refines() {
			// learning.skill_refinement.enabled gates BOTH halves — the
			// post-turn refiner and this tool — because they write the
			// same rows through the same version archive. A company that
			// turned refinement off and still had the tool would have
			// skills changing under it with the knob that says they
			// cannot set to false.
			deps.Refinable = skills
		}
		deps.Episodes = learning.NewEpisodes(db)
		deps.Diary = e.diary(db, c)
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
	// THE NATIVE TOOLS FOLLOW THE BACKENDS THIS EPOCH NAMES, not the ones
	// this node holds. Both native backends start with the node and keep
	// running through a revision that moves the company off them, so a
	// registry equipped from what the node holds would go on offering
	// write_page to a company whose knowledge base is now Confluence — pages
	// written where no search of this company reads ([Engine.Knowledge]
	// answers by the same field), which is the second home the one-backend
	// rule exists to prevent. The tracker's tools follow its backend for the
	// same reason.
	if c.Config.TrackerBackendFor() == config.TrackerNative {
		deps.Work = e.workDeps(c)
	}
	// THE PROJECT-LEAD SEAM, read PER CALL against the epoch current when
	// the tool runs rather than against this one.
	deps.LeadsProject = LeadsProjectOf(e)
	if c.Config.KnowledgeBackendFor() == config.KnowledgeNative {
		deps.Pages = e.pageDeps(c)
	}
	if _, err = builtin.Register(c.Tools, deps); err != nil {
		return err
	}
	// THE SHARED MCP SERVERS ARE NOT STARTED HERE, although they file into
	// this same registry: they are the node's processes rather than the
	// epoch's values, so [Engine.equip] and [Engine.Apply] start them — the
	// apply only once nothing can refuse the revision.
	//
	// The tool skills' ${var} map is NOT refreshed here, and their trigger
	// audit does not run here: the map is written into the node's skill
	// registry, which every seat reads, and the audit reads the epoch that
	// is current — so both wait for this one to be installed
	// ([Engine.installEpoch]).

	// THE FLEET'S CREDENTIAL LEDGER, onto the pools this epoch just built.
	// Local and infallible — it stores a handle on each pool — which is why
	// it can sit after the steps here that can fail: an epoch that is
	// refused never reaches this line, and one that is not must never be
	// published with pools that publish nothing. See cooldowns.go.
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
// see. Kept in one node's own database, a cross-node ask would wake its target
// and then drop the reply as "no such channel".
func (e *Engine) a2aFor(c *Company) builtin.Asker {
	svc := e.a2aService(c)
	if svc == nil {
		// A typed nil in an interface is not nil, and the tool checks
		// its Asker for nil to decide whether to register at all.
		return nil
	}
	return svc
}

// a2aService is the concrete service, for the two callers that need
// different halves of it: the ask tool takes it as a [builtin.Asker], and the
// answer leg needs Reply and Close, which that interface does not carry.
//
// Built per call rather than held on the epoch: it is a struct over the
// coordination store and the queue with no state of its own, so two of them
// are the same service, and holding one would only add a field that has to be
// rebuilt on every apply for no reason.
func (e *Engine) a2aService(c *Company) *a2a.Service {
	if c == nil || e.backends == nil || e.backends.Fleet == nil || e.backends.Queue == nil {
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

// tuneBatching writes the company's inbox coalescing knobs into the value
// every seat attachment on this node already holds.
//
// IN PLACE rather than rebuilt, because that is what [queue.BatchOptions] is
// for: it guards its own fields precisely so an apply lands on the next batch
// with no re-subscription. A fresh value per attach would leave every seat
// claimed before the apply reading the old window for as long as it held its
// seat.
//
// Without it every seat attachment takes queue.DefaultBatchOptions, and setting
// notification_coalesce_window_seconds or notification_coalesce_max_batch
// would be a revision that changes nothing an operator can observe.
//
// CALLED WHEN THE EPOCH IS INSTALLED ([Engine.installEpoch]), never while one
// is being equipped: the value is the NODE's, shared by every seat it holds,
// so written for a revision an apply then refused it would go on coalescing
// every inbox by that revision's numbers while the node serves the one before.
func (e *Engine) tuneBatching(c *Company) {
	if e.batch == nil || c == nil || c.Config == nil {
		return
	}
	e.batch.Set(c.Config.NotificationCoalesceWindowSeconds,
		c.Config.NotificationCoalesceMaxBatch)
}

// KnowledgeSearch wires search_knowledge for a surface serving revision c, or
// nil where the company has no knowledge base at all.
//
// ONE RULE FOR EVERY SURFACE: a seat's registry is equipped with it on every
// apply, and the operator's own assistant is served it per call, so the two
// cannot disagree about whether a company can be searched.
//
// The GATE reads the company's config; the SEARCHER is resolved per call.
// Those cannot be the same read: this runs before an apply reconciles the
// knowledge base (see [Engine.reconcileConfluence]), so a searcher captured
// here is the PREVIOUS epoch's — the backend the company may have left, the
// credentials resolved before a rotation, the skills space before a move.
// Capturing it would give a seat a tool that searches the company it used to
// be.
func KnowledgeSearch(e *Engine, c *Company) builtin.KnowledgeSearcher {
	if c.Config.KnowledgeBackendFor() == config.KnowledgeNone {
		// A NIL INTERFACE, not a live adapter over a nil searcher: the
		// tool is omitted rather than registered-and-empty, so a seat is
		// never offered a search its company cannot serve.
		//
		// THE BACKEND, not the presence of an `integrations.confluence`
		// block: a company on the native knowledge base configures no
		// vendor at all, and gating on the vendor block would leave every
		// native company's seats without search_knowledge while the pages
		// they were meant to find sat in the index.
		return nil
	}
	return LiveKnowledge(e)
}

// LiveKnowledge is the node's knowledge search as the tool layer takes it,
// resolved against [Engine.KnowledgeServed] on every call.
//
// ONE ADAPTER FOR EVERY SURFACE: a seat's registry and the operator's own
// assistant both search through it. Per call, because an apply REPLACES the
// searcher — a new credential, a new lead map, another backend — and a value
// captured when a surface was assembled searches as the company it used to
// be. Its methods are the whole of [builtin.KnowledgeSearcher], which requires
// each of them, so an adapter cannot answer a search and drop the fact that
// this node's index is still building.
func LiveKnowledge(e *Engine) builtin.KnowledgeSearcher { return liveKnowledge{engine: e} }

// liveKnowledge resolves the node's current knowledge searcher per call.
type liveKnowledge struct{ engine *Engine }

// CanSearch reports which of the three states a search that cannot run is in,
// so the tool names the one that is true: no knowledge base, one this node is
// not serving, or one with nothing this caller may read.
//
// A SERVED BACKEND REFUSES ON ONE CONDITION ONLY — the scope rule of
// [knowledge.Permitted] — which is why its own false is reported as
// [knowledge.NoScope]: the native backend never answers false, and Confluence
// answers false exactly when that rule does.
func (k liveKnowledge) CanSearch(seat *org.Role, o *org.Organization) knowledge.Refusal {
	s, refused := k.engine.KnowledgeServed()
	if s == nil {
		return refused
	}
	if !s.CanSearch(seat, o) {
		return knowledge.Refusal{State: knowledge.NoScope,
			Detail: "no read scope (`knowledge.scope`) is declared, and this " +
				"search has no " + s.Backend() + " credential of its own to " +
				"search unscoped with"}
	}
	return knowledge.Refusal{}
}

// Building forwards the current searcher's own answer, and is false with none
// wired: there is no index to wait for.
func (k liveKnowledge) Building(ctx context.Context) bool {
	s := k.engine.Knowledge()
	return s != nil && s.Building(ctx)
}

// Search runs the current searcher's search. With none wired it is an answer
// marked failed rather than an empty one: search_knowledge asks
// [liveKnowledge.CanSearch] first, so a searcher gone by now was taken by an
// apply between the two reads, and the search did not run — which "nothing
// matched" would misstate.
func (k liveKnowledge) Search(ctx context.Context, q knowledge.Query) knowledge.Answer {
	s := k.engine.Knowledge()
	if s == nil {
		return knowledge.Answer{Failed: true}
	}
	return s.Search(ctx, q)
}
