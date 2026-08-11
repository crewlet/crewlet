"""Tests for SlackTransport."""

import hashlib
import hmac
import json
import time
from typing import Any
from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest
import pytest_asyncio

from crewlet.agent.pool import AgentPool
from crewlet.notifications.handle import HandleRegistry
from crewlet.notifications.protocol import (
    InboundNotification,
    OutboundMessage,
)
from crewlet.notifications.transports.slack import (
    SlackAppConfig,
    SlackTransport,
)
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.queue.memory import MemoryEventQueue


class _MemoryThreadFollowRepo:
    """In-memory repo for tests."""

    def __init__(self) -> None:
        self._store: dict[tuple[str, str, str], str] = {}

    async def upsert(
        self, handle: str, channel: str, thread_ts: str, reason: str
    ) -> None:
        self._store[(handle, channel, thread_ts)] = reason

    async def is_following(
        self, handle: str, channel: str, thread_ts: str
    ) -> str | None:
        return self._store.get((handle, channel, thread_ts))


def _make_registry_parts():
    """Create org, queue, pool for building a HandleRegistry."""
    org = Organization(
        name="TestCo",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Engineer",
                roles=[
                    Role(name="Engineer", email="alice@test.com"),
                ],
            )
        ],
    )
    queue = MemoryEventQueue()
    pool = AgentPool(queue)
    return org, queue, pool


@pytest_asyncio.fixture
async def registry():
    org, queue, pool = _make_registry_parts()
    await queue.start()
    await pool.spawn_from_org(org)
    reg = HandleRegistry(pool)
    yield reg
    await queue.stop()


_TEST_SIGNING_SECRET = "test_signing_secret"


def _make_slack_transport(**app_kwargs) -> SlackTransport:
    """Create a SlackTransport with a single registered agent app."""
    app_kwargs.setdefault("signing_secret", _TEST_SIGNING_SECRET)
    transport = SlackTransport(thread_follow_repo=_MemoryThreadFollowRepo())
    transport.register_app("engineer", SlackAppConfig(**app_kwargs))
    return transport


def _make_signature(secret: str, timestamp: str, body: str) -> str:
    sig_basestring = f"v0:{timestamp}:{body}"
    return (
        "v0="
        + hmac.new(
            secret.encode("utf-8"),
            sig_basestring.encode("utf-8"),
            hashlib.sha256,
        ).hexdigest()
    )


def _signed_event_kwargs(
    body: dict[str, Any],
    secret: str = _TEST_SIGNING_SECRET,
) -> dict[str, Any]:
    """Build headers and body_raw for a signed Slack event."""
    body_raw = json.dumps(body)
    ts = str(int(time.time()))
    sig = _make_signature(secret, ts, body_raw)
    return {
        "body_raw": body_raw,
        "headers": {
            "x-slack-request-timestamp": ts,
            "x-slack-signature": sig,
        },
    }


