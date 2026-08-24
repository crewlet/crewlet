package config

import (
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
	// and sent back, and the mask could not be matched to what it hid — the
	// caller reshaped a list of keys, so position no longer says which
	// credential is which. Storing it silently would hand a provider the
	// literal "__redacted__" as an API key, and the failure would surface
	// hours later as an authentication error naming nothing about where it
	// came from.
	for _, path := range c.UnresolvedMasks() {
		p.add(path, ErrUnknownValue,
			"still holds the redaction marker %q — a masked credential could "+
				"not be matched to the value it hid, which happens when a list "+
				"of credentials changes length between the read and the write. "+
				"Write the real value here, or leave the list's shape alone",
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

// validateKnowledgeBackend holds the single-homed rule: the knowledge
// backend is Confluence XOR Plane.
//
// Backend selection keys on integration-block PRESENCE, because the scope
// lists default to empty (which means unscoped) and so cannot be the
// signal. Enabling both is refused — a Confluence-to-Plane migration is a
// cut-over, not an overlap — and a scope list for the disabled backend is
// refused too, because it reads as a working scope and narrows nothing.
func (c *Company) validateKnowledgeBackend() error {
	var p problems
	planeActive := c.Integrations.Plane != nil && c.Integrations.Plane.Enabled

	// THE SINGLE-HOMING RULE HAS ONE BACKEND LEFT TO ENFORCE IT AGAINST.
	//
	// Confluence is refused outright now (see [unservedIntegrations]), so
	// the branches that compared it against Plane could only ever fire
	// alongside that refusal — a second error saying the same thing about
	// a config that was already rejected. They are gone rather than left
	// as guards that cannot fire, which read as invariants stronger than
	// they are.
	if len(c.Knowledge.ConfluenceSpaces) > 0 {
		p.add("knowledge.confluence_spaces", ErrUnimplemented,
			"this is a read scope for a Confluence backend this build does "+
				"not have a searcher for, so it would narrow nothing — the "+
				"knowledge base this build serves is Plane, scoped with "+
				"knowledge.plane_projects")
	}
	if len(c.Knowledge.PlaneProjects) > 0 && !planeActive {
		p.add("knowledge.plane_projects", ErrConflict,
			"a Plane read scope needs integrations.plane enabled")
	}
	return p.err()
}

// Knowledge is the org-wide read scope for the shared-knowledge search.
//
// It is the ONLY thing that narrows the query-time search, and it is
// role-independent on purpose: a unit's own space or project is integration
// IDENTITY (where its webhooks route, where it files work) and letting an
// identity double as a read scope is how an agent ends up unable to read
// the page it was told to follow.
//
// Empty is unscoped, and unscoped is bounded by the backend's own ACLs —
// an agent searching with its own credentials sees every space its account
// can read. Set this only to NARROW to a curated floor.
type Knowledge struct {
	ConfluenceSpaces []string `yaml:"confluence_spaces,omitempty" json:"confluence_spaces,omitempty" js:"unimplemented" desc:"NOT IMPLEMENTED in this build: no searcher reads a Confluence space, so this would narrow nothing. Scope the knowledge base this build serves with knowledge.plane_projects."`
	PlaneProjects    []string `yaml:"plane_projects,omitempty" json:"plane_projects,omitempty" desc:"Org-wide Plane read scope. Empty = unscoped. Requires integrations.plane."`
}

// validateProviderKeys holds the rule that a seat may only name a model the
// company actually has.
//
// Without it a typo is invisible and permanent. `llm_plan: claude-sonet`
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

	for i := range c.Roles {
		role := &c.Roles[i]
		path := idx("roles", i)
		// Both written surfaces are checked, and each is reported at the
		// path the operator typed. Validating the RESOLVED chain instead
		// would hide half of them: the flat field wins over the mapping,
		// so a typo inside `llm.plan` under a role that also sets
		// `llm_plan` never appears in the resolved value at all — and it
		// is still a typo, still in the file, and still what the operator
		// will edit next.
		for _, field := range []struct {
			path string
			keys ProviderKeys
		}{
			{at(path, "llm"), role.LLM.Default},
			{at(at(path, "llm"), "plan"), role.LLM.Plan},
			{at(at(path, "llm"), "execute"), role.LLM.Execute},
			{at(at(path, "llm"), "review"), role.LLM.Review},
			{at(at(path, "llm"), "subagent"), role.LLM.Subagent},
			{at(at(path, "llm"), "auxiliary"), role.LLM.Auxiliary},
			{at(at(path, "llm"), "judge"), role.LLM.Judge},
			{at(at(path, "llm"), "sandbox"), role.LLM.Sandbox},
			{at(path, "llm_plan"), role.LLMPlan},
			{at(path, "llm_execute"), role.LLMExecute},
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
	}
	return p.err()
}
