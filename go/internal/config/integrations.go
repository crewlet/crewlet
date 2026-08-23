package config

import (
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
)

// Integrations is the INBOUND half of the external world: how events reach
// agents, and how notifications get delivered.
//
// It carries no tool credentials. Agent tools are MCP servers under
// mcp_servers, and the engine never derives one from an integration block —
// the two are separate on purpose, because they are opposite directions
// with different identities. A chat-enabled seat names its bot token twice
// (once for the transport, once for the tool server) pointing at ONE
// ${VAR}: two consumers, one secret.
//
// Every block is a POINTER, and nil is meaningful: it is what turns the
// integration off. An empty block is not the same thing — `slack: {}` is
// how Slack is enabled with nothing overridden.
type Integrations struct {
	Jira       *Jira       `yaml:"jira,omitempty" json:"jira,omitempty" desc:"Jira admin account and webhook secret. Absent = disabled."`
	Confluence *Confluence `yaml:"confluence,omitempty" json:"confluence,omitempty" desc:"Confluence admin account. Absent = disabled."`
	Slack      *Slack      `yaml:"slack,omitempty" json:"slack,omitempty" desc:"Slack transport marker and org-wide behaviour. Absent = disabled."`
	Mattermost *Mattermost `yaml:"mattermost,omitempty" json:"mattermost,omitempty" desc:"Mattermost instance and team. Absent = disabled."`
	GitHub     *GitHub     `yaml:"github,omitempty" json:"github,omitempty" desc:"GitHub webhook config. Absent = disabled."`
	GitLab     *GitLab     `yaml:"gitlab,omitempty" json:"gitlab,omitempty" desc:"GitLab instance, webhook signing and provisioning. Absent = disabled."`
	Plane      *Plane      `yaml:"plane,omitempty" json:"plane,omitempty" desc:"Plane instance, workspace and webhook secret. Absent = disabled."`

	// ForgeAppID verifies the Forge app's invocation tokens: the JWT's
	// audience claim must match it. Required when the Forge app is used —
	// the endpoint rejects every request without it.
	ForgeAppID string `yaml:"forge_app_id,omitempty" json:"forge_app_id,omitempty" desc:"Forge app id whose invocation tokens the webhook endpoint accepts."`
}

func (i *Integrations) validate(path string) error {
	var p problems
	if i.Jira != nil {
		p.wrap(i.Jira.validate(at(path, "jira")))
	}
	if i.Confluence != nil {
		p.wrap(i.Confluence.validate(at(path, "confluence")))
	}
	if i.Slack != nil {
		p.wrap(i.Slack.validate(at(path, "slack")))
	}
	if i.Mattermost != nil {
		p.wrap(i.Mattermost.validate(at(path, "mattermost")))
	}
	if i.GitHub != nil {
		p.wrap(i.GitHub.validate(at(path, "github")))
	}
	if i.GitLab != nil {
		p.wrap(i.GitLab.validate(at(path, "gitlab")))
	}
	if i.Plane != nil {
		p.wrap(i.Plane.validate(at(path, "plane")))
	}
	return p.err()
}

// The Atlassian Cloud gateways a cloud id resolves against.
const (
	atlassianJiraGateway       = "https://api.atlassian.com/ex/jira"
	atlassianConfluenceGateway = "https://api.atlassian.com/ex/confluence"
)

// Jira is the org-level Jira admin account.
//
// NOT a per-agent identity — each agent authenticates through its own
// mcp_env credentials. This account exists for the org-wide reads routing
// needs (who is watching this ticket) and for the inbound webhook secret.
type Jira struct {
	// URL is a direct instance URL. Mutually exclusive with CloudID.
	URL string `yaml:"url,omitempty" json:"url,omitempty" desc:"Instance URL. Give this or cloud_id, not both."`

	// CloudID is an Atlassian Cloud id; the gateway URL is built from it.
	CloudID string `yaml:"cloud_id,omitempty" json:"cloud_id,omitempty" desc:"Atlassian Cloud id. Give this or url, not both."`

	// Token is the admin account's API token or PAT.
	Token string `secret:"true" yaml:"token" json:"token" js:"required" desc:"Admin API token or PAT; ${VAR} supported."`

	// Email switches authentication to Basic base64(email:token), which is
	// what Cloud requires. Omitted uses a bearer token, which is what a
	// service account and a Data Center PAT want.
	Email string `yaml:"email,omitempty" json:"email,omitempty" desc:"Set for Cloud Basic auth; omit for bearer-token auth."`

	// WebhookSecret verifies inbound webhook signatures. Empty means
	// signatures are not verified, and the route answers 503 rather than
	// accepting an unverifiable payload.
	WebhookSecret string `secret:"true" yaml:"webhook_secret,omitempty" json:"webhook_secret,omitempty" desc:"HMAC secret for inbound webhooks."`
}