class TestSlackTransport:
    @pytest.mark.asyncio
    async def test_start_and_stop(self):
        transport = _make_slack_transport(bot_token="xoxb-test")

        await transport.start()
        assert transport._client is not None
        await transport.stop()
        assert transport._client is None

    @pytest.mark.asyncio
    async def test_handle_event_after_stop_returns_none(self):
        """handle_event returns None after stop()."""
        transport = _make_slack_transport(bot_token="xoxb-test")

        await transport.start()
        await transport.stop()

        result = await transport.handle_event(
            {
                "type": "event_callback",
                "event": {
                    "type": "message",
                    "user": "U1",
                    "text": "hello",
                    "channel": "C1",
                },
            },
            handle="engineer",
        )
        assert result is None

    @pytest.mark.asyncio
    async def test_handle_url_verification(self):
        transport = _make_slack_transport()

        await transport.start()

        result = await transport.handle_event(
            {"type": "url_verification", "challenge": "abc123"},
            handle="engineer",
        )
        assert result == {"challenge": "abc123"}
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_message_event(self):
        transport = _make_slack_transport()

        await transport.start()

        body = {
            "type": "event_callback",
            "team_id": "T123",
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "Hey engineer, check this",
                "channel": "C_ENG_CHANNEL",
                "ts": "1234.5678",
            },
        }
        result = await transport.handle_event(
            body,
            handle="engineer",
            **_signed_event_kwargs(body),
        )

        assert isinstance(result, InboundNotification)
        assert result.source == "slack"
        assert result.recipient_handle == "engineer"
        assert result.body == "Hey engineer, check this"
        assert result.metadata["channel"] == "C_ENG_CHANNEL"
        assert result.metadata["ts"] == "1234.5678"
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_own_app_message_ignored(self):
        """Messages from our own app_id are skipped to prevent reply loops."""
        transport = _make_slack_transport()

        await transport.start()

        # Event where event.app_id matches top-level api_app_id
        own_body = {
            "type": "event_callback",
            "api_app_id": "A_OUR_APP",
            "event": {
                "type": "message",
                "user": "U_BOT_USER",
                "app_id": "A_OUR_APP",
                "text": "I am the CEO",
                "channel": "C123",
                "ts": "5555.0001",
            },
        }
        result = await transport.handle_event(
            own_body,
            handle="engineer",
            **_signed_event_kwargs(own_body),
        )

        assert result is None

        # Event from a different app — should be delivered
        other_body = {
            "type": "event_callback",
            "api_app_id": "A_OUR_APP",
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "hello from user",
                "channel": "C123",
                "ts": "5555.0002",
            },
        }
        result = await transport.handle_event(
            other_body,
            handle="engineer",
            **_signed_event_kwargs(other_body),
        )

        assert isinstance(result, InboundNotification)
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_non_message_event(self):
        transport = _make_slack_transport()

        await transport.start()

        body = {
            "type": "event_callback",
            "event": {"type": "reaction_added"},
        }
        result = await transport.handle_event(
            body,
            handle="engineer",
            **_signed_event_kwargs(body),
        )
        assert result is None
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_message_changed_skipped(self):
        """Edits (incl. Slack's link-unfurl edits) are bookkeeping, not
        messages — their envelope has no top-level user/text and must
        not wake the agent with an empty notification."""
        transport = _make_slack_transport()

        await transport.start()

        body = {
            "type": "event_callback",
            "team_id": "T123",
            "event": {
                "type": "message",
                "subtype": "message_changed",
                "hidden": True,
                "channel": "C123",
                "ts": "9999.0002",
                "message": {
                    "type": "message",
                    "user": "U_HUMAN",
                    "text": "edited text",
                    "ts": "9998.0001",
                },
            },
        }
        result = await transport.handle_event(
            body,
            handle="engineer",
            **_signed_event_kwargs(body),
        )
        assert result is None
        assert "bookkeeping" in transport.last_skip_reason
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_message_deleted_skipped(self):
        transport = _make_slack_transport()

        await transport.start()

        body = {
            "type": "event_callback",
            "event": {
                "type": "message",
                "subtype": "message_deleted",
                "hidden": True,
                "channel": "C123",
                "ts": "9999.0003",
                "deleted_ts": "9998.0001",
            },
        }
        result = await transport.handle_event(
            body,
            handle="engineer",
            **_signed_event_kwargs(body),
        )
        assert result is None
        assert "bookkeeping" in transport.last_skip_reason
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_hidden_event_without_subtype_skipped(self):
        """``message_replied`` thread bookkeeping arrives WITHOUT its
        subtype (documented Slack bug) — ``hidden: true`` plus the
        nested ``message`` object is its only shape.  It fires on every
        thread reply in the channel and must never wake an agent."""
        transport = _make_slack_transport()

        await transport.start()

        body = {
            "type": "event_callback",
            "event": {
                "type": "message",
                "hidden": True,
                "channel": "C123",
                "ts": "9999.0004",
                "message": {
                    "type": "message",
                    "user": "U_HUMAN",
                    "text": "the parent message",
                    "ts": "9990.0001",
                    "thread_ts": "9990.0001",
                    "reply_count": 1,
                },
            },
        }
        result = await transport.handle_event(
            body,
            handle="engineer",
            **_signed_event_kwargs(body),
        )
        assert result is None
        assert "bookkeeping" in transport.last_skip_reason
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_channel_join_skipped(self):
        """System lines (join/leave/topic) carry text but are about the
        channel, not addressed to anyone — skipped by subtype."""
        transport = _make_slack_transport()

        await transport.start()

        body = {
            "type": "event_callback",
            "event": {
                "type": "message",
                "subtype": "channel_join",
                "user": "U_NEW",
                "text": "<@U_NEW> has joined the channel",
                "channel": "C123",
                "ts": "9999.0005",
            },
        }
        result = await transport.handle_event(
            body,
            handle="engineer",
            **_signed_event_kwargs(body),
        )
        assert result is None
        assert "channel_join" in transport.last_skip_reason
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_bot_message_sender_fallback(self):
        """Legacy ``bot_message`` events (incoming webhooks, workflow
        bots) have no ``user`` — the sender falls back to username /
        bot_id while the ``user`` METADATA key stays the raw (empty)
        id for the learning subsystem."""
        transport = _make_slack_transport()

        await transport.start()

        body = {
            "type": "event_callback",
            "event": {
                "type": "message",
                "subtype": "bot_message",
                "username": "CI Bot",
                "bot_id": "B999",
                "text": "build failed on main",
                "channel": "C123",
                "ts": "9999.0006",
            },
        }
        result = await transport.handle_event(
            body,
            handle="engineer",
            **_signed_event_kwargs(body),
        )
        assert isinstance(result, InboundNotification)
        assert result.sender == "CI Bot"
        assert result.body == "build failed on main"
        assert result.metadata["user"] == ""
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_file_share_without_comment_renders_files(self):
        """A file share posted without a comment has empty ``text`` but
        real content — the body renders the file names instead of
        arriving blank."""
        transport = _make_slack_transport()

        await transport.start()

        body = {
            "type": "event_callback",
            "event": {
                "type": "message",
                "subtype": "file_share",
                "user": "U_HUMAN",
                "text": "",
                "files": [{"name": "report.pdf"}, {"title": "diagram"}],
                "channel": "C123",
                "ts": "9999.0007",
            },
        }
        result = await transport.handle_event(
            body,
            handle="engineer",
            **_signed_event_kwargs(body),
        )
        assert isinstance(result, InboundNotification)
        assert result.body == "(shared file: report.pdf, diagram)"
        assert result.sender == "U_HUMAN"
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_thread_broadcast_delivered(self):
        """``thread_broadcast`` is a real message (a reply also sent to
        the channel) and passes the subtype allowlist."""
        transport = _make_slack_transport()

        await transport.start()

        body = {
            "type": "event_callback",
            "authorizations": [{"user_id": "U_BOT"}],
            "event": {
                "type": "message",
                "subtype": "thread_broadcast",
                "user": "U_HUMAN",
                "text": "heads up <@U_BOT>, see thread",
                "channel": "C123",
                "ts": "9999.0008",
                "thread_ts": "9990.0001",
            },
        }
        result = await transport.handle_event(
            body,
            handle="engineer",
            **_signed_event_kwargs(body),
        )
        assert isinstance(result, InboundNotification)
        assert result.body == "heads up <@U_BOT>, see thread"
        await transport.stop()

    @pytest.mark.asyncio
    async def test_send_no_app_returns_false(self):
        """Send returns False when no app is registered for the handle."""
        transport = SlackTransport()

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="C123",
            body="Hello",
        )
        result = await transport.send(msg)
        assert result is False
        await transport.stop()

    @pytest.mark.asyncio
    async def test_send_no_channel_returns_false(self):
        transport = _make_slack_transport(bot_token="xoxb-test")

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="",
            body="Hello",
        )
        result = await transport.send(msg)
        assert result is False
        await transport.stop()

    @pytest.mark.asyncio
    async def test_send_uses_reply_to_metadata_channel(self):
        """Falls back to reply_to_metadata channel when recipient is empty."""
        transport = _make_slack_transport(bot_token="xoxb-test")

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="",
            body="Replying in thread",
            reply_to_metadata={
                "channel": "C_ORIGINAL",
                "ts": "1234.5678",
            },
        )
        # Will fail at HTTP level but channel resolves from metadata
        result = await transport.send(msg)
        assert result is False
        await transport.stop()

    @pytest.mark.asyncio
    async def test_send_uses_recipient(self):
        """Explicit recipient field is used for channel."""
        transport = _make_slack_transport(bot_token="xoxb-test")

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="C_DIRECT_CHANNEL",
            body="Hello",
        )
        # Will fail at HTTP level but gets past channel resolution
        result = await transport.send(msg)
        assert result is False
        await transport.stop()

    @pytest.mark.asyncio
    async def test_send_success_with_mock(self):
        """Send succeeds when Slack API returns ok=true."""
        transport = _make_slack_transport(bot_token="xoxb-test")

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="C123",
            body="Hello!",
        )

        mock_response = MagicMock()
        mock_response.json.return_value = {"ok": True}
        mock_post = AsyncMock(return_value=mock_response)
        with patch.object(transport._client, "post", mock_post):
            result = await transport.send(msg)
            assert result is True
            mock_post.assert_called_once()
            call_kwargs = mock_post.call_args
            assert call_kwargs[1]["json"]["channel"] == "C123"
            assert call_kwargs[1]["json"]["text"] == "Hello!"
            assert "thread_ts" not in call_kwargs[1]["json"]

        await transport.stop()

    @pytest.mark.asyncio
    async def test_send_threaded_reply_with_mock(self):
        """Send includes thread_ts when reply_to_metadata has ts."""
        transport = _make_slack_transport(bot_token="xoxb-test")

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="C123",
            body="Threaded reply",
            reply_to_metadata={"ts": "1234.5678", "channel": "C123"},
        )

        mock_response = MagicMock()
        mock_response.json.return_value = {"ok": True}
        mock_post = AsyncMock(return_value=mock_response)
        with patch.object(transport._client, "post", mock_post):
            result = await transport.send(msg)
            assert result is True
            payload = mock_post.call_args[1]["json"]
            assert payload["thread_ts"] == "1234.5678"

        await transport.stop()

    @pytest.mark.asyncio
    async def test_send_before_start_returns_false(self):
        """send() returns False when called before start()."""
        transport = _make_slack_transport(bot_token="xoxb-test")

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="C123",
            body="Hello",
        )
        result = await transport.send(msg)
        assert result is False

    @pytest.mark.asyncio
    async def test_send_slack_api_error(self):
        """Send returns False when Slack API returns error."""
        transport = _make_slack_transport(bot_token="xoxb-test")

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="C123",
            body="Hello",
        )

        mock_response = MagicMock()
        mock_response.json.return_value = {
            "ok": False,
            "error": "channel_not_found",
        }
        mock_response.raise_for_status = MagicMock()
        mock_post = AsyncMock(return_value=mock_response)
        with patch.object(transport._client, "post", mock_post):
            result = await transport.send(msg)
            assert result is False

        await transport.stop()

    @pytest.mark.asyncio
    async def test_send_http_error(self):
        """Send returns False on HTTP error (non-2xx)."""
        transport = _make_slack_transport(bot_token="xoxb-test")

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="C123",
            body="Hello",
        )

        mock_response = MagicMock()
        mock_response.raise_for_status.side_effect = httpx.HTTPStatusError(
            "Server Error",
            request=MagicMock(),
            response=MagicMock(status_code=500),
        )
        mock_post = AsyncMock(return_value=mock_response)
        with patch.object(transport._client, "post", mock_post):
            result = await transport.send(msg)
            assert result is False

        await transport.stop()


