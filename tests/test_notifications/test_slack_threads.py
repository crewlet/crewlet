"""Tests for Slack thread-follow tracker and thread routing in SlackTransport."""

from __future__ import annotations

import hashlib
import hmac
import json
import time
from typing import Any

import pytest

from crewlet.notifications.protocol import InboundNotification
from crewlet.notifications.transports.chat_threads import (
    ChatThreadTracker,
    ThreadFollowReason,
)
from crewlet.notifications.transports.slack import SlackAppConfig, SlackTransport
from crewlet.notifications.transports.slack_threads import (
    build_slack_thread_tracker,
    detect_follow_trigger,
)

_TEST_SECRET = "test_signing_secret"


# --- In-memory repo for tests (replaces real PostgreSQL repo) ---


class MemoryThreadFollowRepo:
    """In-memory implementation of ChatThreadFollowRepository for tests."""

    def __init__(self) -> None:
        # (backend, handle, channel, thread_id) → reason
        self._store: dict[tuple[str, str, str, str], str] = {}

    async def upsert(
        self, backend: str, handle: str, channel: str, thread_id: str, reason: str
    ) -> None:
        self._store[(backend, handle, channel, thread_id)] = reason

    async def is_following(
        self, backend: str, handle: str, channel: str, thread_id: str
    ) -> str | None:
        return self._store.get((backend, handle, channel, thread_id))


def _make_tracker() -> ChatThreadTracker:
    return build_slack_thread_tracker(MemoryThreadFollowRepo())


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


def _signed(body: dict[str, Any], secret: str = _TEST_SECRET) -> dict[str, Any]:
    """Build headers and body_raw kwargs for a signed Slack event."""
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


# --- detect_follow_trigger ---


class TestDetectFollowTrigger:
    def test_direct_mention(self):
        reason = detect_follow_trigger("Hey <@U_BOT123> check this", "U_BOT123")
        assert reason == ThreadFollowReason.MENTION

    def test_direct_mention_other_user(self):
        """Mention of a different user does not trigger follow."""
        reason = detect_follow_trigger("Hey <@U_OTHER> check this", "U_BOT123")
        assert reason is None

    def test_channel_collective(self):
        reason = detect_follow_trigger("<!channel> heads up everyone", "U_BOT123")
        assert reason == ThreadFollowReason.COLLECTIVE

    def test_here_collective(self):
        reason = detect_follow_trigger("<!here> quick question", "U_BOT123")
        assert reason == ThreadFollowReason.COLLECTIVE

    def test_collective_with_label(self):
        """<!channel|channel> variant used by some Slack clients."""
        reason = detect_follow_trigger("<!channel|channel> update", "U_BOT123")
        assert reason == ThreadFollowReason.COLLECTIVE

    def test_no_trigger(self):
        reason = detect_follow_trigger("Just a normal message", "U_BOT123")
        assert reason is None

    def test_no_bot_user_id(self):
        """Without bot_user_id, mentions are not checked."""
        reason = detect_follow_trigger("Hey <@U_BOT123> check this", "")
        assert reason is None

    def test_mention_takes_priority_over_collective(self):
        """When both mention and collective are present, mention wins."""
        reason = detect_follow_trigger("<!channel> <@U_BOT123> take a look", "U_BOT123")
        assert reason == ThreadFollowReason.MENTION


# --- ChatThreadTracker (Slack-bound) ---


