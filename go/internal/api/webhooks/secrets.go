package webhooks

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
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
	Plane      string
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

// SecretsOf reads the verification material out of a company epoch.
//
// The organization is taken as an argument rather than rebuilt from the config,
// because the caller holding an epoch already has one and building a second
// would let the two drift over a reload.
func SecretsOf(c *config.Company, o *org.Organization) Secrets {
	var s Secrets
	if c != nil {
		in := c.Integrations
		s.ForgeAppID = in.ForgeAppID
		if in.GitHub != nil {
			s.GitHub = in.GitHub.WebhookSecret
		}
		if in.GitLab != nil {
			s.GitLab = in.GitLab.SigningSecret
		}
		if in.Plane != nil {
			s.Plane = in.Plane.WebhookSecret
		}
		if in.Jira != nil {
			s.Jira = in.Jira.WebhookSecret
		}
		if in.Confluence != nil {
			s.Confluence = in.Confluence.WebhookSecret
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
		if !role.IsAgent() || role.Slack.SigningSecret == "" {
			continue
		}
		if s.Slack == nil {
			s.Slack = map[string]string{}
		}
		s.Slack[role.Handle()] = role.Slack.SigningSecret
	}
	return s
}