# --- Slack working status ("is thinking…") ---


class TestSlackSetStatus:
    """``assistant.threads.setStatus`` — the bot-visible typing indicator."""

    @pytest.mark.asyncio
    async def test_set_status_posts_to_slack(self):
        transport = _make_slack_transport(bot_token="xoxb-test")
        await transport.start()

        mock_response = MagicMock()
        mock_response.json.return_value = {"ok": True}
        mock_post = AsyncMock(return_value=mock_response)
        with patch.object(transport._client, "post", mock_post):
            result = await transport.set_status(
                "engineer", "C123", "1234.5678", "is thinking..."
            )
        assert result is True
        url = mock_post.call_args[0][0]
        assert url.endswith("/assistant.threads.setStatus")
        payload = mock_post.call_args[1]["json"]
        assert payload == {
            "channel_id": "C123",
            "thread_ts": "1234.5678",
            "status": "is thinking...",
        }
        headers = mock_post.call_args[1]["headers"]
        assert headers["Authorization"] == "Bearer xoxb-test"

        await transport.stop()

    @pytest.mark.asyncio
    async def test_set_status_clears_with_empty_string(self):
        transport = _make_slack_transport(bot_token="xoxb-test")
        await transport.start()

        mock_response = MagicMock()
        mock_response.json.return_value = {"ok": True}
        mock_post = AsyncMock(return_value=mock_response)
        with patch.object(transport._client, "post", mock_post):
            assert await transport.set_status("engineer", "C1", "1.0", "") is True
        assert mock_post.call_args[1]["json"]["status"] == ""

        await transport.stop()

    @pytest.mark.asyncio
    async def test_set_status_requires_channel_and_thread(self):
        transport = _make_slack_transport(bot_token="xoxb-test")
        await transport.start()
        assert await transport.set_status("engineer", "", "1.0", "x") is False
        assert await transport.set_status("engineer", "C1", "", "x") is False
        await transport.stop()

    @pytest.mark.asyncio
    async def test_set_status_unknown_handle_returns_false(self):
        transport = _make_slack_transport(bot_token="xoxb-test")
        await transport.start()
        assert await transport.set_status("nobody", "C1", "1.0", "x") is False
        await transport.stop()

    @pytest.mark.asyncio
    async def test_set_status_before_start_returns_false(self):
        transport = _make_slack_transport(bot_token="xoxb-test")
        assert await transport.set_status("engineer", "C1", "1.0", "x") is False

    @pytest.mark.asyncio
    async def test_set_status_slack_error_returns_false(self):
        transport = _make_slack_transport(bot_token="xoxb-test")
        await transport.start()

        mock_response = MagicMock()
        mock_response.json.return_value = {"ok": False, "error": "invalid_thread_ts"}
        mock_post = AsyncMock(return_value=mock_response)
        with patch.object(transport._client, "post", mock_post):
            assert await transport.set_status("engineer", "C1", "1.0", "x") is False

        await transport.stop()

    @pytest.mark.asyncio
    async def test_set_status_http_error_never_raises(self):
        transport = _make_slack_transport(bot_token="xoxb-test")
        await transport.start()

        mock_post = AsyncMock(side_effect=httpx.ConnectError("boom"))
        with patch.object(transport._client, "post", mock_post):
            assert await transport.set_status("engineer", "C1", "1.0", "x") is False

        await transport.stop()

    @pytest.mark.asyncio
    async def test_stop_clears_live_indicators_before_closing_the_client(self):
        """The clearing call goes through the HTTP client, so it must run
        while the client is still open."""
        transport = _make_slack_transport(bot_token="xoxb-test")
        await transport.start()

        mock_response = MagicMock()
        mock_response.json.return_value = {"ok": True}
        mock_post = AsyncMock(return_value=mock_response)
        with patch.object(transport._client, "post", mock_post):
            session = await transport.typing_status.begin(
                handle="engineer",
                turn_id="t1",
                metadata={
                    "transport": "slack",
                    "channel": "D1",
                    "channel_type": "im",
                    "ts": "1.0",
                },
            )
            assert session is not None
            await transport.stop()

        assert mock_post.call_args[1]["json"]["status"] == ""
        assert transport.typing_status.active_conversations == []


