package config

import (
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// Role is the AUTHORED shape of one seat — a `roles:` entry.
//
// It is the wire format a founder writes, not the runtime model. [Role.Seat]
// transforms it into an [org.Role]: the per-agent chat identities are
// materialised out of the integrations block, the `llm` mapping form is
// collapsed into per-phase chains, and the integration identities become
// the flat project/space fields the runtime reads.
//
// The two shapes are separate on purpose. The authored one carries what is
// convenient to WRITE (a scalar llm, an integrations block grouping four
// surfaces); the runtime one carries what is convenient to READ, and every
// consumer of a seat gets exactly one shape to reason about.
type Role struct {
	// Name is the seat's unique identity, and the source of the derived
	// handle.
	Name string `yaml:"name" json:"name" js:"required" desc:"Unique seat name; also the source of the auto-derived handle."`

	// Kind is agent (the default, a spawned runtime seat) or human (an
	// addressable-only seat that is never spawned and needs at least one
	// contact identity).
	Kind org.RoleKind `yaml:"kind,omitempty" json:"kind,omitempty" js:"enum=agent|human" desc:"agent (default) or human."`

	// Contact is a HUMAN seat's external identities — how agents mention
	// and reach the person, and how their inbound activity is attributed.
	Contact *org.HumanContact `yaml:"contact,omitempty" json:"contact,omitempty" desc:"Human seats: external account ids per surface."`

	// Availability is free text rendered into rosters, so a report knows
	// what to expect before it waits: "CET business hours; replies within
	// ~4h".
	Availability string `yaml:"availability,omitempty" json:"availability,omitempty" desc:"Human seats: free-text availability shown in rosters."`

	// Handle is the canonical identity slug, derived from Name when empty.
	//
	// EFFECTIVELY PERMANENT: the seat's durable id is derived from the
	// company name and this handle, so changing either orphans that seat's
	// diary, onboarding markers and counterparty profiles.
	Handle string `yaml:"handle,omitempty" json:"handle,omitempty" js:"pattern=^[a-z0-9][a-z0-9-]*$" desc:"Canonical slug. Effectively permanent — it derives the seat's durable id."`

	Email string `yaml:"email,omitempty" json:"email,omitempty" desc:"Seat email; plus-addressing derives from the handle."`

	// Unit is a home-unit reference for a ROOT-level seat, resolved into
	// that unit's members before anything else runs. It is what
	// PUT /config/roles/{handle} writes; a hand-authored config normally nests the
	// seat under its unit directly.
	Unit string `yaml:"unit,omitempty" json:"unit,omitempty" desc:"Home unit for a root-level seat; the seat is moved into it."`

	Goal             string   `yaml:"goal,omitempty" json:"goal,omitempty" desc:"What this seat is for; reaches its prompt."`
	Backstory        string   `yaml:"backstory,omitempty" json:"backstory,omitempty" desc:"Who this seat is; reaches its prompt."`
	Responsibilities []string `yaml:"responsibilities,omitempty" json:"responsibilities,omitempty" desc:"What this seat owns."`

	// Manages names seats OR units this seat manages; a unit name expands
	// to every seat in it and its descendants.
	Manages []string `yaml:"manages,omitempty" json:"manages,omitempty" desc:"Seats or units this seat manages; a unit expands to its seats."`

	BehavioralGuidelines []string `yaml:"behavioral_guidelines,omitempty" json:"behavioral_guidelines,omitempty" desc:"How this seat should work; reaches its prompt."`

	// TokenBudget is this seat's own ceiling; 0 is unlimited.
	TokenBudget int `yaml:"token_budget,omitempty" json:"token_budget,omitempty" js:"min=0" desc:"Per-seat token ceiling; 0 = unlimited."`

	// LLM points the seat at providers.llm — one key, a fallback chain, or
	// a per-phase mapping.
	LLM PhaseLLM `yaml:"llm,omitempty" json:"llm,omitzero" desc:"Provider key, chain, or per-phase mapping."`

	// The flat per-phase fields. Each WINS over the same phase inside the
	// mapping form, so a seat can take a shared mapping and override one
	// phase without restating the block.
	LLMPlan      ProviderKeys `yaml:"llm_plan,omitempty" json:"llm_plan,omitzero" desc:"Provider chain for Plan; wins over llm.plan."`
	LLMExecute   ProviderKeys `yaml:"llm_execute,omitempty" json:"llm_execute,omitzero" desc:"Provider chain for Execute."`
	LLMReview    ProviderKeys `yaml:"llm_review,omitempty" json:"llm_review,omitzero" desc:"Provider chain for Review."`
	LLMSubagent  ProviderKeys `yaml:"llm_subagent,omitempty" json:"llm_subagent,omitzero" desc:"Provider chain for spawned sub-agents."`
	LLMAuxiliary ProviderKeys `yaml:"llm_auxiliary,omitempty" json:"llm_auxiliary,omitzero" desc:"Cheap model for reflection and summaries."`
	LLMJudge     ProviderKeys `yaml:"llm_judge,omitempty" json:"llm_judge,omitzero" desc:"Cheap model for the round-cap extension judge."`
	LLMSandbox   ProviderKeys `yaml:"llm_sandbox,omitempty" json:"llm_sandbox,omitzero" desc:"Model the sandboxed coding agent runs on."`

	// LearningEnabled overrides the system-wide learning setting for this
	// seat alone. Unset inherits it; false skips reflection for this
	// seat's turns while still writing episodes, which are cheap and
	// useful to a person regardless.
	LearningEnabled Toggle `yaml:"learning_enabled,omitempty" json:"learning_enabled,omitzero" desc:"Override company learning for this seat."`

	// MCPEnv is this seat's TOOL credentials, keyed by server name then by
	// variable: environment variables for a stdio server, HTTP headers for
	// an http one. This is what makes each seat authenticate as a distinct
	// identity, and the whole reason a shared: false server is launched
	// once per seat.
	MCPEnv org.MCPEnv `secret:"true" yaml:"mcp_env,omitempty" json:"mcp_env,omitempty" desc:"Per-seat MCP credentials: env vars (stdio) or headers (http)."`

	// Sandbox is the per-seat code-runtime gate. Absent means the seat is
	// never offered the sandbox tool at all.
	Sandbox *RoleSandbox `yaml:"sandbox,omitempty" json:"sandbox,omitempty" desc:"Per-seat code sandbox gate. Absent = never offered."`

	// Placement constrains which nodes may run this seat; absent is
	// anywhere, which is what a single-process deployment always means.
	Placement *RolePlacement `yaml:"placement,omitempty" json:"placement,omitempty" desc:"Which nodes may run this seat. Absent = any node that runs seats."`

	// Integrations is this seat's non-tool identity on external surfaces.
	Integrations RoleIntegrations `yaml:"integrations,omitempty" json:"integrations,omitzero"`

	// Schedules are this seat's own recurring work.
	Schedules []org.Schedule `yaml:"schedules,omitempty" json:"schedules,omitempty" desc:"Recurring work fired into this seat's inbox."`
}

// RolePlacement is the authored form of a seat's placement constraint. It
// maps onto [placement.SeatPlacement], which is where the matching and the
// capacity arithmetic live.
type RolePlacement struct {
	// Node pins to one node id, matched exactly. A seat pinned to a single
	// node is unserved whenever that node is down — deliberate, and
	// reported rather than quietly widened.
	Node string `yaml:"node,omitempty" json:"node,omitempty" desc:"Exact node id to pin this seat to."`

	// Labels are pairs a node must carry, ALL of them, matched exactly.
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty" desc:"Node labels that must all match."`
}

// Seat is the placement constraint in the form the seat host consumes.
func (p *RolePlacement) Seat() placement.SeatPlacement {
	if p == nil {
		return placement.SeatPlacement{}
	}
	labels := make(map[string]string, len(p.Labels))
	for k, v := range p.Labels {
		labels[k] = v
	}
	if len(labels) == 0 {
		labels = nil
	}
	return placement.SeatPlacement{Node: p.Node, Labels: labels}
}

// RoleSandbox is the authored per-seat code-runtime gate.
type RoleSandbox struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled" desc:"Offer this seat the sandbox tool."`

	// CodingAgent overrides the provider-wide default for this seat only.
	// Empty inherits it, resolved at LAUNCH rather than at config time, so
	// a provider swap reaches seats that never named one.
	CodingAgent CodingAgent `yaml:"coding_agent,omitempty" json:"coding_agent,omitempty" js:"enum=claude-code|opencode" desc:"Override the provider default for this seat."`

	// PauseTTLSeconds bounds how long this seat's blocked sandbox stays
	// paused. UNSET inherits the provider default; an explicit 0 means
	// never pause — tear the box down as soon as the run blocks, and
	// re-seed from the pushed branch when the answer arrives.
	//
	// A POINTER, because those are three states and a float64 has only
	// two: an unset field's zero value would read as "never pause", so
	// every seat that said nothing would silently lose its checkout the
	// moment a coding agent asked a question.
	//
	// A NEGATIVE VALUE ALSO MEANS INHERIT, and is accepted rather than
	// refused: -1 is how the field's earlier form spelled it, and a
	// company document that says so is asking for exactly what leaving it
	// out now asks for. Refusing it would fail a config over a spelling.
	// It is never "no expiry" — an unbounded pause is the leak this knob
	// exists to prevent.
	PauseTTLSeconds *float64 `yaml:"pause_ttl_seconds,omitempty" json:"pause_ttl_seconds,omitempty" desc:"Paused-box TTL. Unset (or negative) = provider default; 0 = never pause."`

	// MCP scopes which of the seat's MCP servers the coding agent gets.
	//
	// Scoping is at the SERVER level only, and that is a platform fact
	// rather than a simplification: the coding CLIs offer no per-tool
	// allowlist that could be enforced uniformly, so each gets the servers
	// it is given.
	MCP RoleSandboxMCP `yaml:"mcp,omitempty" json:"mcp,omitzero"`

	// Env is injected into this seat's sandbox run. External service
	// tokens are DECLARED here — the engine names no tool-specific
	// variable of its own, and only LLM credentials derive automatically.
	Env map[string]string `secret:"true" yaml:"env,omitempty" json:"env,omitempty" desc:"Env for this seat's sandbox run; declare external tokens here."`

	// Setup is this seat's own provisioning, applied after the
	// engine-wide steps.
	Setup []SandboxSetupStep `yaml:"setup,omitempty" json:"setup,omitempty" desc:"Per-seat provisioning steps, applied after the engine-wide ones."`
}

