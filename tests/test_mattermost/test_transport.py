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


class _StubRegistry:
    """Minimal HandleRegistry stand-in for outbound resolution."""

    def __init__(self, mapping: dict[str, str]) -> None:
        self._mapping = mapping

    def get_external_id(self, transport_name: str, handle: str) -> str:
        return self._mapping.get(handle, "")

    def register_external_id(self, *args: Any, **kwargs: Any) -> None:
        return None

    def unregister_external_id(self, *args: Any, **kwargs: Any) -> bool:
        return True


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


class _FakeClient:
    """Records what the transport asked the server to do."""

    def __init__(self, *, users: dict[str, str] | None = None) -> None:
        self.posts: list[dict[str, Any]] = []
        self.typing: list[dict[str, Any]] = []
        self.direct: list[tuple[str, str]] = []
        self.users = users or {}
        self.channels: dict[str, str] = {"eng": "chan1eng000000000000000000"}
        self.team = {"id": "team100000000000000000000"}

    async def create_post(
        self, channel_id: str, message: str, *, root_id: str = "", **_: Any
    ) -> dict[str, Any]:
        self.posts.append(
            {"channel_id": channel_id, "message": message, "root_id": root_id}
        )
        return {"id": "newpost00000000000000000000"}

    async def publish_typing(self, channel_id: str, **kwargs: Any) -> None:
        self.typing.append({"channel_id": channel_id, **kwargs})

    async def get_user_by_username(self, username: str) -> dict[str, Any] | None:
        user_id = self.users.get(username)
        return {"id": user_id, "username": username} if user_id else None

    async def create_direct_channel(self, a: str, b: str) -> dict[str, Any]:
        self.direct.append((a, b))
        return {"id": "dmchannel00000000000000000"}

    async def get_team_by_name(self, name: str) -> dict[str, Any] | None:
        return self.team

    async def get_channel_by_name(
        self, team_id: str, name: str
    ) -> dict[str, Any] | None:
        channel_id = self.channels.get(name)
        return {"id": channel_id, "name": name} if channel_id else None

    async def me(self) -> dict[str, Any]:
        return {"id": BOT_ID, "username": "agent-swe"}

    async def client_config(self) -> dict[str, Any]:
        return {"SiteURL": "https://chat.example"}

    async def close(self) -> None:
        return None


def _with_fake_client(transport: MattermostTransport, **kwargs: Any) -> _FakeClient:
    """Swap in a fake so no test touches the network."""
    fake = _FakeClient(**kwargs)
    transport._clients["engineer"] = fake  # type: ignore[assignment]
    return fake


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
        """Mattermost rewrites ``mentions`` per connection: the field is
        present only when THIS bot was mentioned, and is then exactly
        ``[own id]``. A list naming somebody else can only mean this
        seat's id is stale — it must not subscribe the bot to a thread it
        was not addressed in."""
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
        assert result is None
        assert "not following" in transport.last_skip_reason

    @pytest.mark.asyncio
    async def test_a_broadcast_is_collective_not_a_personal_mention(self):
        """``@channel`` expands into every member's id, so the server list
        alone cannot tell a broadcast from being named. The text can, and
        the difference decides whether the agent counts as *addressed*."""
        transport = _make_transport()
        result = await transport.handle_event(
            _event(post_id="p2", message="@channel standup in 5", mentions=[BOT_ID]),
            "engineer",
        )
        assert isinstance(result, InboundNotification)
        assert result.metadata["thread_follow_reason"] == "collective"

    @pytest.mark.asyncio
    async def test_being_named_outranks_a_broadcast_in_the_same_message(self):
        transport = _make_transport()
        result = await transport.handle_event(
            _event(
                post_id="p2",
                message="@channel — @agent-swe can you take this?",
                mentions=[BOT_ID],
            ),
            "engineer",
        )
        assert isinstance(result, InboundNotification)
        assert result.metadata["thread_follow_reason"] == "mention"

    @pytest.mark.asyncio
    async def test_a_keyword_mention_the_text_cannot_show_still_counts(self):
        """A group mention, or a notification keyword, puts the bot in the
        server's list without any ``@username`` in the text. The server is
        authoritative about *whether*; absent a textual reason it is a
        mention."""
        transport = _make_transport()
        result = await transport.handle_event(
            _event(post_id="p2", message="who owns deploys?", mentions=[BOT_ID]),
            "engineer",
        )
        assert isinstance(result, InboundNotification)
        assert result.metadata["thread_follow_reason"] == "mention"

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
    async def test_clearing_makes_no_call(self):
        """There is no clear operation — the indicator lapses on its own,
        so clearing must not emit a meaningless request."""
        transport = _make_transport()
        fake = _with_fake_client(transport)
        assert await transport.clear_status("engineer", "c1", "p1") is False
        assert fake.typing == []

    @pytest.mark.asyncio
    async def test_the_indicator_is_raised_despite_carrying_no_text(self):
        """The driver sends an empty status to a backend that renders no
        text. Reading that as "clear" made the indicator unraisable — the
        whole feature was dead on this backend."""
        transport = _make_transport()
        fake = _with_fake_client(transport)

        assert await transport.set_status("engineer", "c1", "p1", "") is True

        assert fake.typing[0]["channel_id"] == "c1"
        assert fake.typing[0]["parent_id"] == "p1"

    @pytest.mark.asyncio
    async def test_the_typing_deadline_tracks_the_heartbeat(self):
        transport = _make_transport()
        transport.status_refresh_interval = 3.0
        fake = _with_fake_client(transport)
        await transport.set_status("engineer", "c1", "", "")
        assert fake.typing[0]["timeout"] == 3.0