# --- Slack Signature Verification ---


class TestSlackSignatureVerification:
    def test_valid_signature(self):
        secret = "test_signing_secret"
        transport = SlackTransport()
        ts = str(int(time.time()))
        body = '{"type":"event_callback"}'
        sig = _make_signature(secret, ts, body)

        result = transport.verify_signature(
            body,
            {
                "x-slack-request-timestamp": ts,
                "x-slack-signature": sig,
            },
            signing_secret=secret,
        )
        assert result is True

    def test_invalid_signature(self):
        transport = SlackTransport()
        ts = str(int(time.time()))

        result = transport.verify_signature(
            '{"type":"event_callback"}',
            {
                "x-slack-request-timestamp": ts,
                "x-slack-signature": "v0=invalid_hex",
            },
            signing_secret="test_signing_secret",
        )
        assert result is False

    def test_missing_headers(self):
        transport = SlackTransport()
        assert transport.verify_signature("{}", {}, signing_secret="secret") is False

    def test_old_timestamp_rejected(self):
        secret = "test_signing_secret"
        transport = SlackTransport()
        ts = str(int(time.time()) - 600)
        body = '{"type":"event_callback"}'
        sig = _make_signature(secret, ts, body)

        result = transport.verify_signature(
            body,
            {
                "x-slack-request-timestamp": ts,
                "x-slack-signature": sig,
            },
            signing_secret=secret,
        )
        assert result is False

    def test_no_secret_rejects(self):
        transport = SlackTransport()
        assert transport.verify_signature("{}", {}, signing_secret="") is False

    @pytest.mark.asyncio
    async def test_handle_event_rejects_bad_signature(self):
        transport = SlackTransport()
        transport.register_app(
            "engineer",
            SlackAppConfig(signing_secret="test_secret"),
        )

        await transport.start()

        result = await transport.handle_event(
            {"type": "event_callback", "event": {"type": "message"}},
            handle="engineer",
            headers={
                "x-slack-request-timestamp": str(int(time.time())),
                "x-slack-signature": "v0=bad",
            },
            body_raw='{"type":"event_callback","event":{"type":"message"}}',
        )
        assert result is None
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_event_accepts_valid_signature(self):
        """handle_event processes message when signature is valid."""
        secret = "test_secret"
        transport = SlackTransport()
        transport.register_app(
            "engineer",
            SlackAppConfig(signing_secret=secret),
        )

        await transport.start()

        body_raw = (
            '{"type":"event_callback","event":'
            '{"type":"message","user":"U1","text":"hi","channel":"C1"}}'
        )
        ts = str(int(time.time()))
        sig = _make_signature(secret, ts, body_raw)

        result = await transport.handle_event(
            json.loads(body_raw),
            handle="engineer",
            headers={
                "x-slack-request-timestamp": ts,
                "x-slack-signature": sig,
            },
            body_raw=body_raw,
        )
        assert isinstance(result, InboundNotification)
        assert result.body == "hi"
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_event_rejects_missing_headers(self):
        """With signing secret, missing headers causes rejection."""
        transport = SlackTransport()
        transport.register_app(
            "engineer",
            SlackAppConfig(signing_secret="test_secret"),
        )

        await transport.start()

        result = await transport.handle_event(
            {
                "type": "event_callback",
                "event": {
                    "type": "message",
                    "user": "U_HUMAN",
                    "text": "hello",
                    "channel": "C123",
                },
            },
            handle="engineer",
        )
        assert result is None
        await transport.stop()


