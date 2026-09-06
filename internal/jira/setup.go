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
// vendor has no fallback: without a credential the engine cannot resolve a
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
			Label:      "Jira site",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.jira.url",
			Required:   cloudID == "",
			Help: "Your Cloud site or Data Center instance, for example " +
				"https://acme.atlassian.net. Give this or a cloud id, not both.",
			Blocks: integration.FindingCredentialMissing,
		},
		{
			Field:      "cloud_id",
			Label:      "Cloud id",
			Kind:       setup.KindID,
			ConfigPath: "integrations.jira.cloud_id",
			Required:   false,
			Help: "The alternative to the site address, for a deployment reaching " +
				"Atlassian through the API gateway. Give one or the other.",
		},
		{
			Field:      "email",
			Label:      "Account email",
			Kind:       setup.KindText,
			ConfigPath: "integrations.jira.email",
			Required:   false,
			Help: "Set this for an Atlassian Cloud site, which authenticates as " +
				"email and token together. Leave it empty for a Data Center " +
				"personal access token, which is a bearer credential.",
		},
		{
			Field:      "token",
			Label:      "API token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.jira.token",
			SecretName: "JIRA_TOKEN",
			Required:   true,
			Help: "The engine reads Jira as this account: it resolves which seat " +
				"an issue belongs to, and who is watching a thread. Read access " +
				"is enough for routing; registering a webhook needs an " +
				"administrator.",
			Where:     "Create an API token on your Atlassian account, or a personal access token on Data Center.",
			VendorURL: "https://id.atlassian.com/manage-profile/security/api-tokens",
			Blocks:    integration.FindingCredentialMissing,
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
			Help: "Every delivery is signed with this and checked at the edge. " +
				"Cloud sites signing with X-Hub-Signature use it too; a route " +
				"with nothing to check against answers 503 rather than " +
				"accepting a delivery it cannot verify.",
			Blocks: integration.FindingIngressBlocked,
		},
		{
			Field:      "site_url",
			Label:      "Link address",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.jira.site_url",
			Required:   false,
			Help: "Only needed alongside a cloud id: the gateway address is not " +
				"something to hand a person, and a link built from it looks " +
				"right and opens nothing.",
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