// BaseURL is the REST base: the gateway for a cloud id, the instance URL
// otherwise.
func (j *Jira) BaseURL() string {
	if j.CloudID != "" {
		return atlassianJiraGateway + "/" + j.CloudID
	}
	return j.URL
}

func (j *Jira) validate(path string) error {
	var p problems
	p.wrap(urlXorCloudID(path, j.URL, j.CloudID, "https://mycompany.atlassian.net"))
	if strings.TrimSpace(j.Token) == "" {
		p.add(at(path, "token"), ErrMissing,
			"the admin account's token — routing needs it to read ticket watchers")
	}
	return p.err()
}

// Confluence is the org-level Confluence admin account.
//
// Like Jira, this is the org-wide read account behind webhook routing, not
// a per-agent identity. The org-wide READ SCOPE is knowledge.confluence_spaces
// and lives nowhere near here.
type Confluence struct {
	URL     string `yaml:"url,omitempty" json:"url,omitempty" desc:"Instance URL. Give this or cloud_id, not both."`
	CloudID string `yaml:"cloud_id,omitempty" json:"cloud_id,omitempty" desc:"Atlassian Cloud id. Give this or url, not both."`

	// SiteURL is the human-readable base for shareable links, needed only
	// with a cloud id — the API gateway URL is not something to hand a
	// person. With a direct URL this defaults to it.
	SiteURL string `yaml:"site_url,omitempty" json:"site_url,omitempty" desc:"Human-readable base for shareable links; needed with cloud_id."`

	Token         string `secret:"true" yaml:"token" json:"token" js:"required" desc:"Admin API token or PAT; ${VAR} supported."`
	Email         string `yaml:"email,omitempty" json:"email,omitempty" desc:"Set for Cloud Basic auth; omit for bearer-token auth."`
	WebhookSecret string `secret:"true" yaml:"webhook_secret,omitempty" json:"webhook_secret,omitempty" desc:"HMAC secret for Data Center webhooks."`
}

// BaseURL is the REST base.
func (c *Confluence) BaseURL() string {
	if c.CloudID != "" {
		return atlassianConfluenceGateway + "/" + c.CloudID
	}
	return c.URL
}

// ShareableBaseURL is the base for links handed to a person. With a cloud
// id and no site URL there is none, and returning the gateway URL would
// produce links that look right and open nothing.
func (c *Confluence) ShareableBaseURL() string {
	if c.SiteURL != "" {
		return c.SiteURL
	}
	return c.URL
}

func (c *Confluence) validate(path string) error {
	var p problems
	p.wrap(urlXorCloudID(path, c.URL, c.CloudID, "https://mycompany.atlassian.net/wiki"))
	if strings.TrimSpace(c.Token) == "" {
		p.add(at(path, "token"), ErrMissing, "the admin account's token")
	}
	return p.err()
}

// urlXorCloudID holds the Atlassian addressing rule both blocks share:
// exactly one of the two. Neither leaves the client with no address at all;
// both leave two, and nothing decides between them.
func urlXorCloudID(path, url, cloudID, example string) error {
	switch {
	case url != "" && cloudID != "":
		return fault(path, ErrConflict, "give either url or cloud_id, not both")
	case url == "" && cloudID == "":
		return fault(path, ErrMissing,
			"give either url (e.g. %q) or cloud_id (the Atlassian Cloud id)", example)
	}
	return nil
}

// WorkingStatus is when a seat raises the "is thinking…" indicator while it
// reasons about a chat message.
type WorkingStatus string