class TestSlackSendRegistryLookup:
    @pytest.mark.asyncio
    async def test_send_resolves_channel_via_sender_handle(self, registry):
        """When recipient is empty, registry looks up sender_handle."""
        registry.register_external_id("slack", "C_ENG_OUT", "engineer")
        transport = SlackTransport(handle_registry=registry)
        transport.register_app(
            "engineer",
            SlackAppConfig(bot_token="xoxb-test"),
        )

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="",
            body="Hello from registry",
        )

        mock_response = MagicMock()
        mock_response.json.return_value = {"ok": True}
        mock_post = AsyncMock(return_value=mock_response)
        with patch.object(transport._client, "post", mock_post):
            result = await transport.send(msg)
            assert result is True
            payload = mock_post.call_args[1]["json"]
            assert payload["channel"] == "C_ENG_OUT"

        await transport.stop()

    @pytest.mark.asyncio
    async def test_send_resolves_recipient_handle_to_channel(self, registry):
        """When recipient is an agent handle, resolve via registry."""
        registry.register_external_id("slack", "C_DESIGNER_CH", "designer")
        transport = SlackTransport(handle_registry=registry)
        transport.register_app(
            "engineer",
            SlackAppConfig(bot_token="xoxb-eng"),
        )

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="designer",
            body="Hey designer",
        )

        mock_response = MagicMock()
        mock_response.json.return_value = {"ok": True}
        mock_post = AsyncMock(return_value=mock_response)
        with patch.object(transport._client, "post", mock_post):
            result = await transport.send(msg)
            assert result is True
            payload = mock_post.call_args[1]["json"]
            assert payload["channel"] == "C_DESIGNER_CH"

        await transport.stop()


