package cloudflare

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/words"
)

// ProvisionerSecret is the sealed name the provisioner token is kept under.
const ProvisionerSecret = "CLOUDFLARE_API_TOKEN"

// Requirements says what this company still needs for Cloudflare. A nil
// block answers every field absent, so the form still renders.
//
// The zone list is a document field rather than a form field: the setup
// surface writes scalar values only.
func Requirements(in *config.Cloudflare, resolve func(string) (string, bool)) []setup.Requirement {
	var accountID, token string
	var enabled bool
	if in != nil {
		enabled = in.Enabled
		if in.Provisioning != nil {
			accountID, token = in.Provisioning.AccountID, in.Provisioning.APIToken
		}
	}
	reqs := []setup.Requirement{
		{
			Field:      "account_id",
			Label:      "Account ID",
			Kind:       setup.KindID,
			ConfigPath: "integrations.cloudflare.provisioning.account_id",
			Required:   true,
			Connect:    true,
			Format:     "32 hexadecimal characters",
			Hint:       "Shown on your account's {link}.",
			LinkText:   "overview page",
			VendorURL:  "https://dash.cloudflare.com/",
			Blocks:     integration.FindingCredentialMissing,
		},
		{
			Field:      "api_token",
			Label:      "Provisioner token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.cloudflare.provisioning.api_token",
			SecretName: ProvisionerSecret,
			Required:   true,
			Connect:    true,
			Checkable:  true,
			CheckWith:  []string{"account_id"},
			// A PASTE IS CHECKED AGAINST CLOUDFLARE'S ALPHABET before it is
			// stored or tested.
			Shape:    setup.ShapeOpaqueToken,
			Hint:     "An account API token that can create tokens, from your {link}.",
			More:     provisionerMore(),
			LinkText: "account API tokens page",
			// THE ACCOUNT PICKED AT CLOUDFLARE (`:account`), not typed here
			// first: the token is step one, before the account ID is known,
			// and Cloudflare asks which account where a person has several.
			VendorURL: "https://dash.cloudflare.com/?to=/:account/api-tokens",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			// Hidden and on: connecting a switched-off integration is not a
			// thing anybody means.
			Field:      "enabled",
			Label:      "Mint agent tokens",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.cloudflare.enabled",
			Required:   true,
			Hidden:     true,
			Default:    "true",
		},
	}
	for i := range reqs {
		switch reqs[i].Field {
		case "account_id":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(accountID, resolve)
		case "api_token":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(token, resolve)
		case "enabled":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(enabled)
		}
	}
	return reqs
}

// Guide is what the connect form says around its fields: the token's
// permissions as steps in the builder's own names, generated from the levels
// so a permission an agent level gains is one the steps ask for.
func Guide() setup.Guide {
	account, zone := provisionerGrants()
	return setup.Guide{
		Summary: "Crewlet gives each agent its own Cloudflare API token, scoped to your zones, and rolls, " +
			"narrows and deletes it for you.",
		Prepare: []setup.Step{
			{Text: "Create an account API token on your {link}.", Link: "api_token", Yields: "api_token"},
			{Text: "Give it these account permissions:", Grants: account},
			{Text: "And these zone permissions, on all zones:", Grants: zone},
			// OPTIONAL, AND SAID SO (ADR-0145): a token without them is whole.
			{Text: "Optional, to see " + words.And(atlasShows()) + " in Atlas, also give it these account permissions:",
				Grants: atlasGrants()},
			{Text: "Paste the token here and test it."},
		},
		Caution: &setup.Caution{
			Title: "The token holds every permission an agent can be given",
			Body: "Crewlet grants an agent nothing this token does not hold itself, so the token needs every " +
				"permission listed in the steps.",
		},
	}
}

// provisionerMore is the provisioner token's permissions said as prose, for a
// person replacing the token once the steps are put away.
func provisionerMore() string {
	account, zone := provisionerGrants()
	return "Account permissions: " + words.And(said(account)) + ". Zone permissions, on all zones: " +
		words.And(said(zone)) + ". Crewlet grants an agent nothing this token does not hold. Optional, to see " +
		words.And(atlasShows()) + " in Atlas: the account permissions " + words.And(said(atlasGrants())) +
		", which no agent is ever given. Remove them from the token to undo it."
}

// atlasGrants is the atlas's optional permissions as the token builder
// names them, and atlasShows what each lets the atlas show.
func atlasGrants() []string {
	out := make([]string, 0, len(atlasPermissions))
	for _, p := range atlasPermissions {
		out = append(out, p.builder)
	}
	return out
}

func atlasShows() []words.Said {
	out := make([]words.Said, 0, len(atlasPermissions))
	for _, p := range atlasPermissions {
		out = append(out, words.Said(p.shows))
	}
	return out
}

// said is permission names as prose names them, unquoted, as the token
// builder shows them.
func said(names []string) []words.Said {
	out := make([]words.Said, len(names))
	for i, n := range names {
		out[i] = words.Said(n)
	}
	return out
}
