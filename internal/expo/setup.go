package expo

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// Requirements says what this company still needs for Expo. A nil block
// answers every field absent, so the form still renders.
func Requirements(in *config.Expo, resolve func(string) (string, bool)) []setup.Requirement {
	var account, token string
	var enabled bool
	if in != nil {
		enabled = in.Enabled
		if in.Provisioning != nil {
			account, token = in.Provisioning.AccountName, in.Provisioning.AccessToken
		}
	}
	reqs := []setup.Requirement{
		{
			Field:      "account_name",
			Label:      "Account name",
			Kind:       setup.KindID,
			ConfigPath: "integrations.expo.provisioning.account_name",
			Required:   true,
			Connect:    true,
			Format:     "the name in expo.dev/accounts/<name>",
			Help:       "The account or organization your projects belong to, as it is named on your",
			LinkText:   "Expo dashboard",
			VendorURL:  "https://expo.dev/",
			Blocks:     integration.FindingCredentialMissing,
		},
		{
			Field:      "access_token",
			Label:      "Provisioner token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.expo.provisioning.access_token",
			SecretName: ProvisionerSecret,
			Required:   true,
			Connect:    true,
			Checkable:  true,
			CheckWith:  []string{"account_name"},
			Help: "An access token whose user or robot holds the Admin role on the account, " +
				"which is what manages robot users. Crewlet creates each agent's robot through " +
				"it and never grants an agent Admin. Create one on your",
			LinkText:  "access tokens page",
			VendorURL: "https://expo.dev/accounts/{account_name}/settings/access-tokens",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			// Hidden and on: connecting a switched-off integration is not a
			// thing anybody means.
			Field:      "enabled",
			Label:      "Create agent robots",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.expo.enabled",
			Required:   true,
			Hidden:     true,
			Default:    "true",
		},
	}
	for i := range reqs {
		switch reqs[i].Field {
		case "account_name":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(account, resolve)
		case "access_token":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(token, resolve)
		case "enabled":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(enabled)
		}
	}
	return reqs
}

// Summary is the sentence the connect form opens with.
func Summary() string {
	return "Crewlet gives each agent its own Expo robot user on your account at the level " +
		"you choose, with its own access token, so builds, updates and submissions are " +
		"attributable to the agent. Connecting takes an access token with the Admin role."
}
