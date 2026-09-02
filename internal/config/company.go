package config

import (
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Company is Tier B — founder-owned, live-editable, versioned in the store.
//
// Everything that defines what the company IS and how its agents behave:
// identity, providers, integrations, tool servers, seats, units, learning
// and turn-engine settings, budgets. One revision is active at a time and is
// delivered to a running engine; nothing here needs a restart.
//
// Secrets in it are POINTERS. A ${VAR} survives parsing, storage and export
// untouched and is resolved only where a provider or transport is built —
// see [Resolver].
type Company struct {
	// Name is the company's identity. It is also half of every agent
	// seat's derived id (uuid5 over org name and handle), so renaming a
	// company orphans every seat's diary, onboarding markers and
	// counterparty profiles.
	Name string `yaml:"name" json:"name" js:"required" desc:"Company name. Half of every seat's derived id — effectively permanent."`

	Mission  string   `yaml:"mission,omitempty" json:"mission,omitempty" desc:"One line on what the company is for; reaches every seat's prompt."`
	Vision   string   `yaml:"vision,omitempty" json:"vision,omitempty" desc:"Longer statement of where the company is going."`
	Policies []string `yaml:"policies,omitempty" json:"policies,omitempty" desc:"Company-wide rules injected into every seat's prompt."`

	// Integrations is how external events REACH agents and how
	// notifications are delivered — the inbound side. Agent TOOLS are
	// configured separately under MCPServers; the two are deliberately
	// different blocks because they are different directions.
	Integrations Integrations `yaml:"integrations,omitempty" json:"integrations"`

	// Knowledge is the org-wide read scope for the shared-knowledge
	// search.
	Knowledge Knowledge `yaml:"knowledge,omitempty" json:"knowledge"`

	// SkillVariables is an operator-defined name -> value map substituted
	// into tool-skill text wherever a skill writes ${name}.
	//
	// It lets a static, knowledge-base-authored skill reference a per-org
	// fact its body cannot know — the tenant's wiki base URL, say, so
	// agents share human-clickable links rather than the gateway URLs the
	// tools return. The engine carries no integration-specific knowledge:
	// operators name the variables, skills reference them.
	//
	// Values render into LLM prompts (and the event store, and the
	// dashboard, like any other prompt content) — treat them like a policy
	// string and do not point them at secrets.
	SkillVariables map[string]string `yaml:"skill_variables,omitempty" json:"skill_variables,omitempty" desc:"name -> value facts substituted into tool-skill text. Keys must be identifiers."`

	Providers  Providers  `yaml:"providers,omitempty" json:"providers"`
	TurnEngine TurnEngine `yaml:"turn_engine,omitempty" json:"turn_engine"`
	Learning   Learning   `yaml:"learning,omitempty" json:"learning"`
	Scheduling Scheduling `yaml:"scheduling,omitempty" json:"scheduling"`

	// MCPServers are the tool servers agents call through. ALL tool
	// servers are declared here, including the ones an integration block
	// also mentions: integrations carry non-tool config (admin
	// credentials, webhook secrets) and the engine never derives an MCP
	// server from one.
	MCPServers []MCPServer `yaml:"mcp_servers,omitempty" json:"mcp_servers,omitempty" desc:"Tool servers, stdio or http. Per-agent credentials live in role.mcp_env."`

	// TokenBudget is the org-wide LLM token ceiling across every seat;
	// 0 is unlimited. Charged before the per-seat budget, so the first
	// completion that would cross it stops that turn.
	TokenBudget int `yaml:"token_budget,omitempty" json:"token_budget,omitempty" js:"min=0" desc:"Org-wide token ceiling across all seats; 0 = unlimited."`

	// NotificationRateLimit caps outbound notifications; 0 is unlimited.
	NotificationRateLimit int `yaml:"notification_rate_limit,omitempty" json:"notification_rate_limit,omitempty" js:"min=0" desc:"Outbound notification cap; 0 = unlimited."`

	// NotificationCoalesceWindowSeconds is how long after the first
	// pending event an idle seat's inbox waits to absorb a burst before
	// dispatching. 0 adds no latency — a backlog that piled up while the
	// seat was busy still coalesces, because the drain collects everything
	// already queued.
	//
	// Capped at 60s: the window counts against the broker's ack-timeout
	// budget, which has to fit collection plus one whole turn, so an
	// unbounded linger reintroduces mid-drain redelivery.
	NotificationCoalesceWindowSeconds float64 `yaml:"notification_coalesce_window_seconds,omitempty" json:"notification_coalesce_window_seconds,omitempty" js:"min=0;max=60" desc:"Inbox linger before dispatch, 0..60s. 0 adds no latency."`

	// NotificationCoalesceMaxBatch caps the events merged into one
	// coalesced digest, so a larger backlog arrives as successive capped
	// batches rather than one unbounded megaprompt.
	NotificationCoalesceMaxBatch int `yaml:"notification_coalesce_max_batch,omitempty" json:"notification_coalesce_max_batch,omitempty" js:"min=1" desc:"Events merged into one digest trigger."`

	// Workers are the reusable delegate templates a seat's executor hands
	// work to, keyed by the name it types into a `delegate` call. See
	// workers.go for why a template exists at all and why naming a tool in
	// one grants nothing.
	Workers map[string]Worker `yaml:"workers,omitempty" json:"workers,omitempty" desc:"Reusable delegate templates, keyed by the name an executor calls them by."`

	// Roles are the seats that belong to no unit — a CEO, a cross-cutting
	// advisor, the founder's own human seat.
	Roles []Role `yaml:"roles,omitempty" json:"roles,omitempty" desc:"Seats belonging to no unit."`

	// Units are the hierarchy, nesting to any depth.
	Units []Unit `yaml:"units,omitempty" json:"units,omitempty" desc:"The org hierarchy; units nest to any depth."`
}

// coalesceWindowMax bounds the inbox linger. See the field's own comment:
// the window is spent out of the broker's ack-timeout budget.
const coalesceWindowMax = 60.0

// DefaultCompany is a Tier B config with every default applied. The loader
// decodes into it, so an absent key keeps its default and a valueless key
// (`learning:`) reads as unset rather than as a wiped block.
func DefaultCompany() Company {
	return Company{
		Providers:                    DefaultProviders(),
		TurnEngine:                   DefaultTurnEngine(),
		Learning:                     DefaultLearning(),
		Scheduling:                   DefaultScheduling(),
		NotificationCoalesceMaxBatch: 20,
	}
}

// skillVariableKey is the substitution-identifier rule.
//
// The operator key space has to be provably identical to the ${name} render
// grammar the skill layer uses, or a key like base-url would be accepted
// here and then never substituted anywhere — a silent no-op at render time
// rather than an error at load.
var skillVariableKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate reports every Tier B rule this config breaks, joined.
//
// It validates the ORG as well: building the hierarchy is where duplicate
// handles, unrunnable schedules and human-seat rule violations surface, and
// a config that parses into a company nobody can run has not been validated.
func (c *Company) Validate() error {
	var p problems

	// An UNRESOLVED REDACTION MASK, first, because it is the one fault here
	// that comes from this process rather than from the document's author.
	// A credential still holding the marker means a config read was edited
	// and sent back, and the mask could not be matched to what it hid: a
	// member that is NEW or RENAMED has no prior value of its own, and a
	// list of bare credentials that changed length no longer says by
	// position which one is which. Storing it silently would hand a
	// provider the literal "__redacted__" as an API key, and the failure
	// would surface hours later as an authentication error naming nothing
	// about where it came from.
	for _, path := range c.UnresolvedMasks() {
		p.add(path, ErrUnknownValue,
			"still holds the redaction marker %q — a masked credential could "+
				"not be matched to the value it hid. Either this member is new "+
				"or was renamed, so there is no prior value to restore, or a "+
				"list of bare credentials changed length. Write the real value "+
				"here",
			Redacted)
	}

	if strings.TrimSpace(c.Name) == "" {
		p.add("name", ErrMissing, "the company needs a name — it is half of every seat's derived id")
	}

	for key := range c.SkillVariables {
		if !skillVariableKey.MatchString(key) {
			p.add(at("skill_variables", key), ErrUnknownValue,
				"keys must be substitution identifiers matching "+
					"[A-Za-z_][A-Za-z0-9_]* — a key like %q would never be "+
					"substituted into a skill's ${name} reference", key)
		}
	}

	if c.TokenBudget < 0 {
		p.add("token_budget", ErrOutOfRange, "must not be negative, got %d", c.TokenBudget)
	}
	if c.NotificationRateLimit < 0 {
		p.add("notification_rate_limit", ErrOutOfRange,
			"must not be negative, got %d", c.NotificationRateLimit)
	}
	if w := c.NotificationCoalesceWindowSeconds; w < 0 || w > coalesceWindowMax {
		p.add("notification_coalesce_window_seconds", ErrOutOfRange,
			"must be 0..%v seconds, got %v — the window is spent out of the "+
				"broker's ack-timeout budget, which also has to fit a whole turn",
			coalesceWindowMax, w)
	}
	if c.NotificationCoalesceMaxBatch < 1 {
		p.add("notification_coalesce_max_batch", ErrOutOfRange,
			"must be at least 1, got %d", c.NotificationCoalesceMaxBatch)
	}

	p.wrap(c.Providers.validate("providers"))
	p.wrap(c.TurnEngine.validate("turn_engine"))
	p.wrap(c.Learning.validate("learning"))
	p.wrap(c.Scheduling.validate("scheduling"))
	p.wrap(c.Integrations.validate("integrations"))
	p.wrap(c.validateKnowledgeBackend())
	p.wrap(c.validateProviderKeys())
	p.wrap(c.validateWorkers())
	p.wrap(c.validateSandboxPlacement())

	seen := make(map[string]struct{}, len(c.MCPServers))
	for i := range c.MCPServers {
		path := idx("mcp_servers", i)
		p.wrap(c.MCPServers[i].validate(path))
		name := c.MCPServers[i].Name
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			// Two servers under one name is not a merge — the second
			// wins wherever the engine indexes by name, so the first's
			// tools silently vanish from every prompt.
			p.add(at(path, "name"), ErrConflict, "duplicate MCP server name %q", name)
		}
		if name == BridgeServerName {
			// The same collision one layer out: the engine writes the
			// seat's tool bridge into every agent-mode box under this
			// name, and a company server called the same is overwritten
			// there — its tools gone from the run with nothing saying
			// why. See [BridgeServerName].
			p.add(at(path, "name"), ErrConflict,
				"%q is reserved for the seat's own tool bridge, which every "+
					"agent-mode box sees under that name; a server called the "+
					"same would be replaced by it in the box. Rename the server",
				name)
		}
		seen[name] = struct{}{}
	}

	for i := range c.Roles {
		p.wrap(c.Roles[i].validate(idx("roles", i)))
	}
	for i := range c.Units {
		p.wrap(c.Units[i].validate(idx("units", i)))
	}

	// The hierarchy's own rules — duplicate handles, human seats carrying
	// runtime fields, schedules with no runner — are the org model's, and
	// they only exist once the tree is built.
	p.wrap(c.organization().Validate())
	return p.err()
}