class TestChatThreadTracker:
    @pytest.mark.asyncio
    async def test_not_following_by_default(self):
        tracker = _make_tracker()
        assert not await tracker.is_following("engineer", "C1", "1234.5678")

    @pytest.mark.asyncio
    async def test_start_and_check_following(self):
        tracker = _make_tracker()
        await tracker.start_following("engineer", "C1", "1234.5678")
        assert await tracker.is_following("engineer", "C1", "1234.5678")
        assert not await tracker.is_following("designer", "C1", "1234.5678")

    @pytest.mark.asyncio
    async def test_record_participation(self):
        tracker = _make_tracker()
        await tracker.record_participation("engineer", "C1", "1234.5678")
        assert await tracker.is_following("engineer", "C1", "1234.5678")

    @pytest.mark.asyncio
    async def test_record_participation_no_thread_ts(self):
        """No-op when thread_ts is empty."""
        tracker = _make_tracker()
        await tracker.record_participation("engineer", "C1", "")
        assert not await tracker.is_following("engineer", "C1", "")

    @pytest.mark.asyncio
    async def test_record_participation_already_following(self):
        """Doesn't overwrite existing follow reason."""
        tracker = _make_tracker()
        await tracker.start_following(
            "engineer", "C1", "1234.5678", ThreadFollowReason.MENTION
        )
        await tracker.record_participation("engineer", "C1", "1234.5678")
        assert await tracker.is_following("engineer", "C1", "1234.5678")

    @pytest.mark.asyncio
    async def test_state_survives_new_tracker_instance(self):
        """Verify DB persistence: new tracker on same repo sees old state."""
        repo = MemoryThreadFollowRepo()
        tracker1 = build_slack_thread_tracker(repo)
        await tracker1.start_following("engineer", "C1", "1234.5678")

        # Simulate engine restart: new tracker, same repo
        tracker2 = build_slack_thread_tracker(repo)
        assert await tracker2.is_following("engineer", "C1", "1234.5678")


# --- SlackTransport thread routing integration ---


def _make_transport(**kwargs) -> SlackTransport:
    """Create a SlackTransport with thread routing enabled by default."""
    kwargs.setdefault("thread_follow_repo", MemoryThreadFollowRepo())
    transport = SlackTransport(**kwargs)
    transport.register_app("engineer", SlackAppConfig(signing_secret=_TEST_SECRET))
    return transport


async def _send_event(
    transport: SlackTransport,
    body: dict[str, Any],
    handle: str = "engineer",
    secret: str = _TEST_SECRET,
) -> InboundNotification | dict[str, Any] | None:
    """Send a signed event to the transport."""
    return await transport.handle_event(
        body,
        handle=handle,
        **_signed(body, secret),
    )


