package confluence

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// What an operator has to supply before Confluence page changes reach a seat,
// and before this engine can register a hook.
//
// # Two webhook credentials, and which one matters depends on the deployment
//
// A Data Center instance SIGNS its deliveries, so the engine verifies an HMAC
// against `webhook_secret`. Confluence Cloud signs nothing at all: the only
// thing that can carry a credential is the registered URL, so `webhook_token`
// rides in the query and is compared constant-time. A deployment needs one of
// them, and which one is decided by what it is rather than by an operator's
// choice, so both are listed as optional and the site address is what
// actually settles it.

// Requirements says what this company still needs for Confluence.
func Requirements(in *config.Confluence, resolve func(string) (string, bool)) []setup.Requirement {
	var url, cloudID, token, email, secret, webhookToken, siteURL string
	if in != nil {
		url, cloudID = in.URL, in.CloudID
		token, email = in.Token, in.Email
		secret, webhookToken, siteURL = in.WebhookSecret, in.WebhookToken, in.SiteURL
	}

	reqs := []setup.Requirement{
		{
			Field:      "url",
			Label:      "Confluence site",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.confluence.url",
			Required:   cloudID == "",
			Help: "Your Cloud site or Data Center instance, for example " +
				"https://acme.atlassian.net/wiki. Give this or a cloud id, not both.",
			Blocks: integration.FindingCredentialMissing,
		},
		{
			Field:      "cloud_id",
			Label:      "Cloud id",
			Kind:       setup.KindID,
			ConfigPath: "integrations.confluence.cloud_id",
			Required:   false,
			Help:       "The alternative to the site address, through the API gateway.",
		},
		{
			Field:      "email",
			Label:      "Account email",
			Kind:       setup.KindText,
			ConfigPath: "integrations.confluence.email",
			Required:   false,
			Help: "Set this for an Atlassian Cloud site, which authenticates as " +
				"email and token together. Leave it empty for a Data Center " +
				"personal access token.",
		},
		{
			Field:      "token",
			Label:      "API token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.confluence.token",
			SecretName: "CONFLUENCE_TOKEN",
			Required:   true,
			Help: "The engine reads Confluence as this account: it is what backs " +
				"knowledge search and what registers a webhook. Registering one " +
				"on Cloud needs Confluence administrator rights.",
			Where:     "Create an API token on your Atlassian account, or a personal access token on Data Center.",
			VendorURL: "https://id.atlassian.com/manage-profile/security/api-tokens",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			Field:      "webhook_token",
			Label:      "Cloud webhook token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.confluence.webhook_token",
			SecretName: "CONFLUENCE_WEBHOOK_TOKEN",
			Required:   false,
			Mintable:   true,
			Help: "For a Cloud site, which signs nothing: this token rides in the " +
				"registered URL and is the whole check on a delivery. Treat it " +
				"as a signing key, and note that changing the public address " +
				"republishes it, so a base change is a rotation.",
			Blocks: integration.FindingIngressBlocked,
		},
		{
			Field:      "webhook_secret",
			Label:      "Data Center webhook secret",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.confluence.webhook_secret",
			SecretName: "CONFLUENCE_WEBHOOK_SECRET",
			Required:   false,
			Mintable:   true,
			Help: "For a Data Center instance, which signs every delivery. The " +
				"engine verifies an HMAC against this, and a route with nothing " +
				"to check against answers 503 rather than accepting one.",
			Blocks: integration.FindingIngressBlocked,
		},
		{
			Field:      "site_url",
			Label:      "Link address",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.confluence.site_url",
			Required:   false,
			Help: "Only needed alongside a cloud id, so a link handed to a person " +
				"opens the page rather than the gateway.",
		},
	}

	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Plain(url)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Plain(cloudID)
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Plain(email)
	reqs[3].Present, reqs[3].Resolved, reqs[3].Stored = setup.Held(token, resolve)
	reqs[4].Present, reqs[4].Resolved, reqs[4].Stored = setup.Held(webhookToken, resolve)
	reqs[5].Present, reqs[5].Resolved, reqs[5].Stored = setup.Held(secret, resolve)
	reqs[6].Present, reqs[6].Resolved, reqs[6].Stored = setup.Plain(siteURL)
	return reqs
}
