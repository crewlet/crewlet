package org

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// RoleKind is who holds a seat in the org chart.
//
// The zero value is an agent seat, which is what an unannotated role in a
// config file means. Human is always explicit — a seat that is never
// spawned is the exception, and making it the accident of a missing field
// would be the wrong way round.
type RoleKind string

const (
	// KindAgent is an AI agent: spawned with an inbox, an LLM and the full
	// turn-engine runtime.
	KindAgent RoleKind = "agent"

	// KindHuman is a human teammate: a first-class seat in the hierarchy —
	// rosters, colleague lookup, unit lead, escalation target — that is
	// never executable. No instance, no inbox topic, no LLM, no learning
	// rows. Agents reach a human the same way they reach each other, on the
	// external surfaces with an @-mention; the engine never sends as itself.
	KindHuman RoleKind = "human"
)

// Transport is an external surface a human seat can be reached on.
type Transport string

// The transports a contact identity can name. Jira and Confluence are two
// transports over one Atlassian account id.
const (
	TransportSlack      Transport = "slack"
	TransportMattermost Transport = "mattermost"
	TransportJira       Transport = "jira"
	TransportConfluence Transport = "confluence"
	TransportGitHub     Transport = "github"
	TransportGitLab     Transport = "gitlab"
	TransportPlane      Transport = "plane"
)

// handlePattern is the canonical handle shape. Slugify output always
// conforms; an explicit handle is checked against it at validation.
var handlePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidHandle reports whether s is a registry-safe seat handle: lowercase
// alphanumerics and hyphens, starting with an alphanumeric.
//
// This is the ONE definition of the shape, exported because the party
// registry enforces it again at registration time — a second regex there
// that disagreed by one character would accept a handle at config time and
// reject the same seat during engine start.
func ValidHandle(s string) bool { return handlePattern.MatchString(s) }

// EnvLookup resolves an environment variable, reporting whether it is set.
// It matches os.LookupEnv, which is what a nil lookup falls back to.
type EnvLookup func(name string) (string, bool)

// HumanContact is how agents reach a human seat: the account ids to mention
// on each external surface, and the ids inbound webhooks carry when that
// person acts.
//
// Every field takes a literal id or exactly one whole-value ${VAR}
// reference — instance-specific ids cannot ship in a config file that gets
// committed. A reference is stored VERBATIM and resolved at consumption
// time by [HumanContact.ResolvedIdentities]; it is never case-mangled,
// because variable names are case-sensitive and a lowercased reference is
// permanently unresolvable.
type HumanContact struct {
	// SlackUserID is a Slack member id (U0123456789) — <@…> mentions and
	// DM delivery.
	SlackUserID string `yaml:"slack_user_id,omitempty" json:"slack_user_id,omitempty"`

	// MattermostUserID is a Mattermost USERNAME, not the 26-character user
	// id: Mattermost mentions address a person by name, and the name is
	// what an agent has to write into a message for it to render.
	MattermostUserID string `yaml:"mattermost_user_id,omitempty" json:"mattermost_user_id,omitempty"`

	// AtlassianAccountID is an Atlassian Cloud account id — one id covers
	// both Jira and Confluence: assignments, <ri:user> mention markup, and
	// sender attribution on inbound webhooks.
	AtlassianAccountID string `yaml:"atlassian_account_id,omitempty" json:"atlassian_account_id,omitempty"`

	// GitHubLogin is a GitHub username — review requests and sender
	// attribution. Literal values are lowercased: webhook parsers and
	// agent-side identity resolution both lowercase logins, so a mixed-case
	// value here would never match.
	GitHubLogin string `yaml:"github_login,omitempty" json:"github_login,omitempty"`

	// GitLabUsername is a GitLab username — assignment, review requests and
	// sender attribution. Lowercased for the same reason as GitHubLogin.
	GitLabUsername string `yaml:"gitlab_username,omitempty" json:"gitlab_username,omitempty"`

	// PlaneUserID is a Plane workspace-member user UUID — assignment,
	// subscriber and <mention-component> attribution. Lowercased: webhook
	// payloads and GET /users/me both carry lowercase UUIDs.
	PlaneUserID string `yaml:"plane_user_id,omitempty" json:"plane_user_id,omitempty"`
}