// validateKnowledgeBackend holds the rule that a read scope needs the
// backend it narrows.
//
// The knowledge base is single-homed, and since Confluence is the only
// backend that is now structural rather than enforced — there is no pair of
// searchers left to disagree about what the company already knows. What is
// still worth refusing is a scope for a backend that is not configured: it
// reads as a working narrowing and narrows nothing, which is the silence
// this rule exists to end, one level down from the block itself.
//
// Selection keys on integration-block PRESENCE, because the scope list
// defaults to empty (which means unscoped) and so cannot be the signal.
func (c *Company) validateKnowledgeBackend() error {
	var p problems
	if len(c.Knowledge.ConfluenceSpaces) > 0 && c.Integrations.Confluence == nil {
		p.add("knowledge.confluence_spaces", ErrConflict,
			"a Confluence read scope needs integrations.confluence")
	}
	return p.err()
}

// Knowledge is the org-wide read scope for the shared-knowledge search.
//
// It is the ONLY thing that narrows the query-time search, and it is
// role-independent on purpose: a unit's own space is integration IDENTITY
// (where its webhooks route, where it files work) and letting an
// identity double as a read scope is how an agent ends up unable to read
// the page it was told to follow.
//
// Empty is unscoped, and unscoped is bounded by the backend's own ACLs —
// an agent searching with its own credentials sees every space its account
// can read. Set this only to NARROW to a curated floor.
type Knowledge struct {
	ConfluenceSpaces []string `yaml:"confluence_spaces,omitempty" json:"confluence_spaces,omitempty" desc:"Org-wide Confluence read scope. Empty = unscoped, bounded by each seat's own ACLs. Requires integrations.confluence."`
}

