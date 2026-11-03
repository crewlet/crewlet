package cloudflare

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
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
			Help:       "Shown on your account's",
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
			Shape: setup.ShapeOpaqueToken,
			// ONE LINE PER GROUP, in the builder's own names, so a person can
			// tick them off; the form shows each line on its own.
			Help: "An account API token that holds every permission an agent can be given, since " +
				"Crewlet grants an agent nothing this token does not hold itself.\n" +
				provisionerHelp() + "\nCreate one on your",
			LinkText:  "account API tokens page",
			VendorURL: "https://dash.cloudflare.com/{account_id}/api-tokens",
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

// Summary is the sentence the connect form opens with.
func Summary() string {
	return "Crewlet gives each agent its own Cloudflare account API token at the level you " +
		"choose, scoped to your zones, and rolls, narrows and deletes it for you. " +
		"Connecting takes an account API token that can create tokens."
}