const (
	// StatusAddressed shows it only when a human is plausibly waiting on
	// this seat: a DM, a direct mention, or a thread it already follows.
	StatusAddressed WorkingStatus = "addressed"
	// StatusAlways shows it on every chat-triggered turn, including
	// passive channel messages and broadcasts.
	StatusAlways WorkingStatus = "always"
	// StatusOff disables it.
	StatusOff WorkingStatus = "off"
)

// WorkingStatuses is the closed set.
var WorkingStatuses = []WorkingStatus{StatusAddressed, StatusAlways, StatusOff}

// Slack is the org-level Slack block.
//
// Every CREDENTIAL is per-agent (role.integrations.slack); this block is
// the transport-enable marker plus the genuinely org-wide behaviour.
// Declaring it at all — even as `slack: {}` — turns the transport and
// per-agent webhook routing on.
type Slack struct {
	// TypingStatus defaults to addressed: only when someone is plausibly
	// waiting.
	TypingStatus WorkingStatus `yaml:"typing_status,omitempty" json:"typing_status,omitempty" js:"enum=addressed|always|off" desc:"When to show the working indicator (default addressed)."`

	// StatusPhrases replaces the words the indicator shows.
	StatusPhrases StatusPhrases `yaml:"status_phrases,omitempty" json:"status_phrases,omitzero"`
}

func (s *Slack) validate(path string) error {
	var p problems
	if s.TypingStatus != "" && !oneOf(s.TypingStatus, WorkingStatuses) {
		p.add(at(path, "typing_status"), ErrUnknownValue, "%q (want %s)",
			s.TypingStatus, names(WorkingStatuses))
	}
	p.wrap(s.StatusPhrases.validate(at(path, "status_phrases")))
	return p.err()
}

// Status is the indicator mode, applying the default.
func (s *Slack) Status() WorkingStatus {
	if s.TypingStatus == "" {
		return StatusAddressed
	}
	return s.TypingStatus
}

// StatusPhrases overrides the per-phase working-status lines.
//
// Each line is suffixed to the seat's name, so every phrase must complete
// that sentence: "is thinking very hard..." reads as "Agent SWE is thinking
// very hard…". A phase draws from its whole list — one line is picked per
// phase and held for its duration — so a list of one is a fixed label and a
// longer list gives the indicator variety across turns.
//
// Keep every phrase GENERIC TO THE PHASE. The pick is arbitrary and never
// inspects what the agent is doing, so a line naming real work ("is
// checking Jira...") is a claim that is false most of the time it shows.
type StatusPhrases struct {
	Onboarding []string `yaml:"onboarding,omitempty" json:"onboarding,omitempty" desc:"Lines shown during the first-turn onboarding pass."`
	Plan       []string `yaml:"plan,omitempty" json:"plan,omitempty" desc:"Lines shown during Plan."`
	Execute    []string `yaml:"execute,omitempty" json:"execute,omitempty" desc:"Lines shown during Execute."`
	Review     []string `yaml:"review,omitempty" json:"review,omitempty" desc:"Lines shown during Review."`
	// Default covers any phase added later that has no pool of its own.
	Default []string `yaml:"default,omitempty" json:"default,omitempty" desc:"Lines for any phase with no pool of its own."`
}

// IsZero lets an unset block drop out of a round trip.
func (s StatusPhrases) IsZero() bool {
	return len(s.Onboarding) == 0 && len(s.Plan) == 0 && len(s.Execute) == 0 &&
		len(s.Review) == 0 && len(s.Default) == 0
}

func (s StatusPhrases) validate(path string) error {
	var p problems
	pools := []struct {
		name  string
		lines []string
	}{
		{"onboarding", s.Onboarding}, {"plan", s.Plan}, {"execute", s.Execute},
		{"review", s.Review}, {"default", s.Default},
	}
	for _, pool := range pools {
		for i, phrase := range pool.lines {
			if strings.TrimSpace(phrase) == "" {
				// An empty LIST is the documented "keep the built-in
				// pool"; a list containing an empty string would post a
				// cleared indicator, the exact opposite of the intent.
				p.add(idx(at(path, pool.name), i), ErrMissing,
					"a status phrase must not be empty — omit the phase (or "+
						"give it []) to keep the built-in pool")
			}
		}
	}
	return p.err()
}