// RoleSandboxMCP is the server-level scope for a seat's coding agent.
type RoleSandboxMCP struct {
	Servers []string `yaml:"servers,omitempty" json:"servers,omitempty" desc:"MCP server names the coding agent may use."`
}

// IsZero lets an unset scope drop out of a round trip.
func (m RoleSandboxMCP) IsZero() bool { return len(m.Servers) == 0 }

func (s *RoleSandbox) validate(path string) error {
	var p problems
	if s.CodingAgent != "" && !oneOf(s.CodingAgent, CodingAgents) {
		p.add(at(path, "coding_agent"), ErrUnknownValue, "%q (want %s)",
			s.CodingAgent, names(CodingAgents))
	}
	if !s.Enabled {
		// A gate that is off with provisioning under it is an operator who
		// configured a sandbox and will watch nothing happen.
		if len(s.Setup) > 0 || len(s.Env) > 0 || len(s.MCP.Servers) > 0 {
			p.add(at(path, "enabled"), ErrConflict,
				"this seat's sandbox is configured but not enabled, so none of "+
					"it is read. Set enabled: true, or remove the block")
		}
	}
	for i := range s.Setup {
		p.wrap(s.Setup[i].validate(idx(at(path, "setup"), i)))
	}
	return p.err()
}

// RoleIntegrations is a seat's non-tool identity on external surfaces: the
// chat apps it speaks as, and the tracker project and wiki space it owns.
//
// TOOL credentials are not here — they live in mcp_env and go straight to
// the server that consumes them. Nothing in this block scopes knowledge
// reads either; read scope is the org-wide knowledge block only.
type RoleIntegrations struct {
	Slack      *RoleSlack      `yaml:"slack,omitempty" json:"slack,omitempty" desc:"This seat's own Slack app: bot token and signing secret."`
	Mattermost *RoleMattermost `yaml:"mattermost,omitempty" json:"mattermost,omitempty" desc:"This seat's Mattermost bot: one token covers everything."`
	Jira       *ProjectRef     `yaml:"jira,omitempty" json:"jira,omitempty" desc:"The Jira project this seat owns."`
	Confluence *SpaceRef       `yaml:"confluence,omitempty" json:"confluence,omitempty" desc:"The Confluence space this seat owns."`
	Atlassian  *RoleAtlassian  `yaml:"atlassian,omitempty" json:"atlassian,omitempty" desc:"Which Atlassian products this seat holds a licence for. Read by crewlet atlassian provision."`
}

