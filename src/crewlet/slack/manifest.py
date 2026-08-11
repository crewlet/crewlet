"""Canonical Slack app manifest for a Crewlet agent.

The single source of truth for what a Crewlet agent's Slack app looks
like: bot scopes, event subscriptions, and the per-handle webhook URL.
Both the automated provisioning (``crewlet slack provision``) and the
manual-setup documentation (``docs/integrations/slack.md``) derive from
the lists here — when a new Slack MCP tool needs another scope, add it
to :data:`BOT_SCOPES` and re-run the provisioner (``apps.manifest.update``
pushes the change to every app).
"""

from __future__ import annotations

from typing import Any
from urllib.parse import quote

#: Bot token scopes every Crewlet agent app gets.  Two consumers share
#: the token (see ``docs/integrations/slack.md``): the notification
#: transport (``chat.postMessage`` + ``auth.test``) and the Slack MCP
#: server (korotovsky/slack-mcp-server) whose enabled tools
#: (``SLACK_MCP_ENABLED_TOOLS``) require the usergroup / user-read
#: scopes below.  The ``groups:*`` / ``mpim:*`` scopes are REQUIRED
#: even if agents never join private channels: slack-mcp-server's
#: startup channel-cache refresh is one hard-coded ``conversations.list``
#: call across all four conversation types, and a single missing scope
#: fails the whole call — the bot then sees zero channels.
BOT_SCOPES: tuple[str, ...] = (
    "app_mentions:read",  # app_mention events (thread-follow trigger)
    "channels:history",  # read public-channel messages (threading + MCP history)
    "channels:read",  # list public channels (MCP channels_list)
    "chat:write",  # send messages (transport send() + MCP add_message)
    "files:read",  # read shared files
    "groups:history",  # read private-channel messages
    "groups:read",  # list private channels (channel-cache refresh, see above)
    "im:history",  # read DM history
    "im:read",  # read direct messages
    "im:write",  # DM humans (escalation to contact.slack_user_id)
    "mpim:history",  # read group-DM messages
    "mpim:read",  # list group DMs (channel-cache refresh, see above)
    "reactions:write",  # MCP reactions_add / reactions_remove
    "search:read.public",  # bot-token search scope (search:read is user-token-only)
    "usergroups:read",  # MCP usergroups_list / usergroups_me
    "usergroups:write",  # MCP usergroups_create / update / users_update
    "users:read",  # MCP users_search + sender attribution
)

#: Events every agent app subscribes to — one ``message.*`` event per
#: conversation type the transport handles (``SlackTransport`` routes
#: channel_type ``channel``/``group``/``im``/``mpim``), so a bot invited
#: to a private channel or group DM wakes on non-mention messages and
#: thread replies exactly like in public channels.  ``app_mention`` is
#: subscribed alongside so a mention follows its thread even when the
#: message event loses the dedup race (the transport dedups the double
#: delivery by ``handle:channel:ts``).
BOT_EVENTS: tuple[str, ...] = (
    "app_mention",
    "message.channels",
    "message.groups",
    "message.im",
    "message.mpim",
)

#: Where the OAuth install redirects after the operator approves.  The
#: API serves a landing page here that displays the temporary code for
#: pasting back into ``crewlet slack provision``.  Deliberately OUTSIDE
#: the ``/webhooks/slack/{handle}`` namespace so no agent handle can
#: ever collide with it (``oauth`` is a perfectly valid handle slug).
OAUTH_CALLBACK_PATH = "/webhooks/slack-oauth"

# Slack manifest field limits (docs.slack.dev/reference/app-manifest).
_APP_NAME_MAX = 35
_DESCRIPTION_MAX = 140


def events_request_url(base_url: str, handle: str) -> str:
    """The per-agent Events API request URL served by the Crewlet API."""
    return f"{base_url.rstrip('/')}/webhooks/slack/{handle}"


def oauth_redirect_url(base_url: str) -> str:
    """The OAuth redirect URL (shared by every agent app)."""
    return f"{base_url.rstrip('/')}{OAUTH_CALLBACK_PATH}"


def build_agent_manifest(
    *,
    role_name: str,
    handle: str,
    base_url: str,
) -> dict[str, Any]:
    """Build the app manifest for one agent.

    Args:
        role_name: The seat's display name (``role.name``) — becomes the
            app name, truncated to Slack's 35-char limit.
        handle: The agent's canonical handle — becomes the bot display
            name (already handle-safe: lowercase alphanumerics + hyphens)
            and the webhook path segment.
        base_url: Public HTTPS base URL of the Crewlet API server
            (e.g. ``https://crewlet.example.com``).
    """
    description = f"Crewlet agent @{handle}"[:_DESCRIPTION_MAX]
    return {
        "display_information": {
            "name": role_name[:_APP_NAME_MAX],
            "description": description,
        },
        "features": {
            "bot_user": {
                "display_name": handle,
                "always_online": True,
            },
        },
        "oauth_config": {
            "redirect_urls": [oauth_redirect_url(base_url)],
            "scopes": {"bot": list(BOT_SCOPES)},
        },
        "settings": {
            "event_subscriptions": {
                "request_url": events_request_url(base_url, handle),
                "bot_events": list(BOT_EVENTS),
            },
            "org_deploy_enabled": False,
            "socket_mode_enabled": False,
            "token_rotation_enabled": False,
        },
    }


def build_authorize_url(
    *,
    client_id: str,
    redirect_url: str,
    state: str = "",
) -> str:
    """The ``oauth/v2/authorize`` URL the operator opens to install an app.

    ``state`` carries the agent handle so the landing page can say which
    agent the pasted code belongs to.
    """
    scope = quote(",".join(BOT_SCOPES), safe=",")
    url = (
        "https://slack.com/oauth/v2/authorize"
        f"?client_id={quote(client_id, safe='')}"
        f"&scope={scope}"
        f"&redirect_uri={quote(redirect_url, safe='')}"
    )
    if state:
        url += f"&state={quote(state, safe='')}"
    return url