// Mattermost is the org-level block for the self-hosted chat backend.
//
// One structural difference shapes the whole integration: MATTERMOST HAS NO
// USABLE INBOUND WEBHOOK. Its outgoing webhooks fire only in public
// channels and carry no thread root, no channel type and no mention list,
// so DMs, private channels and thread attribution are all unreachable
// through them. The engine holds one websocket per Mattermost-enabled seat
// instead — which is also why there is no webhook secret here, and why a
// seat needs only ONE credential: the bot's token drives the websocket, the
// REST calls and the MCP tool server alike.
type Mattermost struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// URL is the instance base URL. Required when enabled: the websocket
	// endpoint, the REST calls and provisioning all derive from it.
	URL string `yaml:"url,omitempty" json:"url,omitempty" desc:"Instance base URL, e.g. https://chat.example.com."`

	// Team is the team slug agents belong to. Required when enabled —
	// channels are team-scoped, so the provisioner cannot place a bot
	// without it.
	Team string `yaml:"team,omitempty" json:"team,omitempty" desc:"Team slug the agent bots belong to."`

	// TypingStatus defaults OFF here, unlike Slack.
	//
	// Mattermost's indicator has a fixed vocabulary ("is typing…"), so it
	// conveys only BUSY where Slack's carries the phase, and it must be
	// re-asserted every few seconds against Slack's 45 — a multi-minute
	// turn costs one to two orders of magnitude more requests for strictly
	// less information. There is deliberately no status_phrases analogue:
	// no text this backend accepts would ever be rendered.
	TypingStatus WorkingStatus `yaml:"typing_status,omitempty" json:"typing_status,omitempty" js:"enum=addressed|always|off" desc:"When to show the typing indicator (default off)."`

	// Provisioning is read ONLY by the provisioning CLI. The engine never
	// looks at it; it is here so a provisioning-ready config validates.
	Provisioning *MattermostProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"Inputs for the provisioning CLI; ignored by the engine."`
}

// Status is the indicator mode, applying the off default.
func (m *Mattermost) Status() WorkingStatus {
	if m.TypingStatus == "" {
		return StatusOff
	}
	return m.TypingStatus
}

// APIBase is the REST base derived from URL.
func (m *Mattermost) APIBase() string { return strings.TrimRight(m.URL, "/") + "/api/v4" }

// MattermostProvisioning is the provisioning CLI's inputs.
type MattermostProvisioning struct {
	// UsernamePrefix is prepended to each agent handle to form the bot's
	// username — set it when the server is shared with humans and a handle
	// could collide with a person's.
	UsernamePrefix string `yaml:"username_prefix,omitempty" json:"username_prefix,omitempty" desc:"Prefix on each bot username, e.g. agent-."`

	// Channels are channel NAMES (the URL slug) every bot joins, on top of
	// whatever each seat names. A bot only receives messages from channels
	// it is a member of.
	Channels []string `yaml:"channels,omitempty" json:"channels,omitempty" desc:"Channels every bot joins."`

	// DisplayNameSuffix marks agents apart from colleagues at a glance.
	DisplayNameSuffix string `yaml:"display_name_suffix,omitempty" json:"display_name_suffix,omitempty" desc:"Suffix on each bot display name, e.g. \" (AI)\"."`
}

func (m *Mattermost) validate(path string) error {
	var p problems
	if m.TypingStatus != "" && !oneOf(m.TypingStatus, WorkingStatuses) {
		p.add(at(path, "typing_status"), ErrUnknownValue, "%q (want %s)",
			m.TypingStatus, names(WorkingStatuses))
	}
	if !m.Enabled {
		return p.err()
	}
	if strings.TrimSpace(m.URL) == "" {
		p.add(at(path, "url"), ErrMissing, "required when mattermost is enabled")
	} else if !hasHTTPScheme(m.URL) {
		// A schemeless URL produces no useful error anywhere: the HTTP
		// client rejects the base at the first request and the websocket
		// dialer rejects a URI with no ws scheme, both long after
		// validation passed. An unresolved ${VAR} is let through — the
		// reference resolves later, and rejecting it here would forbid
		// configuring the URL from the environment.
		p.add(at(path, "url"), ErrUnknownValue,
			"%q must start with http:// or https:// — it is the instance URL "+
				"browsers use", m.URL)
	}
	if strings.TrimSpace(m.Team) == "" {
		p.add(at(path, "team"), ErrMissing,
			"required when mattermost is enabled — channels are team-scoped")
	}
	return p.err()
}

// hasHTTPScheme reports a URL the clients can actually use, treating a
// value that still carries a ${VAR} as unknown rather than wrong.
func hasHTTPScheme(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") ||
		envref.Has(url)
}

// GitHub is the org-level GitHub block — the webhook side only.
//
// The tool server is a shared: false http MCP entry; each agent supplies
// its token there as an Authorization header.
type GitHub struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn inbound GitHub webhooks on."`

	// WebhookSecret verifies inbound deliveries. Required when enabled: a
	// route with nothing to verify with cannot tell a real delivery from
	// anyone's POST.
	WebhookSecret string `secret:"true" yaml:"webhook_secret,omitempty" json:"webhook_secret,omitempty" desc:"HMAC secret for inbound deliveries."`
}

