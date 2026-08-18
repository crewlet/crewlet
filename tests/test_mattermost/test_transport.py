"""Tests for MattermostTransport — inbound parsing, threading, suppression."""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.notifications.protocol import InboundNotification, OutboundMessage
from crewlet.notifications.transports.mattermost import (
    MattermostBotConfig,
    MattermostTransport,
    mattermost_post_skip_reason,
    mattermost_post_text,
)
from crewlet.notifications.typing_status import WorkingStatusMode

BOT_ID = "botuserid00000000000000000"
HUMAN_ID = "humanuserid0000000000000000"


class MemoryThreadFollowRepo:
    """In-memory ChatThreadFollowRepository for tests."""

    def __init__(self) -> None:
        self._store: dict[tuple[str, str, str, str], str] = {}

    async def upsert(
        self, backend: str, handle: str, channel: str, thread_id: str, reason: str
    ) -> None:
        self._store[(backend, handle, channel, thread_id)] = reason

    async def is_following(
        self, backend: str, handle: str, channel: str, thread_id: str
    ) -> str | None:
        return self._store.get((backend, handle, channel, thread_id))


def _make_transport(**kwargs: Any) -> MattermostTransport:
    kwargs.setdefault("thread_follow_repo", MemoryThreadFollowRepo())
    transport = MattermostTransport(
        base_url="https://chat.example",
        team="nimbus",
        **kwargs,
    )
    transport.register_bot(
        "engineer",
        MattermostBotConfig(bot_token="tok", username="agent-swe", channel="eng"),
    )
    # Identity normally resolves in start(); set it directly so tests do
    # not need a live server.
    transport._user_ids["engineer"] = BOT_ID
    transport._started = True
    return transport


def _event(
    *,
    post_id: str = "p1",
    channel: str = "c1",
    user_id: str = HUMAN_ID,
    message: str = "hello team",
    root_id: str = "",
    channel_type: str = "O",
    mentions: list[str] | None = None,
    post_type: str = "",
    replayed: bool = False,
    **post_extra: Any,
) -> dict[str, Any]:
    post: dict[str, Any] = {
        "id": post_id,
        "channel_id": channel,
        "user_id": user_id,
        "message": message,
        "create_at": 1700000000000,
    }
    if root_id:
        post["root_id"] = root_id
    if post_type:
        post["type"] = post_type
    post.update(post_extra)
    return {
        "event": "posted",
        "post": post,
        "channel_type": channel_type,
        "channel_name": "engineering",
        "mentions": mentions if mentions is not None else [],
        "sender_name": "@alice",
        "bot_user_id": BOT_ID,
        "replayed": replayed,
    }


# --- post content helpers -------------------------------------------------


class TestPostHelpers:
    def test_system_posts_are_skipped(self):
        assert mattermost_post_skip_reason({"type": "system_join_channel"})
        assert mattermost_post_skip_reason({"type": "system_header_change"})

    def test_normal_post_passes(self):
        assert mattermost_post_skip_reason({"type": "", "message": "hi"}) == ""

    def test_deleted_post_is_skipped(self):
        assert mattermost_post_skip_reason({"delete_at": 1700000000000})

    def test_file_only_post_renders_a_body(self):
        """A comment-less upload has real content but no message text —
        without this it would wake the agent with a blank body."""
        assert mattermost_post_text({"message": "", "file_ids": ["a", "b"]}) == (
            "(shared 2 files)"
        )
        assert mattermost_post_text({"message": "", "file_ids": ["a"]}) == (
            "(shared 1 file)"
        )

    def test_message_wins_over_file_rendering(self):
        assert mattermost_post_text({"message": "look", "file_ids": ["a"]}) == "look"


# --- inbound parsing ------------------------------------------------------