# --- Per-agent app tests ---


class TestSlackMultiApp:
    @pytest.mark.asyncio
    async def test_register_app(self):
        """register_app stores per-agent config."""
        transport = SlackTransport()
        config = SlackAppConfig(
            bot_token="xoxb-eng",
            signing_secret="secret-eng",
            channel="C_ENG",
        )
        transport.register_app("engineer", config)

        assert "engineer" in transport.apps
        assert transport.apps["engineer"].bot_token == "xoxb-eng"

    @pytest.mark.asyncio
    async def test_register_app_auto_registers_channel(self, registry):
        """register_app auto-registers channel in HandleRegistry."""
        transport = SlackTransport(handle_registry=registry)
        config = SlackAppConfig(
            bot_token="xoxb-eng",
            signing_secret="secret-eng",
            channel="C_ENG_AUTO",
        )
        transport.register_app("engineer", config)

        agent = registry.resolve_external_id("slack", "C_ENG_AUTO")
        assert agent is not None
        assert agent.handle == "engineer"

    @pytest.mark.asyncio
    async def test_send_uses_per_agent_token(self):
        """Outbound messages use the agent's own bot token."""
        transport = SlackTransport()
        transport.register_app(
            "engineer",
            SlackAppConfig(bot_token="xoxb-eng-token"),
        )

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="C123",
            body="Hello from engineer's app",
        )

        mock_response = MagicMock()
        mock_response.json.return_value = {"ok": True}
        mock_post = AsyncMock(return_value=mock_response)
        with patch.object(transport._client, "post", mock_post):
            result = await transport.send(msg)
            assert result is True
            call_kwargs = mock_post.call_args
            auth_header = call_kwargs[1]["headers"]["Authorization"]
            assert auth_header == "Bearer xoxb-eng-token"

        await transport.stop()

    @pytest.mark.asyncio
    async def test_send_unregistered_handle_returns_false(self):
        """Send returns False for an agent with no registered app."""
        transport = SlackTransport()
        transport.register_app(
            "engineer",
            SlackAppConfig(bot_token="xoxb-eng-token"),
        )

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="designer",
            recipient="C123",
            body="Hello from designer",
        )
        result = await transport.send(msg)
        assert result is False
        await transport.stop()

    @pytest.mark.asyncio
    async def test_send_uses_app_default_channel(self):
        """Outbound resolves channel from per-agent app config."""
        transport = SlackTransport()
        transport.register_app(
            "engineer",
            SlackAppConfig(
                bot_token="xoxb-eng",
                channel="C_ENG_DEFAULT",
            ),
        )

        await transport.start()

        msg = OutboundMessage(
            transport="slack",
            sender_handle="engineer",
            recipient="",
            body="Hello",
        )

        mock_response = MagicMock()
        mock_response.json.return_value = {"ok": True}
        mock_post = AsyncMock(return_value=mock_response)
        with patch.object(transport._client, "post", mock_post):
            result = await transport.send(msg)
            assert result is True
            payload = mock_post.call_args[1]["json"]
            assert payload["channel"] == "C_ENG_DEFAULT"

        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_event_uses_agent_secret(self):
        """Inbound with handle uses that agent's signing secret."""
        secret = "eng-secret"
        transport = SlackTransport()
        transport.register_app(
            "engineer",
            SlackAppConfig(
                bot_token="xoxb-eng",
                signing_secret=secret,
            ),
        )

        await transport.start()

        body_raw = (
            '{"type":"event_callback","event":'
            '{"type":"message","user":"U1","text":"hi","channel":"C1"}}'
        )
        ts = str(int(time.time()))
        sig = _make_signature(secret, ts, body_raw)

        result = await transport.handle_event(
            json.loads(body_raw),
            handle="engineer",
            headers={
                "x-slack-request-timestamp": ts,
                "x-slack-signature": sig,
            },
            body_raw=body_raw,
        )
        assert isinstance(result, InboundNotification)
        assert result.recipient_handle == "engineer"
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_event_rejects_wrong_secret(self):
        """Inbound rejects events signed with wrong secret."""
        transport = SlackTransport()
        transport.register_app(
            "engineer",
            SlackAppConfig(
                bot_token="xoxb-eng",
                signing_secret="eng-secret",
            ),
        )

        await transport.start()

        body_raw = (
            '{"type":"event_callback","event":'
            '{"type":"message","user":"U1","text":"hi","channel":"C1"}}'
        )
        ts = str(int(time.time()))
        wrong_sig = (
            "v0="
            + hmac.new(
                b"wrong-secret",
                f"v0:{ts}:{body_raw}".encode(),
                hashlib.sha256,
            ).hexdigest()
        )

        result = await transport.handle_event(
            json.loads(body_raw),
            handle="engineer",
            headers={
                "x-slack-request-timestamp": ts,
                "x-slack-signature": wrong_sig,
            },
            body_raw=body_raw,
        )
        assert result is None
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_url_verification_with_handle(self):
        """URL verification works with per-agent handle."""
        transport = SlackTransport()
        transport.register_app(
            "engineer",
            SlackAppConfig(bot_token="xoxb-eng", signing_secret="eng-secret"),
        )

        await transport.start()

        result = await transport.handle_event(
            {"type": "url_verification", "challenge": "abc123"},
            handle="engineer",
        )
        assert result == {"challenge": "abc123"}
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_url_verification_without_registered_app_rejected(self):
        """URL verification is rejected when no app is registered for the handle."""
        transport = SlackTransport()

        await transport.start()

        result = await transport.handle_event(
            {"type": "url_verification", "challenge": "slack_challenge_token"},
            handle="unknown-agent",
        )
        assert result is None
        await transport.stop()

    @pytest.mark.asyncio
    async def test_handle_event_rejects_unregistered_handle(self):
        """Events for unregistered handles are rejected."""
        transport = SlackTransport()
        transport.register_app(
            "engineer",
            SlackAppConfig(bot_token="xoxb-eng"),
        )

        await transport.start()

        result = await transport.handle_event(
            {
                "type": "event_callback",
                "event": {
                    "type": "message",
                    "user": "U1",
                    "text": "hi",
                    "channel": "C1",
                },
            },
            handle="nonexistent",
        )
        assert result is None
        await transport.stop()

    @pytest.mark.asyncio
    async def test_start_reports_app_count(self):
        """start() tracks registered per-agent apps."""
        transport = SlackTransport()
        transport.register_app("eng-a", SlackAppConfig(bot_token="xoxb-a"))
        transport.register_app("eng-b", SlackAppConfig(bot_token="xoxb-b"))

        await transport.start()
        assert len(transport.apps) == 2
        await transport.stop()