func (g *GitHub) validate(path string) error {
	if g.Enabled && strings.TrimSpace(g.WebhookSecret) == "" {
		return fault(at(path, "webhook_secret"), ErrMissing,
			"required when github is enabled — without it the route has "+
				"nothing to verify a delivery with")
	}
	return nil
}

// GitLabAccessLevel is a service account's membership level.
type GitLabAccessLevel string

// The membership levels the provisioner grants.
const (
	GitLabDeveloper  GitLabAccessLevel = "developer"
	GitLabMaintainer GitLabAccessLevel = "maintainer"
)

// GitLabAccessLevels is the closed set.
var GitLabAccessLevels = []GitLabAccessLevel{GitLabDeveloper, GitLabMaintainer}

// GroupWebhookMode is how the provisioner registers hooks.
type GroupWebhookMode string

// The group-webhook modes.
const (
	// GroupWebhookAuto uses one group hook where the instance accepts it
	// and falls back to per-project hooks where it does not.
	GroupWebhookAuto GroupWebhookMode = "auto"
	// GroupWebhookRequire demands a group hook and fails without one.
	GroupWebhookRequire GroupWebhookMode = "true"
	// GroupWebhookNever always registers per-project hooks.
	GroupWebhookNever GroupWebhookMode = "false"
)

// GroupWebhookModes is the closed set.
var GroupWebhookModes = []GroupWebhookMode{GroupWebhookAuto, GroupWebhookRequire, GroupWebhookNever}

