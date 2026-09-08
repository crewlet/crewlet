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
			Field:   "bot_token",
			Connect: true,
			Label:   "Bot token",
			Kind:    setup.KindSecret,
			// RELATIVE TO THE SEAT. A seat is addressed by its handle
			// through the entity route, because a merge patch cannot
			// reach one element of a list without replacing the list.
			ConfigPath: "integrations.slack.bot_token",
			Seat:       handle,
			Required:   true,
			// THE ROUTE TO THE VALUE, not a description of it.
			//
			// Nothing this engine can provision is behind these two boxes:
			// somebody has to walk Slack's own pages and come back with
			// what they find, and the only thing that shortens that walk is
			// being told exactly where to go. It said what the value WAS
			// ("what this agent posts as"), which a reader looking at a
			// field called Bot token already knows, and then where to find
			// it in prose ending in a bare "Open Slack" that landed on an
			// app list with no clue which of four pages was meant.
			//
			// The path is a literal, in the face this screen gives one, for
			// the reason [mattermost.AdminCredential] gives: a menu path is
			// followed exactly, and prose runs it into the sentence around
			// it. The link is the first hop rather than a trailing
			// afterthought, so the line reads in the order it is walked.
			Help: "Navigate to [api.slack.com/apps](https://api.slack.com/apps) > " +
				"this agent's app > `OAuth & Permissions > Install to Workspace` " +
				"and paste the `Bot User OAuth Token` here.",
			Blocks: integration.FindingIdentityMissing,
		},
		{
			Field:      "signing_secret",
			Connect:    true,
			Label:      "Signing secret",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.slack.signing_secret",
			Seat:       handle,
			Required:   true,
			Help: "Navigate to [api.slack.com/apps](https://api.slack.com/apps) > " +
				"this agent's app > `Basic Information > App Credentials` " +
				"and paste the `Signing Secret` here.",
			Blocks: integration.FindingCredentialMissing,
		},
		{
			Field:      "channel",
			Label:      "Default channel",
			Kind:       setup.KindText,
			ConfigPath: "integrations.slack.channel",
			Seat:       handle,
			Required:   false,
			// NO PATH, because there is nothing to fetch: this is a name the
			// operator chooses. What it does carry is the step that is
			// invisible until the agent stays silent, which is that a Slack
			// bot reads and posts only in channels it has been invited to.
			Help: "The channel this agent posts in when nothing else says. " +
				"Invite its bot to that channel in Slack, or it cannot post there.",
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
		Help:       "Whether an agent shows that it is working before it answers.",
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
//
// WHAT THE READER HAS TO DO FIRST. It said the apps are created from the
// command line, which is true of `crewlet slack provision` and useless to
// somebody looking at this form: that command needs an app-configuration
// token Slack issues by hand and an organisation may decline to allow at
// all, so the person reading this is usually the person about to create the
// apps themselves.
func Summary() string {
	return "Each agent talks in Slack as its own app, so every agent needs one " +
		"created at api.slack.com/apps and installed to your workspace. The two " +
		"values each agent asks for below are on that app's own pages."
}