// IsZero lets an unset block drop out of a round trip.
func (r RoleIntegrations) IsZero() bool {
	return r.Slack == nil && r.Mattermost == nil && r.Jira == nil &&
		r.Confluence == nil && r.Atlassian == nil
}

// AtlassianProduct is one Atlassian app a seat can hold a licence for.
type AtlassianProduct string

const (
	// AtlassianJira is Jira, whichever flavours the site runs.
	AtlassianJira AtlassianProduct = "jira"
	// AtlassianConfluence is Confluence.
	AtlassianConfluence AtlassianProduct = "confluence"
)

// AtlassianProducts is the closed set, in the order a report reads them.
var AtlassianProducts = []AtlassianProduct{AtlassianJira, AtlassianConfluence}

// RoleAtlassian narrows which Atlassian products a seat is provisioned into.
//
// # It exists because a product licence is BILLABLE
//
// A service account with a Confluence licence and a Jira one costs twice what
// a documentation agent needs, and the free service-account allowance an
// organization gets without Atlassian Guard is small. So the choice is
// exposed rather than collapsed into "every product the company configures":
// `products: [confluence]` is how a writer agent consumes one licence and
// holds no credential that can move a sprint.
//
// # The pointer carries three states and all three are real
//
// Absent means every product the company configures — the sensible default
// for a seat that works across the org. An explicit empty list means NONE,
// which is how a seat that lives entirely in chat is kept out of a
// provisioning run without deleting its mcp_env. A list means exactly those.
// A plain slice could not tell the first from the second.
type RoleAtlassian struct {
	Products []AtlassianProduct `yaml:"products" json:"products" js:"enum=jira|confluence" desc:"Products this seat is licensed for. Absent block = all configured; empty list = none."`
}