# --- PlaneTransport registration ---


class TestPlaneTransportRegistration:
    """The transport contract the engine wiring relies on: the
    ``Transport`` protocol, the ``"plane"`` name (the key
    ``build_notification_transports`` output lands under in
    ``NotificationService.transports``), and the outbound no-op."""

    @staticmethod
    def _plane_transport():
        from crewlet.config import PlaneConfig
        from crewlet.notifications.transports.plane import PlaneTransport

        return PlaneTransport(PlaneConfig(url="https://plane.test", workspace="testco"))

    def test_satisfies_transport_protocol(self):
        from crewlet.notifications.protocol import Transport

        transport = self._plane_transport()
        assert transport.name == "plane"
        assert isinstance(transport, Transport)

    def test_service_exposes_under_plane_key(self):
        from crewlet.notifications.service import NotificationService

        org, queue, pool = _make_registry_parts()
        transport = self._plane_transport()
        service = NotificationService(
            event_queue=queue,
            transports={transport.name: transport},
            handle_registry=HandleRegistry(pool),
        )
        assert service.transports["plane"] is transport

    @pytest.mark.asyncio
    async def test_send_not_supported(self):
        transport = self._plane_transport()
        await transport.start()
        msg = OutboundMessage(
            transport="plane", sender_handle="engineer", recipient="ENG-1"
        )
        assert await transport.send(msg) is False
        await transport.stop()