class TestInboundParsing:
    @pytest.mark.asyncio
    async def test_top_level_message_delivered(self):
        transport = _make_transport()
        result = await transport.handle_event(_event(), "engineer")
        assert isinstance(result, InboundNotification)
        assert result.source == "mattermost"
        assert result.body == "hello team"
        assert result.metadata["transport"] == "mattermost"
        assert result.metadata["channel_type"] == "O"
        assert result.metadata["bot_username"] == "agent-swe"

    @pytest.mark.asyncio
    async def test_unknown_handle_is_rejected(self):
        transport = _make_transport()
        assert await transport.handle_event(_event(), "nobody") is None
        assert "no Mattermost bot registered" in transport.last_skip_reason

    @pytest.mark.asyncio
    async def test_system_post_skipped(self):
        transport = _make_transport()
        result = await transport.handle_event(
            _event(post_type="system_join_channel", message="alice joined"),
            "engineer",
        )
        assert result is None
        assert "system post type" in transport.last_skip_reason

    @pytest.mark.asyncio
    async def test_own_message_suppressed(self):
        transport = _make_transport()
        result = await transport.handle_event(
            _event(user_id=BOT_ID, message="my reply"), "engineer"
        )
        assert result is None
        assert "loop prevention" in transport.last_skip_reason

    @pytest.mark.asyncio
    async def test_own_thread_reply_records_participation(self):
        """Replying in a thread is how anyone follows it — the agent's own
        reply must subscribe it to what comes back."""
        repo = MemoryThreadFollowRepo()
        transport = _make_transport(thread_follow_repo=repo)
        await transport.handle_event(
            _event(post_id="p2", user_id=BOT_ID, root_id="root1"), "engineer"
        )
        assert (
            await repo.is_following("mattermost", "engineer", "c1", "root1") is not None
        )
        # ...and a later human reply in that thread now lands.
        result = await transport.handle_event(
            _event(post_id="p3", root_id="root1", message="thanks"), "engineer"
        )
        assert isinstance(result, InboundNotification)

    @pytest.mark.asyncio
    async def test_empty_post_skipped(self):
        transport = _make_transport()
        result = await transport.handle_event(
            _event(message="", **{"file_ids": []}), "engineer"
        )
        assert result is None
        assert "no user-visible content" in transport.last_skip_reason


# --- thread routing -------------------------------------------------------


class TestThreadRouting:
    @pytest.mark.asyncio
    async def test_unfollowed_thread_reply_blocked(self):
        transport = _make_transport()
        result = await transport.handle_event(
            _event(post_id="p2", root_id="p1", message="any update?"), "engineer"
        )
        assert result is None
        assert "not following" in transport.last_skip_reason

    @pytest.mark.asyncio
    async def test_server_mention_list_starts_a_follow(self):
        """The server-computed mentions list is exact — preferred over any
        text matching."""
        transport = _make_transport()
        result = await transport.handle_event(
            _event(
                post_id="p2",
                root_id="p1",
                message="@agent-swe take a look",
                mentions=[BOT_ID],
            ),
            "engineer",
        )
        assert isinstance(result, InboundNotification)
        assert result.metadata["thread_follow_reason"] == "mention"
        assert result.metadata["thread_following"] == "true"

    @pytest.mark.asyncio
    async def test_mention_of_someone_else_does_not_follow(self):
        transport = _make_transport()
        result = await transport.handle_event(
            _event(
                post_id="p2",
                root_id="p1",
                message="@someone-else take a look",
                mentions=["anotheruserid00000000000000"],
            ),
            "engineer",
        )
        # The server resolved a mention list this bot is a member of only
        # via a collective address — recorded as collective, not mention.
        assert isinstance(result, InboundNotification)
        assert result.metadata["thread_follow_reason"] == "collective"

    @pytest.mark.asyncio
    async def test_following_persists_for_later_replies(self):
        transport = _make_transport()
        await transport.handle_event(
            _event(post_id="p2", root_id="p1", mentions=[BOT_ID]), "engineer"
        )
        result = await transport.handle_event(
            _event(post_id="p3", root_id="p1", message="and another thing"),
            "engineer",
        )
        assert isinstance(result, InboundNotification)

    @pytest.mark.asyncio
    async def test_dm_always_follows(self):
        """There is nobody else a DM could be addressed to."""
        transport = _make_transport()
        result = await transport.handle_event(
            _event(channel="d1", channel_type="D", message="hi"), "engineer"
        )
        assert isinstance(result, InboundNotification)
        assert result.metadata["thread_follow_reason"] == "mention"

    @pytest.mark.asyncio
    async def test_backfilled_post_falls_back_to_the_regex_grammar(self):
        """Reconnect backfill reads posts over REST, which carry no
        mention list — the literal @username grammar is the fallback."""
        transport = _make_transport()
        result = await transport.handle_event(
            _event(
                post_id="p9",
                root_id="r9",
                message="hey @agent-swe, thoughts?",
                mentions=[],
                replayed=True,
            ),
            "engineer",
        )
        assert isinstance(result, InboundNotification)
        assert result.metadata["thread_follow_reason"] == "mention"
        assert result.metadata["replayed"] == "true"

    @pytest.mark.asyncio
    async def test_thread_routing_disabled_delivers_everything(self):
        transport = _make_transport(thread_routing=False)
        result = await transport.handle_event(
            _event(post_id="p2", root_id="p1", message="a reply"), "engineer"
        )
        assert isinstance(result, InboundNotification)

    @pytest.mark.asyncio
    async def test_threads_are_independent(self):
        transport = _make_transport()
        await transport.handle_event(
            _event(post_id="a2", root_id="a1", mentions=[BOT_ID]), "engineer"
        )
        blocked = await transport.handle_event(
            _event(post_id="b2", root_id="b1", message="other thread"), "engineer"
        )
        assert blocked is None