// validateProviderKeys holds the rule that a seat may only name a model the
// company actually has.
//
// Without it a typo is invisible and permanent. `llm_review: claude-sonet`
// resolves through the runtime's fallback — per-phase, then the role's default
// chain, then the "default" key, then the first provider configured — so the
// seat boots, thinks, and bills, on a model the operator never chose. Nothing
// downstream can catch it: the fallback exists precisely so a role that names
// NOTHING still runs, and from inside resolution a name that misses and a name
// that was never written are the same absence.
//
// Skipped entirely when providers.llm is empty. A company with no models is a
// documented authoring state — an org chart written before the credentials
// exist — and it fails at the first turn, where the failure is actionable.
// Rejecting every role's key against an empty map would turn that supported
// flow into a wall of errors about models the author has not added yet.
func (c *Company) validateProviderKeys() error {
	var p problems
	if len(c.Providers.LLM) == 0 {
		return nil
	}
	known := make([]string, 0, len(c.Providers.LLM))
	for key := range c.Providers.LLM {
		known = append(known, key)
	}
	slices.Sort(known)

	c.EachRole(func(path string, role *Role) {
		// Both written surfaces are checked, and each is reported at the
		// path the operator typed. Validating the RESOLVED chain instead
		// would hide half of them: the flat field wins over the mapping,
		// so a typo inside `llm.review` under a role that also sets
		// `llm_review` never appears in the resolved value at all — and it
		// is still a typo, still in the file, and still what the operator
		// will edit next.
		for _, field := range []struct {
			path string
			keys ProviderKeys
		}{
			{at(path, "llm"), role.LLM.Default},
			{at(at(path, "llm"), "review"), role.LLM.Review},
			{at(at(path, "llm"), "subagent"), role.LLM.Subagent},
			{at(at(path, "llm"), "auxiliary"), role.LLM.Auxiliary},
			{at(at(path, "llm"), "judge"), role.LLM.Judge},
			{at(at(path, "llm"), "sandbox"), role.LLM.Sandbox},
			{at(path, "llm_review"), role.LLMReview},
			{at(path, "llm_subagent"), role.LLMSubagent},
			{at(path, "llm_auxiliary"), role.LLMAuxiliary},
			{at(path, "llm_judge"), role.LLMJudge},
			{at(path, "llm_sandbox"), role.LLMSandbox},
		} {
			for _, key := range field.keys {
				if _, ok := c.Providers.LLM[key]; !ok {
					p.add(field.path, ErrUnknownValue,
						"%q is not a configured provider — providers.llm has %s. "+
							"A key that misses is not an error at run time: the seat "+
							"falls back to another model and bills against it, so this "+
							"is the only place the typo can be seen",
						key, strings.Join(known, ", "))
				}
			}
		}
	})
	return p.err()
}

