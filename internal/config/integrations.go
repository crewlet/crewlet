package config

import (
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/whsec"
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
	Jira       *Jira       `yaml:"jira,omitempty" json:"jira,omitempty" desc:"Jira instance or Cloud site, org read account and webhook secret. Absent = disabled."`
	Confluence *Confluence `yaml:"confluence,omitempty" json:"confluence,omitempty" js:"unimplemented" desc:"NOT IMPLEMENTED in this build: no searcher and no parser. The knowledge base this build serves is integrations.plane."`
	Slack      *Slack      `yaml:"slack,omitempty" json:"slack,omitempty" js:"unimplemented" desc:"NOT IMPLEMENTED in this build: no parser routes a Slack delivery. The chat surface this build serves is integrations.mattermost."`
	Mattermost *Mattermost `yaml:"mattermost,omitempty" json:"mattermost,omitempty" desc:"Mattermost instance and team. Absent = disabled."`
	GitHub     *GitHub     `yaml:"github,omitempty" json:"github,omitempty" js:"unimplemented" desc:"NOT IMPLEMENTED in this build: no parser routes a GitHub delivery. The code host this build serves is integrations.gitlab."`
	GitLab     *GitLab     `yaml:"gitlab,omitempty" json:"gitlab,omitempty" desc:"GitLab instance, webhook signing and provisioning. Absent = disabled."`
	Plane      *Plane      `yaml:"plane,omitempty" json:"plane,omitempty" desc:"Plane instance, workspace and webhook secret. Absent = disabled."`

	// ForgeAppID verifies the Forge app's invocation tokens: the JWT's
	// audience claim must match it. Required when the Forge app is used —
	// the endpoint rejects every request without it.
	ForgeAppID string `yaml:"forge_app_id,omitempty" json:"forge_app_id,omitempty" desc:"Forge app id, verified against a relayed Cloud event's invocation token. Required for Jira or Confluence Cloud."`
}

func (i *Integrations) validate(path string) error {
	var p problems

	// REFUSED FIRST, AND INSTEAD. A block this build cannot serve gets one
	// error saying so — not that plus "url is required", which would send
	// an operator off to fix a field on an integration they cannot use
	// either way. See [unservedIntegrations].
	if refused := i.refuseUnserved(&p, path); refused {
		return p.err()
	}
	if i.Jira != nil {
		p.wrap(i.Jira.validate(at(path, "jira")))
	}
	if i.Mattermost != nil {
		p.wrap(i.Mattermost.validate(at(path, "mattermost")))
	}
	if i.GitLab != nil {
		p.wrap(i.GitLab.validate(at(path, "gitlab")))
	}
	if i.Plane != nil {
		p.wrap(i.Plane.validate(at(path, "plane")))
	}
	return p.err()
}

// The integrations this build VALIDATES and does not SERVE, with what fills
// the same role instead.
//
// The engine wires Mattermost for chat, Plane and Jira for the tracker, and
// GitLab for the code host (decisions/701, decisions/703). The
// remaining three have config models, webhook routes and generated schema,
// and no parser, no transport and no searcher behind them:
// startNotifications builds its parser and prompt lists from the served
// blocks alone.
//
// So a company naming one got a block that validated, appeared in the
// dashboard, and did nothing. Silently, which is the part that matters —
// integrations.confluence is how an operator says where the company's
// knowledge lives, and the answer was an empty "## Relevant knowledge" on
// every Plan phase.
//
// Refusing is what providers.sandbox.type: e2b already does, one layer down,
// for the same reason. Each entry is deleted by the change that ships that
// vendor's parser, transport and prompt — a config field ships with the code
// that reads it.
var unservedIntegrations = []struct {
	field string
	// active reports whether the block is switched on, in that block's own
	// terms: two of the three are on by their mere presence, and GitHub
	// carries an Enabled flag.
	active func(*Integrations) bool
	// instead names the role and what serves it, because an error a person
	// reads has to say what to do about it.
	instead string
}{
	{"confluence", func(i *Integrations) bool { return i.Confluence != nil },
		"the knowledge base this build serves is Plane (integrations.plane)"},
	{"slack", func(i *Integrations) bool { return i.Slack != nil },
		"the chat surface this build serves is Mattermost (integrations.mattermost)"},
	{"github", func(i *Integrations) bool { return i.GitHub != nil && i.GitHub.Enabled },
		"the code host this build serves is GitLab (integrations.gitlab)"},
}