// contactField describes one identity field: what an operator writes, how
// its literal values are normalised, and where the value lives.
type contactField struct {
	// key is the config key, so an error names what the operator typed
	// rather than a Go field name they have never seen.
	key string

	// lowercase marks a field whose LITERAL values are canonically
	// lowercase. Both sides of the match lowercase — the webhook parser and
	// the boot-time identity lookup — so a mixed-case literal here would
	// never resolve. A ${VAR} reference is exempt and is lowercased after
	// resolution instead.
	lowercase bool

	// value addresses the field, so normalisation and validation write
	// through the same table they read through: adding a surface is one
	// entry here rather than an edit in four loops.
	value func(*HumanContact) *string
}

// Indices into contactFields. Named so the transport table below reads as
// a mapping rather than as magic numbers.
const (
	fieldSlackUserID = iota
	fieldMattermostUserID
	fieldAtlassianAccountID
	fieldGitHubLogin
	fieldGitLabUsername
	fieldPlaneUserID
)

// contactFields is every identity a contact can carry, in config order.
// Treat as immutable.
var contactFields = []contactField{
	{key: "slack_user_id", value: func(c *HumanContact) *string { return &c.SlackUserID }},
	{key: "mattermost_user_id", value: func(c *HumanContact) *string { return &c.MattermostUserID }},
	{key: "atlassian_account_id", value: func(c *HumanContact) *string { return &c.AtlassianAccountID }},
	{key: "github_login", lowercase: true, value: func(c *HumanContact) *string { return &c.GitHubLogin }},
	{key: "gitlab_username", lowercase: true, value: func(c *HumanContact) *string { return &c.GitLabUsername }},
	{key: "plane_user_id", lowercase: true, value: func(c *HumanContact) *string { return &c.PlaneUserID }},
}

// contactTransports is THE transport-to-field table: which field carries a
// given transport's external id, in the order identities are enumerated.
//
// Single source of truth — registration, party resolution and identity
// rendering all derive from it, so a new surface reaches every consumer at
// once. Jira and Confluence deliberately share one field.
var contactTransports = []struct {
	transport Transport
	field     int
}{
	{TransportSlack, fieldSlackUserID},
	{TransportMattermost, fieldMattermostUserID},
	{TransportJira, fieldAtlassianAccountID},
	{TransportConfluence, fieldAtlassianAccountID},
	{TransportGitHub, fieldGitHubLogin},
	{TransportGitLab, fieldGitLabUsername},
	{TransportPlane, fieldPlaneUserID},
}

// Identity is one external account a human seat can be reached at.
type Identity struct {
	Transport  Transport
	ExternalID string
}

// Normalize strips whitespace and lowercases the literal values of the
// case-normalised fields. It is idempotent.
//
// Whitespace goes first: it is never part of any of these ids, and a padded
// reference (" ${VAR} ") would otherwise fail the whole-value test and be
// silently case-mangled into something that can never resolve.
func (c *HumanContact) Normalize() {
	if c == nil {
		return
	}
	for _, f := range contactFields {
		p := f.value(c)
		v := strings.TrimSpace(*p)
		if v != "" && f.lowercase {
			if _, isRef := envref.Whole(v); !isRef {
				v = strings.ToLower(v)
			}
		}
		*p = v
	}
}

// Validate rejects a value that embeds a ${VAR} reference inside a longer
// string.
//
// Such a value is neither a literal id nor a reference: resolution
// substitutes embedded references too, so letting it through would register
// a truncated or mangled identity that silently matches nobody. "${A}${B}"
// is embedded for the same reason — the resolved value is a concatenation,
// not an identity.
//
// The trigger is any "${" rather than [envref.Has], deliberately: a value
// like "${1}" is one the resolver would leave alone, so it would register
// verbatim as somebody's Slack id. No account id on any of these platforms
// contains a brace, so the broader read costs nothing and catches the near
// miss.
func (c *HumanContact) Validate() error {
	if c == nil {
		return nil
	}
	var errs []error
	for _, f := range contactFields {
		v := strings.TrimSpace(*f.value(c))
		if !strings.Contains(v, "${") {
			continue
		}
		if _, isRef := envref.Whole(v); !isRef {
			errs = append(errs, fmt.Errorf("contact.%s: %w: %q", f.key, ErrEmbeddedEnvRef, v))
		}
	}
	return errors.Join(errs...)
}

