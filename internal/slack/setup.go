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
// # Where the apps come from
//
// With an app-configuration token on the company block, the reconcile loop
// creates and keeps every agent's app itself (D11), and a person only
// approves each install from the Agents tab. Without one, a company creates
// each app by hand from the manifest this surface shows, and these fields take
// the two values Slack shows on the app's own page, as credentials handed in
// from outside (D4b).
//
// The webhook URL is not a field for the same reason it is not one anywhere
// else: it is `<webhook base>/webhooks/slack/<handle>`, built on the Tier A
// address a vendor reaches this deployment at ([config.API.WebhookBase] —
// `api.public.external_url` behind a public listener, `api.external_url`
// otherwise), which the surface shows to copy rather than inviting somebody to
// type a different one.

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
			// RELATIVE TO THE SEAT. A seat is addressed by its handle
			// through the entity route, because a merge patch cannot
			// reach one element of a list without replacing the list.
			ConfigPath: "integrations.slack.bot_token",
			Seat:       handle,
			Required:   true,
			Checkable:  true,
			// The hint says where the token comes from; More walks the manifest
			// wizard's own steps, each path set apart as a literal to follow.
			Hint: "From this agent's app, once it is installed.",
			More: "At [api.slack.com/apps](https://api.slack.com/apps), choose " +
				"`Create New App > From an app manifest` and paste this agent's manifest. " +
				"Then choose your workspace, `Next`, `Create and Install` and `Allow`, and " +
				"copy the `Bot token` from `Your app credentials` into this box.",
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
			// NOT CHECKABLE AHEAD. Slack never presents a signing secret;
			// it signs each event with it, and the route proves the match.
			CheckNote: "Slack proves it with the first event it signs for this agent.",
			Hint:      "Under `Basic Information > App Credentials`, in this agent's app.",
			More: "Open this agent's app at [api.slack.com/apps](https://api.slack.com/apps), " +
				"and paste its `Signing Secret` here.",
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

// CompanyRequirements is what the company-wide Slack block carries: the
// app-configuration token the engine creates and keeps every agent's app with
// (D11), and whether an agent shows a working indicator while it thinks.
// Everything that authenticates an agent is per seat.
func CompanyRequirements(in *config.Slack, resolve func(string) (string, bool)) []setup.Requirement {
	var status, token string
	if in != nil {
		status, token = string(in.TypingStatus), in.AppConfigToken
	}
	configToken := setup.Requirement{
		Field:      "app_config_token",
		Connect:    true,
		Label:      "App configuration token",
		Kind:       setup.KindSecret,
		ConfigPath: "integrations.slack.app_config_token",
		// SEALED UNDER THE ONE NAME the engine rotates it under: the pair
		// Slack answers each rotation with is saved back there as one value
		// ([EncodeConfigToken]), and the refresh token pasted here is the
		// first of them.
		SecretName: ConfigTokenVar,
		Required:   false,
		// NOT CHECKED AHEAD: the only way to try a refresh token is to
		// rotate it, which spends it, so a check would be the first use.
		CheckNote: "Slack proves it when the next sync rotates it and reads an agent's app.",
		Hint:      "Lets Crewlet create and update each agent's app.",
		More: "Paste the refresh token of an [app configuration token]" +
			"(https://api.slack.com/reference/manifests#config-tokens). Without it, you create " +
			"each app by hand from its manifest.",
	}
	configToken.Present, configToken.Resolved, configToken.Stored = setup.Held(token, resolve)
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
		Hint:    "Whether an agent shows that it is working before it answers.",
	}
	req.Present, req.Resolved, req.Stored = setup.Plain(status)
	return []setup.Requirement{configToken, req}
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
// It is where a person deletes an app the engine cannot: one created by hand
// in a company with no app-configuration token, or one whose token Slack
// refused. With a usable token the engine deletes the app itself
// (apps.manifest.delete).
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

// Guide is what the connect form says around its fields: what the reader has
// to do first, which depends on whether they hold an app configuration token.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Each agent talks in Slack as its own app. With an app configuration token, Crewlet " +
			"creates the apps and you approve each install.",
		Prepare: []setup.Step{
			{
				Text:   "Optionally, create an app configuration token so Crewlet makes each agent's app.",
				Yields: "app_config_token",
			},
			{Text: "Without one, create each agent's app from the manifest in its block below."},
			{Text: "Install each app, and paste its Bot token and Signing secret.", Yields: "bot_token"},
		},
	}
}