// refuseUnserved rejects any integration block nothing behind the config
// reads, and reports whether it found one. See [unservedIntegrations].
func (i *Integrations) refuseUnserved(p *problems, path string) bool {
	var unserved bool
	for _, entry := range unservedIntegrations {
		if entry.active(i) {
			unserved = true
			p.add(at(path, entry.field), ErrUnimplemented,
				"no parser or transport is wired for it, so a delivery would "+
					"be verified, stored, and then reach nobody — %s",
				entry.instead)
		}
	}
	return unserved
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

	// SiteURL is the human-readable base for shareable links, needed only
	// with a cloud id — the API gateway URL is not something to hand a
	// person, and a link built from it looks right and opens nothing.
	// With a direct URL this defaults to it.
	SiteURL string `yaml:"site_url,omitempty" json:"site_url,omitempty" desc:"Human-readable base for shareable links; needed with cloud_id."`

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

// ShareableBaseURL is the base for links handed to a person.
//
// With a cloud id and no site url there is NONE, and the empty answer is the
// honest one: the API gateway is not a place a browser can go, so a link
// built from it looks right and opens nothing. A prompt omits the link
// rather than printing a dead one.
func (j *Jira) ShareableBaseURL() string {
	if j.SiteURL != "" {
		return j.SiteURL
	}
	return j.URL
}

// validate checks the org account.
//
// The one refusal that matters is the ADDRESS. url and cloud_id are two ways
// to say where the instance is, and giving both is an ambiguity the engine
// would resolve silently — BaseURL prefers the gateway — so a company that
// moved from Data Center to Cloud and left the old url behind would keep
// looking correct while every read went to the new place and every link to
// the old one.
func (j *Jira) validate(path string) error {
	var probs problems
	url, cloud := strings.TrimSpace(j.URL), strings.TrimSpace(j.CloudID)
	switch {
	case url == "" && cloud == "":
		probs.add(path, ErrMissing,
			"give url (a Data Center instance or a Cloud site) or cloud_id "+
				"(an Atlassian Cloud id) — without one there is nowhere to read "+
				"an issue's watchers from")
	case url != "" && cloud != "":
		probs.add(path, ErrConflict,
			"url (%q) and cloud_id (%q) are two ways to name one instance; "+
				"give one. The engine reads through the cloud gateway when both "+
				"are set, so the url would be used for links only", j.URL, j.CloudID)
	case url != "" && !hasHTTPScheme(j.URL):
		probs.add(at(path, "url"), ErrUnknownValue,
			"%q must start with http:// or https://", j.URL)
	}
	if site := strings.TrimSpace(j.SiteURL); site != "" && !hasHTTPScheme(j.SiteURL) {
		probs.add(at(path, "site_url"), ErrUnknownValue,
			"%q must start with http:// or https://", j.SiteURL)
	}
	if strings.TrimSpace(j.Token) == "" {
		probs.add(at(path, "token"), ErrMissing,
			"required — the org account is what reads an issue's watchers, "+
				"which is the one routing input a Jira webhook never carries")
	}
	if strings.TrimSpace(j.WebhookSecret) == "" && cloud == "" {
		// CLOUD IS EXEMPT: its events arrive through the Forge app on
		// /webhooks/forge, verified by the app's invocation token, and
		// there is no HMAC secret in that path at all. Requiring one
		// would refuse the correct Cloud config.
		probs.add(at(path, "webhook_secret"), ErrMissing,
			"required for a Data Center instance — the /webhooks/jira route "+
				"has nothing to verify a delivery with otherwise, and answers "+
				"503 to every one")
	}
	return probs.err()
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

// validSigningSecret reports whether a value is one GitLab would accept.
//
// "Must be in whsec_<base64> format encoding a 32-byte key" — the API's own
// words. STANDARD base64: the URL-safe alphabet usually still decodes to
// something, which is a mismatch with no message rather than an error.
func validSigningSecret(secret string) bool { return whsec.Valid(secret) }

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
		secret := strings.TrimSpace(g.SigningSecret)
		_, isRef := envref.Whole(secret)
		switch {
		case secret == "":
			p.add(at(path, "signing_secret"), ErrMissing,
				"required when gitlab is enabled — it is the only supported "+
					"webhook verification mode")
		case isRef:
			// A ${VAR} is checked where it is RESOLVED, not here. Tier B
			// stores the reference verbatim, so the reference is all this
			// layer ever sees and validating its shape would reject every
			// correctly-written config.
		case !validSigningSecret(secret):
			// THE SHAPE IS A CONTRACT WITH GITLAB, and getting it wrong is
			// silent in both directions: the API rejects the hook with a
			// 400 an operator may never see, and a value that slips past
			// produces an HMAC that cannot match anything GitLab computes
			// — an endless run of signature mismatches that reads as an
			// attack, with nothing naming the encoding.
			p.add(at(path, "signing_secret"), ErrShape,
				"must be whsec_ followed by standard base64 over a 32-byte "+
					"key, which is the only shape GitLab's API accepts. "+
					"`crewlet gitlab provision` mints one, and GitLab's own "+
					"Generate signing token button produces the same shape")
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

	// SkillsProject is the engine-managed project holding tool-skill
	// pages. Its content is MACHINERY rather than knowledge, and it is
	// excluded twice over: its webhooks route to nobody (there is no
	// recipient by design, and without the exclusion every skill edit
	// falls through to lead routing as an undeliverable notification), and
	// its pages never surface in a knowledge search (a planner shown a
	// tool-skill page would follow an instruction meant for a different
	// phase of a different turn).
	//
	// Absent takes [DefaultSkillsProject]. There is no "off", and none is
	// needed: a company that publishes no tool skills has no project by
	// that identifier, so both exclusions match nothing and cost nothing.
	// The one case that needs this field is a company whose skills live
	// somewhere other than the reserved default.
	SkillsProject string `yaml:"skills_project,omitempty" json:"skills_project,omitempty" desc:"Project holding tool-skill pages; excluded from routing and knowledge search. Default TS."`

	Provisioning *PlaneProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"Inputs for the provisioning CLI; ignored by the engine."`
}

// APIBase is the REST base derived from URL.
func (p *Plane) APIBase() string { return strings.TrimRight(p.URL, "/") + "/api/v1" }

// DefaultSkillsProject is the identifier a tool-skills project has unless
// the operator names another.
//
// "TS" is the convention the publishing CLI writes into and the docs name, so
// a company that follows the guide works with nothing configured. The cost is
// that a company using TS as an ordinary work project has it silently
// excluded from knowledge search — which is why the field exists: set it to
// something else, or to "" to turn the exclusions off entirely.
const DefaultSkillsProject = "TS"

// SkillsProjectKey is the tool-skills project, normalised.
//
// UPPER, because every identifier comparison in the integration is
// case-insensitive and a config written in lower case must not silently mean
// a different project from the same word written in upper.
func (p *Plane) SkillsProjectKey() string {
	if p == nil {
		return ""
	}
	if trimmed := strings.TrimSpace(p.SkillsProject); trimmed != "" {
		return strings.ToUpper(trimmed)
	}
	return DefaultSkillsProject
}

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
