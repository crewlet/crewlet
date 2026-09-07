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
//
// It takes the deployment ASKED FOR rather than deriving it from
// an address: a form has to decide what to ask before the operator has
// answered anything, and an empty address reads as Data Center.
func Requirements(
	in *config.Confluence, cloud bool, resolve func(string) (string, bool),
) []setup.Requirement {
	var url, cloudID, token, email, secret, webhookToken, siteURL string
	if in != nil {
		url, cloudID = in.URL, in.CloudID
		token, email = in.Token, in.Email
		secret, webhookToken, siteURL = in.WebhookSecret, in.WebhookToken, in.SiteURL
	}

	reqs := []setup.Requirement{
		{
			Field: "url",
			// NOT ASKED ON CLOUD. The organization key reads every site
			// this company has, along with its cloud id and its address,
			// so a form that asked would be asking an operator to copy a
			// value out of a console this engine is already reading. Worse
			// than redundant: an address typed here is used INSTEAD of the
			// gateway, and a service account's token authenticates only at
			// the gateway, so answering it is what breaks the seats.
			//
			// On Data Center it is the only way in, and required.
			Hidden:     cloud,
			Connect:    true,
			Label:      "Confluence site",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.confluence.url",
			Required:   !cloud && cloudID == "",
			// WHERE TO READ IT, and nothing else. It described the
			// address, said that connecting Atlassian discovers it, and
			// named the deployment that needs it typed anyway. Three
			// sentences under a field whose own label says what it holds.
			// The one thing a person cannot work out from the label is
			// which page of their admin console the value is on.
			Help: "Find the Confluence site value under",
			// FILLED FROM THE ORGANIZATION ID, and not a link until there
			// is one: the page is per-organization, and the console's front
			// door is somewhere a person then has to navigate out of.
			LinkText:  "App URLs",
			VendorURL: "https://admin.atlassian.com/o/{org_id}/product-urls",
			Blocks:    integration.FindingCredentialMissing,
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
			ConfigPath: "integrations.confluence.cloud_id",
			Required:   false,
			Help:       "The alternative to the site address, through the API gateway.",
		},
		{
			Field:      "email",
			Shared:     true,
			Connect:    true,
			Label:      "Atlassian user account email",
			Kind:       setup.KindEmail,
			ConfigPath: "integrations.confluence.email",
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
			// THE DECLARED DEPLOYMENT, not a guess from an address.
			Required: cloud,
			// WHERE TO READ IT. The address belongs to the account whose
			// API token is in the field below, so it is the one on that
			// account's own profile rather than anything in the
			// organization's directory. That a Data Center instance needs
			// none is what the optional marker already says, from the same
			// rule that decides it.
			Help:      "Find the value at the end of the",
			LinkText:  "Profile and visibility page",
			VendorURL: "https://id.atlassian.com/manage-profile/profile-and-visibility",
		},
		{
			Field:      "token",
			Shared:     true,
			Connect:    true,
			Label:      "Atlassian user API token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.confluence.token",
			SecretName: "CONFLUENCE_TOKEN",
			Required:   true,
			// NO SENTENCE OF ITS OWN. This is ONE input shared by Jira
			// and Confluence, and each named the product it was declared
			// in, so which description rendered depended on which section
			// happened to claim the field. What is left is where to make
			// the token, which is the same answer for both.
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
			ConfigPath: "integrations.confluence.site_url",
			Required:   false,
			Help:       "Only alongside a cloud id: the address a person's links open.",
		},
	}

	// HELD, NOT PLAIN, for every field the engine reads through the
	// resolver. Plain claims present AND resolved on the strength of
	// something being written down, which is true of a literal and false of
	// a `${VAR}` naming an entry that is not there: the form went green
	// over a field every call then failed on, which is the exact silent
	// outage this pair of facts exists to separate.
	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Held(url, resolve)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Held(cloudID, resolve)
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Held(email, resolve)
	reqs[3].Present, reqs[3].Resolved, reqs[3].Stored = setup.Held(token, resolve)
	reqs[4].Present, reqs[4].Resolved, reqs[4].Stored = setup.Held(webhookToken, resolve)
	reqs[5].Present, reqs[5].Resolved, reqs[5].Stored = setup.Held(secret, resolve)
	reqs[6].Present, reqs[6].Resolved, reqs[6].Stored = setup.Held(siteURL, resolve)
	return forDeployment(reqs, cloud)
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
func forDeployment(reqs []setup.Requirement, cloud bool) []setup.Requirement {
	// THE DEPLOYMENT IS DECLARED, so this is now a filter rather than a
	// guess. A field that cannot apply is not an optional field: a Cloud
	// operator was offered a Data Center signing secret their site will
	// never send, and a Data Center operator a cloud id, a link address and
	// a Cloud webhook token that mean nothing off Atlassian's gateway.
	//
	// A field with a VALUE survives whatever this decides, because the
	// document is a fact and hiding a setting a company has written down
	// would make the form disagree with the configuration, and Save would
	// then clear it.
	cloudOnly := map[string]bool{"cloud_id": true, "site_url": true, "webhook_token": true}
	dataCenterOnly := map[string]bool{"webhook_secret": true}

	out := make([]setup.Requirement, 0, len(reqs))
	for _, r := range reqs {
		switch {
		case r.Present:
		case cloudOnly[r.Field] && !cloud:
			continue
		case dataCenterOnly[r.Field] && cloud:
			continue
		}
		out = append(out, r)
	}
	return out
}

// Summary is empty, for the reason [jira.Summary] gives: Confluence is a
// surface of the Atlassian card, and the card has already said what
// connecting it does.
func Summary() string { return "" }
