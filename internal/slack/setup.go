package slack

import (
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
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
func Requirements(handle string, seat *org.Role, resolve func(string) (string, bool)) []setup.Requirement {
	var botToken, signingSecret string
	if seat != nil {
		botToken, signingSecret = seat.Slack.BotToken, seat.Slack.SigningSecret
	}

	// TWO BOXES, AND THERE WAS A THIRD. A "Default channel" sat here while
	// the transport had a `Send` whose empty-channel fallback was the seat's
	// configured one — and that send had no caller in the tree. Every message
	// an agent posts is the Slack MCP server's call on this same token, and
	// it names its own channel, so the field this form asked for was read by
	// nothing: an operator typed a channel id and the agent went on posting
	// wherever it was addressed. A form that collects a value nothing reads is
	// worse than one that does not ask, because the answer LOOKS like
	// configuration. Where a seat's room genuinely belongs is `units[].channel`
	// on the org chart, which the executor prompt renders as the team channel.
	reqs := []setup.Requirement{
		{
			Field:   "bot_token",
			Connect: true,
			Label:   "Bot token",
			Kind:    setup.KindSecret,
			// RELATIVE TO THE SEAT, and to the RUNTIME seat: a
			// per-seat write goes through the org chart, whose blob
			// holds the shape org.Role declares, so this identity is
			// `slack` rather than `integrations.slack`. A seat is
			// addressed by its handle there, which is the subject its
			// own changes are arbitrated on.
			ConfigPath: "slack.bot_token",
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
			// THE MANIFEST WIZARD'S OWN STEPS, in the order somebody
			// walking it actually sees them.
			//
			// It named `OAuth & Permissions > Install to Workspace`, which
			// is the FROM-SCRATCH path: an app built by hand has its scopes
			// added on that page and is installed from it. An app created
			// from a manifest is installed by the wizard itself — pick a
			// workspace, Next, Create and Install, Allow — and lands on a
			// page where the token is under App Credentials. So the one
			// line on this form described a walk the form's own manifest
			// does not take, which is worse than no directions: it names
			// real pages, so somebody follows it and concludes the value is
			// missing rather than that they are in the wrong place.
			Help: "Navigate to [api.slack.com/apps](https://api.slack.com/apps) > " +
				"`Create New App > From an app manifest` and paste this agent's " +
				"manifest, then select your Slack workspace > `Next` > " +
				"`Create and Install` > `Allow` > `Your app credentials` " +
				"and copy the `Bot token` value here.",
			Blocks: integration.FindingIdentityMissing,
		},
		{
			Field:      "signing_secret",
			Connect:    true,
			Label:      "Signing secret",
			Kind:       setup.KindSecret,
			ConfigPath: "slack.signing_secret",
			Seat:       handle,
			Required:   true,
			Help: "Navigate to [api.slack.com/apps](https://api.slack.com/apps) > " +
				"this agent's app > `Basic Information > App Credentials` " +
				"and paste the `Signing Secret` here.",
			// SECOND, ALWAYS. The bot token's line is the one that creates
			// the app, so this one describes a page that exists only once
			// that has been followed.
			Blocks: integration.FindingCredentialMissing,
		},
	}

	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Held(botToken, resolve)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Held(signingSecret, resolve)
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
		// PRESELECTED, because a required-looking picker reading "Choose
		// one" over a field that already has an answer asks a question the
		// engine has already settled. It is the first choice for the same
		// reason it is the default.
		Default: string(config.StatusAlways),
		Help:    "Whether an agent shows that it is working before it answers.",
	}
	req.Present, req.Resolved, req.Stored = setup.Plain(status)
	return []setup.Requirement{req}
}

// statusChoices are the indicator modes, from the config package's own list so
// a value it accepts and a value this offers can never diverge.
func statusChoices() []setup.Choice {
	labels := map[config.WorkingStatus]string{
		"always":    "On every message it takes up",
		"addressed": "Only where somebody is waiting on this agent",
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

// ManageURL is one agent's app at Slack, where a person deletes it.
//
// DELETING AN APP IS NOT AN API CALL for this engine: apps.manifest.delete
// authenticates with an app-configuration token Slack issues by hand, which
// is the same credential the whole surface exists because an operator may not
// have. So a disconnect hands over a link.
//
// The APP ID, because it is the only thing that identifies one agent's app:
// two agents may carry the same display name, and nothing else on the seat
// names the app at all.
func ManageURL(appID string) string {
	id := strings.TrimSpace(appID)
	if id == "" {
		return ""
	}
	return "https://api.slack.com/apps/" + url.PathEscape(id)
}

// ManagePath is what a person clicks once [ManageURL] has opened.
//
// The last two steps rather than the whole journey: the link lands them on
// the app, and what is left is the control that removes it, at the foot of a
// page named nothing like "delete".
func ManagePath() string { return "Settings > Basic Information > Delete App" }

// Summary is the sentence the connect form opens with.
//
// WHAT THE READER HAS TO DO FIRST. It said the apps are created from the
// command line, which is true of `crewlet slack provision` and useless to
// somebody looking at this form: that command needs an app-configuration
// token Slack issues by hand and an organisation may decline to allow at
// all, so the person reading this is usually the person about to create the
// apps themselves.
func Summary() string {
	return "Each agent talks in Slack as its own app. Every agent below carries " +
		"the manifest its app is created from, which sets the scopes, the events " +
		"and that agent's own delivery address; install it, and its two values " +
		"are on the app's own pages."
}
