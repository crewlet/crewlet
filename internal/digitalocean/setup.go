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
			Field: "team",
			Label: "Team",
			// TEXT, NOT AN ID: a team's name may hold a space, as the
			// document's own rule allows, and an id refuses every one.
			Kind:       setup.KindText,
			ConfigPath: "integrations.digitalocean.provisioning.team",
			Required:   true,
			Connect:    true,
			Format:     "the team's name as the team menu shows it",
			Hint:       "Your team's name, as the {link} team menu shows it.",
			More: "Each agent gets its own personal access token in this team, created when you turn " +
				"the agent on.",
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

// Guide is what the connect form says around its one field: the team to
// name, then each agent's token, made in the control panel when the agent is
// turned on.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Each agent acts at DigitalOcean with its own personal access token, so you can cut off " +
			"one agent without touching the others.",
		Prepare: []setup.Step{
			{Text: "Find your team's name in the team menu of the {link}.", Link: "team", Yields: "team"},
			{Text: "Connect, then turn agents on from the Agents tab."},
			{Text: "Create each agent's token there when asked. The first token proves the team."},
		},
	}
}

// Probe answers the connect form's Test button, which DigitalOcean has nothing
// for: the form holds no credential, and each agent's token is checked when it
// is pasted for its seat. httpClient is accepted for the shape every probe has.
func Probe(*http.Client) setup.Probe {
	return func(_ context.Context, check setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.NotCheckable("DigitalOcean", check.Field)
	}
}
