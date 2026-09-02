package webhooks

import (
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/whsec"
)

// Secrets is the verification material one configuration epoch supplies.
//
// A SNAPSHOT, taken per request rather than captured once at startup. Config is
// hot-reloadable, and a rotated webhook secret that only took effect on restart
// would leave a node rejecting every genuine delivery until somebody noticed —
// the failure looks exactly like an attack.
//
// An empty field is "cannot verify", never "nothing to verify". Every route
// whose secret is empty answers 503, because a verifier with nothing to check
// against is not a verifier: it would accept anyone's POST as the provider's.
type Secrets struct {
	GitHub     string
	GitLab     string
	Jira       string
	Confluence string

	// ForgeAppID is the audience claim a Forge invocation token must
	// carry. It is not a shared secret — the signature is checked against
	// Atlassian's published keys — but it plays the same role here: with no
	// app id there is nothing to check the token against.
	ForgeAppID string

	// Slack is PER SEAT, keyed by agent handle, because each agent has its
	// own Slack app. The handle comes from the URL path, which is why that
	// route is the only one whose secret depends on where it was addressed.
	Slack map[string]string
}

// Verifiable names the surfaces whose material can actually verify a
// delivery, sorted. The answer to "would a real delivery be accepted right
// now", asked without sending one.
//
// NOT "is a secret configured", which is what every operator surface showed
// before this: a secret lives in the config as a ${VAR}, and one that did not
// resolve renders as present while the route answers 503 to every delivery
// and the vendor's settings page reports a healthy hook. The config says set,
// the vendor says fine, and the deliveries stop — with nothing anywhere
// naming the variable.
//
// The rule is per surface and it is the ROUTE's own: GitLab needs a key the
// vendor could have signed with, not merely a non-empty string, because a
// value that is not one cannot be the HMAC key for any delivery. Mattermost
// is absent by design — it holds a websocket rather than a route, so there is
// no delivery to verify — and so is Slack, whose material is per seat.
func (s Secrets) Verifiable() []string {
	var out []string
	if whsec.Valid(s.GitLab) {
		out = append(out, "gitlab")
	}
	for _, pair := range []struct {
		kind, secret string
	}{
		{"github", s.GitHub},
		{"jira", s.Jira},
		{"confluence", s.Confluence},
		{"forge", s.ForgeAppID},
	} {
		if pair.secret != "" {
			out = append(out, pair.kind)
		}
	}
	slices.Sort(out)
	return out
}

// SecretsOf reads the verification material out of a company epoch.
//
// The organization is taken as an argument rather than rebuilt from the config,
// because the caller holding an epoch already has one and building a second
// would let the two drift over a reload.
//
// # Resolved, and that is the whole point
//
// A secret lives in the config as a ${VAR}: verbatim in the stored payload,
// resolved at construction, which is what makes rotating one a change to the
// environment rather than to the company. Every consumer resolves — and this
// one did not.
//
// The consequence was not a degraded route. It was SEVEN routes verifying
// against the literal string "${GITLAB_SIGNING_SECRET}": every delivery from
// every vendor refused, with the engine logging one warning per delivery and
// the vendor's settings page showing a healthy hook. Measured against a real
// GitLab. Worse than the outage is what the literal IS — a config field the
// dashboard renders, not a secret — so a forged delivery would have verified
// against a string an attacker could read.
//
// A DISABLED BLOCK CONTRIBUTES NOTHING, so its route answers 503 rather than
// verifying and ingesting a delivery the routing half will then drop:
// `enabled: false` is what an operator sets after a credential leak, and
// accepting deliveries that go nowhere is worse than refusing them. It also
// makes this agree with RoutedSources, which reports what can actually reach
// a seat.
//
// Only GitHub and GitLab have that switch. For Jira and Confluence the block
// being PRESENT is the enablement — removing it is the gesture — and a nil
// block already contributes nothing here.
//
// resolve is what a ${VAR} answers to; nil resolves nothing, which leaves
// every route with an empty secret and therefore refusing to serve. That is
// the safe direction: a route with nothing to verify with answers 503 rather
// than accepting.
func SecretsOf(c *config.Company, o *org.Organization, resolve func(string) string) Secrets {
	if resolve == nil {
		resolve = func(string) string { return "" }
	}
	var s Secrets
	if c != nil {
		in := c.Integrations
		// The Forge app id is the JWT AUDIENCE, not a credential — it is
		// in every manifest an operator installs — so it is config
		// rather than a secret, and resolved the same way anything else
		// that can be a ${VAR} is.
		s.ForgeAppID = resolve(in.ForgeAppID)
		if in.GitHub != nil && in.GitHub.Enabled {
			s.GitHub = resolve(in.GitHub.WebhookSecret)
		}
		if in.GitLab != nil && in.GitLab.Enabled {
			s.GitLab = resolve(in.GitLab.SigningSecret)
		}
		if in.Jira != nil {
			s.Jira = resolve(in.Jira.WebhookSecret)
		}
		if in.Confluence != nil {
			s.Confluence = resolve(in.Confluence.WebhookSecret)
		}
	}
	if o == nil {
		return s
	}
	for role := range o.AllRoles() {
		// A human seat can hold a Slack identity — that is how a person
		// is addressed — but nothing delivers Events API traffic to one,
		// and a signing secret on a seat the engine never wakes would
		// open a route that can only ever be a dead end.
		if !role.IsAgent() {
			continue
		}
		secret := resolve(role.Slack.SigningSecret)
		if secret == "" {
			continue
		}
		if s.Slack == nil {
			s.Slack = map[string]string{}
		}
		s.Slack[role.Handle()] = secret
	}
	return s
}
