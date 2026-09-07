package jira

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// What an operator has to supply before Jira issues reach a seat.
//
// The one thing that shapes this list is that a Cloud site and a Data Center
// instance are addressed differently, and the config takes ONE of the two: a
// direct URL, or a cloud id whose gateway URL is derived. Offering both as
// required fields would produce a document the validator refuses; offering
// neither leaves an operator guessing. So the URL is the required one and the
// cloud id sits beside it as the alternative, which is how the config's own
// refusal reads.
//
// The org account is REQUIRED here where GitHub's is optional, because this
// third-party app has no fallback: without a credential the engine cannot resolve a
// seat's account id, and an issue assigned to an agent then reaches nobody.

// Requirements says what this company still needs for Jira.
func Requirements(in *config.Jira, resolve func(string) (string, bool)) []setup.Requirement {
	var url, cloudID, token, email, secret, siteURL string
	if in != nil {
		url, cloudID = in.URL, in.CloudID
		token, email, secret, siteURL = in.Token, in.Email, in.WebhookSecret, in.SiteURL
	}

	reqs := []setup.Requirement{
		{
			Field:      "url",
			Connect:    true,
			Label:      "Jira site",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.jira.url",
			Required:   cloudID == "",
			Help:       "Your Cloud site or Data Center instance. Give this or a cloud id.",
			Blocks:     integration.FindingCredentialMissing,
		},
		{
			Field:      "cloud_id",
			Shared:     true,
			Connect:    true,
			Label:      "Cloud id",
			Kind:       setup.KindID,
			ConfigPath: "integrations.jira.cloud_id",
			Required:   false,
			Help:       "The alternative to the site address, through the API gateway.",
		},
		{
			Field:      "email",
			Shared:     true,
			Connect:    true,
			Label:      "Account email",
			Kind:       setup.KindText,
			ConfigPath: "integrations.jira.email",
			Required:   false,
			Help:       "Cloud authenticates as email and token together. Leave empty for Data Center.",
		},
		{
			Field:      "token",
			Shared:     true,
			Connect:    true,
			Label:      "API token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.jira.token",
			SecretName: "JIRA_TOKEN",
			Required:   true,
			Help:       "The engine reads Jira as this account, to resolve seats and watchers.",
			Where:      "Create an API token on your Atlassian account, or a personal access token on Data Center.",
			VendorURL:  "https://id.atlassian.com/manage-profile/security/api-tokens",
			Blocks:     integration.FindingCredentialMissing,
		},
		{
			Field:      "webhook_secret",
			Label:      "Webhook secret",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.jira.webhook_secret",
			SecretName: "JIRA_WEBHOOK_SECRET",
			Required:   false,
			// MINTABLE: Jira signs with whatever string the hook was
			// registered with, so both ends belong to the engine, and
			// running the setup pass registers the hook with this value.
			Mintable: true,
			Help:     "Signs every delivery. Data Center needs it; Cloud relays through Forge.",
			Blocks:   integration.FindingIngressBlocked,
		},
		{
			Field:      "site_url",
			Shared:     true,
			Label:      "Link address",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.jira.site_url",
			Required:   false,
			Help:       "Only alongside a cloud id: the address a person's links open.",
		},
	}

	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Plain(url)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Plain(cloudID)
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Plain(email)
	reqs[3].Present, reqs[3].Resolved, reqs[3].Stored = setup.Held(token, resolve)
	reqs[4].Present, reqs[4].Resolved, reqs[4].Stored = setup.Held(secret, resolve)
	reqs[5].Present, reqs[5].Resolved, reqs[5].Stored = setup.Plain(siteURL)
	return reqs
}

// Summary is the sentence the connect form opens with.
func Summary() string {
	return "Agents are assigned issues and mentioned by name. This engine " +
		"registers a webhook so Jira's events reach it; the accounts are " +
		"ones you already have."
}
