"""Tests for the Mattermost websocket fleet — framing, dedupe, backfill."""

from __future__ import annotations

import asyncio
import contextlib
import json
import time
from typing import Any
from unittest import mock

import pytest

from crewlet.mattermost import events as events_module
from crewlet.mattermost.client import MattermostError
from crewlet.mattermost.events import (
    _BACKOFF_JITTER,
    _STABLE_CONNECTION_SECONDS,
    MAX_BACKFILL_WINDOW_SECONDS,
    RECONNECT_BACKOFF_SECONDS,
    MattermostAuthError,
    MattermostEventFleet,
    _decode_embedded,
)

BOT_ID = "botuserid00000000000000000"

#: The module's own ``random``, so tests can pin the reconnect jitter.
events_random = events_module.random


class _QueueStub:
    def __init__(self) -> None:
        self.published: list[tuple[str, Any]] = []

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))

    @property
    def posts(self) -> list[dict[str, Any]]:
        return [e.payload["body"]["post"] for _, e in self.published]


def _fleet(queue: Any = None) -> MattermostEventFleet:
    return MattermostEventFleet(
        base_url="https://chat.example",
        websocket_url="wss://chat.example/api/v4/websocket",
        team="nimbus",
        event_queue=queue if queue is not None else _QueueStub(),
    )


def _frame(post_id: str, *, message: str = "hi", root_id: str = "") -> str:
    """A websocket frame in Mattermost's real shape — the post nested as
    a JSON *string* rather than an object."""
    post: dict[str, Any] = {
        "id": post_id,
        "channel_id": "c1",
        "user_id": "humanid0000000000000000000",
        "message": message,
        "create_at": 1700000000000,
    }
    if root_id:
        post["root_id"] = root_id
    return json.dumps(
        {
            "event": "posted",
            "data": {
                "post": json.dumps(post),
                "channel_type": "O",
                "channel_name": "engineering",
                "mentions": json.dumps([BOT_ID]),
                "sender_name": "@alice",
            },
            "seq": 3,
        }
    )


# --- embedded JSON decoding ----------------------------------------------


class TestEmbeddedDecoding:
    def test_decodes_a_json_string(self):
        assert _decode_embedded('{"a": 1}') == {"a": 1}
        assert _decode_embedded('["x"]') == ["x"]

    def test_passes_through_already_decoded_values(self):
        """The backfill path reads real JSON from REST, so one forwarder
        has to accept both shapes."""
        assert _decode_embedded({"a": 1}) == {"a": 1}
        assert _decode_embedded(["x"]) == ["x"]

    def test_bad_input_is_none(self):
        assert _decode_embedded("not json") is None
        assert _decode_embedded("") is None
        assert _decode_embedded(None) is None


# --- registration ---------------------------------------------------------