# --- outbound -------------------------------------------------------------


class TestOutbound:
    @pytest.mark.asyncio
    async def test_a_reply_threads_under_the_triggering_message(self):
        """A top-level trigger carries no ``thread_ts``; the agent is told
        to answer in a thread, so the anchor is what it must post under —
        and the participation record depends on the same value."""
        transport = _make_transport()
        fake = _with_fake_client(transport)

        ok = await transport.send(
            OutboundMessage(
                transport="mattermost",
                sender_handle="engineer",
                recipient="",
                body="on it",
                reply_to_metadata={
                    "channel": "chan1eng000000000000000000",
                    "thread_ts": "",
                    "thread_anchor": "trigger0000000000000000000",
                },
            )
        )

        assert ok is True
        assert fake.posts[0]["root_id"] == "trigger0000000000000000000"

    @pytest.mark.asyncio
    async def test_a_thread_reply_still_uses_the_root(self):
        transport = _make_transport()
        fake = _with_fake_client(transport)

        await transport.send(
            OutboundMessage(
                transport="mattermost",
                sender_handle="engineer",
                recipient="",
                body="x",
                reply_to_metadata={
                    "channel": "chan1eng000000000000000000",
                    "thread_ts": "root00000000000000000000000",
                    "thread_anchor": "root00000000000000000000000",
                },
            )
        )

        assert fake.posts[0]["root_id"] == "root00000000000000000000000"

    @pytest.mark.asyncio
    async def test_a_recipient_handle_becomes_a_dm_channel(self):
        """A username is not a channel id; posting one as though it were
        is how every fallback DM to a colleague or a human used to fail."""
        transport = _make_transport()
        fake = _with_fake_client(
            transport, users={"jane": "janeid00000000000000000000"}
        )
        transport._handle_registry = _StubRegistry({"jane-founder": "jane"})

        ok = await transport.send(
            OutboundMessage(
                transport="mattermost",
                sender_handle="engineer",
                recipient="jane-founder",
                body="escalating",
            )
        )

        assert ok is True
        assert fake.direct == [(BOT_ID, "janeid00000000000000000000")]
        assert fake.posts[0]["channel_id"] == "dmchannel00000000000000000"

    @pytest.mark.asyncio
    async def test_the_default_channel_name_is_resolved_to_an_id(self):
        """``role.integrations.mattermost.channel`` is a NAME; the API
        takes ids only."""
        transport = _make_transport()
        fake = _with_fake_client(transport)

        ok = await transport.send(
            OutboundMessage(
                transport="mattermost",
                sender_handle="engineer",
                recipient="",
                body="status update",
            )
        )

        assert ok is True
        assert fake.posts[0]["channel_id"] == "chan1eng000000000000000000"

    @pytest.mark.asyncio
    async def test_an_unknown_default_channel_fails_cleanly(self):
        transport = _make_transport()
        fake = _with_fake_client(transport)
        fake.channels = {}

        ok = await transport.send(
            OutboundMessage(
                transport="mattermost",
                sender_handle="engineer",
                recipient="",
                body="x",
            )
        )

        assert ok is False

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


