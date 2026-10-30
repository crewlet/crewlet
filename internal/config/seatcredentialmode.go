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
// company (D4b): the engine only where it actually holds the authority that
// creates an agent's account there. managed is a deployment whose company
// document only named systems write (`api.auth.company_writers`, an operator
// rendering it from its custom resources).
//
//   - datadog: an enabled block with `provisioning`, whose keys create
//     service accounts.
//   - gitlab: an enabled block with `provisioning`, whose owner token creates
//     them.
//   - atlassian (Jira and Confluence): the organization block, on a Cloud
//     site; a Data Center site has no organization to create an account in.
//   - mattermost: an enabled block with `provisioning`, whose admin token
//     creates bots.
//   - github: an enabled block with `provisioning` on a document the engine
//     writes. An
//     agent's app is made from a manifest whose result the engine records in
//     the document, so where only the operator writes it the apps are made
//     outside Crewlet and handed in, however the block reads.
//   - slack: the sealed app-configuration token
//     (`integrations.slack.app_config_token`) the engine creates each agent's
//     app with (D11). The block alone grants no authority: an operator renders
//     it for apps made by hand.
//
// Any other kind, and a company that does not connect kind, is external: the
// engine provisions nothing it was not given the authority for. So is a block
// switched off (`enabled: false`), whose pass does not run: the engine holds
// the authority there and has been told not to use it.
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
	}
	if engine {
		return SeatCredentialEngine
	}
	return SeatCredentialExternal
}