// EachRole visits EVERY seat in the company, at the path an operator typed:
// the top-level `roles:` and every seat inside `units:`, to any depth.
//
// It exists because the walk was written inline once and covered only the
// top-level list, so a cross-field rule silently exempted every seat that
// belonged to a unit — which, in a company with an org chart, is most of them.
// A rule that holds for a seat in `roles:` and not for the identical seat one
// level down is not a rule, and the seats it missed are exactly the ones whose
// mistakes have no run-time symptom to find them by.
//
// EXPORTED because the ENGINE needs the same walk: a seat's sandbox block is
// looked up by name at launch, and a lookup that stopped at the top level
// answered nil for every seat in a unit — so run_sandbox refused each of them
// with "this seat's sandbox is not enabled" on a seat whose block said
// otherwise. One walker, so the two can never disagree about which seats exist.
func (c *Company) EachRole(visit func(path string, role *Role)) {
	for i := range c.Roles {
		visit(idx("roles", i), &c.Roles[i])
	}
	var walk func(path string, units []Unit)
	walk = func(path string, units []Unit) {
		for i := range units {
			unit := &units[i]
			here := idx(path, i)
			for j := range unit.Roles {
				visit(idx(at(here, "roles"), j), &unit.Roles[j])
			}
			walk(at(here, "children"), unit.Children)
		}
	}
	walk("units", c.Units)
}

