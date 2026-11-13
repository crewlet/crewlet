package digitalocean

import (
	"context"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// AtlasField is the form's field for the atlas's token.
const AtlasField = "atlas_token"

// Requirements says what this company still needs for DigitalOcean: the team
// name, and optionally a read token for the atlas. A nil block answers every
// field absent, so the form still renders.
func Requirements(in *config.DigitalOcean, resolve func(string) (string, bool)) []setup.Requirement {
	var team, atlasToken string
	var enabled bool
	if in != nil {
		enabled = in.Enabled
		if in.Provisioning != nil {
			team = in.Provisioning.Team
		}
		if in.Atlas != nil {
			atlasToken = in.Atlas.Token
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
		{
			// OPTIONAL, AND NEVER AN AGENT'S: without it every agent works
			// as before and the atlas shows the account alone.
			Field:      AtlasField,
			Label:      "Atlas read token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.digitalocean.atlas.token",
			Checkable:  true,
			CheckWith:  []string{"team"},
			Hint:       "See more in Atlas: a read-only {link} made for the atlas alone.",
			More: "Optional. In the team above, create a token named " + AtlasTokenName + " with Custom " +
				"Scopes, ticking these and no other: " + strings.Join(atlasScopes, ", ") + ". The atlas then " +
				"shows the team's Droplets, Kubernetes clusters, databases, load balancers, domains, apps " +
				"and Spaces buckets, read again every hour, and reads no password, key or environment " +
				"value. A token that can change anything is refused. To undo it, delete the token at " +
				"DigitalOcean and clear this field.",
			LinkText:  "personal access token",
			VendorURL: NewTokenURL,
		},
	}
	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Held(team, resolve)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Toggle(enabled)
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Held(atlasToken, resolve)
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
			{
				Text: "Optional, to see more in Atlas: create one more {link} named " + AtlasTokenName +
					", for the atlas alone, with Custom Scopes and only:",
				Link: AtlasField, Grants: AtlasScopes(), Yields: AtlasField,
			},
		},
	}
}

// probeCallTimeout bounds the whole check inside [setup.VerifyBudget]: about
// thirty requests, six at a time, each answered in well under a second.
const probeCallTimeout = setup.VerifyBudget

// Probe answers the connect form's Test button for the atlas's token, the one
// credential the form holds: it is checked as [CheckAtlas] checks it before
// every read. Each agent's token is checked when it is pasted for its seat.
//
// httpClient is nil in production; a test hands one that reaches a fake.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != AtlasField {
			return setup.Verdict{}, setup.NotCheckable("DigitalOcean", check.Field)
		}
		token := strings.TrimSpace(check.Value(AtlasField))
		team := strings.TrimSpace(check.Value("team"))
		switch {
		case token == "":
			return setup.Verdict{}, setup.Unusable("Paste the atlas's token to test it.")
		case team == "":
			return setup.Verdict{}, setup.Unusable("Enter the team's name: the token is checked against it.")
		}
		client, err := NewClient(ClientOptions{Token: token, HTTP: httpClient})
		if err != nil {
			return setup.Verdict{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, probeCallTimeout)
		defer cancel()
		checked, err := CheckAtlas(ctx, client, AtlasInput{Token: token, Team: team})
		if refusal, ok := IsRefusal(err); ok {
			return setup.Refused(refusal.Detail, setup.Warning{Action: refusal.Hint}), nil
		}
		if err != nil {
			return setup.Judge(setup.Failure{App: "DigitalOcean", Status: Status(err), Err: err}, token), nil
		}
		details := []setup.Detail{
			setup.Fact{Label: "Team", Value: checked.Team.Name},
			setup.Fact{Label: "Team ID", Value: checked.Team.UUID},
		}
		for _, scope := range checked.Lacks {
			details = append(details, setup.Warning{
				Title:  "The token lacks " + scope + ", so that part of the team is not read.",
				Action: "To read it, create the token again with " + scope + " ticked.",
			})
		}
		return setup.Works("Works. The token reads the team and changes nothing.", details...), nil
	}
}
