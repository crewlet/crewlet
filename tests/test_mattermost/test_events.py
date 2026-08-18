"""Tests for the Mattermost websocket fleet — framing, dedupe, backfill."""

from __future__ import annotations

import asyncio
import json
import time
from typing import Any
from unittest import mock

import pytest

from crewlet.mattermost.events import (
    _STABLE_CONNECTION_SECONDS,
    MAX_BACKFILL_WINDOW_SECONDS,
    RECONNECT_BACKOFF_SECONDS,
    MattermostAuthError,
    MattermostEventFleet,
    _decode_embedded,
)

BOT_ID = "botuserid00000000000000000"


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
        with mock.patch.object(asyncio, "sleep", _record_sleep):
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

        async def _long_connection(_seat: Any) -> None:
            nonlocal attempts
            attempts += 1
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
