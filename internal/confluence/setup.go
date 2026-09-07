package confluence

import (
	"cmp"

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
			Connect:    true,
			Label:      "Confluence site",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.confluence.url",
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
			ConfigPath: "integrations.confluence.cloud_id",
			Required:   false,
			Help:       "The alternative to the site address, through the API gateway.",
		},
		{
			Field:      "email",
			Shared:     true,
			Connect:    true,
			Label:      "Account email",
			Kind:       setup.KindText,
			ConfigPath: "integrations.confluence.email",
			Required:   false,
			Help:       "Cloud authenticates as email and token together. Leave empty for Data Center.",
		},
		{
			Field:      "token",
			Shared:     true,
			Connect:    true,
			Label:      "API token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.confluence.token",
			SecretName: "CONFLUENCE_TOKEN",
			Required:   true,
			Help:       "The engine reads Confluence as this account: knowledge search, and the webhook.",
			Where:      "Create an API token on your Atlassian account, or a personal access token on Data Center.",
			VendorURL:  "https://id.atlassian.com/manage-profile/security/api-tokens",
			Blocks:     integration.FindingCredentialMissing,
		},
		{
			Field:      "webhook_token",
			Label:      "Cloud webhook token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.confluence.webhook_token",
			SecretName: "CONFLUENCE_WEBHOOK_TOKEN",
			Required:   false,
			Mintable:   true,
			Help:       "Cloud signs nothing, so this token in the URL is the whole check.",
			Blocks:     integration.FindingIngressBlocked,
		},
		{
			Field:      "webhook_secret",
			Label:      "Data Center webhook secret",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.confluence.webhook_secret",
			SecretName: "CONFLUENCE_WEBHOOK_SECRET",
			Required:   false,
			Mintable:   true,
			Help:       "Data Center signs every delivery, and the engine checks it against this.",
			Blocks:     integration.FindingIngressBlocked,
		},
		{
			Field:      "site_url",
			Shared:     true,
			Label:      "Link address",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.confluence.site_url",
			Required:   false,
			Help:       "Only alongside a cloud id: the address a person's links open.",
		},
	}

	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Plain(url)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Plain(cloudID)
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Plain(email)
	reqs[3].Present, reqs[3].Resolved, reqs[3].Stored = setup.Held(token, resolve)
	reqs[4].Present, reqs[4].Resolved, reqs[4].Stored = setup.Held(webhookToken, resolve)
	reqs[5].Present, reqs[5].Resolved, reqs[5].Stored = setup.Held(secret, resolve)
	reqs[6].Present, reqs[6].Resolved, reqs[6].Stored = setup.Plain(siteURL)
	return forDeployment(reqs, DeploymentOf(cmp.Or(url, siteURL)), cloudID)
}

// forDeployment drops the fields the other Atlassian deployment uses.
//
// The same rule Jira's form follows, for the same reason: a Cloud operator
// was offered a Data Center signing secret their site will never send, and a
// Data Center operator a Cloud webhook token, a cloud id and a link address
// that mean nothing off Atlassian's gateway. A field that cannot apply is
// not an optional field.
//
// A field with a value SURVIVES, because the derivation is a guess from an
// address and the document is a fact.
func forDeployment(reqs []setup.Requirement, deploy Deployment, cloudID string) []setup.Requirement {
	gateway := map[string]bool{"cloud_id": true, "site_url": true}
	cloudOnly := map[string]bool{"webhook_token": true}
	dataCenter := map[string]bool{"webhook_secret": true}

	out := make([]setup.Requirement, 0, len(reqs))
	for _, r := range reqs {
		switch {
		case r.Present:
		case gateway[r.Field] && deploy != Cloud:
			continue
		case r.Field == "site_url" && cloudID == "":
			continue
		case cloudOnly[r.Field] && deploy != Cloud:
			continue
		case dataCenter[r.Field] && deploy == Cloud:
			continue
		}
		out = append(out, r)
	}
	return out
}

// Summary is the sentence the connect form opens with.
func Summary() string {
	// Same correction Jira's summary carries: a Cloud site has no webhook
	// REST API for an API token to call, so its changes arrive through the
	// Forge relay.
	return "Agents read the company's pages and answer comments on them. " +
		"Cloud delivers through the Crewlet Forge app; Data Center " +
		"registers a webhook."
}