// GitLab is the org-level GitLab block.
//
// Symmetric with GitHub with two differences: url is REQUIRED (webhook
// links, boot-time identity resolution and provisioning all need the
// instance address), and inbound webhooks are verified by the signing
// token — a Standard-Webhooks HMAC over id.timestamp.body — so
// signing_secret is required when enabled. The weaker plain-token scheme is
// deliberately unsupported.
type GitLab struct {
	Enabled bool   `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`
	URL     string `yaml:"url,omitempty" json:"url,omitempty" desc:"Instance base URL, e.g. https://gitlab.com."`

	// SigningSecret verifies inbound webhooks.
	SigningSecret string `secret:"true" yaml:"signing_secret,omitempty" json:"signing_secret,omitempty" desc:"Standard-Webhooks signing token; required when enabled."`

	// Token is an optional READ credential for participants-based routing.
	//
	// Webhook payloads carry assignees and reviewers but not the
	// participants list, so mirroring GitLab's own notification semantics
	// — everyone in a thread hears its activity — needs one REST call per
	// comment or state change. When empty, routing degrades to the
	// payload-derived targets; directed events are unaffected.
	Token string `secret:"true" yaml:"token,omitempty" json:"token,omitempty" desc:"Read PAT for participants-based routing; empty degrades routing."`

	Provisioning *GitLabProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"Inputs for the provisioning CLI; ignored by the engine."`
}

// APIBase is the REST base derived from URL.
func (g *GitLab) APIBase() string { return strings.TrimRight(g.URL, "/") + "/api/v4" }

// GitLabProvisioning is the provisioning CLI's inputs: one service account
// per agent seat, memberships, per-agent tokens minted into the config's
// own ${VAR} references, and project webhooks.
type GitLabProvisioning struct {
	// Group is the top-level group the service accounts join.
	Group string `yaml:"group,omitempty" json:"group,omitempty" desc:"Top-level group the service accounts join."`

	AccessLevel  GitLabAccessLevel            `yaml:"access_level,omitempty" json:"access_level,omitempty" js:"enum=developer|maintainer" desc:"Default membership level."`
	AccessLevels map[string]GitLabAccessLevel `yaml:"access_levels,omitempty" json:"access_levels,omitempty" desc:"Per-handle membership overrides."`

	UsernamePrefix string `yaml:"username_prefix,omitempty" json:"username_prefix,omitempty" desc:"Prefix on each service-account username."`

	// Projects are extra projects to add each account to and register
	// webhooks on, beyond the group itself.
	Projects []string `yaml:"projects,omitempty" json:"projects,omitempty" desc:"Extra projects to join and hook."`

	GroupWebhook GroupWebhookMode `yaml:"group_webhook,omitempty" json:"group_webhook,omitempty" js:"enum=auto|true|false" desc:"auto (one group hook if the plan allows), true, or false."`

	// TokenScopes are minted on each service-account token.
	TokenScopes []string `yaml:"token_scopes,omitempty" json:"token_scopes,omitempty" desc:"Scopes minted on each service-account token."`
}

func (g *GitLab) validate(path string) error {
	var p problems
	if g.Enabled {
		if strings.TrimSpace(g.URL) == "" {
			p.add(at(path, "url"), ErrMissing, "required when gitlab is enabled")
		} else if !hasHTTPScheme(g.URL) {
			p.add(at(path, "url"), ErrUnknownValue,
				"%q must start with http:// or https://", g.URL)
		}
		if strings.TrimSpace(g.SigningSecret) == "" {
			p.add(at(path, "signing_secret"), ErrMissing,
				"required when gitlab is enabled — it is the only supported "+
					"webhook verification mode")
		}
	}
	if g.Provisioning == nil {
		return p.err()
	}
	pv := g.Provisioning
	pp := at(path, "provisioning")
	if pv.AccessLevel != "" && !oneOf(pv.AccessLevel, GitLabAccessLevels) {
		p.add(at(pp, "access_level"), ErrUnknownValue, "%q (want %s)",
			pv.AccessLevel, names(GitLabAccessLevels))
	}
	for _, handle := range sortedKeys(pv.AccessLevels) {
		if !oneOf(pv.AccessLevels[handle], GitLabAccessLevels) {
			p.add(at(at(pp, "access_levels"), handle), ErrUnknownValue, "%q (want %s)",
				pv.AccessLevels[handle], names(GitLabAccessLevels))
		}
	}
	if pv.GroupWebhook != "" && !oneOf(pv.GroupWebhook, GroupWebhookModes) {
		p.add(at(pp, "group_webhook"), ErrUnknownValue, "%q (want %s)",
			pv.GroupWebhook, names(GroupWebhookModes))
	}
	return p.err()
}

// PlaneRole is a service account's workspace role.
type PlaneRole string

// The workspace roles.
const (
	PlaneAdmin  PlaneRole = "admin"
	PlaneMember PlaneRole = "member"
	PlaneGuest  PlaneRole = "guest"
)

// PlaneRoles is the closed set.
var PlaneRoles = []PlaneRole{PlaneAdmin, PlaneMember, PlaneGuest}

// Plane is the org-level Plane block — webhook and engine-read side.
//
// Symmetric with GitLab, joined by workspace (every resource path the
// integration touches is workspace-scoped) and webhook_secret (the only
// verification mode Plane offers — generated BY Plane at webhook creation
// and captured by the provisioner, which is why it must stay a ${VAR}
// reference in an authored config).
type Plane struct {
	Enabled   bool   `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`
	URL       string `yaml:"url,omitempty" json:"url,omitempty" desc:"Instance base URL."`
	Workspace string `yaml:"workspace,omitempty" json:"workspace,omitempty" desc:"Workspace slug; every resource path is workspace-scoped."`

	// WebhookSecret is the HMAC key. Required when enabled.
	WebhookSecret string `secret:"true" yaml:"webhook_secret,omitempty" json:"webhook_secret,omitempty" desc:"HMAC key; generated by Plane and captured by the provisioner."`

	// Token is the engine's read credential — subscriber lookups, member
	// and project resolution, webhook enrichment. When empty, routing
	// degrades to payload-derived targets and the project cache can only
	// learn from payloads.
	Token string `secret:"true" yaml:"token,omitempty" json:"token,omitempty" desc:"Engine read credential; empty degrades routing."`

	Provisioning *PlaneProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"Inputs for the provisioning CLI; ignored by the engine."`
}

