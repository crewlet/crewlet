"""Tests for the canonical per-agent Slack app manifest."""

from __future__ import annotations

from urllib.parse import parse_qs, urlsplit

from crewlet.slack.manifest import (
    BOT_EVENTS,
    BOT_SCOPES,
    build_agent_manifest,
    build_authorize_url,
    events_request_url,
    oauth_redirect_url,
)

BASE = "https://crewlet.example.com"


def test_manifest_shape() -> None:
    manifest = build_agent_manifest(
        role_name="Agent Engineer", handle="agent-engineer", base_url=BASE
    )
    assert manifest["display_information"]["name"] == "Agent Engineer"
    assert manifest["features"]["bot_user"]["display_name"] == "agent-engineer"
    assert manifest["oauth_config"]["scopes"]["bot"] == list(BOT_SCOPES)
    assert manifest["oauth_config"]["redirect_urls"] == [f"{BASE}/webhooks/slack-oauth"]
    events = manifest["settings"]["event_subscriptions"]
    assert events["request_url"] == f"{BASE}/webhooks/slack/agent-engineer"
    assert events["bot_events"] == list(BOT_EVENTS)
    assert manifest["settings"]["socket_mode_enabled"] is False


def test_scopes_cover_transport_and_mcp_tools() -> None:
    """The scope list must cover both credential consumers: the
    notification transport and every enabled Slack MCP tool."""
    required = {
        "chat:write",  # transport send()
        "app_mentions:read",  # app_mention subscription
        "channels:history",  # MCP conversations_history / replies
        "channels:read",  # MCP channels_list
        "reactions:write",  # MCP reactions_add / remove
        "usergroups:read",  # MCP usergroups_list / me
        "usergroups:write",  # MCP usergroups_create / update
        "users:read",  # MCP users_search
        "im:write",  # DM escalation to human seats
        # slack-mcp-server's startup channel-cache refresh is a single
        # conversations.list across ALL four conversation types — one
        # missing scope and the bot sees zero channels.
        "groups:read",
        "groups:history",
        "mpim:read",
        "mpim:history",
    }
    assert required <= set(BOT_SCOPES)
    # The plain search:read is user-token-only — a manifest listing it
    # under bot scopes is invalid; the bot-token variant is required.
    assert "search:read" not in BOT_SCOPES
    assert "search:read.public" in BOT_SCOPES


def test_bot_events_cover_every_transport_conversation_type() -> None:
    """One message.* event per conversation type SlackTransport handles
    (channel/group/im/mpim) — without message.groups/message.mpim a bot
    in a private channel or group DM never woke on non-mention replies,
    even though the scopes granted read access."""
    assert set(BOT_EVENTS) == {
        "app_mention",
        "message.channels",
        "message.groups",
        "message.im",
        "message.mpim",
    }


def test_app_name_truncated_to_slack_limit() -> None:
    manifest = build_agent_manifest(role_name="A" * 60, handle="a", base_url=BASE)
    assert len(manifest["display_information"]["name"]) == 35


def test_url_builders_strip_trailing_slash() -> None:
    assert events_request_url(BASE + "/", "dev") == f"{BASE}/webhooks/slack/dev"
    assert oauth_redirect_url(BASE + "/") == f"{BASE}/webhooks/slack-oauth"


def test_authorize_url_carries_scopes_redirect_and_state() -> None:
    url = build_authorize_url(
        client_id="123.456",
        redirect_url=f"{BASE}/webhooks/slack-oauth",
        state="agent-ceo",
    )
    parts = urlsplit(url)
    assert parts.scheme == "https"
    assert parts.netloc == "slack.com"
    assert parts.path == "/oauth/v2/authorize"
    query = parse_qs(parts.query)
    assert query["client_id"] == ["123.456"]
    assert query["state"] == ["agent-ceo"]
    assert query["redirect_uri"] == [f"{BASE}/webhooks/slack-oauth"]
    assert query["scope"] == [",".join(BOT_SCOPES)]
