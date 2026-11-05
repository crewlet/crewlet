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
			Hint:       "The account or organization your projects belong to, on your {link}.",
			More: "As named in expo.dev/accounts/<name>. Each agent gets its own robot there when you " +
				"turn it on.",
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

// Guide is what the connect form says around its one field: the account to
// name, then each agent's robot, made at Expo when the agent is turned on.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Each agent acts at Expo as its own robot user, so its builds and updates are " +
			"attributable to it.",
		Prepare: []setup.Step{
			{
				Text: "Find the account or organization name on your {link}.",
				Link: "account_name", Yields: "account_name",
			},
			{Text: "Connect, then turn agents on from the Agents tab."},
			{Text: "Create each agent's robot at Expo when asked. The first robot's token proves the account."},
		},
	}
}