// SandboxPlacements is every cell a seat could ACTUALLY land in, each mapped
// to the path of the first thing that named it.
//
// ONE COMPUTATION, TWO CALLERS, and they have to agree or the company is
// refused for a rule the engine then breaks anyway. Validation requires a
// container image only where a container is reached; the engine builds a
// backend only for what is reached. Built eagerly from the catalogue instead,
// the engine constructed a container backend for a company whose seats all run
// direct — and failed the apply demanding an image the validator had just
// refused as a field nothing would read.
//
// The DEFAULT IS ALWAYS REACHED when the catalogue is enabled: it is where a
// seat that names nothing goes, including a seat added later by a founder
// editing Tier B live, so a company must be able to run it before one exists.
func (c *Company) SandboxPlacements() map[Placement]string {
	reached := make(map[Placement]string)
	catalogue := c.Providers.Sandbox
	if !catalogue.Enabled() {
		return reached
	}
	if run := catalogue.RunIn(); run != "" {
		where := "providers.sandbox.default_run_in"
		if catalogue.DefaultRunIn == "" {
			// Resolved from the catalogue's own shape rather than
			// written, so the message points at the block, not at a
			// field the operator will not find.
			where = "providers.sandbox"
		}
		reached[run] = where
	}
	c.EachRole(func(path string, role *Role) {
		gate := role.Sandbox
		if gate == nil || !gate.Enabled || gate.RunIn == "" || !gate.RunIn.NeedsBackend() {
			return
		}
		if _, seen := reached[gate.RunIn]; !seen {
			reached[gate.RunIn] = at(at(path, "sandbox"), "run_in")
		}
	})
	// AN AGENT-MODE EXECUTOR IS A RUN TOO, placed by its entry's own
	// cli.run_in rather than by the seat's gate — and for as long as this
	// walk stopped at the gates, that cell was never built: an entry
	// naming `e2b` in a company whose seats all ran direct validated
	// cleanly and failed at the seat's first turn, every turn, with the
	// manager's "no backend for e2b".
	for _, key := range c.agentModeExecutorKeys() {
		run := c.Providers.LLM[key].CLI.RunIn
		if run == "" || !run.NeedsBackend() {
			continue
		}
		if _, seen := reached[run]; !seen {
			reached[run] = at(agentModeEntryPath(key), "run_in")
		}
	}
	return reached
}