class TestIdentityIsServerAuthoritative:
    """The configured username is a guess: the provisioner may have
    applied `provisioning.username_prefix`, or the account may have been
    renamed. That name is what the agent is told to answer to, what it
    writes when mentioning itself, and what the backfill's text grammar
    matches."""

    @pytest.mark.asyncio
    async def test_start_corrects_the_username_from_the_server(self):
        transport = MattermostTransport(base_url="https://chat.example", team="n")
        transport.register_bot(
            "engineer", MattermostBotConfig(bot_token="t", username="swe")
        )

        class _Client(_FakeClient):
            async def me(self) -> dict[str, Any]:
                return {"id": BOT_ID, "username": "agent-swe"}

            async def client_config(self) -> dict[str, Any]:
                return {"SiteURL": "https://chat.example"}

        transport._clients["engineer"] = _Client()  # type: ignore[assignment]
        await transport.start()

        assert transport.bots["engineer"].username == "agent-swe"


class TestFleetWiring:
    """The two one-line mistakes that make the integration silently
    inbound-dead — a hand-rolled ws URL and a missing event queue — both
    present exactly as "nothing errors and no agent ever hears anything"."""

    @pytest.mark.asyncio
    async def test_an_https_instance_gets_a_wss_fleet(self, monkeypatch):
        captured: dict[str, Any] = {}

        class _FakeFleet:
            def __init__(self, **kwargs: Any) -> None:
                captured.update(kwargs)
                self.seats: list[tuple[str, str]] = []

            async def register_seat(self, handle: str, token: str) -> None:
                self.seats.append((handle, token))
                captured["seats"] = self.seats

            async def start(self) -> None:
                captured["started"] = True

        import crewlet.mattermost.events as events_mod

        monkeypatch.setattr(events_mod, "MattermostEventFleet", _FakeFleet)

        transport = MattermostTransport(base_url="https://chat.example", team="n")
        transport.register_bot(
            "engineer", MattermostBotConfig(bot_token="tok", username="agent-swe")
        )
        transport._clients["engineer"] = _FakeClient()  # type: ignore[assignment]
        transport.set_event_queue(object())
        await transport.start()

        assert captured["websocket_url"] == "wss://chat.example/api/v4/websocket"
        assert captured["seats"] == [("engineer", "tok")]
        assert captured["started"] is True

    @pytest.mark.asyncio
    async def test_without_a_queue_the_fleet_is_not_started_silently(self, caplog):
        transport = MattermostTransport(base_url="https://chat.example", team="n")
        transport.register_bot("engineer", MattermostBotConfig(bot_token="tok"))
        transport._clients["engineer"] = _FakeClient()  # type: ignore[assignment]

        with caplog.at_level("WARNING"):
            await transport.start()

        assert transport.fleet is None
        assert "mattermost_fleet_not_started_no_queue" in caplog.text


class TestUnregisterBot:
    @pytest.mark.asyncio
    async def test_it_releases_the_seat_and_its_socket(self):
        transport = _make_transport()
        _with_fake_client(transport)
        transport._handle_registry = _StubRegistry({})

        class _Fleet:
            def __init__(self) -> None:
                self.dropped: list[str] = []

            async def unregister_seat(self, handle: str) -> None:
                self.dropped.append(handle)

        fleet = _Fleet()
        transport._fleet = fleet

        await transport.unregister_bot("engineer")

        assert fleet.dropped == ["engineer"]
        assert "engineer" not in transport.bots
        assert transport.user_id_for("engineer") == ""


# --- identity churn -------------------------------------------------------


def _real_registry() -> Any:
    """A real HandleRegistry — the stub above cannot show a stale alias."""
    from crewlet.agent.pool import AgentPool
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.queue.memory import MemoryEventQueue

    return HandleRegistry(AgentPool(MemoryEventQueue()))


