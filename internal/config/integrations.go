package config

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/secrets"
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
	Confluence *Confluence `yaml:"confluence,omitempty" json:"confluence,omitempty" desc:"Confluence instance or Cloud site, org read account and webhook secret. Absent = disabled."`
	Slack      *Slack      `yaml:"slack,omitempty" json:"slack,omitempty" desc:"Slack working-indicator settings. Each seat carries its own app under role.integrations.slack. Absent = disabled."`
	Mattermost *Mattermost `yaml:"mattermost,omitempty" json:"mattermost,omitempty" desc:"Mattermost instance and team. Absent = disabled."`
	GitHub     *GitHub     `yaml:"github,omitempty" json:"github,omitempty" desc:"GitHub.com or Enterprise Server, webhook secret and provisioning. Absent = disabled."`
	GitLab     *GitLab     `yaml:"gitlab,omitempty" json:"gitlab,omitempty" desc:"GitLab instance, webhook signing and provisioning. Absent = disabled."`
	Datadog    *Datadog    `yaml:"datadog,omitempty" json:"datadog,omitempty" desc:"Datadog monitor alerts delivered as inbound events. Absent = disabled."`
	Atlassian  *Atlassian  `yaml:"atlassian,omitempty" json:"atlassian,omitempty" desc:"Atlassian organization and its unscoped API key, used to create one service account per agent. Absent = accounts are made by hand."`

	// ForgeAppID verifies the Forge app's invocation tokens: the JWT's
	// audience claim must match it. Required when the Forge app is used —
	// the endpoint rejects every request without it.
	ForgeAppID string `yaml:"forge_app_id,omitempty" json:"forge_app_id,omitempty" desc:"Forge app id, verified against a relayed Cloud event's invocation token. Required for Jira or Confluence Cloud."`

	// PublicBaseURL is where a third-party app reaches THIS deployment: the HTTPS
	// base every webhook path is built on.
	//
	// # Why it belongs in the company document
	//
	// It was a flag on five subcommands (`-public-url`) and nowhere else,
	// which made it a fact only the person running a command knew. Two
	// things need it that are not a person running a command:
	//
	//   - The reconcile loop, to say anything at all about ingress. Every
	//     integration Result reports the hook a RUN registered, which is empty
	//     for a read-only pass by construction, so without this the loop
	//     cannot tell a company whose webhook is missing from one it was
	//     never asked to register. It therefore says nothing, which is
	//     honest and useless.
	//   - Anything that has to build a URL for a third-party app to call back on,
	//     which is what a self-service app deployment needs.
	//
	// It is NOT a secret and should be a literal rather than a ${VAR}: a
	// reference that resolves to nothing yields a hook pointing at "",
	// which a third-party app accepts and then delivers nowhere.
	//
	// Empty is meaningful and is the default: it means this deployment has
	// no address a third-party app can reach, which is the honest state of an
	// engine on a laptop. Nothing is guessed from it, because a hook
	// pointing at the wrong host is worse than no hook, and a subcommand's
	// `-public-url` still overrides it for a one-off run.
	PublicBaseURL string `yaml:"public_base_url,omitempty" json:"public_base_url,omitempty" desc:"HTTPS base a vendor reaches this deployment on, e.g. https://crewlet.example.com. Empty means no inbound address."`

	// CheckIntervalSeconds is how long a CONVERGED integration is trusted
	// before the loop reads it back, and therefore how long access somebody
	// revoked by hand at the third-party app goes unnoticed.
	//
	// # Why this is a company's choice rather than one number for everybody
	//
	// It is the only thing that finds a revoked credential at all — nothing
	// tells this engine, and every other cadence in the loop is a retry of
	// something already known to be wrong. So it is a straight trade against
	// what a converged pass COSTS at the vendor, and that cost is the
	// company's own size: a pass asks each seat's credential who it is and
	// reads the memberships and hooks per seat and per project, so it is
	// O(seats x projects) requests per surface per interval — tens for a
	// small company, a few hundred for a large one on GitLab or Mattermost.
	//
	// Ten minutes is the default because it is affordable at the large end.
	// At the small end it is simply slow, and measured as such: an operator
	// who deleted an agent's token by hand watched the card say Connected
	// for eight minutes. A five-seat company can afford one minute; a
	// two-hundred-seat one should probably lengthen it.
	//
	// # Zero is the default, not "never"
	//
	// A settled surface that is never read back is one this engine would
	// report healthy for the life of the deployment, which is the state the
	// whole subsystem exists to refuse — so there is no "off". Zero takes
	// the default and anything below [MinCheckInterval] is refused naming
	// the field, rather than silently clamped: a value typed in seconds when
	// the writer meant minutes should say so.
	CheckIntervalSeconds int `yaml:"check_interval_seconds,omitempty" json:"check_interval_seconds,omitempty" desc:"How often a converged integration is read back, in seconds. 0 takes the default of 600; the floor is 60."`
}

// MinCheckInterval is the floor under [Integrations.CheckIntervalSeconds].
//
// One minute, which is four of the loop's own ticks — below that the interval
// stops being a schedule and becomes the tick rate, and a converged company
// would spend O(seats x projects) requests a minute at every vendor for ever
// to shorten a detection window nobody is watching.
const MinCheckInterval = time.Minute

// DefaultCheckInterval is what an unset [Integrations.CheckIntervalSeconds]
// means.
//
// Restated here rather than imported from internal/integration for the reason
// [Datadog.HandleTagOrDefault] gives — config is the leaf every other package
// depends on — and asserted equal to integration.DefaultSchedule.Settled by a
// test.
const DefaultCheckInterval = 10 * time.Minute

// CheckInterval is how long a converged integration is trusted.
func (i *Integrations) CheckInterval() time.Duration {
	if i == nil || i.CheckIntervalSeconds <= 0 {
		return DefaultCheckInterval
	}
	return time.Duration(i.CheckIntervalSeconds) * time.Second
}

// WebhookBase is the base every inbound path is built on, without a trailing
// slash, or empty when this deployment has no inbound address THIS PROCESS
// CAN READ.
//
// Trimmed here rather than at each caller, because five of them would each
// have to remember: a base ending in "/" yields "…//webhooks/jira", which
// some third-party apps normalise, some reject, and some accept while signing the
// unnormalised form.
//
// # It takes a resolver, and that is the whole point of the signature
//
// `public_base_url` is a Tier B field, so a whole `${VAR}` is a legal way to
// write it and the document stores it VERBATIM like every other pointer. Read
// raw, that value is not an address: it is the seven characters `${VAR}`, and
// every caller here is building something a third-party app will HOLD — a
// registered webhook, a manifest an operator pastes, an app's baked-in
// redirect. Slack refuses such a manifest and names nothing; a webhook
// registered at `${VAR}/webhooks/gitlab` is accepted, reported healthy, and
// delivers nowhere. The same mistake was measured on the Atlassian pass,
// which sent the literal `${ATLASSIAN_ORG_ID}` to Atlassian.
//
// So there is no raw accessor to reach for by accident. A caller that cannot
// resolve has to pass nil and be handed "", which every reader here already
// treats as "no inbound address" — the honest answer for a node that cannot
// read the value, and the one that stops a literal reaching a third-party app.
//
// EMPTY RATHER THAN THE REFERENCE when it will not resolve, for the same
// reason: no manifest beats a manifest built from a value nothing can read.
func (i *Integrations) WebhookBase(resolve func(string) (string, bool)) string {
	base := strings.TrimSpace(i.PublicBaseURL)
	if name, isRef := envref.Whole(base); isRef {
		if resolve == nil {
			return ""
		}
		got, ok := resolve(name)
		if !ok {
			return ""
		}
		base = strings.TrimSpace(got)
	}
	return strings.TrimRight(base, "/")
}

