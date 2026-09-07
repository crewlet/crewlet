package atlassian

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// What an operator supplies before this engine can give each agent its own
// Atlassian account.
//
// TWO FIELDS, and they are the two the control plane's own console asks for.
// Everything else is discovered: the site, its cloud id and its address all
// come from the organization's own product listing, so a person who has
// already told Atlassian what their sites are is not asked to type them
// again into this.

// Requirements says what this company still needs to provision Atlassian.
func Requirements(in *config.Atlassian, resolve func(string) (string, bool)) []setup.Requirement {
	var orgID, key string
	if in != nil {
		orgID, key = in.OrgID, in.APIKey
	}

	reqs := []setup.Requirement{
		{
			Field: "org_id",
			// SHARED, so Jira's and Confluence's own fields can cite it.
			// The organization is one answer for the whole of Atlassian,
			// and the site addresses under it are read from a page whose
			// address contains it.
			Shared:     true,
			Connect:    true,
			Label:      "Organization id",
			Kind:       setup.KindID,
			ConfigPath: "integrations.atlassian.org_id",
			Required:   true,
			Help:       "In your admin console's own address, after /o/.",
			LinkText:   "Open the admin console",
			VendorURL:  "https://admin.atlassian.com",
			Blocks:     integration.FindingCredentialMissing,
		},
		{
			Field:      "api_key",
			Connect:    true,
			Label:      "Organization API key",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.atlassian.api_key",
			SecretName: "ATLASSIAN_ORG_API_KEY",
			Required:   true,
			// UNSCOPED, and the form says so because getting it wrong is
			// invisible: a scoped key authenticates for everything here
			// except the one call that creates an account, which answers
			// 403 whatever scopes it holds.
			Help:     "Create it without scopes: a scoped key cannot create accounts.",
			LinkText: "API keys",
			// FILLED FROM THE ORGANIZATION ID, and not a link until there is
			// one: the keys live at a per-organization address, and the
			// console's front door is somewhere a person then has to
			// navigate out of.
			VendorURL: "https://admin.atlassian.com/o/{org_id}/api-keys",
			Blocks:    integration.FindingCredentialMissing,
		},
	}

	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Plain(orgID)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Held(key, resolve)
	return reqs
}

// Summary is the sentence the connect form opens with.
func Summary() string {
	return "Each agent gets its own Atlassian account, so it owns what it " +
		"does in Jira and Confluence. Connecting takes an unscoped " +
		"organization API key."
}