class TestSlackTransportThreadRouting:
    @pytest.mark.asyncio
    async def test_top_level_message_always_delivered(self):
        """Top-level channel messages are always delivered."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        body = {
            "type": "event_callback",
            "team_id": "T123",
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "Hello team",
                "channel": "C1",
                "ts": "1000.0001",
            },
        }

        result = await _send_event(transport, body)
        assert isinstance(result, InboundNotification)
        await transport.stop()

    @pytest.mark.asyncio
    async def test_thread_reply_blocked_when_not_following(self):
        """Thread replies are blocked when agent is not following."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        body = {
            "type": "event_callback",
            "team_id": "T123",
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "A reply in the thread",
                "channel": "C1",
                "ts": "1000.0002",
                "thread_ts": "1000.0001",
            },
        }

        result = await _send_event(transport, body)
        assert result is None
        assert "not following" in transport.last_skip_reason
        await transport.stop()

    @pytest.mark.asyncio
    async def test_thread_reply_delivered_when_mentioned(self):
        """Thread reply with a mention auto-follows and delivers."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "Hey <@U_BOT_ENG> check this thread",
                "channel": "C1",
                "ts": "1000.0002",
                "thread_ts": "1000.0001",
            },
        }

        result = await _send_event(transport, body)
        assert isinstance(result, InboundNotification)
        assert result.metadata["thread_follow_reason"] == "mention"
        await transport.stop()

    @pytest.mark.asyncio
    async def test_thread_reply_delivered_on_collective(self):
        """Thread reply with <!channel> auto-follows and delivers."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "<!channel> important update",
                "channel": "C1",
                "ts": "1000.0002",
                "thread_ts": "1000.0001",
            },
        }

        result = await _send_event(transport, body)
        assert isinstance(result, InboundNotification)
        assert result.metadata["thread_follow_reason"] == "collective"
        await transport.stop()

    @pytest.mark.asyncio
    async def test_subsequent_replies_delivered_after_follow(self):
        """Once following, subsequent replies are delivered."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        # First: mention starts following
        mention_body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "Hey <@U_BOT_ENG> check this",
                "channel": "C1",
                "ts": "1000.0002",
                "thread_ts": "1000.0001",
            },
        }
        result1 = await _send_event(transport, mention_body)
        assert isinstance(result1, InboundNotification)

        # Second: plain reply — should be delivered because following
        reply_body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "Thanks, any progress?",
                "channel": "C1",
                "ts": "1000.0003",
                "thread_ts": "1000.0001",
            },
        }
        result2 = await _send_event(transport, reply_body)
        assert isinstance(result2, InboundNotification)
        await transport.stop()

    @pytest.mark.asyncio
    async def test_top_level_mention_auto_follows_thread(self):
        """A top-level mention auto-follows the thread for future replies."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        # Top-level app_mention event
        mention_body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "app_mention",
                "user": "U_HUMAN",
                "text": "<@U_BOT_ENG> What skills do you have?",
                "channel": "C1",
                "ts": "1000.0001",
            },
        }
        result1 = await _send_event(transport, mention_body)
        assert isinstance(result1, InboundNotification)

        # Thread reply — should be delivered
        reply_body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "Can you elaborate?",
                "channel": "C1",
                "ts": "1000.0002",
                "thread_ts": "1000.0001",
            },
        }
        result2 = await _send_event(transport, reply_body)
        assert isinstance(result2, InboundNotification)
        await transport.stop()

    @pytest.mark.asyncio
    async def test_top_level_no_mention_no_auto_follow(self):
        """Top-level message without mention does NOT auto-follow."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        # Top-level message without mention
        top_body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "General discussion topic",
                "channel": "C1",
                "ts": "2000.0001",
            },
        }
        result1 = await _send_event(transport, top_body)
        assert isinstance(result1, InboundNotification)

        # Thread reply — should be blocked
        reply_body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "Any thoughts?",
                "channel": "C1",
                "ts": "2000.0002",
                "thread_ts": "2000.0001",
            },
        }
        result2 = await _send_event(transport, reply_body)
        assert result2 is None
        await transport.stop()

    @pytest.mark.asyncio
    async def test_thread_routing_disabled(self):
        """When thread_routing=False, all messages are delivered."""
        transport = _make_transport(thread_routing=False)
        await transport.start()

        body = {
            "type": "event_callback",
            "team_id": "T123",
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "A thread reply",
                "channel": "C1",
                "ts": "1000.0002",
                "thread_ts": "1000.0001",
            },
        }

        result = await _send_event(transport, body)
        assert isinstance(result, InboundNotification)
        await transport.stop()

    @pytest.mark.asyncio
    async def test_outbound_participation_auto_follows(self):
        """Sending a message in a thread auto-follows for future replies."""
        repo = MemoryThreadFollowRepo()
        transport = _make_transport(thread_follow_repo=repo)
        transport.register_app(
            "engineer",
            SlackAppConfig(
                bot_token="xoxb-test",
                signing_secret=_TEST_SECRET,
                channel="C1",
            ),
        )
        await transport.start()

        # Bot's own message echoed back — records participation
        echo_body = {
            "type": "event_callback",
            "api_app_id": "A_OUR_APP",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_BOT_ENG",
                "app_id": "A_OUR_APP",
                "text": "Here is my response",
                "channel": "C1",
                "ts": "3000.0002",
                "thread_ts": "3000.0001",
            },
        }
        result1 = await _send_event(transport, echo_body)
        assert result1 is None  # Own message skipped

        # But now a reply should be delivered (we're participating)
        reply_body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "Thanks for that!",
                "channel": "C1",
                "ts": "3000.0003",
                "thread_ts": "3000.0001",
            },
        }
        result2 = await _send_event(transport, reply_body)
        assert isinstance(result2, InboundNotification)
        await transport.stop()

    @pytest.mark.asyncio
    async def test_metadata_includes_thread_info(self):
        """InboundNotification metadata includes thread routing info."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "<@U_BOT_ENG> follow up",
                "channel": "C1",
                "ts": "1000.0002",
                "thread_ts": "1000.0001",
            },
        }

        result = await _send_event(transport, body)
        assert isinstance(result, InboundNotification)
        assert result.metadata["thread_ts"] == "1000.0001"
        assert result.metadata["thread_following"] == "true"
        assert result.metadata["thread_follow_reason"] == "mention"
        await transport.stop()

    @pytest.mark.asyncio
    async def test_here_collective_in_thread(self):
        """<!here> in a thread reply triggers auto-follow."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "<!here> anyone seen this?",
                "channel": "C1",
                "ts": "1000.0002",
                "thread_ts": "1000.0001",
            },
        }

        result = await _send_event(transport, body)
        assert isinstance(result, InboundNotification)
        assert result.metadata["thread_follow_reason"] == "collective"
        await transport.stop()

    @pytest.mark.asyncio
    async def test_different_threads_independent(self):
        """Following one thread does not affect another."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        # Follow thread A via mention
        mention_a = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "<@U_BOT_ENG> thread A",
                "channel": "C1",
                "ts": "1000.0002",
                "thread_ts": "1000.0001",
            },
        }
        r1 = await _send_event(transport, mention_a)
        assert isinstance(r1, InboundNotification)

        # Reply in thread B — should be blocked
        reply_b = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "Thread B reply",
                "channel": "C1",
                "ts": "2000.0002",
                "thread_ts": "2000.0001",
            },
        }
        r2 = await _send_event(transport, reply_b)
        assert r2 is None

        # Reply in thread A — should be delivered
        reply_a = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "Thread A follow-up",
                "channel": "C1",
                "ts": "1000.0003",
                "thread_ts": "1000.0001",
            },
        }
        r3 = await _send_event(transport, reply_a)
        assert isinstance(r3, InboundNotification)
        await transport.stop()

    @pytest.mark.asyncio
    async def test_app_mention_top_level_delivered(self):
        """app_mention event at top level is delivered and auto-follows."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "app_mention",
                "user": "U_HUMAN",
                "text": "<@U_BOT_ENG> hi",
                "channel": "C1",
                "ts": "5000.0001",
            },
        }
        result = await _send_event(transport, body)
        assert isinstance(result, InboundNotification)

        # Subsequent thread reply should be delivered
        reply_body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "Follow up question",
                "channel": "C1",
                "ts": "5000.0002",
                "thread_ts": "5000.0001",
            },
        }
        result2 = await _send_event(transport, reply_body)
        assert isinstance(result2, InboundNotification)
        await transport.stop()

    @pytest.mark.asyncio
    async def test_app_mention_in_thread_auto_follows(self):
        """app_mention in thread reply auto-follows and delivers."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "app_mention",
                "user": "U_HUMAN",
                "text": "<@U_BOT_ENG> what do you think?",
                "channel": "C1",
                "ts": "6000.0002",
                "thread_ts": "6000.0001",
            },
        }
        result = await _send_event(transport, body)
        assert isinstance(result, InboundNotification)
        await transport.stop()

    @pytest.mark.asyncio
    async def test_app_mention_own_app_skipped(self):
        """Own app's app_mention is skipped but records participation."""
        repo = MemoryThreadFollowRepo()
        transport = _make_transport(thread_follow_repo=repo)
        await transport.start()

        body = {
            "type": "event_callback",
            "api_app_id": "A_OUR_APP",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "app_mention",
                "user": "U_BOT_ENG",
                "app_id": "A_OUR_APP",
                "text": "<@U_BOT_ENG> self-mention",
                "channel": "C1",
                "ts": "7000.0002",
                "thread_ts": "7000.0001",
            },
        }
        result = await _send_event(transport, body)
        assert result is None  # Own message skipped

        # Participation should be recorded
        assert (
            await repo.is_following("slack", "engineer", "C1", "7000.0001") is not None
        )
        await transport.stop()

    @pytest.mark.asyncio
    async def test_dedup_message_and_app_mention(self):
        """Duplicate events for the same message are deduplicated."""
        transport = _make_transport(thread_routing=True)
        await transport.start()

        body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "<@U_BOT_ENG> hello",
                "channel": "C1",
                "ts": "8000.0001",
            },
        }
        result1 = await _send_event(transport, body)
        assert isinstance(result1, InboundNotification)

        # Same ts → dedup
        body2 = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "app_mention",
                "user": "U_HUMAN",
                "text": "<@U_BOT_ENG> hello",
                "channel": "C1",
                "ts": "8000.0001",
            },
        }
        result2 = await _send_event(transport, body2)
        assert result2 is None
        assert "duplicate" in transport.last_skip_reason
        await transport.stop()

    @pytest.mark.asyncio
    async def test_bot_echo_records_participation(self):
        """Bot's own message echo records thread participation."""
        repo = MemoryThreadFollowRepo()
        transport = _make_transport(thread_follow_repo=repo)
        await transport.start()

        body = {
            "type": "event_callback",
            "api_app_id": "A_OUR_APP",
            "team_id": "T123",
            "event": {
                "type": "message",
                "user": "U_BOT_ENG",
                "app_id": "A_OUR_APP",
                "text": "My reply",
                "channel": "C1",
                "ts": "9000.0002",
                "thread_ts": "9000.0001",
            },
        }
        result = await _send_event(transport, body)
        assert result is None  # Own message skipped

        # But participation is recorded
        assert (
            await repo.is_following("slack", "engineer", "C1", "9000.0001") is not None
        )
        await transport.stop()

    @pytest.mark.asyncio
    async def test_state_survives_transport_restart(self):
        """Thread follow state survives transport stop/start (DB-backed)."""
        repo = MemoryThreadFollowRepo()
        transport = _make_transport(thread_follow_repo=repo)
        await transport.start()

        # Mention starts following
        mention_body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "app_mention",
                "user": "U_HUMAN",
                "text": "<@U_BOT_ENG> remember this",
                "channel": "C1",
                "ts": "5555.0001",
            },
        }
        r1 = await _send_event(transport, mention_body)
        assert isinstance(r1, InboundNotification)
        await transport.stop()

        # Simulate restart: new transport, same repo
        transport2 = SlackTransport(thread_follow_repo=repo)
        transport2.register_app("engineer", SlackAppConfig(signing_secret=_TEST_SECRET))
        await transport2.start()

        # Thread reply should be delivered (state recovered from DB)
        reply_body = {
            "type": "event_callback",
            "team_id": "T123",
            "authorizations": [{"user_id": "U_BOT_ENG"}],
            "event": {
                "type": "message",
                "user": "U_HUMAN",
                "text": "Following up on that",
                "channel": "C1",
                "ts": "5555.0002",
                "thread_ts": "5555.0001",
            },
        }
        r2 = await _send_event(transport2, reply_body)
        assert isinstance(r2, InboundNotification)
        await transport2.stop()


@pytest.mark.asyncio
async def test_metadata_carries_channel_type() -> None:
    """``channel_type`` must ride the notification metadata: it drives
    DM-level inbox coalescing (an ``im`` channel keys as one
    conversation — see ``SlackNotificationPrompt.conversation_key``)
    and the learning subsystem's channel-kind derivation."""
    transport = _make_transport(thread_routing=True)
    await transport.start()

    body = {
        "type": "event_callback",
        "team_id": "T123",
        "event": {
            "type": "message",
            "user": "U_HUMAN",
            "text": "hello there",
            "channel": "D123",
            "channel_type": "im",
            "ts": "1718.001",
        },
    }
    result = await _send_event(transport, body)
    assert isinstance(result, InboundNotification)
    assert result.metadata["channel_type"] == "im"
    await transport.stop()