// APIBase is the REST base derived from URL.
func (p *Plane) APIBase() string { return strings.TrimRight(p.URL, "/") + "/api/v1" }

// PlaneProvisioning is the provisioning CLI's inputs.
type PlaneProvisioning struct {
	Role  PlaneRole            `yaml:"role,omitempty" json:"role,omitempty" js:"enum=admin|member|guest" desc:"Default workspace role for agent accounts."`
	Roles map[string]PlaneRole `yaml:"roles,omitempty" json:"roles,omitempty" desc:"Per-handle workspace-role overrides."`

	UsernamePrefix string   `yaml:"username_prefix,omitempty" json:"username_prefix,omitempty" desc:"Prefix on each service-account username."`
	Projects       []string `yaml:"projects,omitempty" json:"projects,omitempty" desc:"Project identifiers the accounts join."`

	// TokenExpiryDays is minted on each service-account token. 0 means the
	// token NEVER expires. A negative value is refused rather than read as
	// 0 — it would silently mean the inverse of a shorter-expiry intent.
	TokenExpiryDays int `yaml:"token_expiry_days,omitempty" json:"token_expiry_days,omitempty" js:"min=0" desc:"Expiry on each minted token; 0 = never expires."`
}

func (p *Plane) validate(path string) error {
	var probs problems
	if p.Enabled {
		if strings.TrimSpace(p.URL) == "" {
			probs.add(at(path, "url"), ErrMissing, "required when plane is enabled")
		} else if !hasHTTPScheme(p.URL) {
			probs.add(at(path, "url"), ErrUnknownValue,
				"%q must start with http:// or https://", p.URL)
		}
		if strings.TrimSpace(p.Workspace) == "" {
			probs.add(at(path, "workspace"), ErrMissing,
				"required when plane is enabled — every resource path is workspace-scoped")
		}
		if strings.TrimSpace(p.WebhookSecret) == "" {
			probs.add(at(path, "webhook_secret"), ErrMissing,
				"required when plane is enabled — it is the only verification mode Plane offers")
		}
	}
	if p.Provisioning == nil {
		return probs.err()
	}
	pv := p.Provisioning
	pp := at(path, "provisioning")
	if pv.Role != "" && !oneOf(pv.Role, PlaneRoles) {
		probs.add(at(pp, "role"), ErrUnknownValue, "%q (want %s)", pv.Role, names(PlaneRoles))
	}
	for _, handle := range sortedKeys(pv.Roles) {
		if !oneOf(pv.Roles[handle], PlaneRoles) {
			probs.add(at(at(pp, "roles"), handle), ErrUnknownValue, "%q (want %s)",
				pv.Roles[handle], names(PlaneRoles))
		}
	}
	if pv.TokenExpiryDays < 0 {
		probs.add(at(pp, "token_expiry_days"), ErrOutOfRange,
			"must not be negative: it would silently mean 0, which is the "+
				"inverse of a shorter expiry. Use 0 for a token that never expires")
	}
	return probs.err()
}

// mattermostUsername is what the Mattermost server itself accepts:
// lowercase, alphanumeric plus '.', '-' and '_'. Checked at config load so
// a bad username fails on the line that authored it rather than midway
// through a provisioning run that has already created half the fleet.
var mattermostUsername = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
