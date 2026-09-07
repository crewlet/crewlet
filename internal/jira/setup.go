package jira

import (
	"cmp"

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

	addressed := cmp.Or(url, siteURL) != ""

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
			Field:  "cloud_id",
			Shared: true,
			// DISCOVERED, NOT ASKED. The Atlassian pass reads the site and
			// its cloud id from the organization key and records them, so a
			// form that asked would be asking an operator to copy a value
			// out of a console this engine is already reading. It still
			// round-trips whatever the document holds.
			Hidden:     true,
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
			// REQUIRED ON CLOUD, which authenticates an API token as
			// email and token together and refuses it as a bearer. It was
			// optional for the Data Center case, which made it optional on
			// every form including the one where it is the difference
			// between a working credential and a 401.
			// REQUIRED UNLESS THIS IS KNOWN TO BE DATA CENTER.
			//
			// Cloud authenticates an API token as Basic base64(email:token)
			// and refuses it as a bearer: without the address the engine
			// sends a bearer and Atlassian answers 403, so the webhook this
			// integration exists to register is never created. Data Center
			// takes the token as a bearer and wants no address at all.
			//
			// An UNKNOWN deployment counts as Cloud here, and that is the
			// case this got wrong: with no site typed yet DeploymentOf
			// answers DataCenter for the empty string, so the one field a
			// fresh Cloud connect cannot do without was marked optional.
			Required: !addressed || DeploymentOf(cmp.Or(url, siteURL)) == Cloud,
			Help:     "Cloud authenticates as email and token together. Leave empty for Data Center.",
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
			Help:     "Signs every delivery, on both deployments.",
			Blocks:   integration.FindingIngressBlocked,
		},
		{
			Field:  "site_url",
			Shared: true,
			// DISCOVERED, NOT ASKED. The Atlassian pass reads the site and
			// its cloud id from the organization key and records them, so a
			// form that asked would be asking an operator to copy a value
			// out of a console this engine is already reading. It still
			// round-trips whatever the document holds.
			Hidden:     true,
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
	return forDeployment(reqs, DeploymentOf(cmp.Or(url, siteURL)), cloudID, addressed)
}

// forDeployment drops the fields the other Atlassian deployment uses.
//
// A Cloud site and a Data Center instance need genuinely different things,
// and the form asked for BOTH sets at once: a Cloud operator was offered a
// webhook signing secret their site will never send, and a Data Center
// operator a cloud id and a link address that mean nothing off Atlassian's
// gateway. A field that cannot apply is not an optional field; it is a
// question with no answer, and it invites one anyway.
//
// A field with a value SURVIVES whatever this derives, because the
// derivation is a guess from an address and the document is a fact: hiding a
// setting a company has written down would make the form disagree with the
// configuration, and Save would then clear it.
func forDeployment(
	reqs []setup.Requirement, deploy Deployment, cloudID string, addressed bool,
) []setup.Requirement {
	// THE ORGANIZATION KNOWS ITS OWN SITE. The Atlassian pass discovers the
	// cloud id and the host from the organization key and records them, so
	// these are not questions for a person: a form that asked would be
	// asking an operator to copy three values out of a console this engine
	// is already reading. They appear only where nothing has discovered
	// them, which is a company that has not connected Atlassian.
	gateway := map[string]bool{"cloud_id": true, "site_url": true}

	// AN EMPTY ADDRESS IS NOT A DATA CENTER INSTANCE. DeploymentOf answers
	// DataCenter for a blank string, which is the right default for a real
	// address it cannot place and the wrong answer for no address at all:
	// that is a company mid-connect, and dropping the gateway fields there
	// left nothing for the discovered cloud id to be written into.
	known := deploy == Cloud || cloudID != "" || addressed
	out := make([]setup.Requirement, 0, len(reqs))
	for _, r := range reqs {
		switch {
		case r.Present:
		case gateway[r.Field] && known && deploy != Cloud:
			continue
		case r.Field == "site_url" && cloudID == "" && known:
			continue
		}
		out = append(out, r)
	}
	return out
}

// Summary is empty on purpose.
//
// Jira is one surface of the Atlassian card, and the card already opens with
// what connecting Atlassian does. A sentence per surface put three
// paragraphs above the first field of one form, each explaining a product to
// somebody who has just chosen it by name.
func Summary() string { return "" }