class TestUsernameRebinding:
    """A seat's username is not fixed: a config apply can change it, and
    ``start()`` replaces the configured guess with the name the server
    reports. ``register_external_id`` overwrites only the handle → name
    direction, so a plain re-register leaves the OLD name resolving to
    this handle — an alias that survives decommission."""

    @pytest.mark.asyncio
    async def test_a_server_correction_retires_the_configured_name(self):
        registry = _real_registry()
        transport = MattermostTransport(
            base_url="https://chat.example", team="n", handle_registry=registry
        )
        transport.register_bot(
            "engineer", MattermostBotConfig(bot_token="t", username="swe")
        )
        assert registry.known_external_ids("mattermost") == {"swe"}

        class _Client(_FakeClient):
            async def me(self) -> dict[str, Any]:
                return {"id": BOT_ID, "username": "agent-swe"}

            async def client_config(self) -> dict[str, Any]:
                return {"SiteURL": "https://chat.example"}

        transport._clients["engineer"] = _Client()  # type: ignore[assignment]
        await transport.start()

        assert registry.known_external_ids("mattermost") == {"agent-swe"}
        assert registry.get_external_id("mattermost", "engineer") == "agent-swe"

    def test_a_config_apply_rename_retires_the_previous_name(self):
        registry = _real_registry()
        transport = MattermostTransport(
            base_url="https://chat.example", team="n", handle_registry=registry
        )
        transport.register_bot(
            "engineer", MattermostBotConfig(bot_token="t", username="swe")
        )
        transport.register_bot(
            "engineer", MattermostBotConfig(bot_token="t", username="eng")
        )

        assert registry.known_external_ids("mattermost") == {"eng"}

    @pytest.mark.asyncio
    async def test_decommission_leaves_no_alias_behind(self):
        """The whole point: after unregister_bot nothing in the
        ``mattermost`` namespace still resolves to the departed seat."""
        registry = _real_registry()
        transport = MattermostTransport(
            base_url="https://chat.example", team="n", handle_registry=registry
        )
        transport.register_bot(
            "engineer", MattermostBotConfig(bot_token="t", username="swe")
        )

        class _Client(_FakeClient):
            async def me(self) -> dict[str, Any]:
                return {"id": BOT_ID, "username": "agent-swe"}

            async def client_config(self) -> dict[str, Any]:
                return {"SiteURL": "https://chat.example"}

        transport._clients["engineer"] = _Client()  # type: ignore[assignment]
        await transport.start()
        await transport.unregister_bot("engineer")

        assert registry.known_external_ids("mattermost") == set()
        assert registry.known_external_ids("mattermost_bot") == set()

    def test_a_freed_name_can_be_claimed_by_another_seat(self):
        registry = _real_registry()
        transport = MattermostTransport(
            base_url="https://chat.example", team="n", handle_registry=registry
        )
        transport.register_bot(
            "engineer", MattermostBotConfig(bot_token="t", username="swe")
        )
        transport.register_bot(
            "engineer", MattermostBotConfig(bot_token="t", username="eng")
        )
        transport.register_bot(
            "designer", MattermostBotConfig(bot_token="d", username="swe")
        )

        assert registry.known_external_ids("mattermost") == {"eng", "swe"}
        assert registry.get_external_id("mattermost", "designer") == "swe"
        assert registry.get_external_id("mattermost", "engineer") == "eng"


class TestRetiredClientsAreActuallyClosed:
    """A token swap displaces a client. Closing it in a fire-and-forget
    ``create_task`` is not a close: the loop keeps only a weak reference
    to a running task, so it can be collected mid-flight and the
    connection pool leaks — the exact failure the close exists to
    prevent."""

    @pytest.mark.asyncio
    async def test_a_token_swap_closes_the_displaced_client(self):
        transport = _make_transport()
        closed: list[str] = []

        class _Closable:
            async def close(self) -> None:
                closed.append("displaced")

        transport._clients["engineer"] = _Closable()  # type: ignore[assignment]
        transport.register_bot("engineer", MattermostBotConfig(bot_token="new-token"))

        assert transport._pending_closes  # held, not dropped on the floor
        await transport.stop()
        assert closed == ["displaced"]
        assert transport._pending_closes == set()

    def test_a_swap_without_a_loop_says_so_instead_of_suppressing(self, caplog):
        transport = _make_transport()

        class _Closable:
            async def close(self) -> None:  # pragma: no cover - never awaited
                raise AssertionError("must not run without a loop")

        transport._clients["engineer"] = _Closable()  # type: ignore[assignment]
        with caplog.at_level("WARNING"):
            transport.register_bot(
                "engineer", MattermostBotConfig(bot_token="new-token")
            )

        assert "mattermost_client_close_unscheduled" in caplog.text