// IsEmpty reports whether no identity is declared. A ${VAR} reference
// counts as declared — it is an identity the operator has committed to,
// resolved at use time — so a seat whose ids all live in the environment is
// still reachable.
func (c *HumanContact) IsEmpty() bool {
	if c == nil {
		return true
	}
	for _, f := range contactFields {
		if *f.value(c) != "" {
			return false
		}
	}
	return true
}

// Identities returns the declared (transport, external id) pairs exactly as
// configured — values may still be unresolved ${VAR} references.
//
// Anything feeding routing, registration, prompts or mention markup wants
// [HumanContact.ResolvedIdentities] instead; this is for showing an
// operator what their config says.
func (c *HumanContact) Identities() []Identity {
	if c == nil {
		return nil
	}
	var out []Identity
	for _, t := range contactTransports {
		if v := *contactFields[t.field].value(c); v != "" {
			out = append(out, Identity{Transport: t.transport, ExternalID: v})
		}
	}
	return out
}

// ResolvedIdentities returns the pairs ready to consume: every ${VAR}
// reference resolved through lookup, then lowercased for the
// case-normalised transports. A nil lookup reads the process environment.
//
// An identity whose reference does not resolve is OMITTED. Emitting the raw
// ${VAR} text would poison registration, prompts and mention markup with a
// string no webhook payload or platform lookup could ever match — the
// failure would surface as a person who mysteriously never gets mentioned,
// rather than as a missing variable.
//
// The lookup is a parameter rather than a package-level read because the
// environment is the one input to this model that is genuinely ambient, and
// ambient inputs are what make a test suite serial.
func (c *HumanContact) ResolvedIdentities(lookup EnvLookup) []Identity {
	if c == nil {
		return nil
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	var out []Identity
	for _, t := range contactTransports {
		f := contactFields[t.field]
		raw := strings.TrimSpace(*f.value(c))
		if raw == "" {
			continue
		}
		resolved := raw
		if name, isRef := envref.Whole(raw); isRef {
			v, ok := lookup(name)
			if !ok {
				continue
			}
			resolved = strings.TrimSpace(v)
			if resolved == "" {
				continue
			}
		}
		if f.lowercase {
			resolved = strings.ToLower(resolved)
		}
		out = append(out, Identity{Transport: t.transport, ExternalID: resolved})
	}
	return out
}

// SlackIdentity is a seat's own Slack app: the credentials its transport
// verifies inbound requests with and delivers outbound messages through.
// Each agent has its own app, which is what makes an agent's messages come
// from that agent rather than from a shared company bot.
//
// These drive the TRANSPORT only. The Slack tool server is a separate MCP
// entry, and its bot token is named again under mcp_env.slack — two
// consumers, one secret, named twice on purpose so an operator can split
// them.
type SlackIdentity struct {
	BotToken      string `yaml:"bot_token,omitempty" json:"bot_token,omitempty"`
	SigningSecret string `yaml:"signing_secret,omitempty" json:"signing_secret,omitempty"`
	// Channel is an optional default channel id for this seat.
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`
}

// IsZero reports an unconfigured Slack identity.
func (s SlackIdentity) IsZero() bool { return s == SlackIdentity{} }

// MattermostIdentity is a seat's Mattermost bot. One credential covers
// everything on this backend: the inbound websocket, the outbound REST
// calls, and — named again under mcp_env.mattermost — the MCP tool server.
type MattermostIdentity struct {
	BotToken string `yaml:"bot_token,omitempty" json:"bot_token,omitempty"`
	// Username defaults to the seat handle when empty.
	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	// Channel is an optional default channel for this seat.
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`
}

// IsZero reports an unconfigured Mattermost identity.
func (m MattermostIdentity) IsZero() bool { return m == MattermostIdentity{} }

// RoleSandboxMCP is which of the seat's MCP servers the coding agent gets.
//
// Scoping is at the SERVER level only, and that is a platform fact rather
// than a simplification: OpenCode has no per-tool allowlist flag and Claude
// Code runs headless with permissions bypassed, so a curated tool list
// could not be enforced uniformly. Each platform gets the servers it is
// given.
type RoleSandboxMCP struct {
	Servers []string `yaml:"servers,omitempty" json:"servers,omitempty"`
}

// RoleSandbox is the per-seat code-runtime gate. Absent means the seat
// never sees the option: the run_sandbox Execute tool is not offered to it.
type RoleSandbox struct {
	Enabled bool `yaml:"enabled" json:"enabled"`

	// CodingAgent overrides the provider-wide default for this seat only.
	// Empty inherits it, resolved at launch rather than at config time so a
	// provider swap reaches seats that never named one.
	CodingAgent string `yaml:"coding_agent,omitempty" json:"coding_agent,omitempty"`

	// PauseTTLSeconds bounds how long a sandbox blocked on a clarification
	// stays paused before it is reaped and re-seeded.
	//
	// A POINTER, because there are three states and a float64 has two:
	// UNSET inherits the provider default, an explicit 0 means never pause.
	// A plain number would read an unset field as "never pause", so every
	// seat that said nothing would lose its checkout the moment a coding
	// agent asked a question.
	PauseTTLSeconds *float64 `yaml:"pause_ttl_seconds,omitempty" json:"pause_ttl_seconds,omitempty"`

	MCP RoleSandboxMCP `yaml:"mcp,omitempty" json:"mcp,omitzero"`

	// Env is injected into this seat's sandbox run. External service
	// tokens are DECLARED here (GITHUB_TOKEN for the git-auth recipe, a
	// private-registry token, a test DATABASE_URL): the engine names no
	// tool-specific variable of its own, and only LLM credentials derive
	// automatically.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

// Role is a SEAT in the org chart, held by an AI agent or a human.
//
// Each Role is one individual with its own identity, expertise and
// position. Agent seats map 1:1 to a running instance — they are not
// interchangeable. Human seats sit in the same hierarchy (manages, unit
// lead, rosters, lookup, escalation) and are never spawned.
//
// A Role is used by pointer everywhere: normalisation mutates it (a lead
// gains manages entries, a seat inherits its unit's MCP credentials) and
// callers compare seats by identity, so a copy would be a different seat
// that looks the same.
type Role struct {
	Name string   `yaml:"name" json:"name"`
	Kind RoleKind `yaml:"kind,omitempty" json:"kind,omitempty"`

	// Contact is the external identities of a HUMAN seat. Nil on an agent
	// seat; a human seat needs at least one identity in it.
	Contact *HumanContact `yaml:"contact,omitempty" json:"contact,omitempty"`

	// Availability is a free-text note for a human seat, rendered into
	// rosters and colleague lookups: "CET business hours; replies within
	// ~4h". It is what sets a report's expectations before it waits.
	Availability string `yaml:"availability,omitempty" json:"availability,omitempty"`

	// DeclaredHandle is the operator's explicit handle override, empty when
	// the handle is derived from the name. Read [Role.Handle] instead —
	// this field is the config, not the identity.
	DeclaredHandle string `yaml:"handle,omitempty" json:"handle,omitempty"`

	Email string `yaml:"email,omitempty" json:"email,omitempty"`

	// UnitRef is a SOFT reference to the unit this seat belongs to, used
	// when the seat is declared at the org root rather than nested inside
	// the unit — which is how the per-entity config API adds one.
	// [Organization.Normalize] moves such a seat into the named unit so
	// that everything downstream can treat it as unit-scoped; without the
	// move it would miss the unit's MCP inheritance and be invisible to the
	// unit lead. Empty for a genuinely org-wide seat.
	UnitRef string `yaml:"unit,omitempty" json:"unit,omitempty"`

	Goal             string   `yaml:"goal,omitempty" json:"goal,omitempty"`
	Backstory        string   `yaml:"backstory,omitempty" json:"backstory,omitempty"`
	Responsibilities []string `yaml:"responsibilities,omitempty" json:"responsibilities,omitempty"`

	// Manages is the seats this one manages. An entry naming a UNIT
	// expands to every role in it, including descendants — see
	// [Organization.Normalize]. Read it after normalisation and it is
	// always role names.
	Manages []string `yaml:"manages,omitempty" json:"manages,omitempty"`

	BehavioralGuidelines []string `yaml:"behavioral_guidelines,omitempty" json:"behavioral_guidelines,omitempty"`

	// TokenBudget caps this seat's spend; 0 is unlimited.
	TokenBudget int `yaml:"token_budget,omitempty" json:"token_budget,omitempty"`

	// LLM is the provider chain used when a phase does not name its own.
	LLM ProviderKeys `yaml:"llm,omitempty" json:"llm,omitzero"`

	// The per-phase chains. Each falls back to LLM when unset.
	LLMPlan    ProviderKeys `yaml:"llm_plan,omitempty" json:"llm_plan,omitzero"`
	LLMExecute ProviderKeys `yaml:"llm_execute,omitempty" json:"llm_execute,omitzero"`
	LLMReview  ProviderKeys `yaml:"llm_review,omitempty" json:"llm_review,omitzero"`
	// LLMSubagent runs ephemeral sub-agents spawned from this seat.
	LLMSubagent ProviderKeys `yaml:"llm_subagent,omitempty" json:"llm_subagent,omitzero"`
	// LLMAuxiliary runs the cheap work the learning subsystem drives —
	// episode summarisation and post-turn reflection. Point it at a fast
	// model so reflection does not inflate the cost of every turn.
	LLMAuxiliary ProviderKeys `yaml:"llm_auxiliary,omitempty" json:"llm_auxiliary,omitzero"`
	// LLMJudge runs the round-cap extension judge, which decides whether a
	// phase that exhausted its tool rounds is making progress or thrashing.
	// The decision is cheap and the prompt is bounded, so a small fast
	// model is the right default.
	LLMJudge ProviderKeys `yaml:"llm_judge,omitempty" json:"llm_judge,omitzero"`
	// LLMSandbox runs the coding agent inside the sandbox. It falls back to
	// LLMExecute before LLM: sandboxed work IS this seat's Execute phase,
	// just running somewhere else.
	LLMSandbox ProviderKeys `yaml:"llm_sandbox,omitempty" json:"llm_sandbox,omitzero"`

	// LearningEnabled overrides the system-wide learning setting for this
	// seat. Unset inherits it; false skips every reflection worker for this
	// seat's turns while still writing episodes, which are cheap and useful
	// to a human regardless. It exists so an operator can opt out one noisy
	// or sensitive seat without disabling the subsystem.
	LearningEnabled Toggle `yaml:"learning_enabled,omitempty" json:"learning_enabled,omitzero"`

	MCPEnv MCPEnv `yaml:"mcp_env,omitempty" json:"mcp_env,omitempty"`

	// Sandbox is the code-runtime gate; nil means this seat has none.
	Sandbox *RoleSandbox `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`

	// Placement constrains which nodes may run this seat; the zero value is
	// anywhere.
	//
	// It is a constraint on the fleet, not a schedule: the seat is claimed
	// by whichever eligible node reaches it first, and a node that stops
	// matching hands it back at the next turn boundary. Narrowing it to a
	// set no live node satisfies leaves the seat unserved, and the engine
	// reports that rather than quietly widening the constraint — widening
	// is exactly what the operator asked it not to do.
	//
	// Only meaningful for an agent seat; a human seat is never claimed.
	Placement placement.SeatPlacement `yaml:"placement,omitempty" json:"placement,omitzero"`

	Slack      SlackIdentity      `yaml:"slack,omitempty" json:"slack,omitzero"`
	Mattermost MattermostIdentity `yaml:"mattermost,omitempty" json:"mattermost,omitzero"`

	// JiraProject, ConfluenceSpace and PlaneProject are this seat's
	// integration IDENTITY: where inbound activity with no better recipient
	// routes, and where the seat files its work. Meaningful for a root-level
	// seat; for a unit-nested one the unit carries it.
	//
	// None of them is an MCP credential, and none of them scopes what the
	// seat can READ — read scope is org-wide, on the Organization.
	JiraProject     string `yaml:"jira_project,omitempty" json:"jira_project,omitempty"`
	ConfluenceSpace string `yaml:"confluence_space,omitempty" json:"confluence_space,omitempty"`
	PlaneProject    string `yaml:"plane_project,omitempty" json:"plane_project,omitempty"`

	// Schedules are this seat's own recurring work; each fires a task into
	// this seat's inbox on its cron expression.
	Schedules []Schedule `yaml:"schedules,omitempty" json:"schedules,omitempty"`
}

// IsHuman reports whether a human holds this seat.
func (r *Role) IsHuman() bool { return r.Kind == KindHuman }

// IsAgent reports whether an AI agent holds this seat — the case an unset
// kind means. Validate rejects any other kind, so these two are total.
func (r *Role) IsAgent() bool { return !r.IsHuman() }

// Handle is the canonical identity for this seat: the explicit handle if
// the operator set one, otherwise the slugified name.
//
// Everything that names a seat outside the org model goes through here —
// inbox topics, plus-addressed email, external-id registration, the derived
// agent id — so a caller reading the raw field would be identifying a
// different seat from the one the engine runs.
func (r *Role) Handle() string {
	if r.DeclaredHandle != "" {
		return r.DeclaredHandle
	}
	return Slugify(r.Name)
}

// humanForbidden is the runtime-only surface a human seat must not carry,
// in the order an error reports it. Human seats are never spawned, so every
// one of these would be config that looks live and does nothing.
//
// The names are the ones an operator WROTE (integrations.jira, not
// jira_project), because the error's job is to point at a line in a file.
func (r *Role) humanForbidden() []string {
	fields := []struct {
		name string
		set  bool
	}{
		{"llm", len(r.LLM) > 0},
		{"llm_plan", len(r.LLMPlan) > 0},
		{"llm_execute", len(r.LLMExecute) > 0},
		{"llm_review", len(r.LLMReview) > 0},
		{"llm_subagent", len(r.LLMSubagent) > 0},
		{"llm_auxiliary", len(r.LLMAuxiliary) > 0},
		{"llm_judge", len(r.LLMJudge) > 0},
		{"llm_sandbox", len(r.LLMSandbox) > 0},
		{"sandbox", r.Sandbox != nil},
		{"token_budget", r.TokenBudget != 0},
		{"learning_enabled", r.LearningEnabled.IsSet()},
		{"schedules", len(r.Schedules) > 0},
		{"slack", !r.Slack.IsZero()},
		{"mattermost", !r.Mattermost.IsZero()},
		{"integrations.jira", r.JiraProject != ""},
		{"integrations.confluence", r.ConfluenceSpace != ""},
		{"integrations.plane", r.PlaneProject != ""},
		{"mcp_env", len(r.MCPEnv) > 0},
		{"behavioral_guidelines", len(r.BehavioralGuidelines) > 0},
	}
	var out []string
	for _, f := range fields {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// Validate reports every rule this seat breaks, joined.
//
// It reports ALL of them rather than the first: a config author fixing one
// field at a time through a validate-edit loop pays a round trip per
// mistake, and the mistakes here are usually made together.
func (r *Role) Validate() error {
	var errs []error
	name := strings.TrimSpace(r.Name)
	if name == "" {
		errs = append(errs, fmt.Errorf("role: %w", ErrMissingName))
	}

	switch r.Kind {
	case "", KindAgent, KindHuman:
	default:
		errs = append(errs, fmt.Errorf("role %q: %w: %q (want agent or human)", name, ErrUnknownKind, r.Kind))
	}

	switch {
	case r.DeclaredHandle != "" && !ValidHandle(r.DeclaredHandle):
		suggestion := Slugify(r.DeclaredHandle)
		if suggestion == "" {
			suggestion = "my-handle"
		}
		errs = append(errs, fmt.Errorf(
			"role %q: %w: %q — e.g. %q",
			name, ErrInvalidHandle, r.DeclaredHandle, suggestion))
	case name != "" && r.Handle() == "":
		// A name of nothing but punctuation slugifies to nothing, and a
		// seat with no handle derives no agent id and owns no inbox — it
		// would sit in the chart looking fine and never receive anything.
		errs = append(errs, fmt.Errorf(
			"role %q: %w: the name yields no handle, so set one explicitly",
			name, ErrInvalidHandle))
	}

	if r.IsHuman() {
		if offending := r.humanForbidden(); len(offending) > 0 {
			errs = append(errs, fmt.Errorf(
				"role %q: %w: %s",
				name, ErrHumanSeatField, strings.Join(offending, ", ")))
		}
		if r.Contact.IsEmpty() {
			errs = append(errs, fmt.Errorf("role %q: %w", name, ErrNoContact))
		}
	} else {
		var humanOnly []string
		if r.Contact != nil {
			humanOnly = append(humanOnly, "contact")
		}
		if r.Availability != "" {
			humanOnly = append(humanOnly, "availability")
		}
		if len(humanOnly) > 0 {
			errs = append(errs, fmt.Errorf(
				"role %q: %w: %s (did you mean kind: human?)",
				name, ErrAgentSeatField, strings.Join(humanOnly, ", ")))
		}
	}

	if err := r.Contact.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("role %q: %w", name, err))
	}
	if err := validateSchedules(fmt.Sprintf("role %q", name), r.Schedules); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
