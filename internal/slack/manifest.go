package slack

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// The canonical app manifest for a Crewlet agent.
//
// The single source of truth for what an agent's Slack app looks like: bot
// scopes, event subscriptions and the per-handle request URL. Provisioning
// pushes it and the integration page documents it, both from here — so a new
// Slack MCP tool that needs another scope is one edit and a re-run, rather
// than a scope added to a document nothing enforces.

// BotScopes are the token scopes every agent app gets.
//
// TWO CONSUMERS SHARE THE TOKEN: the notification transport (auth.test,
// assistant.threads.setStatus) and the Slack MCP server the agent's tools
// run against. Almost the whole list is the second one's — the transport
// resolves an identity and raises an indicator, and every message an agent
// sends is the MCP server's call on this same token.
//
// The groups:* and mpim:* scopes are required EVEN IF agents never join a
// private channel. The MCP server's startup channel-cache refresh is one
// conversations.list call across all four conversation types, and a single
// missing scope fails the whole call — with no cache left by an earlier run
// the server then exits at startup, taking every Slack tool the seat has with
// it, and the cause is in its stderr rather than in anything Slack reports.
//
// NOTHING HERE IS FOR A CALL NO CONSUMER MAKES, which is why
// search:read.public is absent although it reads like the bot-token search
// scope: the MCP server registers its one search tool only for a user token
// (Slack's search.messages refuses a bot's) and the transport searches
// nothing, so the scope would grant every app a read of the workspace's
// public messages that no call uses.
var BotScopes = []string{
	"app_mentions:read", // app_mention events — the thread-follow trigger
	"channels:history",  // read public-channel messages
	"channels:read",     // list public channels
	"chat:write",        // MCP posts with it; the working indicator needs it too
	"files:read",        // read shared files
	"groups:history",    // read private-channel messages
	"groups:read",       // list private channels — see the cache note above
	"im:history",        // read DM history
	"im:read",           // read direct messages
	"im:write",          // DM a human, for an escalation
	"mpim:history",      // read group-DM messages
	"mpim:read",         // list group DMs — see the cache note above
	"reactions:write",   // react to a message
	"usergroups:read",   // resolve a user group a message named
	"usergroups:write",  // manage a user group
	"users:read",        // resolve a sender, and look a colleague's id up
}

// BotEvents are the events every agent app subscribes to.
//
// ONE message.* EVENT PER CONVERSATION TYPE, so a bot invited to a private
// channel or a group DM wakes on ordinary messages and thread replies
// exactly as it does in a public one. app_mention is subscribed ALONGSIDE
// them deliberately: it is the only event that survives when the message
// event loses the delivery-id race, and the edge collapses the double
// delivery by Slack's own event id.
var BotEvents = []string{
	"app_mention",
	"message.channels",
	"message.groups",
	"message.im",
	"message.mpim",
}

// OAuthCallbackPath is where an install redirects after the operator
// approves.
//
// Deliberately OUTSIDE the /webhooks/slack/{handle} namespace, because
// "oauth" is a perfectly valid seat handle and a collision would make one
// agent's request URL swallow every install.
const OAuthCallbackPath = "/webhooks/slack-oauth"

// Slack's own manifest field limits.
const (
	appNameMax     = 35
	descriptionMax = 140
)

// EventsRequestURL is the per-agent Events API request URL.
func EventsRequestURL(base, handle string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/") + "/webhooks/slack/" + handle
}

// OAuthRedirectURL is the install redirect, shared by every agent app.
func OAuthRedirectURL(base string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/") + OAuthCallbackPath
}

// Manifest builds the app manifest for one agent.
//
// The APP is named for the role and the BOT for the handle, which is not
// interchangeable: the app name is what an operator sees in the workspace's
// app directory ("Engineering Lead"), and the bot display name is what
// appears beside every message it posts — and it has to be the handle,
// because that is the identity the rest of the engine addresses.
//
// AN OVER-LONG NAME IS REFUSED, not truncated. Slack's limits are real, but
// silently cutting to them puts an app in the operator's workspace under a
// name their config does not contain — and two roles sharing a 35-character
// prefix become two apps a person cannot tell apart. The operator can shorten
// a role name; only they can decide which words to lose.
func Manifest(roleName, handle, base string) (map[string]any, error) {
	description := "Crewlet agent @" + handle
	switch {
	case utf8.RuneCountInString(roleName) > appNameMax:
		return nil, fmt.Errorf(
			"slack: role name %q is %d characters and Slack caps an app name at %d — "+
				"shorten the role's `name` in the company config",
			roleName, utf8.RuneCountInString(roleName), appNameMax)
	case utf8.RuneCountInString(description) > descriptionMax:
		return nil, fmt.Errorf(
			"slack: handle %q makes a %d-character app description and Slack caps it "+
				"at %d — shorten the seat's handle",
			handle, utf8.RuneCountInString(description), descriptionMax)
	}
	return map[string]any{
		"display_information": map[string]any{
			"name":        roleName,
			"description": description,
		},
		"features": map[string]any{
			"bot_user": map[string]any{
				"display_name":  handle,
				"always_online": true,
			},
		},
		"oauth_config": map[string]any{
			"redirect_urls": []string{OAuthRedirectURL(base)},
			"scopes":        map[string]any{"bot": BotScopes},
		},
		"settings": map[string]any{
			"event_subscriptions": map[string]any{
				"request_url": EventsRequestURL(base, handle),
				"bot_events":  BotEvents,
			},
			// A Crewlet app is workspace-scoped, receives events over
			// HTTP rather than a socket, and holds a token that does
			// not rotate — each of which the engine depends on: org
			// deploy would change the id shape, socket mode would mean
			// nothing arrives at the request URL, and token rotation
			// would expire every seat's credential in twelve hours
			// with nothing to refresh it.
			"org_deploy_enabled":     false,
			"socket_mode_enabled":    false,
			"token_rotation_enabled": false,
		},
	}, nil
}

// ManifestJSON is [Manifest] as the text a person pastes into Slack.
//
// INDENTED, because it is read as well as pasted: an operator comparing what
// they are about to create against what the app already has is reading a
// diff, and one line of JSON is not one a person can diff. Slack accepts
// either.
//
// It exists so the setup screen and the provisioning command share one
// definition of what an agent's app IS. Written twice, the screen's copy
// would drift from the one the command pushes, and the difference between
// them is a scope or an event: a bot that reports success and hears nothing,
// or one that sees an empty workspace.
func ManifestJSON(roleName, handle, base string) (string, error) {
	manifest, err := Manifest(roleName, handle, base)
	if err != nil {
		return "", err
	}
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("slack: render %s's manifest: %w", handle, err)
	}
	return string(out), nil
}

// AuthorizeURL is where a person approves one app's install, carrying a
// state the caller made.
//
// The state is what proves the redirect belongs to an install this engine
// began, for that seat, recently ([Installs.Link] signs it): the landing
// exchanges the code and seals a bot token into a seat, so nothing else may
// ride there.
func AuthorizeURL(clientID, base, state string) string {
	params := url.Values{
		"client_id":    {clientID},
		"scope":        {strings.Join(BotScopes, ",")},
		"redirect_uri": {OAuthRedirectURL(base)},
		"state":        {state},
	}
	return "https://slack.com/oauth/v2/authorize?" + params.Encode()
}
