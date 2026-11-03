package expo

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// Requirements says what this company still needs for Expo: the account name
// alone. A nil block answers every field absent, so the form still renders.
func Requirements(in *config.Expo, resolve func(string) (string, bool)) []setup.Requirement {
	var account string
	var enabled bool
	if in != nil {
		enabled = in.Enabled
		if in.Provisioning != nil {
			account = in.Provisioning.AccountName
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
			Help: "The account or organization your projects belong to, as it is named on your " +
				"Expo dashboard. Each agent gets its own robot there, set up in a few clicks " +
				"when you turn the agent on.",
			LinkText:  "Expo dashboard",
			VendorURL: "https://expo.dev/",
			Blocks:    integration.FindingCredentialMissing,
			// Nothing to test here: the first agent's token proves the name.
			CheckNote: "The first agent's robot token proves the account when it is pasted.",
		},
		{
			// Hidden and on: connecting a switched-off integration is not a
			// thing anybody means.
			Field:      "enabled",
			Label:      "Give agents robots",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.expo.enabled",
			Required:   true,
			Hidden:     true,
			Default:    "true",
		},
	}
	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Held(account, resolve)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Toggle(enabled)
	return reqs
}

// Summary is the sentence the connect form opens with.
func Summary() string {
	return "Each agent acts at Expo as its own robot user, at the role you choose, so its " +
		"builds and updates are attributable to it. You create each robot at Expo and " +
		"paste its token, and Crewlet checks it and keeps it."
}
