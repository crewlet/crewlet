package cloudflare

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
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
			Hint: vendortext.Of(vendortext.W("Shown on your account's "),
				vendortext.W("overview page").At("https://dash.cloudflare.com/"), vendortext.W(".")),
			Blocks: integration.FindingCredentialMissing,
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
			Shape: setup.ShapeOpaqueToken,
			Hint: vendortext.Of(vendortext.W("An account API token that can create tokens, from your "),
				vendortext.W("account API tokens page").At(apiTokensURL), vendortext.W(".")),
			More:   provisionerMore(),
			Blocks: integration.FindingCredentialMissing,
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
			{Text: vendortext.Of(vendortext.W("Create an account API token on your "),
				vendortext.W("account API tokens page").At(apiTokensURL), vendortext.W(".")), Yields: "api_token"},
			{Text: vendortext.Plain("Give it these account permissions:"), Grants: account},
			{Text: vendortext.Plain("And these zone permissions, on all zones:"), Grants: zone},
			// OPTIONAL, AND SAID SO (ADR-0145): a token without them is whole.
			{Text: vendortext.Plain("Optional, to see " + words.And(atlasShows()) + " in Atlas, also give it these account permissions:"),
				Grants: atlasGrants()},
			{Text: vendortext.Plain("Paste the token here and test it.")},
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
func provisionerMore() vendortext.Text {
	account, zone := provisionerGrants()
	t := vendortext.Of(vendortext.W("Account permissions: "))
	t = append(append(t, vendortext.And(account...)...), vendortext.W(". Zone permissions, on all zones: "))
	t = append(append(t, vendortext.And(zone...)...), vendortext.W(". Crewlet grants an agent nothing this token does not hold. "+
		"Optional, to see "+words.And(atlasShows())+" in Atlas: the account permissions "))
	return append(append(t, vendortext.And(atlasGrants()...)...),
		vendortext.W(", which no agent is ever given. Remove them from the token to undo it."))
}

// apiTokensURL is the account's API tokens page, the account picked at
// Cloudflare (`:account`): the token comes before the account ID is known.
const apiTokensURL = "https://dash.cloudflare.com/?to=/:account/api-tokens"

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
