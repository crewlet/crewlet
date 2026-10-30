package config

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
// company (D4b), a pure function of the document: the facts each vendor's
// provisioning needs before it can act.
//
//   - datadog: a `provisioning` block, whose keys create service accounts.
//   - gitlab: an enabled block with `provisioning`, whose owner token creates
//     them.
//   - atlassian (Jira and Confluence): the organization block, on a Cloud
//     site; a Data Center site has no organization to create an account in.
//   - mattermost and github: a `provisioning` block (GitHub's apps are made
//     from the manifest the engine prepares).
//   - slack: the company block, under which the engine creates each agent's
//     app (D11).
//
// Any other kind, and a company that does not connect kind, is external: the
// engine provisions nothing it was not given the authority for.
func (c *Company) SeatCredentialMode(kind string) SeatCredentialMode {
	if c == nil {
		return SeatCredentialExternal
	}
	in := c.Integrations
	engine := false
	switch kind {
	case "datadog":
		engine = in.Datadog != nil && in.Datadog.Provisioning != nil
	case "gitlab":
		engine = in.GitLab != nil && in.GitLab.Enabled && in.GitLab.Provisioning != nil
	case "atlassian":
		engine = in.Atlassian != nil && in.Atlassian.IsCloud()
	case "mattermost":
		engine = in.Mattermost != nil && in.Mattermost.Provisioning != nil
	case "github":
		engine = in.GitHub != nil && in.GitHub.Provisioning != nil
	case "slack":
		engine = in.Slack != nil
	}
	if engine {
		return SeatCredentialEngine
	}
	return SeatCredentialExternal
}