// validate checks a seat's product list.
func (a *RoleAtlassian) validate(path string) error {
	var p problems
	seen := make(map[AtlassianProduct]bool, len(a.Products))
	for i, product := range a.Products {
		switch {
		case !oneOf(product, AtlassianProducts):
			p.add(idx(at(path, "products"), i), ErrUnknownValue, "%q (want %s)",
				product, names(AtlassianProducts))
		case seen[product]:
			// Harmless to the run, and a signal that the author meant to
			// write the other product — which is a licence they did not
			// get, on a seat that looks configured for it.
			p.add(idx(at(path, "products"), i), ErrConflict,
				"%q is named twice", product)
		default:
			seen[product] = true
		}
	}
	return p.err()
}

// ProjectRef is the tracker project a seat or unit owns. Integration
// identity — where activity with no better recipient routes, and where work
// is filed. Not a credential, and not a read scope.
type ProjectRef struct {
	Project string `yaml:"project,omitempty" json:"project,omitempty" desc:"Project key or identifier."`
}

// SpaceRef is the wiki space a seat or unit owns, on the same terms as
// [ProjectRef].
type SpaceRef struct {
	Space string `yaml:"space,omitempty" json:"space,omitempty" desc:"Space key."`
}

// RoleSlack is a seat's own Slack app.
//
// One app per agent is what makes an agent's messages come from that agent
// rather than from a shared company bot. These credentials drive the
// TRANSPORT only; the Slack tool server is a separate MCP entry whose token
// is named again under mcp_env — two consumers, one secret, named twice on
// purpose so an operator can split them.
type RoleSlack struct {
	BotToken      string `secret:"true" yaml:"bot_token,omitempty" json:"bot_token,omitempty" desc:"Bot token for outbound Web API calls."`
	SigningSecret string `secret:"true" yaml:"signing_secret,omitempty" json:"signing_secret,omitempty" desc:"Verifies inbound webhooks for this seat's app."`
	Channel       string `yaml:"channel,omitempty" json:"channel,omitempty" desc:"Default channel id for this seat."`
}

