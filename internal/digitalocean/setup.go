package digitalocean

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// Requirements says what this company still needs for DigitalOcean: the team
// name alone. A nil block answers every field absent, so the form still
// renders.
func Requirements(in *config.DigitalOcean, resolve func(string) (string, bool)) []setup.Requirement {
	var team string
	var enabled bool
	if in != nil {
		enabled = in.Enabled
		if in.Provisioning != nil {
			team = in.Provisioning.Team
		}
	}
	reqs := []setup.Requirement{
		{
			Field:      "team",
			Label:      "Team",
			Kind:       setup.KindID,
			ConfigPath: "integrations.digitalocean.provisioning.team",
			Required:   true,
			Connect:    true,
			Format:     "the team's name as the team menu shows it",
			Help: "The team your Droplets and other resources live in, by its name in the control " +
				"panel's team menu. Each agent gets its own personal access token there, created in a " +
				"few clicks when you turn the agent on.",
			LinkText:  "DigitalOcean control panel",
			VendorURL: "https://cloud.digitalocean.com/account/team",
			Blocks:    integration.FindingCredentialMissing,
			// Nothing to test here: the first agent's token proves the team.
			CheckNote: "The first agent's token proves the team when it is pasted.",
		},
		{
			// Hidden and on: connecting a switched-off integration is not a
			// thing anybody means.
			Field:      "enabled",
			Label:      "Give agents tokens",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.digitalocean.enabled",
			Required:   true,
			Hidden:     true,
			Default:    "true",
		},
	}
	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Held(team, resolve)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Toggle(enabled)
	return reqs
}

// Summary is the sentence the connect form opens with.
func Summary() string {
	return "Each agent acts at DigitalOcean with its own personal access token, at the level you " +
		"choose, so you can cut off one agent without touching the others. You create each token " +
		"in the control panel and paste it, and Crewlet checks its scopes and keeps it."
}

// Probe answers the connect form's Test button, which DigitalOcean has nothing
// for: the form holds no credential, and each agent's token is checked when it
// is pasted for its seat. httpClient is accepted for the shape every probe has.
func Probe(*http.Client) setup.Probe {
	return func(_ context.Context, check setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.NotCheckable("DigitalOcean", check.Field)
	}
}
