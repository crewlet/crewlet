package config

import "strings"

// SeatCredentialMode is who holds an agent seat's credential at one
// integration: the engine, which provisions an account per agent and seals its
// credential under the seat's own name, or somebody outside Crewlet, who handed
// the credential in.
type SeatCredentialMode string

const (
	// SeatCredentialEngine is an integration the engine provisions for the
	// company: it holds the company-level authority the vendor's account API
	// takes, so it creates, changes, turns off and rotates each agent's
	// account, and mints and seals the credential itself.
	SeatCredentialEngine SeatCredentialMode = "engine"

	// SeatCredentialExternal is an integration the engine does not provision:
	// a seat's credential was handed in from outside Crewlet (an operator's
	// Secret, an app somebody created by hand, a Data Center site the account
	// API does not reach) and stays where the seat names it, exclusive to the
	// seat. The engine binds it to the seat's tool and never writes, rotates
	// or re-mints it.
	SeatCredentialExternal SeatCredentialMode = "external"
)

// SeatCredentialMode is who holds an agent seat's credential at kind for this
// company (D4b): the engine only where it holds the authority that creates an
// agent's account there. managed is a deployment whose company document only
// named systems write (`api.auth.company_writers`).
//
// Engine mode: an enabled `provisioning` block for datadog, gitlab, mattermost,
// cloudflare, expo, scaleway, aws, google_cloud and ssh; atlassian's
// organization block on a Cloud site (Data Center has none); github with
// `provisioning` on a document the engine writes (the manifest flow records its
// result there); slack's sealed `integrations.slack.app_config_token` (D11).
// Any other kind, an unconnected one, and a block with `enabled: false` is
// external: the engine provisions nothing it lacks the authority for.
func (c *Company) SeatCredentialMode(kind string, managed bool) SeatCredentialMode {
	if c == nil {
		return SeatCredentialExternal
	}
	in := c.Integrations
	engine := false
	switch kind {
	case "datadog":
		engine = in.Datadog != nil && in.Datadog.Enabled && in.Datadog.Provisioning != nil
	case "gitlab":
		engine = in.GitLab != nil && in.GitLab.Enabled && in.GitLab.Provisioning != nil
	case "atlassian":
		engine = in.Atlassian != nil && in.Atlassian.IsCloud()
	case "mattermost":
		engine = in.Mattermost != nil && in.Mattermost.Enabled && in.Mattermost.Provisioning != nil
	case "github":
		engine = in.GitHub != nil && in.GitHub.Enabled && in.GitHub.Provisioning != nil && !managed
	case "slack":
		engine = in.Slack != nil && strings.TrimSpace(in.Slack.AppConfigToken) != ""
	case "cloudflare":
		engine = in.Cloudflare != nil && in.Cloudflare.Enabled && in.Cloudflare.Provisioning != nil
	case "expo":
		engine = in.Expo != nil && in.Expo.Enabled && in.Expo.Provisioning != nil
	case "digitalocean":
		engine = in.DigitalOcean != nil && in.DigitalOcean.Enabled && in.DigitalOcean.Provisioning != nil
	case "scaleway":
		engine = in.Scaleway != nil && in.Scaleway.Enabled && in.Scaleway.Provisioning != nil
	case "aws":
		engine = in.AWS != nil && in.AWS.Enabled && in.AWS.Provisioning != nil
	case "google_cloud":
		engine = in.GoogleCloud != nil && in.GoogleCloud.Enabled && in.GoogleCloud.Provisioning != nil
	case "ssh":
		engine = in.SSH != nil && in.SSH.Enabled && in.SSH.Provisioning != nil
	}
	if engine {
		return SeatCredentialEngine
	}
	return SeatCredentialExternal
}