// validate checks a seat's Slack app.
//
// THE TWO CREDENTIALS ARE NOT INTERCHANGEABLE and each half fails silently
// on its own. Without a bot token the seat receives messages it can never
// answer; without a signing secret its route answers 503 to every delivery
// while the app's own settings page reports a healthy request URL. So
// declaring the block at all means declaring both.
func (s *RoleSlack) validate(path string) error {
	var p problems
	if strings.TrimSpace(s.BotToken) == "" {
		p.add(at(path, "bot_token"), ErrMissing,
			"required — without it this seat receives messages it cannot "+
				"answer. `crewlet slack provision` mints one into the ${VAR} "+
				"this field points at")
	}
	if strings.TrimSpace(s.SigningSecret) == "" {
		p.add(at(path, "signing_secret"), ErrMissing,
			"required — this seat's /webhooks/slack/<handle> route has nothing "+
				"to verify a delivery with otherwise and answers 503 to every "+
				"one, while the app's own settings page reports a healthy "+
				"request URL")
	}
	return p.err()
}

// RoleMattermost is a seat's Mattermost bot.
//
// Unlike Slack a seat needs exactly ONE credential: the bot's personal
// access token authenticates the inbound websocket, the outbound REST calls
// and the MCP tool server alike. There is no signing secret because there
// is no inbound webhook to verify.
type RoleMattermost struct {
	// BotToken is the bot account's personal access token. A whole-value
	// ${VAR} is what makes the seat provisionable; a literal marks a
	// manually managed bot, which the provisioner reports and leaves alone.
	BotToken string `secret:"true" yaml:"bot_token,omitempty" json:"bot_token,omitempty" desc:"Bot personal access token; one credential covers everything."`

	// Username defaults to the seat handle with the provisioning prefix
	// applied. Set it only when the account already exists under another
	// name.
	Username string `yaml:"username,omitempty" json:"username,omitempty" js:"pattern=^[a-z0-9][a-z0-9._-]*$" desc:"Bot username; defaults to the seat handle."`

	Channel string `yaml:"channel,omitempty" json:"channel,omitempty" desc:"Default channel name for this seat."`
}

func (m *RoleMattermost) validate(path string) error {
	if m.Username == "" || envref.Has(m.Username) {
		// A reference resolves later; rejecting the unresolved form would
		// forbid configuring the username from the environment.
		return nil
	}
	if !mattermostUsername.MatchString(m.Username) {
		return fault(at(path, "username"), ErrUnknownValue,
			"%q — Mattermost usernames are lowercase and contain only letters, "+
				"digits, '.', '-' and '_', starting with a letter or digit", m.Username)
	}
	return nil
}

func (r *Role) validate(path string) error {
	var p problems
	if strings.TrimSpace(r.Name) == "" {
		p.add(at(path, "name"), ErrMissing, "every seat needs a name")
	}
	if s := r.Integrations.Slack; s != nil {
		p.wrap(s.validate(at(path, "integrations.slack")))
	}
	if r.Integrations.Mattermost != nil {
		p.wrap(r.Integrations.Mattermost.validate(at(path, "integrations.mattermost")))
	}
	if a := r.Integrations.Atlassian; a != nil {
		p.wrap(a.validate(at(path, "integrations.atlassian")))
	}
	if r.Sandbox != nil {
		p.wrap(r.Sandbox.validate(at(path, "sandbox")))
	}
	// The seat's own rules — kind, handle shape, human-versus-agent
	// fields, schedule shape — belong to the org model and are checked
	// there, on the transformed seat, so there is one definition of what a
	// seat may be rather than two that drift.
	return p.err()
}