func (i *Integrations) validate(path string) error {
	var p problems

	// A URL that is not one is refused HERE rather than discovered by a
	// third-party app. Every webhook path is built on this, so a value missing its
	// scheme registers a hook the third-party app reports as healthy and delivers
	// nowhere, which is the failure mode this whole field exists to close.
	if base := strings.TrimSpace(i.PublicBaseURL); base != "" && !hasHTTPScheme(base) {
		p.add(at(path, "public_base_url"), ErrUnknownValue,
			"%q must start with http:// or https://: it is the base every "+
				"webhook URL is built on, so a value without a scheme yields "+
				"an address the third-party app accepts and never reaches", i.PublicBaseURL)
	}

	// A CHECK INTERVAL BELOW THE FLOOR IS REFUSED RATHER THAN CLAMPED.
	// Silently raising it would leave a document saying one thing and a loop
	// doing another, and the likeliest way to get here is a value typed in
	// seconds by somebody who meant minutes — which a clamp hides and this
	// says out loud. Zero is not a value: it is the field being unset.
	if secs := i.CheckIntervalSeconds; secs != 0 {
		if d := time.Duration(secs) * time.Second; d < MinCheckInterval {
			p.add(at(path, "check_interval_seconds"), ErrUnknownValue,
				"%d is below the floor of %d: a converged pass costs one read "+
					"per seat and per project at every vendor, so an interval "+
					"this short spends that every minute for ever. Leave it "+
					"unset for the default of %d",
				secs, int(MinCheckInterval.Seconds()), int(DefaultCheckInterval.Seconds()))
		}
	}

	// THE ORGANIZATION HAS NO BLOCK VALIDATOR of its own: it is two fields,
	// and the only rule either carries is that neither is a phrase. A space
	// in the organization id is sent to Atlassian as the subject of every
	// admin call and refused with nothing naming the character.
	if i.Atlassian != nil {
		noSpaces(&p, at(path, "atlassian.org_id"), i.Atlassian.OrgID)
		if d := strings.TrimSpace(i.Atlassian.Deployment); d != "" &&
			!slices.Contains(AtlassianDeployments, strings.ToLower(d)) {
			p.add(at(path, "atlassian.deployment"), ErrUnknownValue,
				"%q is not an Atlassian deployment; give %q or %q",
				d, AtlassianCloud, AtlassianDataCenter)
		}
		// THE ORGANIZATION IS A CLOUD CONCEPT. admin.atlassian.com has no
		// Data Center equivalent: there is no organization, no cloud id and
		// no service account to create, so a key here would be a credential
		// nothing can spend.
		if !i.Atlassian.IsCloud() && strings.TrimSpace(i.Atlassian.OrgID) != "" {
			p.add(at(path, "atlassian.org_id"), ErrConflict,
				"a Data Center deployment has no organization: the admin APIs "+
					"this names are Cloud only, so nothing would read it")
		}
	}
	if i.Jira != nil {
		p.wrap(i.Jira.validate(at(path, "jira"), i.DiscoversAtlassianSite()))
	}
	if i.Datadog != nil {
		p.wrap(i.Datadog.validate(at(path, "datadog")))
	}
	if i.Confluence != nil {
		p.wrap(i.Confluence.validate(at(path, "confluence"), i.DiscoversAtlassianSite()))
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
	if i.Slack != nil {
		p.wrap(i.Slack.validate(at(path, "slack")))
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

	// WebhookName is the name the engine's own hook is registered under,
	// and therefore WHICH HOOK ON THIS INSTANCE IS THIS DEPLOYMENT'S.
	//
	// The reconcile converges the hook carrying this name, whatever
	// address it currently points at, which is what stops a change of
	// public base leaving a live orphan behind delivering to somewhere
	// that no longer answers — one per change, all enabled.
	//
	// So it has to differ between two deployments watching ONE instance:
	// staging and production of the same company share this document, and
	// with one name each pass would repoint the other's hook and only the
	// last one to run would receive anything. The same knob exists on
	// Datadog for the same reason.
	WebhookName string `yaml:"webhook_name,omitempty" json:"webhook_name,omitempty" desc:"Name the engine's own Jira webhook is registered under; give two deployments watching one instance two names (default crewlet)."`
}

// WebhookNameOrDefault is the name the engine's hook carries at Jira.
//
// Restated here rather than imported from internal/jira for the reason
// [Datadog.HandleTagOrDefault] gives — config is the leaf the vendor packages
// depend on — and asserted equal by a test.
func (j *Jira) WebhookNameOrDefault() string {
	if name := strings.TrimSpace(j.WebhookName); name != "" {
		return name
	}
	return "crewlet"
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
func (j *Jira) validate(path string, discovers bool) error {
	var probs problems
	noSpaces(&probs, at(path, "url"), j.URL)
	noSpaces(&probs, at(path, "cloud_id"), j.CloudID)
	noSpaces(&probs, at(path, "site_url"), j.SiteURL)
	noSpaces(&probs, at(path, "email"), j.Email)
	url, cloud := strings.TrimSpace(j.URL), strings.TrimSpace(j.CloudID)
	switch {
	case url == "" && cloud == "" && discovers:
		// NOTHING OUTSTANDING. The Atlassian organization supplies the site
		// and its cloud id on the first pass, which is why the form does not
		// ask — see [Integrations.DiscoversAtlassianSite]. Until it has, the
		// surface reports itself unconverged through the reconcile status,
		// which is where a state that fixes itself belongs. Refusing the
		// document instead blocked the write that starts the pass.
	case url == "" && cloud == "":
		probs.add(path, ErrMissing,
			"give url (a Data Center instance or a Cloud site) or cloud_id "+
				"(an Atlassian Cloud id): without one there is nowhere to read "+
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
			"required: the org account is what reads an issue's watchers, "+
				"which is the one routing input a Jira webhook never carries")
	}
	// AN UNIDENTIFIED INSTANCE IS NOT A DATA CENTER ONE, which is what the
	// missing `url != ""` used to make it. With neither url nor cloud_id the
	// deployment is unknown — the problem above says exactly that — and this
	// fired anyway, telling an operator who had just chosen Atlassian Cloud in
	// the connect dialog that a signing secret was "required for a Data Center
	// instance". Two problems where there is one, and the second one asking
	// for a field Cloud is explicitly exempt from, on a form that offers
	// neither. Naming the deployment is the only thing outstanding until it is
	// named.
	if strings.TrimSpace(j.WebhookSecret) == "" && url != "" && cloud == "" &&
		!IsAtlassianCloud(url) {
		// CLOUD IS EXEMPT, and stays exempt now that it can register an
		// admin webhook of its own. The reason changed rather than
		// disappearing: a Cloud company may take EITHER route, and one
		// on the Forge relay has no HMAC secret in its path at all, so
		// requiring one here would refuse a correct config.
		//
		// What stops that becoming a silent gap is where the refusal
		// moved to: the reconcile will not register a hook without a
		// secret to sign it with, so a Cloud company that chose webhooks
		// is told at the moment it tries to register one, by the command
		// that was going to do it.
		probs.add(at(path, "webhook_secret"), ErrMissing,
			"required for a Data Center instance: the /webhooks/jira route "+
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

	// WebhookToken is the shared token a Confluence CLOUD webhook carries in
	// its delivery URL, compared constant-time by /webhooks/confluence/{event}.
	//
	// A second field rather than a second use of WebhookSecret, because the
	// two are different kinds of thing and a config that conflated them
	// would let an operator believe a Cloud delivery was signed. Data
	// Center signs the body with WebhookSecret; Cloud signs nothing, drops
	// userinfo, and honours no registration field, so the only credential
	// it can carry is one written into the URL it was registered with.
	// That is exactly Datadog's ceiling and it gets Datadog's treatment: a
	// token doing a signing key's job with none of the guarantees, rotated
	// like one, never logged.
	WebhookToken string `secret:"true" yaml:"webhook_token,omitempty" json:"webhook_token,omitempty" desc:"Shared token for Confluence Cloud webhooks, carried in the registered URL; minted by crewlet confluence provision."`

	// SkillsSpace holds the tool-skill pages. Excluded from routing and
	// from knowledge search alike: those pages are machinery, and a seat
	// told to read one would follow an instruction written for a
	// different phase of a different turn.
	//
	// A POINTER because all three states are real settings and the zero
	// value cannot say which: absent takes [DefaultSkillsSpace], a named
	// key takes that key, and an explicit `skills_space: ""` turns the
	// whole tool-skill mechanism OFF — no sync, no routing exclusion, no
	// search exclusion. See [Confluence.SkillsSpaceKey].
	SkillsSpace *string `yaml:"skills_space,omitempty" json:"skills_space,omitempty" desc:"Space holding tool-skill pages; excluded from routing and knowledge search. Default TS; empty string disables tool skills entirely."`
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

// DefaultSkillsSpace is where tool-skill pages live when the config names no
// space.
//
// "TS" is the convention the publishing CLI writes into and the docs name, so
// a company that follows the guide works with nothing configured.
const DefaultSkillsSpace = "TS"

// SkillsSpaceKey is the tool-skills space, normalised — or "" for a company
// that has turned tool skills off.
//
// UPPER, because every space comparison in the integration is
// case-insensitive and a config written in lower case must not silently mean
// a different space from the same word written in upper.
//
// # The empty string is an ANSWER, not an absence
//
// A company whose ordinary work space happens to be `TS` would otherwise have
// it silently dropped from every knowledge search and every routing decision,
// with no way to say so — the default reserving a real space name is the cost
// of having a default at all. `skills_space: ""` is how an operator says "no
// space is reserved": every consumer already reads "" as "no exclusion and no
// sync", so the switch is this accessor and nothing else.
func (c *Confluence) SkillsSpaceKey() string {
	if c == nil || c.SkillsSpace == nil {
		return DefaultSkillsSpaceFor(c)
	}
	return strings.ToUpper(strings.TrimSpace(*c.SkillsSpace))
}

// DefaultSkillsSpaceFor is the key an unset field takes: none at all when
// there is no Confluence config to hold skills, the reserved default when
// there is.
func DefaultSkillsSpaceFor(c *Confluence) string {
	if c == nil {
		return ""
	}
	return DefaultSkillsSpace
}

// validate checks the knowledge account.
//
// The same address rule as the tracker's, for the same reason: url and
// cloud_id are two ways to say where the instance is, and resolving the
// ambiguity silently would let a company that moved from Data Center to
// Cloud keep looking correct while every read went to one place and every
// link to the other.
func (c *Confluence) validate(path string, discovers bool) error {
	var probs problems
	noSpaces(&probs, at(path, "url"), c.URL)
	noSpaces(&probs, at(path, "cloud_id"), c.CloudID)
	noSpaces(&probs, at(path, "site_url"), c.SiteURL)
	noSpaces(&probs, at(path, "email"), c.Email)
	url, cloud := strings.TrimSpace(c.URL), strings.TrimSpace(c.CloudID)
	switch {
	case url == "" && cloud == "" && discovers:
		// NOTHING OUTSTANDING — see the same arm on [Jira.validate].
	case url == "" && cloud == "":
		probs.add(path, ErrMissing,
			"give url (a Data Center instance or a Cloud site) or cloud_id "+
				"(an Atlassian Cloud id): without one there is nowhere to "+
				"search")
	case url != "" && cloud != "":
		probs.add(path, ErrConflict,
			"url (%q) and cloud_id (%q) are two ways to name one instance; "+
				"give one. The engine reads through the cloud gateway when both "+
				"are set, so the url would be used for links only", c.URL, c.CloudID)
	case url != "" && !hasHTTPScheme(c.URL):
		probs.add(at(path, "url"), ErrUnknownValue,
			"%q must start with http:// or https://", c.URL)
	}
	if site := strings.TrimSpace(c.SiteURL); site != "" && !hasHTTPScheme(c.SiteURL) {
		probs.add(at(path, "site_url"), ErrUnknownValue,
			"%q must start with http:// or https://", c.SiteURL)
	}
	if strings.TrimSpace(c.Token) == "" {
		probs.add(at(path, "token"), ErrMissing,
			"required: it is the account a seat with no Confluence "+
				"credential of its own searches under, and the one the "+
				"tool-skill walk reads with")
	}
	sharedToken(&probs, at(path, "webhook_token"), c.WebhookToken)
	// AN UNIDENTIFIED INSTANCE IS NOT A DATA CENTER ONE — see the same guard
	// on [Jira.validate], which had the same bug for the same reason.
	if strings.TrimSpace(c.WebhookSecret) == "" && url != "" && cloud == "" &&
		!IsAtlassianCloud(url) {
		// CLOUD IS EXEMPT, on either of its routes. The Forge relay is
		// verified by the app's invocation token and the token-bearing
		// hook by webhook_token, and neither carries an HMAC, so requiring
		// a signing secret would refuse every correct Cloud config. The
		// refusal lives where it can be honest instead: the provisioner
		// will not register a Cloud hook without a token to put in it.
		probs.add(at(path, "webhook_secret"), ErrMissing,
			"required for a Data Center instance: the /webhooks/confluence "+
				"route has nothing to verify a delivery with otherwise, and "+
				"answers 503 to every one")
	}
	return probs.err()
}

// sharedToken refuses a token too weak to be the whole authentication for a
// route.
//
// TWO ROUTES HAVE NOTHING ELSE. Datadog's provider attaches headers with fixed
// values and Confluence Cloud's attaches nothing at all, so neither delivery
// can be signed and the token is the entire check — see
// [secrets.CheckSharedToken] for the rule and why the number is what it is.
// Every other inbound route verifies an HMAC over the body, where the secret's
// shape is what [whsec] refuses.
//
// A ${VAR} IS UNKNOWN, NOT WRONG, exactly as everywhere else in this file:
// Tier B holds a pointer verbatim and resolves it where the transport is
// built, so the length of what it resolves to is not knowable here. The
// webhook edge makes the same check on the resolved value, which is what
// stops a reference being the way around this.
func sharedToken(p *problems, path, value string) {
	token := strings.TrimSpace(value)
	if token == "" || envref.Has(token) {
		return
	}
	if err := secrets.CheckSharedToken(token); err != nil {
		p.add(path, ErrUnknownValue, "%s", err.Error())
	}
}

// WorkingStatus is when a seat raises the "is thinking…" indicator while it
// reasons about a chat message.
type WorkingStatus string

const (
	// StatusAddressed shows it only when a human is plausibly waiting on
	// this seat: a DM, a direct mention, or a thread it already follows.
	StatusAddressed WorkingStatus = "addressed"
	// StatusAlways shows it on every chat-triggered turn, including
	// passive channel messages and broadcasts. It is the DEFAULT: a turn
	// takes minutes, and a reader who sees nothing cannot tell an agent
	// working from an agent that is dead.
	StatusAlways WorkingStatus = "always"
)

// WorkingStatuses is the closed set.
//
// THERE IS NO "off". It was here, and what it bought was a company whose
// agents think in silence for minutes at a time, which is the state this
// whole feature exists to remove. An operator who finds the indicator noisy
// wants `addressed`, which is the same judgement made per message rather
// than once for the deployment.
var WorkingStatuses = []WorkingStatus{StatusAlways, StatusAddressed}

// validate refuses a status outside the set, at the path it was written.
//
// ONE RULE for both chat blocks. Mattermost's had it inline and Slack's had
// no validator at all, so `typing_status: alwyas` validated clean and then
// degraded silently to the default: the indicator appears on some turns and
// not others, and the operator concludes the feature is flaky rather than
// that they typed it wrong. Empty is valid — it is how a block takes the
// default.
func (w WorkingStatus) validate(path string) error {
	if w == "" || slices.Contains(WorkingStatuses, w) {
		return nil
	}
	var p problems
	p.add(path, ErrUnknownValue, "%q (want %s)", w, names(WorkingStatuses))
	return p.err()
}

// Slack is the org-level Slack block.
//
// Every CREDENTIAL is per-agent (role.integrations.slack); this block is
// the transport-enable marker plus the genuinely org-wide behaviour.
// Declaring it at all — even as `slack: {}` — turns the transport and
// per-agent webhook routing on.
type Slack struct {
	// TypingStatus defaults to always: a Slack turn takes minutes, and its
	// indicator renders TEXT, so the reader waiting on it learns which
	// phase is running rather than merely that something is.
	TypingStatus WorkingStatus `yaml:"typing_status,omitempty" json:"typing_status,omitempty" js:"enum=always|addressed" desc:"When to show the working indicator (default always)."`

	// StatusPhrases replaces the words the indicator shows.
	StatusPhrases StatusPhrases `yaml:"status_phrases,omitempty" json:"status_phrases,omitzero"`
}

// validate refuses a typing_status outside the closed set.
//
// The block had NO validator, which is why an unknown value reached
// [Slack.Status] and silently became the default. StatusPhrases is
// deliberately not validated: its values are free text an operator writes to
// complete the sentence "<seat> is …", so there is no set to check against.
func (s *Slack) validate(path string) error {
	var p problems
	p.wrap(s.TypingStatus.validate(at(path, "typing_status")))
	return p.err()
}

// Status is the indicator mode, applying the default.
func (s *Slack) Status() WorkingStatus {
	if s.TypingStatus == "" {
		return StatusAlways
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
	Execute    []string `yaml:"execute,omitempty" json:"execute,omitempty" desc:"Lines shown while the agent works."`
	Review     []string `yaml:"review,omitempty" json:"review,omitempty" desc:"Lines shown while the reviewer judges the work."`
	// Default covers any phase added later that has no pool of its own.
	Default []string `yaml:"default,omitempty" json:"default,omitempty" desc:"Lines for any phase with no pool of its own."`
}

// IsZero lets an unset block drop out of a round trip.
func (s StatusPhrases) IsZero() bool {
	return len(s.Onboarding) == 0 && len(s.Execute) == 0 &&
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

	// TypingStatus defaults to always, the same as Slack — but this is the
	// backend where that default costs most, so an operator has a real
	// reason to choose `addressed`.
	//
	// Mattermost's indicator has a fixed vocabulary ("is typing…"), so it
	// conveys only BUSY where Slack's carries the phase, and it must be
	// re-asserted every few seconds against Slack's 45 — a multi-minute
	// turn costs one to two orders of magnitude more requests for strictly
	// less information. There is deliberately no status_phrases analogue:
	// no text this backend accepts would ever be rendered.
	TypingStatus WorkingStatus `yaml:"typing_status,omitempty" json:"typing_status,omitempty" js:"enum=always|addressed" desc:"When to show the typing indicator (default always); it costs a request every few seconds per thinking seat."`

	// Provisioning is read by the engine's own reconcile loop AND by the
	// provisioning CLI, which is a reversal this field's doc outlived: it
	// said the engine "never looks at it", and it was true until the loop
	// started provisioning. A reader who believed it would put a value here
	// expecting nothing to act on it.
	Provisioning *MattermostProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"Inputs the reconcile loop and the provisioning CLI both read."`
}

// Status is the indicator mode, applying the always default.
func (m *Mattermost) Status() WorkingStatus {
	if m.TypingStatus == "" {
		return StatusAlways
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

	// AdminToken is the system-administrator credential the provisioning
	// and the teardown both authenticate with. Held for the same reason
	// GitLab's is, and with the same consequence: see
	// [GitLabProvisioning.AdminToken].
	AdminToken string `secret:"true" yaml:"admin_token,omitempty" json:"admin_token,omitempty" desc:"System-admin token used to create and to disable the bots."`

	// Channels are channel NAMES (the URL slug) every bot joins, on top of
	// whatever each seat names. A bot only receives messages from channels
	// it is a member of.
	Channels []string `yaml:"channels,omitempty" json:"channels,omitempty" desc:"Channels every bot joins."`

	// DisplayNameSuffix marks agents apart from colleagues at a glance.
	DisplayNameSuffix string `yaml:"display_name_suffix,omitempty" json:"display_name_suffix,omitempty" desc:"Suffix on each bot display name, e.g. \" (AI)\"."`
}

func (m *Mattermost) validate(path string) error {
	var p problems
	p.wrap(m.TypingStatus.validate(at(path, "typing_status")))
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
			"%q must start with http:// or https://: it is the instance URL "+
				"browsers use", m.URL)
	}
	if strings.TrimSpace(m.Team) == "" {
		p.add(at(path, "team"), ErrMissing,
			"required when mattermost is enabled: channels are team-scoped")
	}
	return p.err()
}

// IsAtlassianCloud reports an address that is an Atlassian-hosted site.
//
// The SAME rule the vendor clients apply (jira.DeploymentOf and
// confluence.DeploymentOf), restated here because config is a leaf they
// depend on. It exists because the validators used to decide "Cloud" from
// cloud_id alone, and a Cloud site given by URL, which is how most companies
// write one, was treated as Data Center and refused for lacking a signing
// secret it cannot use. The two rules are held equal by a test.
func IsAtlassianCloud(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, suffix := range []string{".atlassian.net", ".jira.com"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(raw), "api.atlassian.com/ex/")
}

// noSpaces refuses a value that cannot hold whitespace.
//
// An address, an identifier and an email are each a SINGLE TOKEN, and a space
// in one is never a shorter way of writing something valid: it is a value the
// vendor has no record of. Caught here rather than at the vendor because the
// failure there is a 401 or a DNS miss that names neither the field nor the
// character, and the config is the one place that can say both.
func noSpaces(p *problems, path, value string) {
	if trimmed := strings.TrimSpace(value); strings.ContainsAny(trimmed, " \t\r\n") {
		p.add(path, ErrUnknownValue,
			"%q contains a space, and this field is a single token: an "+
				"address, an identifier and an email each name one thing, "+
				"so a space inside is a value nothing answers to", value)
	}
}

// hasHTTPScheme reports a URL the clients can actually use, treating a
// value that still carries a ${VAR} as unknown rather than wrong.
func hasHTTPScheme(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") ||
		envref.Has(url)
}

// DatadogProvisioning is what the engine creates at Datadog: one service
// account per agent seat, each holding a role and its own application key.
//
// SEPARATE FROM THE INBOUND HALF above, because the two are genuinely
// different integrations sharing a name — but not optional, and that is a
// change from what this was. A company could once accept alerts with nothing
// here, by pasting the engine's address into Datadog's webhook form by hand;
// the engine registers that webhook itself now, so an enabled block without
// this pair has nothing at Datadog pointing at it and receives no delivery
// ever. The identity half — a service account per agent seat, each holding a
// role and its own application key — is what the same pair additionally buys.
type DatadogProvisioning struct {
	// Site is the Datadog region, e.g. datadoghq.eu. A key issued in one
	// region is refused by every other and the hostname is the only thing
	// that tells them apart, so this is checked against Datadog's own list
	// rather than accepted.
	Site string `yaml:"site,omitempty" json:"site,omitempty" desc:"Datadog region hostname, e.g. datadoghq.com."`

	// APIKey and AppKey are the pair every Datadog call carries. They are
	// not interchangeable: the API key says which organization, and the
	// application key says which user acts.
	APIKey string `secret:"true" yaml:"api_key,omitempty" json:"api_key,omitempty" desc:"Datadog API key; says which organization."`
	AppKey string `secret:"true" yaml:"app_key,omitempty" json:"app_key,omitempty" desc:"Datadog application key; says which user acts."`

	// Role is the Datadog role every agent account is created holding.
	// Empty takes the read-only role, which is the honest default for
	// accounts nothing yet authenticates with.
	Role string `yaml:"role,omitempty" json:"role,omitempty" desc:"Role each agent's service account holds (default Datadog Read Only Role)."`

	// EmailDomain is the domain each agent's service-account address is
	// built under. Service accounts need an address Datadog will accept
	// and never deliver to, so it is a domain the company controls rather
	// than a real mailbox.
	EmailDomain string `yaml:"email_domain,omitempty" json:"email_domain,omitempty" desc:"Domain agent service-account addresses are built under."`
}

// GitHub is the org-level GitHub block.
//
// Symmetric with [GitLab] with two differences, and both come from what
// GitHub is rather than from a choice made here:
//
//   - URL IS OPTIONAL. github.com is where most companies are, and its API
//     lives on a different host from its web UI (api.github.com), so there
//     is no instance address to write. An Enterprise Server deployment names
//     itself and the API is derived from it — see [GitHub.APIBase].
//   - THE PROVISIONER MINTS NOTHING. GitHub issues no user account and no
//     personal access token on a provisioner's behalf: a token belongs to
//     the person who created it, and the API to create one on somebody's
//     behalf was withdrawn in 2020. So `crewlet github provision` reports
//     what each seat's own credential authenticates as and registers the
//     webhooks — the same shape as Jira's, for the same reason.
//
// The tool server is a separate `shared: false` http MCP entry; each agent
// supplies its token there as an Authorization header, and that is the same
// credential this integration reads a seat's login from.
type GitHub struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// URL is an Enterprise Server base, or empty for github.com.
	URL string `yaml:"url,omitempty" json:"url,omitempty" desc:"Enterprise Server base URL, e.g. https://github.example.com. Empty = github.com."`

	// WebhookSecret verifies inbound deliveries. Required when enabled: a
	// route with nothing to verify with cannot tell a real delivery from
	// anyone's POST.
	WebhookSecret string `secret:"true" yaml:"webhook_secret,omitempty" json:"webhook_secret,omitempty" desc:"HMAC secret for inbound deliveries; required when enabled."`

	// Token is the optional ORGANIZATION credential, and it now has one
	// job rather than two.
	//
	// IT IS THE WHOLE ORGANIZATION-LEVEL CLIENT. Empty, the reconcile pass
	// builds none and registers nothing at GitHub — no organization hook,
	// no repository hook — which is what `org_webhook: true` with no token
	// reports. Installing an agent's App does not substitute for it: an
	// organization-wide hook needs `admin:org_hook`, a user-token scope
	// that no App installation carries.
	//
	// ITS OTHER JOB IS GONE. It was also the READ credential for
	// participant fan-out — a webhook payload carries the author, the
	// assignees and the requested reviewers, but not who has COMMENTED or
	// REVIEWED, which is most of the set GitHub itself would notify. Each
	// agent's own App answers that now ([github.SeatLookup]), scoped to
	// what that agent may see rather than to whatever the person who
	// minted this token could reach.
	Token string `secret:"true" yaml:"token,omitempty" json:"token,omitempty" desc:"Organization token with admin:org_hook, for one organization-wide hook. Empty means each agent's own app carries its own."`

	// Provisioning is read by the engine's own reconcile loop as well as by
	// the provisioning CLI — see [Mattermost.Provisioning], whose doc made
	// the same claim about the same reversal.
	Provisioning *GitHubProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"Inputs the reconcile loop and the provisioning CLI both read."`
}

// githubAPIHost is github.com's API, which is a different host from its web
// UI rather than a path on it.
const githubAPIHost = "https://api.github.com"

// githubEnterpriseAPIPath is the REST prefix on an Enterprise Server.
const githubEnterpriseAPIPath = "/api/v3"

// APIBase is the REST base for this deployment.
//
// DERIVED, never configured, because the two forms disagree in a way an
// operator has no reason to know: github.com serves its API from
// api.github.com, and an Enterprise Server serves it from /api/v3 on the
// instance itself. A single `api_url` field would be a second address to
// keep in step with the first, and the failure of getting it wrong is a 404
// on every call with nothing naming the cause.
func (g *GitHub) APIBase() string {
	base := strings.TrimRight(strings.TrimSpace(g.URL), "/")
	if base == "" {
		return githubAPIHost
	}
	// An operator writes the instance URL; a copy-paste from GitHub's own
	// docs writes the API base. Accepting both is the difference between a
	// working config and a path with /api/v3 in it twice.
	if strings.HasSuffix(base, githubEnterpriseAPIPath) {
		return base
	}
	return base + githubEnterpriseAPIPath
}

// Bases is where GitHub is reached: the REST base and the browser base, both
// derived from a RESOLVED url.
//
// RESOLVED FIRST, THEN DERIVED, and both halves are load-bearing.
//
// [GitHub.APIBase] and [GitHub.WebURL] are computed FROM the url, so asking
// the raw block points an Enterprise Server deployment at github.com whenever
// its host is written as a `${VAR}` — a legal way to write it, since the
// field takes an embedded reference too.
//
// And the two are genuinely different addresses. The REST base is
// `<url>/api/v3` on Enterprise Server; the browser base is the host without
// it. Passing the raw url as either is only ever right on github.com, where
// an empty field makes both fall back — which is exactly why a caller that
// skipped this looked correct everywhere but Enterprise.
//
// ONE IMPLEMENTATION because three callers need it, in three packages: the
// engine's reconcile client, its seat-app half, and the dashboard's app-
// creation flow. Each had its own idea, and two of them were wrong.
func (g *GitHub) Bases(resolve func(name string) (string, bool)) (apiBase, webBase string) {
	if g == nil {
		return "", ""
	}
	url, _ := envref.Expand(g.URL, resolve)
	derived := GitHub{URL: strings.TrimSpace(url)}
	return derived.APIBase(), derived.WebURL()
}

// WebURL is the base a shareable link is built on.
func (g *GitHub) WebURL() string {
	if base := strings.TrimRight(strings.TrimSpace(g.URL), "/"); base != "" {
		return strings.TrimSuffix(base, githubEnterpriseAPIPath)
	}
	return "https://github.com"
}

// GitHubProvisioning is the provisioning CLI's inputs.
//
// SHORT, and deliberately so: the GitLab block beside it carries access
// levels, a username prefix and token scopes because GitLab's provisioner
// CREATES accounts and mints their tokens. GitHub's cannot, so a field here
// describing an account it will never create would be a promise the command
// does not keep.
type GitHubProvisioning struct {
	// Org is the GitHub organization the repositories live under. Setting
	// it lets one ORG-LEVEL webhook cover every repository in it, which is
	// the difference between one hook and one per repository on a company
	// with fifty of them.
	Org string `yaml:"org,omitempty" json:"org,omitempty" desc:"GitHub organization holding the repositories."`

	// Repos are `owner/repo` entries to register webhooks on, beyond the
	// organization itself.
	Repos []string `yaml:"repos,omitempty" json:"repos,omitempty" desc:"owner/repo entries to hook individually."`

	OrgWebhook ContainerWebhookMode `yaml:"org_webhook,omitempty" json:"org_webhook,omitempty" js:"enum=auto|true|false" desc:"auto (one org hook where the credential may), true, or false."`
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

// ContainerWebhookMode is how a provisioner registers hooks: once on the
// container that holds the repositories, or once per repository.
//
// ONE type for both code hosts, because it is one question — a GitLab group
// and a GitHub organization are the same thing here, and both hosts can
// refuse a container hook for the same kind of reason (a plan that does not
// include them, a credential without the scope). Two enums with three
// identical values would be two `Valid()` methods to keep in step and two
// chances for `auto` to come to mean different things.
//
// The FIELD names stay each third-party app's own (`group_webhook` on
// GitLab, `org_webhook` on GitHub), because those are the words their own
// documentation uses.
type ContainerWebhookMode string

// The container-webhook modes.
const (
	// ContainerWebhookAuto uses one container hook where the host accepts
	// it and falls back to per-repository hooks where it does not.
	ContainerWebhookAuto ContainerWebhookMode = "auto"
	// ContainerWebhookRequire demands a container hook and fails without
	// one.
	ContainerWebhookRequire ContainerWebhookMode = "true"
	// ContainerWebhookNever always registers per-repository hooks.
	ContainerWebhookNever ContainerWebhookMode = "false"
)

// ContainerWebhookModes is the closed set.
var ContainerWebhookModes = []ContainerWebhookMode{
	ContainerWebhookAuto, ContainerWebhookRequire, ContainerWebhookNever}

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

	// WebhookName is the name every hook this engine registers carries,
	// and therefore WHICH HOOKS ON THIS INSTANCE ARE THIS DEPLOYMENT'S.
	//
	// The reconcile converges the hooks carrying this name whatever
	// address they currently point at, which is what stops a change of
	// public base leaving live orphans behind — one group hook and one
	// per project per change, all enabled, all delivering to somewhere
	// that no longer answers. Measured on a real deployment behind a
	// tunnel: three.
	//
	// So it has to differ between two deployments watching ONE instance:
	// staging and production of the same company share this document, and
	// with one name each pass would repoint the other's hooks and only the
	// last one to run would receive anything. The same knob exists on Jira
	// and on Datadog for the same reason.
	WebhookName string `yaml:"webhook_name,omitempty" json:"webhook_name,omitempty" desc:"Name every hook the engine registers on this instance carries; give two deployments watching one instance two names (default crewlet)."`

	// Provisioning is read by the engine's own reconcile loop as well as by
	// the provisioning CLI — see [Mattermost.Provisioning]. [GitLabMode] is
	// what that reversal cost while this said otherwise.
	Provisioning *GitLabProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"Inputs the reconcile loop and the provisioning CLI both read."`
}

// WebhookNameOrDefault is the name this engine's hooks carry at GitLab.
//
// Restated here rather than imported from internal/gitlab for the reason
// [Datadog.HandleTagOrDefault] gives — config is the leaf the vendor packages
// depend on — and asserted equal by a test.
func (g *GitLab) WebhookNameOrDefault() string {
	if name := strings.TrimSpace(g.WebhookName); name != "" {
		return name
	}
	return "crewlet"
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

	// AdminToken is the group Owner credential the provisioning and the
	// teardown both authenticate with.
	//
	// HELD, which is a deliberate reversal. It used to be asked for on
	// every run and dropped, because a token that can create service
	// accounts is a standing power once it is kept. What that cost is a
	// disconnect: removing an account needs the authority that created
	// it, so with nothing held there was no way to take one away except
	// by hand at the third-party app, and every account this engine ever made
	// outlived the integration.
	//
	// It is a ${VAR} like every other credential here: the value is
	// sealed in the fleet's secret store and never in this document, and
	// a disconnect names it in the orphaned list so an operator knows
	// exactly what to revoke.
	AdminToken string `secret:"true" yaml:"admin_token,omitempty" json:"admin_token,omitempty" desc:"Personal access token with the full api scope, belonging to a group owner. Creates and removes the service accounts."`

	AccessLevel  GitLabAccessLevel            `yaml:"access_level,omitempty" json:"access_level,omitempty" js:"enum=developer|maintainer" desc:"Default membership level."`
	AccessLevels map[string]GitLabAccessLevel `yaml:"access_levels,omitempty" json:"access_levels,omitempty" desc:"Per-handle membership overrides."`

	UsernamePrefix string `yaml:"username_prefix,omitempty" json:"username_prefix,omitempty" desc:"Prefix on each service-account username."`

	// Projects are extra projects to add each account to and register
	// webhooks on, beyond the group itself.
	Projects []string `yaml:"projects,omitempty" json:"projects,omitempty" desc:"Extra projects to join and hook."`

	GroupWebhook ContainerWebhookMode `yaml:"group_webhook,omitempty" json:"group_webhook,omitempty" js:"enum=auto|true|false" desc:"auto (one group hook if the plan allows), true, or false."`

	// Mode is WHERE A SERVICE ACCOUNT IS OWNED, and therefore which route
	// creates one, mints its tokens and deletes it.
	//
	// # Why it is in the document rather than only on the command line
	//
	// It was `-mode` on `crewlet gitlab provision` and nowhere else, which
	// made it a fact only the person who typed it knew — and the engine
	// provisions now. Its passes read no flag, so they assumed "group" for
	// every company: against accounts created with `-mode instance` the
	// engine minted through the group route and was refused, and its
	// DISCONNECT deleted through the group route, which answers 404 for an
	// account that route has never heard of — read as success, so every one
	// of those accounts was reported removed and stayed live with every
	// credential it held.
	//
	// Empty is [GitLabModeGroup], which is the only shape GitLab.com has.
	// The flag is still there and still overrides, for one invocation, the
	// way `-public-url` overrides `integrations.public_base_url`.
	Mode GitLabMode `yaml:"mode,omitempty" json:"mode,omitempty" js:"enum=group|instance" desc:"Where service accounts are owned: group (default, and all GitLab.com offers) or instance (self-managed only; needs an instance-administrator token)."`

	// TokenScopes are minted on each service-account token.
	TokenScopes []string `yaml:"token_scopes,omitempty" json:"token_scopes,omitempty" desc:"Scopes minted on each service-account token."`
}

// GitLabMode is where this company's GitLab service accounts are owned.
//
// Restated here rather than imported from internal/gitlab for the reason
// [Datadog.HandleTagOrDefault] gives — config is the leaf the vendor packages
// depend on — and asserted equal to gitlab.Modes() by a test.
type GitLabMode string

const (
	// GitLabModeGroup owns service accounts from provisioning.group, which
	// is the only shape GitLab.com offers.
	GitLabModeGroup GitLabMode = "group"
	// GitLabModeInstance owns them from the instance itself. Self-managed
	// only, and it needs an instance-administrator token.
	GitLabModeInstance GitLabMode = "instance"
)

// GitLabModes is every mode, for an error that has to name them.
func GitLabModes() []string {
	return []string{string(GitLabModeGroup), string(GitLabModeInstance)}
}

// Valid reports a mode this build serves. Empty is [GitLabModeGroup].
func (m GitLabMode) Valid() bool {
	switch m {
	case "", GitLabModeGroup, GitLabModeInstance:
		return true
	}
	return false
}

// Or resolves the empty value.
func (m GitLabMode) Or() GitLabMode {
	if m == "" {
		return GitLabModeGroup
	}
	return m
}

// ModeOrDefault is where this company's service accounts are owned.
func (p *GitLabProvisioning) ModeOrDefault() GitLabMode {
	if p == nil {
		return GitLabModeGroup
	}
	return p.Mode.Or()
}

func (g *GitHub) validate(path string) error {
	var p problems
	if g.Enabled {
		// A URL IS OPTIONAL AND ITS SHAPE IS NOT. An Enterprise Server
		// address without a scheme resolves against nothing and produces
		// a request to a relative path — which fails as a malformed URL
		// rather than as "your instance address is missing https://".
		if url := strings.TrimSpace(g.URL); url != "" && !hasHTTPScheme(url) {
			p.add(at(path, "url"), ErrUnknownValue,
				"%q must start with http:// or https://: leave it unset for "+
					"github.com, which is a different API host rather than a "+
					"path on the web UI", g.URL)
		}
		if strings.TrimSpace(g.WebhookSecret) == "" {
			p.add(at(path, "webhook_secret"), ErrMissing,
				"required when github is enabled: every delivery is verified "+
					"against it, and a route with nothing to verify with "+
					"answers 503 rather than accepting one")
		}
		// NO SHAPE CHECK on the secret, unlike GitLab's. GitHub takes any
		// string as a webhook secret and signs with it verbatim, so there
		// is no wrong shape to catch — only a wrong VALUE, which is
		// indistinguishable from a right one until a delivery arrives.
	}
	if g.Provisioning == nil {
		return p.err()
	}
	pv := g.Provisioning
	pp := at(path, "provisioning")
	if pv.OrgWebhook != "" && !slices.Contains(ContainerWebhookModes, pv.OrgWebhook) {
		p.add(at(pp, "org_webhook"), ErrUnknownValue, "%q (want %s)",
			pv.OrgWebhook, names(ContainerWebhookModes))
	}
	if pv.OrgWebhook == ContainerWebhookRequire && strings.TrimSpace(pv.Org) == "" {
		p.add(at(pp, "org"), ErrMissing,
			"org_webhook: true demands one organization-level hook, and there "+
				"is no organization named to register it on")
	}
	for i, repo := range pv.Repos {
		// owner/repo, both halves present. A bare "repo" is the mistake
		// this catches, and it is otherwise a 404 per repository on a run
		// whose whole promise is that it says what it found.
		owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
		if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" ||
			strings.Contains(name, "/") {
			p.add(idx(at(pp, "repos"), i), ErrShape,
				"%q is not owner/repo: GitHub has no repository-only "+
					"addressing, so there is nothing for a run to look up", repo)
		}
	}
	return p.err()
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
				"required when gitlab is enabled: it is the only supported "+
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
	// REFUSED HERE rather than discovered from a 404 half way through a run.
	// This one input decides which endpoint every account is created on,
	// which tokens are minted through and which a disconnect deletes down —
	// and the delete route answers 404 as success, so a typo here is an
	// account reported removed and still live.
	if !pv.Mode.Valid() {
		p.add(at(pp, "mode"), ErrUnknownValue, "%q is not one of %s",
			pv.Mode, strings.Join(GitLabModes(), ", "))
	}
	if pv.AccessLevel != "" && !slices.Contains(GitLabAccessLevels, pv.AccessLevel) {
		p.add(at(pp, "access_level"), ErrUnknownValue, "%q (want %s)",
			pv.AccessLevel, names(GitLabAccessLevels))
	}
	for _, handle := range sortedKeys(pv.AccessLevels) {
		if !slices.Contains(GitLabAccessLevels, pv.AccessLevels[handle]) {
			p.add(at(at(pp, "access_levels"), handle), ErrUnknownValue, "%q (want %s)",
				pv.AccessLevels[handle], names(GitLabAccessLevels))
		}
	}
	if pv.GroupWebhook != "" && !slices.Contains(ContainerWebhookModes, pv.GroupWebhook) {
		p.add(at(pp, "group_webhook"), ErrUnknownValue, "%q (want %s)",
			pv.GroupWebhook, names(ContainerWebhookModes))
	}
	return p.err()
}

// mattermostUsername is what the Mattermost server itself accepts:
// lowercase, alphanumeric plus '.', '-' and '_'. Checked at config load so
// a bad username fails on the line that authored it rather than midway
// through a provisioning run that has already created half the fleet.
var mattermostUsername = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// The two Atlassian deployments, as [Atlassian.Deployment] spells them.
const (
	AtlassianCloud      = "cloud"
	AtlassianDataCenter = "data_center"
)

// AtlassianDeployments is the closed set, for a form's picker and for
// validation, so a value the form offers and a value the config accepts
// cannot diverge.
var AtlassianDeployments = []string{AtlassianCloud, AtlassianDataCenter}

// DeploymentOrDefault is which Atlassian this company runs.
//
// CLOUD IS THE DEFAULT because it is the deployment this engine can
// provision: an organization key creates the accounts, discovers the sites
// and mints their tokens, and a company that says nothing is far likelier to
// be on it. A Data Center operator says so once, and every question after
// that follows from the answer.
func (a *Atlassian) DeploymentOrDefault() string {
	if a == nil {
		return AtlassianCloud
	}
	if d := strings.ToLower(strings.TrimSpace(a.Deployment)); d == AtlassianDataCenter {
		return AtlassianDataCenter
	}
	return AtlassianCloud
}

// IsCloud reports the deployment this engine can provision.
func (a *Atlassian) IsCloud() bool { return a.DeploymentOrDefault() == AtlassianCloud }

// DiscoversAtlassianSite reports whether this company's Atlassian organization
// will supply the Jira and Confluence site addresses, so nobody has to give one.
//
// THE PREDICATE THE SETUP FORM ALREADY ACTS ON, written down so validation can
// act on the same one. `cloud_id` is declared Hidden on both products —
// "DISCOVERED, NOT ASKED", because the organization key reads every site this
// company has along with its cloud id — and on Cloud the site `url` is hidden
// beside it, since an address typed there is used INSTEAD of the gateway and a
// provisioned account's token authenticates only at the gateway.
//
// Validation did not know that and demanded one of them anyway, so connecting
// Atlassian on Cloud could not succeed: the form deliberately asks for neither,
// the write is refused for both, and the pass that would have discovered them
// never runs because the write is what starts it. The operator is told to fill
// in a field that is not on the screen.
//
// NIL IS NOT CLOUD HERE, although [Atlassian.IsCloud] answers true for it. That
// answer is right for "which deployment is this" and wrong for this question: a
// company with no organization block has nothing to discover a site WITH, so it
// must still name one. Via the dashboard that shape does not arise — Atlassian
// is one card covering the organization and both products, so connecting it
// always writes the organization — and a hand-written document that omits it is
// asked for a url, correctly.
func (i Integrations) DiscoversAtlassianSite() bool {
	return i.Atlassian != nil && i.Atlassian.IsCloud()
}

// DatadogIgnore is the route_to value that means "wake nobody".
//
// A VALUE, not an empty string, and the difference is the whole point. Empty
// is a company that has not answered the question, and an alert reaching
// nobody through it is a silent hole in the coverage this integration exists
// to provide. This is the same outcome ASKED FOR: only the monitors somebody
// has labelled wake an agent, and the rest stay with whatever Datadog already
// does about them.
//
// It lives here rather than in internal/datadog because config validation
// needs it and this package imports nothing from the engine.
const DatadogIgnore = "none"

// Atlassian is the organization one service account per agent is created in.
//
// A DIFFERENT THING FROM THE TWO PRODUCT BLOCKS. Jira and Confluence are
// sites this engine reads and writes AS an account. This is the organization
// those sites belong to, and the only place an identity can be created at
// all: Atlassian's site APIs have no route for it, and its organization APIs
// take a credential no site accepts. The block is separate because the
// credential is, not because the product is.
//
// OPTIONAL, and its absence is a working configuration rather than a missing
// one: a company whose operator creates each agent's Atlassian account by
// hand and pastes the token into the seat's mcp_env needs none of this. What
// it buys is that nobody has to.
type Atlassian struct {
	// Deployment says which Atlassian this company runs, and it is ASKED
	// rather than derived.
	//
	// Every other part of this file works out Cloud from a hostname, which
	// is right once an address exists and useless before one does: a form
	// has to decide what to ask BEFORE the operator has answered anything,
	// and an empty address reads as Data Center. That is how a Cloud connect
	// came to be offered a Data Center's fields, and how the Cloud webhook
	// token was dropped from the list before the address proving the site
	// Cloud was submitted.
	//
	// It also decides which questions are worth asking at all. A Cloud
	// company has an ORGANIZATION: its sites, their cloud ids and their
	// addresses are all readable from one key, so asking a person to type
	// them is asking them to copy values out of a console this engine is
	// already reading. Data Center has no organization, no cloud id and no
	// service accounts, so there the address is the only way in and it is
	// required.
	Deployment string `yaml:"deployment,omitempty" json:"deployment,omitempty" desc:"Which Atlassian this company runs: cloud or data_center. Default cloud."`

	// OrgID is the organization the admin APIs take as their subject. It is
	// in the admin console's own URL.
	OrgID string `yaml:"org_id,omitempty" json:"org_id,omitempty" desc:"Atlassian organization id, from admin.atlassian.com."`

	// APIKey is an organization API key created WITHOUT scopes.
	//
	// Unscoped is not a convenience. The account-management service refuses
	// a scoped key with 403 whatever scopes it holds, so a key created the
	// way the reference suggests authenticates for everything here except
	// the one call that creates an account.
	APIKey string `secret:"true" yaml:"api_key,omitempty" json:"api_key,omitempty" desc:"Unscoped organization API key. A scoped key is refused by the account-management API."`
}

// Datadog turns monitor alerts into inbound events, so a firing monitor can
// wake a seat the same way a comment on a merge request does.
//
// It is the one inbound integration that CANNOT SIGN A BODY. Datadog's
// Webhooks integration attaches custom headers, but only with fixed values,
// so there is nothing varying with the payload to compute an HMAC over. The
// strongest check the provider offers is a shared token sent in a header, and
// that is what this verifies.
//
// The difference from every other route here is real and worth naming: a
// replayed delivery is indistinguishable from a fresh one, and anyone holding
// the token can forge an alert. Treat WebhookToken as a signing key — it is
// doing that job with none of the guarantees.
type Datadog struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// WebhookToken is compared against the X-Crewlet-Token header.
	// Required when enabled, on the same terms as every other secret
	// here: a route with nothing to check against cannot tell a real
	// delivery from anyone's POST.
	WebhookToken string `secret:"true" yaml:"webhook_token,omitempty" json:"webhook_token,omitempty" desc:"Shared token compared against X-Crewlet-Token; required when enabled."`

	// WebhookName is the name of the webhook definition the engine keeps at
	// Datadog, and therefore the handle an operator writes in a monitor
	// message: `@webhook-crewlet` by default.
	//
	// Configurable because the name is Datadog's PRIMARY KEY for a webhook
	// and one organization may be watched by two Crewlet deployments, a
	// staging one and a production one. Sharing a name there would have
	// each deployment rewrite the other's address on every pass, so the
	// alerts would land at whichever reconciled last.
	//
	// CHANGING IT LEAVES THE PREVIOUS DEFINITION IN PLACE, deliberately,
	// and the engine REPORTS that rather than acting on it. The name is
	// also the handle monitors write — `@webhook-crewlet` — so every
	// monitor still naming the old one goes on delivering through the old
	// definition, correctly: same address, same token. Deleting it on a
	// rename would silence exactly those monitors, and Datadog serves no
	// listing (a GET on the collection answers 405) so nothing could find
	// it afterwards either.
	//
	// So the engine remembers the name it registered under and raises
	// [integration.FindingRegistrationOrphaned] when this field moves —
	// an advisory, because nothing is broken. Repoint the monitors and
	// then remove the old definition at Datadog. A disconnect withdraws
	// only the name this field holds.
	WebhookName string `yaml:"webhook_name,omitempty" json:"webhook_name,omitempty" desc:"Name of the webhook the engine keeps at Datadog; monitors name it as @webhook-<name> (default crewlet)."`

	// HandleTag is the monitor tag key that names the seat an alert wakes,
	// so a monitor tagged `crewlet:sre-lead` reaches that seat.
	//
	// Configurable rather than fixed because the key becomes a tag on the
	// operator's own monitors, sitting beside their existing conventions
	// in every Datadog list and filter, and a company with its own
	// ownership scheme should be able to name it accordingly.
	HandleTag string `yaml:"handle_tag,omitempty" json:"handle_tag,omitempty" desc:"Monitor tag key naming the seat an alert wakes (default crewlet)."`

	// Provisioning is what the engine creates AT Datadog, and it is
	// REQUIRED when the block is enabled.
	//
	// It reads as decoration on an otherwise working inbound block and is
	// not: the engine registers the webhook that makes an alert arrive at
	// all, and nothing else does. A block without this pair serves its
	// route, checks its token, reports itself connected, and receives
	// nothing — the same not-there coverage `route_to` guards against one
	// step earlier. Creating a service account per agent seat is a second
	// thing the same pair happens to pay for, not the reason it is
	// required. [Datadog.validate] refuses an enabled block without it.
	Provisioning *DatadogProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"Inputs for provisioning agent identities at Datadog."`

	// RouteTo is the seat an alert whose monitor names nobody wakes.
	//
	// REQUIRED when enabled, and it is the only routing floor in this file.
	// Every other surface routes by identity: an alert is the one delivery
	// that can legitimately name no party at all, because a monitor is not
	// addressed to anyone. Without a floor those alerts are accepted,
	// verified, counted and dropped, which is the worst state an alerting
	// integration can be in: it looks exactly like coverage.
	RouteTo string `yaml:"route_to,omitempty" json:"route_to,omitempty" desc:"Handle of the seat an alert naming no owner wakes; required when enabled."`
}

// DatadogSites is every Datadog region a company may name.
//
// Restated here rather than imported from internal/datadog for the reason
// [Datadog.HandleTagOrDefault] states — config is the leaf the vendor
// packages depend on, and reaching the other way for a list would invert
// that — and asserted equal to datadog.Sites() by the same test.
//
// CHECKED rather than accepted, because the hostname is the only thing that
// distinguishes one region's keys from another's: a typo is a credential that
// authenticates nowhere, reported by Datadog as a rejected key.
//
//nolint:gochecknoglobals // an immutable list, not state
var DatadogSites = []string{
	"datadoghq.com",
	"us3.datadoghq.com",
	"us5.datadoghq.com",
	"datadoghq.eu",
	"ap1.datadoghq.com",
	"ap2.datadoghq.com",
	"ddog-gov.com",
}

// WebhookNameOrDefault is the name of the webhook definition at Datadog.
//
// Restated here rather than imported from internal/datadog for the reason
// [Datadog.HandleTagOrDefault] states, and asserted equal by the same test.
func (d *Datadog) WebhookNameOrDefault() string {
	if name := strings.TrimSpace(d.WebhookName); name != "" {
		return name
	}
	return "crewlet"
}

// HandleTagOrDefault is the monitor tag key naming a seat.
func (d *Datadog) HandleTagOrDefault() string {
	if tag := strings.TrimSpace(d.HandleTag); tag != "" {
		return strings.ToLower(tag)
	}
	// Restated rather than imported from internal/datadog: config is a
	// leaf that the vendor packages depend on, and reaching the other way
	// for one word would invert that. The vendor package's own constant
	// carries the reasoning, and a test asserts the two agree.
	return "crewlet"
}

func (d *Datadog) validate(path string) error {
	var p problems

	if !d.Enabled {
		return nil
	}
	if strings.TrimSpace(d.WebhookToken) == "" {
		p.add(at(path, "webhook_token"), ErrMissing,
			"required when datadog is enabled: every delivery is checked "+
				"against it, and a route with nothing to check against "+
				"answers 503 rather than accepting one")
	}
	sharedToken(&p, at(path, "webhook_token"), d.WebhookToken)
	if strings.TrimSpace(d.RouteTo) == "" {
		p.add(at(path, "route_to"), ErrMissing,
			"required when datadog is enabled: name the handle of the seat "+
				"an alert should wake when no monitor tag names an owner, "+
				"or %q to dismiss those alerts on purpose. Without an answer "+
				"they are verified, counted and then delivered to nobody, "+
				"which looks exactly like working coverage",
			DatadogIgnore)
	}
	if tag := strings.TrimSpace(d.HandleTag); tag != "" && strings.ContainsAny(tag, ":, ") {
		p.add(at(path, "handle_tag"), ErrUnknownValue,
			"a Datadog tag key cannot contain a colon, a comma or a space: "+
				"the colon separates the key from its value and the comma "+
				"separates one tag from the next, so a key holding either "+
				"never matches a monitor")
	}
	if name := strings.TrimSpace(d.WebhookName); name != "" &&
		strings.ContainsAny(name, " @,") {
		p.add(at(path, "webhook_name"), ErrUnknownValue,
			"a Datadog webhook name cannot contain a space, an @ or a comma: "+
				"a monitor names it as @webhook-<name>, and any of those ends "+
				"the handle early so the alert reaches nobody")
	}

	// THE ENGINE REGISTERS THE WEBHOOK, so the keys that let it are not
	// optional decoration on an otherwise working block.
	//
	// Datadog posts to whatever URL its Webhooks integration holds, and
	// there is no other way in: an enabled block with no keys serves a
	// route, checks a token, reports itself connected and never receives
	// one delivery, because nothing at Datadog was ever told this
	// deployment exists. That is the same failure `route_to` guards
	// against one step earlier — coverage that is not there — and it is
	// worth refusing for the same reason.
	switch {
	case d.Provisioning == nil:
		p.add(at(path, "provisioning"), ErrMissing,
			"required when datadog is enabled: the engine registers the "+
				"webhook that makes alerts arrive, and it needs an API key "+
				"and an application key to do it. Without them the block "+
				"reports itself connected and receives nothing")
	default:
		if strings.TrimSpace(d.Provisioning.APIKey) == "" {
			p.add(at(path, "provisioning.api_key"), ErrMissing,
				"required when datadog is enabled: it says which organization "+
					"the webhook is registered in")
		}
		if strings.TrimSpace(d.Provisioning.AppKey) == "" {
			p.add(at(path, "provisioning.app_key"), ErrMissing,
				"required when datadog is enabled: Datadog refuses a write "+
					"carrying only an API key, and its message names neither")
		}
		// AND THE REGION, which is the third of the three and was the one
		// that failed soft. A key issued in one region is refused by every
		// other and the hostname is the only thing that tells them apart,
		// so a block with both keys and no site cannot build a single
		// call — it just fails hours later as a dashboard finding rather
		// than at load, beside two fields that fail closed.
		site := strings.ToLower(strings.TrimSpace(d.Provisioning.Site))
		switch {
		case site == "":
			p.add(at(path, "provisioning.site"), ErrMissing,
				"required when datadog is enabled: it is the region the keys "+
					"were issued in, and a key from one region is refused by "+
					"every other. One of %s",
				strings.Join(DatadogSites, ", "))
		case envref.Has(d.Provisioning.Site):
			// A ${VAR} IS UNKNOWN, NOT WRONG. Tier B holds pointers
			// verbatim and resolves them where a client is built, so the
			// membership check belongs there — datadog.NewClient makes
			// it, on the resolved value.
		case !slices.Contains(DatadogSites, site):
			p.add(at(path, "provisioning.site"), ErrUnknownValue,
				"%q is not a Datadog region: the hostname is the only thing "+
					"that tells one organization's keys from another's, so a "+
					"value Datadog does not serve resolves to no API at all. "+
					"One of %s",
				d.Provisioning.Site, strings.Join(DatadogSites, ", "))
		}
	}

	return p.err()
}