// agentModeExecutorKeys are the providers.llm entries some seat's EXECUTOR
// resolves to that run in agent mode, in key order.
//
// ONLY ENTRIES A SEAT REACHES, resolved as the phase registry resolves them
// (see [Company.ExecutorProvider]): an agent-mode entry nobody's executor
// runs on launches nothing, so the cell it names is not reached and the
// backend behind it is not built — the same rule every gate here follows. It
// is validated the day a seat points at it, like a seat added later.
//
// Sorted, so a message naming "the first seat" or a backend built in this
// order is the same on every run of the same config.
func (c *Company) agentModeExecutorKeys() []string {
	seen := map[string]struct{}{}
	c.EachRole(func(_ string, role *Role) {
		key, entry, resolved := c.ExecutorProvider(role)
		if !resolved || !entry.CLI.AgentMode() {
			return
		}
		seen[key] = struct{}{}
	})
	return slices.Sorted(maps.Keys(seen))
}

// agentModeEntryPath is where an agent-mode entry's placement is written, for
// the messages that point at it.
func agentModeEntryPath(key string) string {
	return at(at(at("providers", "llm"), key), "cli")
}

// ExecutorProvider is the providers.llm entry a seat's EXECUTOR actually runs
// on, resolved exactly as the phase registry resolves it: the seat's own `llm`
// keys, then the entry called "default", then the first provider in
// declaration order.
//
// A SECOND RESOLUTION OF THE SAME QUESTION, and normally that is the mistake
// this codebase spends most of its rules on — but the registry's needs a
// BUILT provider per key, which exists only once a company is constructed, and
// this has to answer during validation, where the answer's whole value is
// catching the mistake before anything is built. The two are held together by
// a test that walks the same fallbacks. What must never happen is this one
// growing a rule the registry does not have.
//
// The second return is false when the company configures no provider at all,
// which the registry refuses at construction and this must not crash on.
func (c *Company) ExecutorProvider(role *Role) (string, LLMProvider, bool) {
	order := c.Providers.ProviderOrder()
	if len(order) == 0 {
		return "", LLMProvider{}, false
	}
	for _, key := range role.LLM.Default {
		if entry, ok := c.Providers.LLM[key]; ok {
			return key, entry, true
		}
	}
	if entry, ok := c.Providers.LLM["default"]; ok {
		return "default", entry, true
	}
	return order[0], c.Providers.LLM[order[0]], true
}

