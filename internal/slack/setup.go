package slack

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// What an operator has to supply before an agent can talk on Slack.
//
// # One app per agent, and the credentials are per seat
//
// Every other third-party app here has one company-wide credential and, at most, a
// per-seat account behind it. Slack is the other way round: each agent has its
// OWN Slack app, so the two values that matter, the bot token and the signing
// secret, live on the seat rather than on the company. The company-wide block
// carries only the working-indicator settings.
//
// # This surface collects them; it does not create the apps
//
// Creating an app goes through Slack's app-manifest API, which authenticates
// with an app-configuration token Slack issues only by hand from its own
// pages, and which an organisation may decline to allow at all. So the engine
// does not pretend it can: `crewlet slack provision` remains the automated
// path where the tokens are available, and this surface is what makes a
// hand-created app usable without one, by taking the two values Slack shows on
// the app's own page and sealing them.
//
// The webhook URL is not a field for the same reason it is not one anywhere
// else: it is `<public_base_url>/webhooks/slack/<handle>`, which the surface
// shows to copy rather than inviting somebody to type a different one.

// Requirements says what one SEAT still needs for Slack.
//
// Per seat, because that is where the credentials live. A caller asks for the
// seats it wants and submits one at a time, which is also what keeps each
// write addressed by a handle rather than by a position in a list.
func Requirements(handle string, seat *config.Role, resolve func(string) (string, bool)) []setup.Requirement {
	var botToken, signingSecret, channel string
	if seat != nil {
		if block := seat.Integrations.Slack; block != nil {
			botToken, signingSecret, channel = block.BotToken, block.SigningSecret, block.Channel
		}
	}

	reqs := []setup.Requirement{
		{
			Field: "bot_token",
			Label: "Bot token",
			Kind:  setup.KindSecret,
			// RELATIVE TO THE SEAT. A seat is addressed by its handle
			// through the entity route, because a merge patch cannot
			// reach one element of a list without replacing the list.
			ConfigPath: "integrations.slack.bot_token",
			Seat:       handle,
			Required:   true,
			Help: "What this agent posts as. Slack shows it on the app's OAuth " +
				"page once the app is installed to the workspace.",
			Where:     "Install the app to your workspace, then copy the Bot User OAuth Token.",
			VendorURL: "https://api.slack.com/apps",
			Blocks:    integration.FindingIdentityMissing,
		},
		{
			Field:      "signing_secret",
			Label:      "Signing secret",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.slack.signing_secret",
			Seat:       handle,
			Required:   true,
			Help: "Every delivery addressed to this agent is verified against it. " +
				"A route with nothing to check against answers 503 rather than " +
				"accepting a delivery it cannot verify.",
			Where:     "On the app's Basic Information page, under App Credentials.",
			VendorURL: "https://api.slack.com/apps",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			Field:      "channel",
			Label:      "Default channel",
			Kind:       setup.KindText,
			ConfigPath: "integrations.slack.channel",
			Seat:       handle,
			Required:   false,
			Help: "Where this agent posts when nothing else says. Its unit's own " +
				"channel is used when this is empty.",
		},
	}

	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Held(botToken, resolve)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Held(signingSecret, resolve)
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Plain(channel)
	return reqs
}

// CompanyRequirements is what the company-wide Slack block carries.
//
// One field, and it is not a credential: whether an agent shows a working
// indicator while it is thinking. Everything that authenticates is per seat.
func CompanyRequirements(in *config.Slack) []setup.Requirement {
	var status string
	if in != nil {
		status = string(in.TypingStatus)
	}
	req := setup.Requirement{
		Field:      "typing_status",
		Label:      "Working indicator",
		Kind:       setup.KindChoice,
		ConfigPath: "integrations.slack.typing_status",
		Required:   false,
		Choices:    statusChoices(),
		Help: "Whether an agent shows that it is working on a message before it " +
			"answers. Addressed shows it only where somebody is waiting.",
	}
	req.Present, req.Resolved, req.Stored = setup.Plain(status)
	return []setup.Requirement{req}
}

// statusChoices are the indicator modes, from the config package's own list so
// a value it accepts and a value this offers can never diverge.
func statusChoices() []setup.Choice {
	labels := map[config.WorkingStatus]string{
		"addressed": "Only where somebody is waiting on this agent",
		"always":    "On every message it takes up",
		"off":       "Never",
	}
	out := make([]setup.Choice, 0, len(config.WorkingStatuses))
	for _, mode := range config.WorkingStatuses {
		label := labels[mode]
		if label == "" {
			label = string(mode)
		}
		out = append(out, setup.Choice{Value: string(mode), Label: label})
	}
	return out
}

// Summary is the sentence the connect form opens with.
func Summary() string {
	return "Each agent talks in Slack as its own app. The apps are created " +
		"from the command line, because Slack issues the credential that " +
		"makes them by hand."
}