class TestRegistration:
    @pytest.mark.asyncio
    async def test_registers_seats(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok-a")
        await fleet.register_seat("designer", "tok-b")
        assert fleet.handles == ["designer", "engineer"]

    @pytest.mark.asyncio
    async def test_tokenless_seat_is_refused(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "")
        assert fleet.handles == []

    @pytest.mark.asyncio
    async def test_reregistering_the_same_token_is_a_noop(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        state = fleet._seats["engineer"]
        await fleet.register_seat("engineer", "tok")
        assert fleet._seats["engineer"] is state

    @pytest.mark.asyncio
    async def test_rotated_token_replaces_the_seat_and_closes_its_client(self):
        """A live socket authenticated with a revoked credential has to be
        dropped, and its HTTP client closed rather than orphaned."""
        fleet = _fleet()
        await fleet.register_seat("engineer", "old")
        old_state = fleet._seats["engineer"]
        old_client = fleet._clients["engineer"]
        await fleet.register_seat("engineer", "new")
        assert fleet._seats["engineer"] is not old_state
        assert fleet._seats["engineer"].token == "new"
        assert fleet._clients["engineer"] is not old_client

    @pytest.mark.asyncio
    async def test_unregister_drops_the_seat(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        await fleet.unregister_seat("engineer")
        assert fleet.handles == []
        assert "engineer" not in fleet._clients


# --- frame handling -------------------------------------------------------


class TestFrameHandling:
    @pytest.mark.asyncio
    async def test_posted_frame_is_published(self):
        queue = _QueueStub()
        fleet = _fleet(queue)
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        seat.user_id = BOT_ID

        await fleet._handle_frame(seat, _frame("p1"))

        assert len(queue.published) == 1
        topic, event = queue.published[0]
        assert topic == "crewlet.notifications.inbound"
        assert event.type == "raw_webhook"
        assert event.source == "mattermost"
        assert event.payload["handle"] == "engineer"
        body = event.payload["body"]
        assert body["post"]["id"] == "p1"
        assert body["mentions"] == [BOT_ID]
        assert body["channel_type"] == "O"
        assert body["replayed"] is False

    @pytest.mark.asyncio
    async def test_non_post_events_are_ignored(self):
        """Typing, presence and channel-viewed chatter would wake an agent
        with nothing to act on."""
        queue = _QueueStub()
        fleet = _fleet(queue)
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        for event_name in ("typing", "status_change", "channel_viewed", "hello"):
            await fleet._handle_frame(
                seat, json.dumps({"event": event_name, "data": {}})
            )
        assert queue.published == []

    @pytest.mark.asyncio
    async def test_undecodable_frame_is_survived(self):
        queue = _QueueStub()
        fleet = _fleet(queue)
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        await fleet._handle_frame(seat, "}{ not json")
        await fleet._handle_frame(seat, json.dumps(["not", "a", "dict"]))
        assert queue.published == []

    @pytest.mark.asyncio
    async def test_duplicate_post_is_published_once(self):
        """A post can arrive twice at the reconnect boundary — once from
        the backfill read, once from the live socket."""
        queue = _QueueStub()
        fleet = _fleet(queue)
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]

        await fleet._handle_frame(seat, _frame("p1"))
        await fleet._handle_frame(seat, _frame("p1"))

        assert len(queue.published) == 1

    @pytest.mark.asyncio
    async def test_last_event_cursor_advances(self):
        """The cursor is what a reconnect backfills from."""
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        assert seat.last_event_ms == 0
        await fleet._handle_frame(seat, _frame("p1"))
        assert seat.last_event_ms == 1700000000000

    @pytest.mark.asyncio
    async def test_dedupe_ring_is_bounded(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        for i in range(600):
            seat.remember(f"p{i}")
        assert len(seat.seen_posts) <= 512
        assert len(seat.seen_lookup) == len(seat.seen_posts)
        # The oldest ids fell out; the newest are still guarded.
        assert seat.remember("p0") is True
        assert seat.remember("p599") is False


# --- backfill -------------------------------------------------------------


class TestBackfill:
    @pytest.mark.asyncio
    async def test_first_connection_does_not_backfill(self):
        """With no cursor there is no gap — "everything since the epoch"
        would replay every message in every channel."""
        queue = _QueueStub()
        fleet = _fleet(queue)
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        seat.user_id = BOT_ID
        assert seat.last_event_ms == 0

        await fleet._backfill(seat)
        assert queue.published == []


class _SocketStub:
    """Just enough of a websockets connection for the auth handshake."""

    def __init__(self, frames: list[str]) -> None:
        self._frames = list(frames)
        self.recv_count = 0

    async def recv(self) -> str:
        self.recv_count += 1
        if not self._frames:
            raise AssertionError("recv() called past the scripted frames")
        return self._frames.pop(0)


class TestAuthenticationHandshake:
    """A rejected token must not look like a healthy connection.

    ``_run_seat`` resets its backoff on a clean return, so an
    unacknowledged challenge would reconnect once a second forever while
    logging success on every pass.
    """

    @pytest.mark.asyncio
    async def test_ok_status_reply_completes_the_handshake(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        socket = _SocketStub([json.dumps({"status": "OK", "seq_reply": 1})])

        early = await fleet._await_authentication(fleet._seats["engineer"], socket)
        assert early == []

    @pytest.mark.asyncio
    async def test_unsolicited_hello_completes_the_handshake(self):
        """Mattermost also signals success with an unsolicited ``hello``,
        which can land before the status reply."""
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        socket = _SocketStub([json.dumps({"event": "hello", "seq": 0})])

        assert await fleet._await_authentication(fleet._seats["engineer"], socket) == []

    @pytest.mark.asyncio
    async def test_fail_status_reply_raises_an_auth_error(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "bad-token")
        socket = _SocketStub(
            [
                json.dumps(
                    {
                        "status": "FAIL",
                        "seq_reply": 1,
                        "error": {"message": "Invalid or expired session"},
                    }
                )
            ]
        )

        with pytest.raises(MattermostAuthError) as caught:
            await fleet._await_authentication(fleet._seats["engineer"], socket)
        assert "engineer" in str(caught.value)

    @pytest.mark.asyncio
    async def test_posts_arriving_before_the_ack_are_kept(self):
        """A post in the same batch as the ack must not be dropped — it is
        replayed by the caller after backfill."""
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        post = _frame("p1")
        socket = _SocketStub([post, json.dumps({"status": "OK", "seq_reply": 1})])

        early = await fleet._await_authentication(fleet._seats["engineer"], socket)
        assert early == [post]

    @pytest.mark.asyncio
    async def test_undecodable_frames_do_not_end_the_handshake(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        socket = _SocketStub(["not json", json.dumps({"status": "OK", "seq_reply": 1})])

        assert await fleet._await_authentication(fleet._seats["engineer"], socket) == []
        assert socket.recv_count == 2


# --- tuning constants -----------------------------------------------------


def test_backoff_schedule_is_capped_and_monotonic():
    """A seat that cannot connect is a config problem an operator has to
    see — the schedule must stay visible rather than growing unbounded."""
    assert list(RECONNECT_BACKOFF_SECONDS) == sorted(RECONNECT_BACKOFF_SECONDS)
    assert RECONNECT_BACKOFF_SECONDS[0] >= 1.0
    assert RECONNECT_BACKOFF_SECONDS[-1] <= 300.0


def test_backfill_window_is_bounded():
    """Replaying an outage in full would cost one agent turn per message
    for conversations that have long since moved on."""
    assert 0 < MAX_BACKFILL_WINDOW_SECONDS <= 3600.0


# --- seat lifecycle -------------------------------------------------------


class TestSeatLifecycle:
    @pytest.mark.asyncio
    async def test_stop_awaits_every_seat_task(self):
        """``stop()`` must not return while a seat loop is still unwinding.

        It closes the seats' HTTP clients, so a task left mid-request runs
        on a closed client and raises into nobody's hands.
        """
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        started = asyncio.Event()
        ended = asyncio.Event()

        async def _loop(seat: Any) -> None:
            started.set()
            try:
                await asyncio.sleep(3600)
            finally:
                # A cancellation still has to unwind before stop() returns.
                ended.set()

        fleet._seat_loop = _loop  # type: ignore[method-assign]
        await fleet.start()
        await started.wait()

        await fleet.stop()
        assert ended.is_set()
        assert fleet._seats["engineer"].task is None

    @pytest.mark.asyncio
    async def test_a_finished_task_does_not_block_a_restart(self):
        """A seat whose loop ended is deaf; treating it as running would
        leave it that way until the process restarts."""
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")

        async def _immediate(seat: Any) -> None:
            return None

        fleet._seat_loop = _immediate  # type: ignore[method-assign]
        fleet._running = True
        fleet._start_seat("engineer")
        first = fleet._seats["engineer"].task
        assert first is not None
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        assert first.done()

        fleet._start_seat("engineer")
        assert fleet._seats["engineer"].task is not first
        await fleet.stop()

    @pytest.mark.asyncio
    async def test_backoff_does_not_reset_on_an_instant_close(self):
        """A server that accepts and immediately hangs up returns cleanly.

        Resetting the schedule on that turns a refusing server into a
        one-per-second reconnect storm that never escalates, so only a
        connection that LASTED counts as healthy.
        """
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        delays: list[float] = []
        attempts = 0

        async def _instant_close(_seat: Any) -> None:
            nonlocal attempts
            attempts += 1
            if attempts >= 4:
                fleet._running = False

        async def _record_sleep(delay: float) -> None:
            delays.append(delay)

        fleet._connect_once = _instant_close  # type: ignore[method-assign]
        fleet._running = True
        with (
            mock.patch.object(asyncio, "sleep", _record_sleep),
            mock.patch.object(events_random, "uniform", lambda a, b: 0.0),
        ):
            await fleet._seat_loop(seat)

        # The loop returns before sleeping on the pass that stops it.
        assert delays == list(RECONNECT_BACKOFF_SECONDS[1:4])

    @pytest.mark.asyncio
    async def test_backoff_resets_after_a_connection_that_lasted(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        delays: list[float] = []
        attempts = 0
        clock = [1000.0]

        async def _long_connection(seat_: Any) -> None:
            nonlocal attempts
            attempts += 1
            seat_.live_since = clock[0]
            clock[0] += _STABLE_CONNECTION_SECONDS + 1
            if attempts >= 3:
                fleet._running = False

        async def _record_sleep(delay: float) -> None:
            delays.append(delay)

        fleet._connect_once = _long_connection  # type: ignore[method-assign]
        fleet._running = True
        with (
            mock.patch.object(asyncio, "sleep", _record_sleep),
            mock.patch.object(time, "monotonic", lambda: clock[0]),
            mock.patch.object(events_random, "uniform", lambda a, b: 0.0),
        ):
            await fleet._seat_loop(seat)

        assert delays == [RECONNECT_BACKOFF_SECONDS[0]] * 2


# --- the reconnect cursor -------------------------------------------------


class _ClockClient:
    """A client stub that only answers the server-clock read."""

    def __init__(self, now_ms: int) -> None:
        self.now_ms = now_ms
        self.calls = 0

    async def server_time_ms(self) -> int:
        self.calls += 1
        return self.now_ms


class TestReconnectCursor:
    @pytest.mark.asyncio
    async def test_now_comes_from_the_server_clock(self):
        """The window compares SERVER-stamped post timestamps against
        "now", so "now" cannot come from the engine host's clock."""
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        fleet._clients["engineer"] = _ClockClient(1700000000000)  # type: ignore[assignment]

        assert await fleet._now_ms(fleet._seats["engineer"]) == 1700000000000

    @pytest.mark.asyncio
    async def test_falls_back_to_the_local_clock(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        fleet._clients["engineer"] = _ClockClient(0)  # type: ignore[assignment]

        now = await fleet._now_ms(fleet._seats["engineer"])
        assert abs(now - int(time.time() * 1000)) < 5000

    @pytest.mark.asyncio
    async def test_first_connect_anchors_the_cursor(self):
        """A seat that has seen no post still needs a reconnect floor.

        Otherwise a drop before the first message looks like a fresh boot
        on the next connect, and the whole outage is skipped in silence.
        """
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        fleet._clients["engineer"] = _ClockClient(1700000000000)  # type: ignore[assignment]

        await fleet._anchor_cursor(seat)
        assert seat.last_event_ms == 1700000000000

    @pytest.mark.asyncio
    async def test_anchoring_never_moves_a_real_cursor_backwards(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        seat.last_event_ms = 1699999999000
        fleet._clients["engineer"] = _ClockClient(1700000000000)  # type: ignore[assignment]

        await fleet._anchor_cursor(seat)
        assert seat.last_event_ms == 1699999999000


class TestForwardedEvents:
    @pytest.mark.asyncio
    async def test_edits_do_not_wake_an_agent(self):
        """Matching the Slack transport, which classes edits as
        bookkeeping: re-answering an already-triaged message costs a full
        turn. Forwarding them was also incoherent — the dedupe ring
        dropped the edit of a recent post and forwarded the edit of an old
        one, so behaviour depended on channel traffic."""
        queue = _QueueStub()
        fleet = _fleet(queue)
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]

        frame = json.loads(_frame("p1"))
        frame["event"] = "post_edited"
        await fleet._handle_frame(seat, json.dumps(frame))

        assert queue.published == []

    @pytest.mark.asyncio
    async def test_the_cursor_tracks_creation_not_update(self):
        """The backfill selects on ``create_at``; a cursor taken from
        ``update_at`` could jump past posts that had not arrived yet."""
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]

        frame = json.loads(_frame("p1"))
        post = json.loads(frame["data"]["post"])
        post["update_at"] = 1700000999000
        frame["data"]["post"] = json.dumps(post)
        await fleet._handle_frame(seat, json.dumps(frame))

        assert seat.last_event_ms == 1700000000000


class _BackfillClient:
    """A client stub for the reconnect gap replay."""

    def __init__(
        self,
        *,
        channels: list[dict[str, Any]] | None = None,
        posts: dict[str, list[dict[str, Any]]] | None = None,
        now_ms: int = 1700000900000,
        fail_channels: set[str] | None = None,
    ) -> None:
        self._channels = (
            channels
            if channels is not None
            else [
                {"id": "c1", "name": "engineering", "type": "O"},
                {"id": "c2", "name": "product", "type": "O"},
            ]
        )
        self._posts = posts or {}
        self._now_ms = now_ms
        self._fail = fail_channels or set()
        self.since_seen: dict[str, int] = {}

    async def server_time_ms(self) -> int:
        return self._now_ms

    async def get_team_by_name(self, name: str) -> dict[str, Any]:
        return {"id": "team1", "name": name}

    async def list_channels_for_user(
        self, user_id: str, team_id: str
    ) -> list[dict[str, Any]]:
        return list(self._channels)

    async def posts_since(self, channel_id: str, since_ms: int) -> list[dict[str, Any]]:
        self.since_seen[channel_id] = since_ms
        if channel_id in self._fail:
            raise MattermostError("channel unreadable", status=403)
        return list(self._posts.get(channel_id, []))


def _post(post_id: str, *, channel: str = "c1", create_at: int = 1700000500000):
    return {
        "id": post_id,
        "channel_id": channel,
        "user_id": "humanid0000000000000000000",
        "message": "hello",
        "create_at": create_at,
    }


class TestBackfillReplay:
    """The subsystem whose whole reason for existing is "a message sent
    while the socket was down must not be lost"."""

    async def _fleet_with(self, client: Any) -> tuple[Any, Any, Any]:
        queue = _QueueStub()
        fleet = _fleet(queue)
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        seat.user_id = BOT_ID
        seat.last_event_ms = 1700000000000
        fleet._clients["engineer"] = client
        return fleet, seat, queue

    @pytest.mark.asyncio
    async def test_every_channel_is_read_not_just_ones_with_traffic(self):
        """A message in a channel the bot was invited to DURING the outage
        would otherwise be invisible forever."""
        client = _BackfillClient(posts={"c2": [_post("p9", channel="c2")]})
        fleet, seat, queue = await self._fleet_with(client)

        await fleet._backfill(seat)

        assert set(client.since_seen) == {"c1", "c2"}
        assert [p["id"] for p in queue.posts] == ["p9"]
        assert queue.published[0][1].payload["body"]["replayed"] is True

    @pytest.mark.asyncio
    async def test_a_gap_wider_than_the_window_is_clamped_and_logged(self, caplog):
        """Replaying an hour of conversation would cost one agent turn per
        message for threads that have long since moved on."""
        client = _BackfillClient(now_ms=1700003600000)
        fleet, seat, queue = await self._fleet_with(client)
        seat.last_event_ms = 1700000000000  # an hour behind

        with caplog.at_level("WARNING"):
            await fleet._backfill(seat)

        assert "mattermost_backfill_window_exceeded" in caplog.text
        floor = 1700003600000 - int(MAX_BACKFILL_WINDOW_SECONDS * 1000)
        assert client.since_seen["c1"] == floor

    @pytest.mark.asyncio
    async def test_one_unreadable_channel_does_not_abandon_the_rest(self):
        client = _BackfillClient(
            posts={"c2": [_post("p9", channel="c2")]}, fail_channels={"c1"}
        )
        fleet, seat, queue = await self._fleet_with(client)

        await fleet._backfill(seat)

        assert [p["id"] for p in queue.posts] == ["p9"]

    @pytest.mark.asyncio
    async def test_the_boundary_duplicate_is_published_once(self):
        """A post can legitimately arrive twice at the reconnect boundary —
        once through the backfill read, once from the live socket."""
        client = _BackfillClient(posts={"c1": [_post("p1")]})
        fleet, seat, queue = await self._fleet_with(client)

        await fleet._backfill(seat)
        await fleet._handle_frame(seat, _frame("p1"))

        assert len(queue.published) == 1

    @pytest.mark.asyncio
    async def test_an_unresolved_identity_skips_rather_than_misattributes(self):
        client = _BackfillClient(posts={"c1": [_post("p1")]})
        fleet, seat, queue = await self._fleet_with(client)
        seat.user_id = ""

        await fleet._backfill(seat)

        assert queue.published == []


class TestAuthRejectionReachesTheOperator:
    """The doc's troubleshooting rows tell an operator to grep for
    ``mattermost_ws_auth_rejected``; nothing asserted the seat loop emits
    it, or that a bad token lands on the backoff schedule rather than
    hot-looping."""

    @pytest.mark.asyncio
    async def test_a_rejected_token_is_named_and_backed_off(self, caplog):
        fleet = _fleet()
        await fleet.register_seat("engineer", "revoked")
        seat = fleet._seats["engineer"]
        delays: list[float] = []
        attempts = 0

        async def _reject(_seat: Any) -> None:
            nonlocal attempts
            attempts += 1
            if attempts >= 3:
                fleet._running = False
            raise MattermostAuthError("websocket authentication rejected")

        async def _record_sleep(delay: float) -> None:
            delays.append(delay)

        fleet._connect_once = _reject  # type: ignore[method-assign]
        fleet._running = True
        with (
            caplog.at_level("ERROR"),
            mock.patch.object(asyncio, "sleep", _record_sleep),
            mock.patch.object(events_random, "uniform", lambda a, b: 0.0),
        ):
            await fleet._seat_loop(seat)

        assert "mattermost_ws_auth_rejected" in caplog.text
        assert delays == list(RECONNECT_BACKOFF_SECONDS[1:3])


class TestBackoffAfterALivedConnection:
    """Mattermost closes without a close frame, so an ordinary disconnect
    after hours of healthy traffic arrives as an EXCEPTION. Judging
    stability only on the clean-return path ratcheted a healthy seat up to
    the 5-minute ceiling and left it there for the life of the process."""

    @pytest.mark.asyncio
    async def test_a_dropped_but_long_lived_connection_resets_the_backoff(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        delays: list[float] = []
        attempts = 0
        clock = [1000.0]

        async def _drop_after_a_long_life(seat_: Any) -> None:
            nonlocal attempts
            attempts += 1
            seat_.live_since = clock[0]
            clock[0] += _STABLE_CONNECTION_SECONDS + 5
            if attempts >= 3:
                fleet._running = False
            raise ConnectionError("no close frame")

        async def _record_sleep(delay: float) -> None:
            delays.append(delay)

        fleet._connect_once = _drop_after_a_long_life  # type: ignore[method-assign]
        fleet._running = True
        with (
            mock.patch.object(asyncio, "sleep", _record_sleep),
            mock.patch.object(time, "monotonic", lambda: clock[0]),
            mock.patch.object(events_random, "uniform", lambda a, b: 0.0),
        ):
            await fleet._seat_loop(seat)

        assert delays == [RECONNECT_BACKOFF_SECONDS[0]] * 2

    @pytest.mark.asyncio
    async def test_a_drop_before_live_traffic_still_escalates(self):
        """The handshake, identity read and backfill can take a minute on
        their own, so stability is timed from the live read — not from the
        start of the attempt."""
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        delays: list[float] = []
        attempts = 0
        clock = [1000.0]

        async def _die_during_setup(_seat: Any) -> None:
            nonlocal attempts
            attempts += 1
            clock[0] += 300  # a long, entirely unproductive attempt
            if attempts >= 3:
                fleet._running = False
            raise ConnectionError("dropped mid-backfill")

        async def _record_sleep(delay: float) -> None:
            delays.append(delay)

        fleet._connect_once = _die_during_setup  # type: ignore[method-assign]
        fleet._running = True
        with (
            mock.patch.object(asyncio, "sleep", _record_sleep),
            mock.patch.object(time, "monotonic", lambda: clock[0]),
            mock.patch.object(events_random, "uniform", lambda a, b: 0.0),
        ):
            await fleet._seat_loop(seat)

        assert delays == list(RECONNECT_BACKOFF_SECONDS[1:3])


class TestReconnectJitter:
    """Every seat drops at the same instant when the server restarts, and
    each reconnect is not one request but a backfill walking every channel
    that seat is in."""

    @pytest.mark.asyncio
    async def test_the_delay_is_smeared(self):
        fleet = _fleet()
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]
        delays: list[float] = []
        attempts = 0

        async def _fail(_seat: Any) -> None:
            nonlocal attempts
            attempts += 1
            if attempts >= 2:
                fleet._running = False
            raise ConnectionError("down")

        async def _record_sleep(delay: float) -> None:
            delays.append(delay)

        fleet._connect_once = _fail  # type: ignore[method-assign]
        fleet._running = True
        with (
            mock.patch.object(asyncio, "sleep", _record_sleep),
            mock.patch.object(events_random, "uniform", lambda a, b: b),
        ):
            await fleet._seat_loop(seat)

        base = RECONNECT_BACKOFF_SECONDS[1]
        assert delays == [pytest.approx(base * (1 + _BACKOFF_JITTER))]


class TestUndeliveredPosts:
    @pytest.mark.asyncio
    async def test_a_failed_publish_is_left_redeliverable(self):
        """Advancing the cursor and the dedupe ring before the publish
        landed meant a queue hiccup dropped the message permanently, with
        the gap already marked as covered."""

        class _BrokenQueue:
            async def publish(self, topic: str, event: Any) -> None:
                raise RuntimeError("broker unavailable")

        fleet = _fleet(_BrokenQueue())
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]

        await fleet._handle_frame(seat, _frame("p1"))

        assert seat.last_event_ms == 0
        assert not seat.already_seen("p1")

    @pytest.mark.asyncio
    async def test_a_delivered_post_advances_both(self):
        queue = _QueueStub()
        fleet = _fleet(queue)
        await fleet.register_seat("engineer", "tok")
        seat = fleet._seats["engineer"]

        await fleet._handle_frame(seat, _frame("p1"))

        assert seat.last_event_ms == 1700000000000
        assert seat.already_seen("p1")


class TestConcurrentDrain:
    """Mattermost gives each write a 30 s deadline, and a socket nobody
    reads stops accepting writes once the kernel buffer fills — so a
    backfill walking a seat's channels one REST call at a time could get
    the connection closed underneath it, reconnect, and backfill again."""

    class _Socket:
        """A socket that yields frames and then closes."""

        def __init__(self, frames: list[str]) -> None:
            self._frames = list(frames)
            self.read_started = asyncio.Event()

        def __aiter__(self):
            return self._iterate()

        async def _iterate(self):
            self.read_started.set()
            for frame in self._frames:
                yield frame

    @pytest.mark.asyncio
    async def test_frames_are_drained_while_the_backfill_runs(self):
        queue = _QueueStub()
        fleet = _fleet(queue)
        await fleet.register_seat("engineer", "tok")
        socket = self._Socket([_frame("p1"), _frame("p2")])
        inbox: asyncio.Queue[Any] = asyncio.Queue()

        pump = asyncio.create_task(fleet._pump_frames(socket, inbox))
        await socket.read_started.wait()
        # Stand in for a slow backfill: the pump must have kept reading.
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        pump.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await pump

        drained = []
        while not inbox.empty():
            drained.append(inbox.get_nowait())
        assert len(drained) >= 2

    @pytest.mark.asyncio
    async def test_a_closed_socket_ends_the_consumer(self):
        """Without the sentinel the consumer would wait forever on a queue
        nothing will ever fill again."""
        fleet = _fleet()
        inbox: asyncio.Queue[Any] = asyncio.Queue()

        await fleet._pump_frames(self._Socket([]), inbox)

        assert inbox.get_nowait() is events_module._SOCKET_CLOSED