# --- Config integration tests ---


class TestSlackConfigIntegration:
    def test_register_slack_apps_from_org(self):
        """register_slack_apps_from_org wires role slack configs."""
        from crewlet.config import register_slack_apps_from_org

        org = Organization(
            name="TestCo",
            units=[
                OrgUnit(
                    name="Core",
                    type="team",
                    lead="Engineer",
                    roles=[
                        Role(
                            name="Engineer",
                            slack={
                                "bot_token": "xoxb-eng",
                                "signing_secret": "secret-eng",
                                "channel": "C_ENG",
                            },
                        ),
                        Role(
                            name="Designer",
                            slack={
                                "bot_token": "xoxb-des",
                            },
                        ),
                        Role(name="PM"),
                    ],
                )
            ],
        )

        transport = SlackTransport()
        register_slack_apps_from_org(transport, org)

        assert "engineer" in transport.apps
        assert transport.apps["engineer"].bot_token == "xoxb-eng"
        assert transport.apps["engineer"].signing_secret == "secret-eng"
        assert transport.apps["engineer"].channel == "C_ENG"
        assert "designer" in transport.apps
        assert transport.apps["designer"].bot_token == "xoxb-des"
        assert "pm" not in transport.apps

    def test_register_slack_apps_resolves_env_vars(self, monkeypatch):
        """register_slack_apps_from_org resolves ${VAR} references."""
        from crewlet.config import register_slack_apps_from_org

        monkeypatch.setenv("ENG_TOKEN", "xoxb-from-env")
        monkeypatch.setenv("ENG_SECRET", "secret-from-env")

        org = Organization(
            name="TestCo",
            units=[
                OrgUnit(
                    name="Core",
                    type="team",
                    lead="Engineer",
                    roles=[
                        Role(
                            name="Engineer",
                            slack={
                                "bot_token": "${ENG_TOKEN}",
                                "signing_secret": "${ENG_SECRET}",
                            },
                        ),
                    ],
                )
            ],
        )

        transport = SlackTransport()
        register_slack_apps_from_org(transport, org)

        assert transport.apps["engineer"].bot_token == "xoxb-from-env"
        assert transport.apps["engineer"].signing_secret == "secret-from-env"

    def test_role_slack_field_in_parse_role(self):
        """_parse_role materialises integrations.slack into role.slack."""
        from crewlet.config import _parse_role

        role = _parse_role(
            {
                "name": "Engineer",
                "integrations": {
                    "slack": {
                        "bot_token": "xoxb-test",
                        "channel": "C_TEST",
                    },
                },
            }
        )
        assert role.slack == {
            "bot_token": "xoxb-test",
            "channel": "C_TEST",
        }

    def test_integrations_slack_does_not_fan_out_to_mcp_env(self):
        """integrations.slack is transport-only — it must NOT populate
        mcp_env.slack.  The Slack MCP token is configured explicitly."""
        from crewlet.config import _parse_role

        role = _parse_role(
            {
                "name": "Engineer",
                "integrations": {"slack": {"bot_token": "xoxb-test"}},
            }
        )
        assert role.slack == {"bot_token": "xoxb-test"}
        assert "slack" not in role.mcp_env

    def test_role_integrations_rejects_unknown_field(self):
        """A typo'd integration / field fails validation (extra=forbid)."""
        from pydantic import ValidationError

        from crewlet.config import _parse_role

        with pytest.raises(ValidationError):
            _parse_role(
                {"name": "Engineer", "integrations": {"slak": {"bot_token": "x"}}}
            )
        with pytest.raises(ValidationError):
            _parse_role(
                {
                    "name": "Engineer",
                    "integrations": {"slack": {"bot_tokenn": "x"}},
                }
            )

    def test_role_slack_field_defaults_empty(self):
        """Role.slack defaults to empty dict."""
        role = Role(name="Engineer")
        assert role.slack == {}