// Seat transforms the authored seat into the runtime one.
//
// A phase's chain is resolved here, once, in the documented order: the flat
// llm_<phase> field wins, then the same phase inside the mapping form, then
// nothing (which the runtime reads as "fall back to the default chain").
// Doing it at the boundary is what keeps every downstream reader from
// having to know the mapping form existed.
func (r *Role) Seat() *org.Role {
	pick := func(flat, mapped ProviderKeys) ProviderKeys {
		if len(flat) > 0 {
			return flat
		}
		return mapped
	}
	seat := &org.Role{
		Name:                 r.Name,
		Kind:                 r.Kind,
		Contact:              r.Contact,
		Availability:         r.Availability,
		DeclaredHandle:       r.Handle,
		Email:                r.Email,
		UnitRef:              r.Unit,
		Goal:                 r.Goal,
		Backstory:            r.Backstory,
		Responsibilities:     append([]string(nil), r.Responsibilities...),
		Manages:              append([]string(nil), r.Manages...),
		BehavioralGuidelines: append([]string(nil), r.BehavioralGuidelines...),
		TokenBudget:          r.TokenBudget,
		LLM:                  r.LLM.Default,
		LLMPlan:              pick(r.LLMPlan, r.LLM.Plan),
		LLMExecute:           pick(r.LLMExecute, r.LLM.Execute),
		LLMReview:            pick(r.LLMReview, r.LLM.Review),
		LLMSubagent:          pick(r.LLMSubagent, r.LLM.Subagent),
		LLMAuxiliary:         pick(r.LLMAuxiliary, r.LLM.Auxiliary),
		LLMJudge:             pick(r.LLMJudge, r.LLM.Judge),
		LLMSandbox:           pick(r.LLMSandbox, r.LLM.Sandbox),
		LearningEnabled:      r.LearningEnabled,
		MCPEnv:               r.MCPEnv.Clone(),
		Placement:            r.Placement.Seat(),
		Schedules:            append([]org.Schedule(nil), r.Schedules...),
	}

	if s := r.Integrations.Slack; s != nil {
		seat.Slack = org.SlackIdentity{
			BotToken:      s.BotToken,
			SigningSecret: s.SigningSecret,
			Channel:       s.Channel,
		}
	}
	if m := r.Integrations.Mattermost; m != nil {
		seat.Mattermost = org.MattermostIdentity{
			BotToken: m.BotToken,
			Username: m.Username,
			Channel:  m.Channel,
		}
	}
	seat.JiraProject, seat.ConfluenceSpace = identities(
		r.Integrations.Jira, r.Integrations.Confluence)
	if a := r.Integrations.Atlassian; a != nil {
		// NON-NIL EVEN WHEN EMPTY, because the empty list is the setting
		// "no Atlassian products" and a nil slice downstream is the
		// setting "every configured product". Collapsing the two here
		// would licence a seat its author deliberately opted out.
		products := make([]string, 0, len(a.Products))
		for _, product := range a.Products {
			products = append(products, string(product))
		}
		seat.AtlassianProducts = products
	}

	if s := r.Sandbox; s != nil {
		env := make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			env[k] = v
		}
		if len(env) == 0 {
			env = nil
		}
		seat.Sandbox = &org.RoleSandbox{
			Enabled:         s.Enabled,
			CodingAgent:     string(s.CodingAgent),
			PauseTTLSeconds: s.PauseTTLSeconds,
			MCP:             org.RoleSandboxMCP{Servers: append([]string(nil), s.MCP.Servers...)},
			Env:             env,
		}
	}
	return seat
}

// IdentityKey is the seat's address inside its list — the derived handle, so
// it is the same identity `PUT /config/roles/{handle}` addresses and the same
// one the agent id is built from.
//
// A VALUE receiver, because the redaction walker holds the prior document by
// value and cannot take an address inside it.
func (r Role) IdentityKey() string { return r.Seat().Handle() }

// identities extracts the tracker project and wiki space a seat or unit
// owns. References are left VERBATIM and resolved at use time, like every
// other Tier B value.
func identities(jira *ProjectRef, confluence *SpaceRef) (string, string) {
	var project, space string
	if jira != nil {
		project = strings.TrimSpace(jira.Project)
	}
	if confluence != nil {
		space = strings.TrimSpace(confluence.Space)
	}
	return project, space
}