# --- working status -------------------------------------------------------


class TestWorkingStatus:
    def test_backend_declares_no_status_text(self):
        """Mattermost's indicator wording is the client's, so the phrase
        pools must go inert rather than render into nothing."""
        transport = _make_transport()
        assert transport.supports_status_text is False
        assert transport.typing_status.supports_status_text is False

    def test_dm_prefix_heuristic_is_disabled(self):
        """Mattermost channel ids are opaque — a prefix test would mark
        arbitrary public channels as DMs."""
        assert MattermostTransport.dm_channel_id_prefix == ""

    def test_typing_status_defaults_off(self):
        transport = _make_transport()
        assert transport.typing_status.mode is WorkingStatusMode.OFF

    @pytest.mark.asyncio
    async def test_clearing_status_makes_no_call(self):
        """There is no clear operation — the indicator lapses on its own,
        so an empty status must not emit a meaningless request."""
        transport = _make_transport()
        assert await transport.set_status("engineer", "c1", "p1", "") is False


# --- outbound -------------------------------------------------------------


class TestOutbound:
    @pytest.mark.asyncio
    async def test_send_without_a_channel_fails_cleanly(self):
        transport = MattermostTransport(base_url="https://chat.example", team="n")
        transport.register_bot("engineer", MattermostBotConfig(bot_token="t"))
        ok = await transport.send(
            OutboundMessage(
                transport="mattermost", sender_handle="engineer", recipient="", body="x"
            )
        )
        assert ok is False

    @pytest.mark.asyncio
    async def test_send_for_unknown_handle_fails_cleanly(self):
        transport = _make_transport()
        ok = await transport.send(
            OutboundMessage(
                transport="mattermost", sender_handle="ghost", recipient="c1", body="x"
            )
        )
        assert ok is False


# --- identity recovery ----------------------------------------------------


class TestOwnIdentityFallback:
    """``start()`` resolves each bot's user id, but that read can fail and
    a live-added seat is never in the map at all. An empty id disables
    own-message suppression, and an agent that cannot recognise its own
    posts answers itself, forever, at one LLM turn per round."""

    @pytest.mark.asyncio
    async def test_own_message_is_suppressed_without_a_resolved_identity(self):
        transport = _make_transport()
        transport._user_ids.clear()

        result = await transport.handle_event(_event(user_id=BOT_ID), "engineer")

        assert result is None
        assert "loop prevention" in transport.last_skip_reason

    @pytest.mark.asyncio
    async def test_the_stamped_identity_is_cached_for_later_events(self):
        transport = _make_transport()
        transport._user_ids.clear()

        await transport.handle_event(_event(user_id=BOT_ID), "engineer")

        assert transport._user_ids["engineer"] == BOT_ID

    @pytest.mark.asyncio
    async def test_a_direct_mention_is_still_recognised_as_one(self):
        """Without the id, a mention of this bot would be misread as a
        weaker collective address."""
        transport = _make_transport()
        transport._user_ids.clear()

        result = await transport.handle_event(
            _event(mentions=[BOT_ID], root_id=""), "engineer"
        )

        assert result is not None
        assert result.metadata["thread_follow_reason"] == "mention"
        assert result.metadata["bot_user_id"] == BOT_ID


# --- the Site URL preflight ----------------------------------------------


class TestSiteURLPreflight:
    """The one Mattermost misconfiguration with no symptom the engine
    would otherwise surface: the server keeps answering the engine while
    every browser loses live updates."""

    def test_a_mismatch_is_logged_with_the_impact(self, caplog):
        transport = _make_transport()
        with caplog.at_level("WARNING"):
            transport._warn_on_site_url_mismatch({"SiteURL": "http://localhost:8065"})
        assert "mattermost_site_url_mismatch" in caplog.text

    def test_a_match_is_silent(self, caplog):
        transport = _make_transport()
        with caplog.at_level("WARNING"):
            transport._warn_on_site_url_mismatch({"SiteURL": "https://chat.example/"})
        assert "mattermost_site_url_mismatch" not in caplog.text

    def test_an_unreported_site_url_is_not_a_mismatch(self, caplog):
        """An older or locked-down server that does not report it must not
        produce a warning naming a value nobody set."""
        transport = _make_transport()
        with caplog.at_level("WARNING"):
            transport._warn_on_site_url_mismatch({})
        assert "mattermost_site_url_mismatch" not in caplog.text
