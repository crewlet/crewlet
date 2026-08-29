package slack

import (
	"encoding/json"
	"net/url"
	"strings"
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
// TWO CONSUMERS SHARE THE TOKEN: the notification transport
// (chat.postMessage, auth.test, assistant.threads.setStatus) and the Slack
// MCP server the agent's tools run against. Most of the list is the second
// one's.
//
// The groups:* and mpim:* scopes are required EVEN IF agents never join a
// private channel. The MCP server's startup channel-cache refresh is one
// conversations.list call across all four conversation types, and a single
// missing scope fails the whole call — the bot then sees no channels at all,
// which looks like an empty workspace rather than a missing scope.
var BotScopes = []string{
	"app_mentions:read",  // app_mention events — the thread-follow trigger
	"channels:history",   // read public-channel messages
	"channels:read",      // list public channels
	"chat:write",         // post, and set the working indicator
	"files:read",         // read shared files
	"groups:history",     // read private-channel messages
	"groups:read",        // list private channels — see the cache note above
	"im:history",         // read DM history
	"im:read",            // read direct messages
	"im:write",           // DM a human, for an escalation
	"mpim:history",       // read group-DM messages
	"mpim:read",          // list group DMs — see the cache note above
	"reactions:write",    // react to a message
	"search:read.public", // the BOT-token search scope; search:read is user-token only
	"usergroups:read",    // resolve a user group a message named
	"usergroups:write",   // manage a user group
	"users:read",         // resolve a sender, and look a colleague's id up
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
func Manifest(roleName, handle, base string) map[string]any {
	return map[string]any{
		"display_information": map[string]any{
			"name":        truncate(roleName, appNameMax),
			"description": truncate("Crewlet agent @"+handle, descriptionMax),
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
	}
}

// AuthorizeURL is where the operator clicks to install one app.
//
// The HANDLE rides as `state`, because the landing page has to say which
// agent an install belongs to — the code Slack returns names nothing, and an
// operator provisioning seven agents would otherwise be pasting seven
// indistinguishable codes.
func AuthorizeURL(clientID, base, handle string) string {
	params := url.Values{
		"client_id":    {clientID},
		"scope":        {strings.Join(BotScopes, ",")},
		"redirect_uri": {OAuthRedirectURL(base)},
		"state":        {handle},
	}
	return "https://slack.com/oauth/v2/authorize?" + params.Encode()
}

// Fingerprint is a stable digest of a manifest, for skipping an update that
// would change nothing.
//
// The manifest methods are Slack's slowest rate class, and a company of
// seven agents re-running provisioning issues seven of them back to back —
// so a re-run that pushes an unchanged manifest spends minutes waiting out
// 429s to achieve nothing.
func Fingerprint(manifest map[string]any) string {
	// json.Marshal sorts map keys, so the encoding is stable across runs
	// and across Go versions — which is the whole requirement.
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return ""
	}
	return digest(encoded)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