// validateSandboxPlacement holds the rules that need BOTH the catalogue and
// the seats: which cells a company actually reaches, and whether the blocks
// configuring them are there.
//
// Neither half can hold it alone. providers.sandbox is validated without the
// roles, so it cannot know that `run_in: container` is reached and an image is
// needed; a role is validated without the providers, so it cannot know that
// the cell it names is not configured. Both failures are silent at run time —
// the seat is offered the tool and the first coding run dies inside a turn.
func (c *Company) validateSandboxPlacement() error {
	var p problems
	catalogue := c.Providers.Sandbox

	c.EachRole(func(path string, role *Role) {
		gate := role.Sandbox
		if gate == nil || !gate.Enabled {
			return
		}
		where := at(at(path, "sandbox"), "run_in")
		if !catalogue.Enabled() {
			p.add(at(at(path, "sandbox"), "enabled"), ErrMissing,
				"this seat runs code, but the company configures no sandbox. "+
					"Add providers.sandbox, or turn the seat's gate off — an "+
					"enabled gate with no catalogue offers the seat nothing and "+
					"says so nowhere")
			return
		}
		run := gate.RunIn
		if run == PlacementSelf {
			// CODE WORK INSIDE THE EXECUTOR'S OWN RUN, which only exists
			// when that executor is a coding CLI in agent mode. On any
			// other runtime the seat has no shell of its own, so `self`
			// would read as a working choice and turn code work off — a
			// seat that plans around a sandbox it is never offered, with
			// nothing anywhere saying why.
			key, entry, resolved := c.ExecutorProvider(role)
			if resolved && !entry.CLI.AgentMode() {
				p.add(where, ErrConflict,
					"`self` means code work rides this seat's own executor run, "+
						"which only a coding CLI in agent mode has. This seat's "+
						"executor is providers.llm.%s. Set `mode: agent` on a "+
						"cli-agent entry and point the seat at it, or name a "+
						"cell the catalogue configures (%s)",
					key, names(catalogue.available()))
			}
			return
		}
		if run == "" {
			run = catalogue.RunIn()
		}
		switch {
		case run == "":
			p.add(where, ErrMissing,
				"this seat runs code and the catalogue offers more than one "+
					"place to do it (%s), so it has to name one — or "+
					"providers.sandbox.default_run_in has to",
				names(catalogue.available()))
		case !oneOf(run, Placements):
			// Spelling is the role's own rule; it already reported.
		case !catalogue.Configured(run):
			p.add(where, ErrConflict,
				"%q needs the %s backend configured under providers.sandbox "+
					"(this catalogue has %s)",
				run, run.backend(), names(catalogue.available()))
		}
	})

	// THE SAME THREE QUESTIONS FOR AN AGENT-MODE EXECUTOR, whose run is
	// placed by its entry rather than by a gate: is there a catalogue at
	// all, is the cell it names configured, and does a silent entry have a
	// default to fall to. Each was unasked, and each failed at the seat's
	// first turn — every turn — with a launch error naming a backend no
	// field in the file appeared to be missing.
	for _, key := range c.agentModeExecutorKeys() {
		entry := c.Providers.LLM[key]
		where := at(agentModeEntryPath(key), "run_in")
		if !catalogue.Enabled() {
			p.add(at(agentModeEntryPath(key), "mode"), ErrMissing,
				"agent mode runs the executor in a box, and this company "+
					"configures no sandbox. Add providers.sandbox, or set "+
					"`mode: text` to keep the CLI a subprocess of the engine")
			continue
		}
		run := entry.CLI.RunIn
		if run == "" {
			run = catalogue.RunIn()
		}
		switch {
		case run == "":
			p.add(where, ErrMissing,
				"agent mode runs the executor in a box and the catalogue "+
					"offers more than one place to do it (%s), so this entry "+
					"has to name one — or providers.sandbox.default_run_in has to",
				names(catalogue.available()))
		case !oneOf(run, BackendPlacements()):
			// Spelling is the entry's own rule; it already reported.
		case !catalogue.Configured(run):
			p.add(where, ErrConflict,
				"%q needs the %s backend configured under providers.sandbox "+
					"(this catalogue has %s)",
				run, run.backend(), names(catalogue.available()))
		}
	}

	reached := c.SandboxPlacements()
	if local := catalogue.localBlock(); local != nil {
		// THE IMAGE IS ONLY NEEDED WHERE A CONTAINER IS ACTUALLY RUN, and
		// only the walk above knows that. Required unconditionally it would
		// refuse a perfectly good direct-only company; unchecked, a seat's
		// first coding run fails at container create, minutes into a turn.
		if named, wanted := reached[PlacementContainer]; wanted && strings.TrimSpace(local.Image) == "" {
			p.add("providers.sandbox.local.image", ErrMissing,
				"%s runs in a container, so the local backend needs an image "+
					"with the coding-agent CLI installed", named)
		}
		if _, wanted := reached[PlacementContainer]; !wanted {
			for _, unread := range local.containerOnly() {
				if strings.TrimSpace(unread.value) == "" {
					continue
				}
				p.add(at("providers.sandbox.local", unread.field), ErrConflict,
					"only a container box reads this, and no seat runs in one. "+
						"Give a seat `run_in: container`, or remove the field")
			}
		}
	}
	return p.err()
}