// Unit is the AUTHORED shape of one `units:` entry, nesting to any depth.
type Unit struct {
	Name string `yaml:"name" json:"name" js:"required" desc:"Unit name; also what a manages entry can reference."`

	// Type is an informational label — department, team, squad, pod, or
	// anything else. Nothing in the engine behaves differently for one.
	Type org.UnitType `yaml:"type,omitempty" json:"type,omitempty" desc:"Informational label: team (default), department, squad, ..."`

	Purpose string `yaml:"purpose,omitempty" json:"purpose,omitempty" desc:"What this unit is for."`

	// Lead names the seat that leads this unit — routing work within it,
	// acting as its point of contact, and auto-managing any direct member
	// nobody else manages. A unit with no lead of its own inherits its
	// parent's, cascading to any depth.
	Lead string `yaml:"lead,omitempty" json:"lead,omitempty" desc:"Seat leading this unit; inherited from the parent when empty."`

	Goals []string `yaml:"goals,omitempty" json:"goals,omitempty" desc:"What this unit is working toward."`

	// Channel is where this unit talks; inherited by children that
	// set none.
	//
	// Vendor-neutral, and it was not always: it was `slack_channel`, which
	// made the ONE way to give a unit a channel name a vendor this build
	// refuses — and put "Team Slack channel" into the prompt of every agent
	// in a company that talks on Mattermost. A unit's channel is a fact
	// about the unit, not about who hosts it.
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty" desc:"Unit channel on the company's chat surface; inherited by child units."`

	// Knowledge is free-text knowledge references for this unit. NOT a
	// read scope — org-wide read scope is the knowledge block.
	Knowledge []string `yaml:"knowledge,omitempty" json:"knowledge,omitempty" desc:"Free-text knowledge references. Not a read scope."`

	// MCPEnv is the tool credentials this unit's DIRECT members share,
	// with each member's own values winning per VARIABLE — a seat that
	// overrides one header must not silently drop the token beside it.
	MCPEnv org.MCPEnv `secret:"true" yaml:"mcp_env,omitempty" json:"mcp_env,omitempty" desc:"Credentials inherited by this unit's direct members."`

	Integrations UnitIntegrations `yaml:"integrations,omitempty" json:"integrations,omitzero"`

	Roles    []Role `yaml:"roles,omitempty" json:"roles,omitempty" desc:"Seats belonging to this unit."`
	Children []Unit `yaml:"children,omitempty" json:"children,omitempty" desc:"Child units."`

	// Schedules is this unit's recurring work. NOT inherited by children:
	// a standup that fanned out to every descendant of a division would
	// wake the whole company.
	Schedules []org.Schedule `yaml:"schedules,omitempty" json:"schedules,omitempty" desc:"Recurring work owned by this unit."`
}

// IdentityKey is the unit's name, which is what every `manages:`, `lead:` and
// `unit:` reference addresses it by.
func (u Unit) IdentityKey() string { return u.Name }

// UnitIntegrations is a unit's integration identity. Chat at the unit level
// is the vendor-neutral channel field, so it is deliberately not here.
type UnitIntegrations struct {
	Jira       *ProjectRef `yaml:"jira,omitempty" json:"jira,omitempty" desc:"The Jira project this unit owns."`
	Confluence *SpaceRef   `yaml:"confluence,omitempty" json:"confluence,omitempty" desc:"The Confluence space this unit owns."`
}

// IsZero lets an unset block drop out of a round trip.
func (u UnitIntegrations) IsZero() bool {
	return u.Jira == nil && u.Confluence == nil
}

func (u *Unit) validate(path string) error {
	var p problems
	if strings.TrimSpace(u.Name) == "" {
		p.add(at(path, "name"), ErrMissing, "every unit needs a name")
	}
	for i := range u.Roles {
		p.wrap(u.Roles[i].validate(idx(at(path, "roles"), i)))
	}
	for i := range u.Children {
		p.wrap(u.Children[i].validate(idx(at(path, "children"), i)))
	}
	return p.err()
}

// Unit transforms the authored unit into the runtime one, recursively.
//
// It does NOT apply inheritance — credentials, leads and channels cascade
// in [org.Organization.Normalize], which runs once over the whole tree.
// Applying them here as well would be a second implementation of the same
// rules, reached only on the load path, and the two would drift the first
// time one was fixed.
func (u *Unit) Unit() *org.Unit {
	unit := &org.Unit{
		Name:          u.Name,
		Type:          u.Type,
		Purpose:       u.Purpose,
		Lead:          u.Lead,
		Goals:         append([]string(nil), u.Goals...),
		Channel:       u.Channel,
		KnowledgeRefs: append([]string(nil), u.Knowledge...),
		MCPEnv:        u.MCPEnv.Clone(),
		Schedules:     append([]org.Schedule(nil), u.Schedules...),
	}
	unit.JiraProject, unit.ConfluenceSpace = identities(
		u.Integrations.Jira, u.Integrations.Confluence)

	for i := range u.Roles {
		unit.Roles = append(unit.Roles, u.Roles[i].Seat())
	}
	for i := range u.Children {
		unit.Children = append(unit.Children, u.Children[i].Unit())
	}
	return unit
}
