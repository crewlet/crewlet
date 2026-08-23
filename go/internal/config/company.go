package config

import (
	"regexp"
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
	confluenceActive := c.Integrations.Confluence != nil

	if planeActive && confluenceActive {
		p.add("integrations", ErrConflict,
			"integrations.confluence and an enabled integrations.plane are "+
				"mutually exclusive — the knowledge backend is single-homed; "+
				"a Confluence-to-Plane migration is a cut-over (disable one)")
	}
	if planeActive && len(c.Knowledge.ConfluenceSpaces) > 0 {
		p.add("knowledge.confluence_spaces", ErrConflict,
			"this is a scope list for the disabled Confluence backend — "+
				"remove it when integrations.plane is enabled, and use "+
				"knowledge.plane_projects")
	}
	if confluenceActive && len(c.Knowledge.PlaneProjects) > 0 {
		p.add("knowledge.plane_projects", ErrConflict,
			"this is a scope list for the disabled Plane backend — remove it "+
				"when integrations.confluence is configured, and use "+
				"knowledge.confluence_spaces")
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
	ConfluenceSpaces []string `yaml:"confluence_spaces,omitempty" json:"confluence_spaces,omitempty" desc:"Org-wide Confluence read scope. Empty = unscoped."`
	PlaneProjects    []string `yaml:"plane_projects,omitempty" json:"plane_projects,omitempty" desc:"Org-wide Plane read scope. Empty = unscoped. Requires integrations.plane."`
}
