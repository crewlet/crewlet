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

	// ConfluenceToken is the Cloud route's credential, and like Datadog's
	// it is a shared TOKEN rather than a signing key: Confluence Cloud
	// attaches no signature to a delivery, so the token written into the
	// registered URL is the strongest check available. Compared
	// constant-time against the query the delivery carries.
	ConfluenceToken string

	// Datadog is a shared TOKEN, not a signing key, and the difference
	// is the provider's: a Datadog webhook attaches headers with fixed
	// values only, so there is nothing varying with the body to sign.
	// It is compared constant-time against the header a delivery
	// carries, which is the strongest check available here.
	Datadog string

	// ForgeAppID is the audience claim a Forge invocation token must
	// carry. It is not a shared secret — the signature is checked against
	// Atlassian's published keys — but it plays the same role here: with no
	// app id there is nothing to check the token against.
	ForgeAppID string

	// ForgeSites are the cloud ids a Forge token must name, keyed by the
	// product ("jira", "confluence") whose events it carries. A product
	// with no entry has no cloud id to bind a token to, and its relayed
	// events answer 503 like a missing secret (forge_site.go).
	ForgeSites map[string]string

	// Slack is PER SEAT, keyed by agent handle, because each agent has its
	// own Slack app. The handle comes from the URL path, which is why that
	// route is the only one whose secret depends on where it was addressed.
	Slack map[string]string

	// GitHubSeat is PER SEAT on the same terms, and for the same reason: a
	// GitHub App has one bot identity, so an agent that acts as itself has
	// an app of its own, and GitHub generated that app its own signing
	// secret at conversion time.
	//
	// IT DOES NOT REPLACE [Secrets.GitHub]. Two different deployments
	// address the seat route: an agent's own app, which signs with the
	// secret here, and a single organization app pointed at one seat,
	// which has only the organization's. So the route prefers this and
	// falls back, and a seat present in this map is one whose deliveries
	// can ONLY have been signed with its own.
	GitHubSeat map[string]string
}

// Verifiable names the surfaces whose material can actually verify a
// delivery, sorted: "would a real delivery be accepted right now", asked
// without sending one. NOT "is a secret configured": a ${VAR} that did not
// resolve renders as present while the route answers 503.
//
// The rule is the ROUTE's own: GitLab needs a key it could have signed with,
// not merely a non-empty string. Mattermost is absent by design (a websocket,
// no delivery to verify), read as "nothing to say".
//
// SLACK IS PER SEAT, verifiable when AT LEAST ONE seat's signing secret
// resolved; leaving it out would read as refusal, since the company row
// carries a secret_present for Slack.
func (s Secrets) Verifiable() []string {
	var out []string
	if whsec.Valid(s.GitLab) {
		out = append(out, "gitlab")
	}
	// Confluence has TWO routes with two credentials, and either one makes
	// the surface verifiable: the signed Data Center route, or the
	// token-bearing Cloud route. Counted once, because the question is
	// whether a delivery from this surface could be accepted.
	if s.Confluence != "" || s.ConfluenceToken != "" {
		out = append(out, "confluence")
	}
	// GitHub is verifiable on either credential, because either one is
	// enough for a real delivery to be accepted: the organization app's
	// secret, or any one agent app's own.
	if s.GitHub == "" {
		for _, secret := range s.GitHubSeat {
			if secret != "" {
				out = append(out, "github")
				break
			}
		}
	}
	for _, pair := range []struct {
		kind, secret string
	}{
		{"github", s.GitHub},
		{"jira", s.Jira},
		{"datadog", s.Datadog},
	} {
		if pair.secret != "" {
			out = append(out, pair.kind)
		}
	}
	// Forge needs a site beside the app id: a token is refused unless it
	// names one of the company's Cloud sites.
	if s.ForgeAppID != "" && len(s.ForgeSites) > 0 {
		out = append(out, "forge")
	}
	// One seat is enough. A company with ten Slack apps and one unresolved
	// ${VAR} is not a company whose Slack is unverifiable; it is one seat
	// whose deliveries are refused, and that is what the per-seat
	// identity findings are for.
	for _, secret := range s.Slack {
		if secret != "" {
			out = append(out, "slack")
			break
		}
	}
	slices.Sort(out)
	return out
}

// SecretsOf reads the verification material out of a company epoch.
//
// Secrets are RESOLVED from their ${VAR}: the literal is a config field the
// dashboard renders, so a forged delivery would verify against a string an
// attacker could read.
//
// A DISABLED BLOCK CONTRIBUTES NOTHING, so its route answers 503 rather than
// ingesting what the routing half then drops (`enabled: false` is set after a
// credential leak; agrees with RoutedSources). Only GitHub and GitLab have
// that switch; for Jira and Confluence the block being PRESENT enables it.
//
// A nil resolve leaves every route's secret empty: 503, never accepting.
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
		site := func(product, rawURL, cloudID string) {
			if id := cloudIDOf(resolve(rawURL), resolve(cloudID)); id != "" {
				if s.ForgeSites == nil {
					s.ForgeSites = map[string]string{}
				}
				s.ForgeSites[product] = id
			}
		}
		if in.Jira != nil {
			s.Jira = resolve(in.Jira.WebhookSecret)
			site("jira", in.Jira.URL, in.Jira.CloudID)
		}
		if in.Confluence != nil {
			s.Confluence = resolve(in.Confluence.WebhookSecret)
			s.ConfluenceToken = resolve(in.Confluence.WebhookToken)
			site("confluence", in.Confluence.URL, in.Confluence.CloudID)
		}
		if in.Datadog != nil && in.Datadog.Enabled {
			s.Datadog = resolve(in.Datadog.WebhookToken)
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
		// THE SEAT'S OWN GITHUB APP, on the same walk as its Slack one: both
		// are the seat's own credential at a third-party app, and the seat
		// is what the org model holds, not the company document (see
		// [org.GitHubApp]).
		//
		// NOT GATED ON integrations.github.enabled. That switch turns off
		// the ORGANIZATION's app and route; an agent's app is its own, and a
		// company of only per-agent apps has no `integrations.github` block.
		if app := role.GitHub; app != nil {
			if secret := resolve(app.WebhookSecret); secret != "" {
				if s.GitHubSeat == nil {
					s.GitHubSeat = map[string]string{}
				}
				s.GitHubSeat[role.Handle()] = secret
			}
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
