"""The Mattermost inbound fleet — one websocket per agent seat.

Every other inbound integration Crewlet has is a stateless webhook route
in the API process.  Mattermost cannot be, and the reason is structural
rather than a matter of taste:

- Its **outgoing webhooks fire only in public channels** — the server
  returns early for anything that is not ``ChannelTypeOpen`` — so DMs and
  private channels would never be delivered at all.
- Their payload carries **no ``root_id``** (no thread attribution), **no
  channel type** and **no mention list**, so even public-channel traffic
  could not be routed the way the engine routes Slack's.

The supported path for an external service is the **WebSocket event
API**, authenticated per user.  So the engine holds one connection per
Mattermost-enabled agent seat, and each connection publishes the events
it receives onto the same ``raw_webhook`` envelope every webhook route
uses — which is what lets everything downstream (the notification
service, coalescing, the prompt registry, the dashboard) stay unaware
that this source arrived over a socket.

**Reconnects are the hard part.**  Mattermost's websocket replays
nothing: a connection that drops and comes back has simply missed
whatever happened in between, with no cursor, sequence gap or resume
token to detect it with.  So each seat records the newest post it has
seen and, on reconnect, re-reads every channel it is a member of via
``GET /channels/{id}/posts?since=`` and replays the gap in order.  The
window is bounded (:data:`MAX_BACKFILL_WINDOW_SECONDS`) because the
purpose is to cover a blip, not to catch up after an outage — replaying
hours of conversation would wake agents into turns about messages that
have long since been resolved, at one LLM turn each.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import time
from dataclasses import dataclass, field
from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import Event
from crewlet.mattermost.client import MattermostClient, MattermostError

logger = get_logger("mattermost.events")

#: The topic every inbound integration publishes onto.  Imported lazily
#: in :meth:`_publish` to keep this module importable without the queue
#: package configured.
INBOUND_TOPIC = "crewlet.notifications.inbound"

#: Backoff schedule between reconnect attempts, in seconds.  Capped
#: rather than unbounded-exponential: a seat that cannot connect is a
#: configuration problem an operator has to see, and a 5-minute ceiling
#: keeps the retry visible in logs without hammering a server that is
#: down.  The last value repeats for every subsequent attempt.
RECONNECT_BACKOFF_SECONDS = (1.0, 2.0, 5.0, 15.0, 30.0, 60.0, 300.0)

#: How far back a reconnect will replay.  Fifteen minutes covers the
#: cases backfill exists for — a network blip, a rolling Mattermost
#: restart, a brief engine pause — while refusing to flood the fleet
#: after a real outage.  Every message replayed costs a full agent turn,
#: so an hour-long gap replayed in full would be both expensive and
#: wrong: the conversations have moved on.  A gap wider than this is
#: logged with the amount skipped rather than silently truncated.
MAX_BACKFILL_WINDOW_SECONDS = 900.0

#: How many post ids each seat remembers for de-duplication.  A post can
#: legitimately arrive twice at the reconnect boundary — once through the
#: backfill read, once from the live socket that came up mid-read — and a
#: duplicate here is a duplicate agent turn.  512 is far more than any
#: single backfill window yields while staying trivially small in memory.
_DEDUPE_RING = 512

#: Websocket ping interval.  Mattermost closes an idle connection after
#: roughly 30 s of silence, so the client pings well inside that.
_PING_INTERVAL = 10.0
_PING_TIMEOUT = 20.0

#: How long to wait for the server to acknowledge the authentication
#: challenge.  It is a single round trip on a socket that has already
#: completed its handshake, so this only has to survive a slow or loaded
#: server; sized to :data:`_PING_TIMEOUT` so an auth that stalls is
#: treated on the same timescale as a connection that has gone quiet.
_AUTH_TIMEOUT = 20.0

#: Sequence number used for the authentication challenge.  The server
#: echoes it back as ``seq_reply``, which is how the ack is identified.
_AUTH_SEQ = 1

#: Mattermost websocket event names this fleet forwards.  Everything else
#: (typing, presence changes, channel-viewed bookkeeping, preference
#: updates) is chatter that would wake an agent with nothing to act on.
_FORWARDED_EVENTS = frozenset({"posted", "post_edited"})


class MattermostAuthError(MattermostError):
    """The server rejected a seat's websocket authentication challenge.

    Distinct from a transport failure because the operator response is
    different: a connection error means the server is unreachable, this
    means the seat's token is wrong, revoked, or belongs to a disabled
    bot — re-run ``crewlet mattermost provision``.
    """


@dataclass
class _SeatState:
    """Per-seat bookkeeping that must survive a reconnect."""

    handle: str
    token: str
    user_id: str = ""
    #: Newest post timestamp seen, in Mattermost's epoch milliseconds.
    #: The reconnect cursor.
    last_event_ms: int = 0
    #: Recently forwarded post ids, oldest first — the reconnect-boundary
    #: duplicate guard.
    seen_posts: list[str] = field(default_factory=list)
    seen_lookup: set[str] = field(default_factory=set)
    task: asyncio.Task[None] | None = field(default=None, repr=False)

    def remember(self, post_id: str) -> bool:
        """Record *post_id*; ``False`` when it was already seen."""
        if post_id in self.seen_lookup:
            return False
        self.seen_lookup.add(post_id)
        self.seen_posts.append(post_id)
        if len(self.seen_posts) > _DEDUPE_RING:
            evicted = self.seen_posts.pop(0)
            self.seen_lookup.discard(evicted)
        return True


class MattermostEventFleet:
    """Holds one authenticated websocket per Mattermost-enabled seat.

    Owned by the engine (not the API process): the connections
    authenticate as each *agent*, so they belong where the per-agent
    credentials already live.  That does give the engine an ingress role
    no other integration needs — a deliberate trade, since the
    alternative is putting every bot token into the API process purely to
    preserve the shape.
    """

    def __init__(
        self,
        *,
        base_url: str,
        websocket_url: str,
        team: str,
        event_queue: Any,
        backfill_window_seconds: float = MAX_BACKFILL_WINDOW_SECONDS,
    ) -> None:
        self._base_url = base_url
        self._websocket_url = websocket_url
        self._team = team
        self._queue = event_queue
        self._backfill_window = backfill_window_seconds
        self._seats: dict[str, _SeatState] = {}
        self._clients: dict[str, MattermostClient] = {}
        self._team_id = ""
        self._running = False

    # ----- registration -------------------------------------------------

    async def register_seat(self, handle: str, token: str) -> None:
        """Declare a seat the fleet should hold a connection for.

        Registration before :meth:`start` is the normal path; registering
        afterwards (a live config apply that adds a role) starts that
        seat's connection immediately.

        Async because it owns resources: a rotated token replaces the
        seat's HTTP client, and the displaced one has to be closed rather
        than dropped on the floor with an open connection pool.
        """
        if not token:
            logger.warning("mattermost_seat_without_token", handle=handle)
            return
        existing = self._seats.get(handle)
        if existing is not None and existing.token == token:
            return
        if existing is not None:
            # A rotated token means the live socket is authenticated with
            # a credential that may already be revoked — drop it so the
            # reconnect picks the new one up.
            self._stop_seat(handle)
            await self._close_client(handle)
        self._seats[handle] = _SeatState(handle=handle, token=token)
        self._clients[handle] = MattermostClient(self._base_url, token)
        if self._running:
            self._start_seat(handle)

    async def unregister_seat(self, handle: str) -> None:
        """Drop a seat (role removed by a live config apply)."""
        self._stop_seat(handle)
        self._seats.pop(handle, None)
        await self._close_client(handle)

    async def _close_client(self, handle: str) -> None:
        """Release a seat's HTTP client, never raising."""
        client = self._clients.pop(handle, None)
        if client is None:
            return
        try:
            await client.close()
        except Exception as exc:
            logger.warning(
                "mattermost_client_close_failed", handle=handle, error=str(exc)
            )

    @property
    def handles(self) -> list[str]:
        """Handles the fleet is holding connections for."""
        return sorted(self._seats)

    # ----- lifecycle ----------------------------------------------------

    async def start(self) -> None:
        """Resolve the team, then open every registered seat's socket."""
        self._running = True
        for handle in list(self._seats):
            self._start_seat(handle)
        logger.info(
            "mattermost_fleet_started",
            seats=len(self._seats),
            team=self._team,
        )

    async def stop(self) -> None:
        """Cancel every connection and release the HTTP clients."""
        self._running = False
        for handle in list(self._seats):
            self._stop_seat(handle)
        tasks = [s.task for s in self._seats.values() if s.task is not None]
        for task in tasks:
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await task
        for client in self._clients.values():
            with contextlib.suppress(Exception):
                await client.close()
        self._clients.clear()
        logger.info("mattermost_fleet_stopped")

    def _start_seat(self, handle: str) -> None:
        seat = self._seats.get(handle)
        if seat is None or seat.task is not None:
            return
        seat.task = asyncio.create_task(
            self._seat_loop(seat), name=f"mattermost-ws-{handle}"
        )

    def _stop_seat(self, handle: str) -> None:
        seat = self._seats.get(handle)
        if seat is None or seat.task is None:
            return
        seat.task.cancel()
        seat.task = None

    # ----- the per-seat connection loop ----------------------------------

    async def _seat_loop(self, seat: _SeatState) -> None:
        """Connect, authenticate, read — forever, with backoff."""
        attempt = 0
        while self._running:
            try:
                await self._connect_once(seat)
                # A clean return means the server closed the socket; that
                # is a normal reconnect, not an error, so the backoff
                # resets.
                attempt = 0
            except asyncio.CancelledError:
                raise
            except MattermostAuthError as exc:
                logger.error(
                    "mattermost_ws_auth_rejected",
                    handle=seat.handle,
                    attempt=attempt + 1,
                    error=str(exc),
                )
                attempt += 1
            except Exception as exc:
                logger.warning(
                    "mattermost_ws_connection_failed",
                    handle=seat.handle,
                    attempt=attempt + 1,
                    error=str(exc),
                )
                attempt += 1
            if not self._running:
                return
            delay = RECONNECT_BACKOFF_SECONDS[
                min(attempt, len(RECONNECT_BACKOFF_SECONDS) - 1)
            ]
            await asyncio.sleep(delay)

    async def _connect_once(self, seat: _SeatState) -> None:
        """One connection's lifetime: auth, backfill the gap, then read."""
        import websockets

        async with websockets.connect(
            self._websocket_url,
            ping_interval=_PING_INTERVAL,
            ping_timeout=_PING_TIMEOUT,
            max_size=None,
        ) as socket:
            await socket.send(
                json.dumps(
                    {
                        "seq": _AUTH_SEQ,
                        "action": "authentication_challenge",
                        "data": {"token": seat.token},
                    }
                )
            )
            early = await self._await_authentication(seat, socket)
            await self._resolve_identity(seat)
            logger.info(
                "mattermost_ws_connected",
                handle=seat.handle,
                user_id=seat.user_id,
            )
            # Cover whatever was missed while the socket was down BEFORE
            # reading live traffic, so the agent sees the conversation in
            # order.  Duplicates across the boundary are caught by the
            # seat's dedupe ring.
            await self._backfill(seat)
            for raw in early:
                await self._handle_frame(seat, raw)
            async for raw in socket:
                await self._handle_frame(seat, raw)

    async def _await_authentication(self, seat: _SeatState, socket: Any) -> list[Any]:
        """Block until the server acknowledges the authentication challenge.

        Mattermost answers ``authentication_challenge`` with a status
        frame carrying ``seq_reply``, and sends an unsolicited ``hello``
        event once the connection is authenticated.  A **rejected** token
        gets ``status: FAIL`` and then a close.

        Without waiting for that, both outcomes look identical from here:
        ``connect()`` succeeds, the send succeeds, and the read loop ends
        immediately when the server hangs up.  That returns cleanly, which
        :meth:`_run_seat` treats as a normal server-side close and resets
        the backoff — so a revoked or mistyped token reconnects once a
        second, forever, logging ``mattermost_ws_connected`` on every
        pass.  Raising here instead puts a bad token on the backoff
        schedule and names it in the log.

        Returns any non-auth frames read while waiting, so a post that
        arrives in the same batch as the ack is not dropped.  They are
        replayed by the caller *after* backfill, keeping the ordering
        guarantee that backfilled history precedes live traffic.
        """
        early: list[Any] = []
        while True:
            raw = await asyncio.wait_for(socket.recv(), timeout=_AUTH_TIMEOUT)
            try:
                frame = json.loads(raw)
            except (TypeError, ValueError):
                logger.debug("mattermost_ws_undecodable_frame", handle=seat.handle)
                continue
            if not isinstance(frame, dict):
                continue
            if frame.get("seq_reply") == _AUTH_SEQ:
                status = str(frame.get("status") or "").upper()
                if status == "OK":
                    return early
                error = frame.get("error")
                raise MattermostAuthError(
                    f"websocket authentication rejected for {seat.handle}: "
                    f"{error or status or 'unknown error'}"
                )
            if frame.get("event") == "hello":
                # Success is also signalled unsolicited, and can land
                # before the status reply.
                return early
            early.append(raw)

    async def _resolve_identity(self, seat: _SeatState) -> None:
        """Learn the bot's own user id (own-message suppression needs it)."""
        if seat.user_id:
            return
        client = self._clients.get(seat.handle)
        if client is None:
            return
        try:
            me = await client.me()
        except MattermostError as exc:
            logger.warning(
                "mattermost_identity_unresolved",
                handle=seat.handle,
                error=str(exc),
            )
            return
        seat.user_id = str((me or {}).get("id") or "")

    async def _backfill(self, seat: _SeatState) -> None:
        """Replay what the socket missed while it was down.

        Reads every channel the bot is a member of rather than only those
        it has already seen traffic in: a message in a channel the bot
        was invited to *during* the outage would otherwise be invisible
        forever.
        """
        if not seat.last_event_ms:
            # First connection of this process — there is no gap to
            # cover, and replaying "everything since the epoch" would be
            # every message in every channel.
            return
        client = self._clients.get(seat.handle)
        if client is None or not seat.user_id:
            return
        if not self._team_id:
            team = await client.get_team_by_name(self._team)
            self._team_id = str((team or {}).get("id") or "")
            if not self._team_id:
                logger.warning("mattermost_team_unresolved", team=self._team)
                return

        now_ms = int(time.time() * 1000)
        floor_ms = now_ms - int(self._backfill_window * 1000)
        since_ms = seat.last_event_ms
        if since_ms < floor_ms:
            logger.warning(
                "mattermost_backfill_window_exceeded",
                handle=seat.handle,
                skipped_seconds=round((floor_ms - since_ms) / 1000, 1),
                window_seconds=self._backfill_window,
            )
            since_ms = floor_ms

        try:
            channels = await client.list_channels_for_user(seat.user_id, self._team_id)
        except MattermostError as exc:
            logger.warning(
                "mattermost_backfill_channels_failed",
                handle=seat.handle,
                error=str(exc),
            )
            return

        replayed = 0
        for channel in channels:
            channel_id = str(channel.get("id") or "")
            if not channel_id:
                continue
            try:
                posts = await client.posts_since(channel_id, since_ms)
            except MattermostError as exc:
                logger.warning(
                    "mattermost_backfill_channel_failed",
                    handle=seat.handle,
                    channel=channel_id,
                    error=str(exc),
                )
                continue
            for post in posts:
                if await self._forward_post(
                    seat,
                    post=post,
                    channel_type=str(channel.get("type") or ""),
                    channel_name=str(channel.get("name") or ""),
                    mentions=[],
                    replayed=True,
                ):
                    replayed += 1
        if replayed:
            logger.info(
                "mattermost_backfill_replayed",
                handle=seat.handle,
                posts=replayed,
                since_ms=since_ms,
            )

    async def _handle_frame(self, seat: _SeatState, raw: str | bytes) -> None:
        """Decode one websocket frame and forward it if it is a post."""
        try:
            frame = json.loads(raw)
        except (TypeError, ValueError):
            logger.debug("mattermost_ws_undecodable_frame", handle=seat.handle)
            return
        if not isinstance(frame, dict):
            return
        event = str(frame.get("event") or "")
        if event not in _FORWARDED_EVENTS:
            return
        data = frame.get("data") or {}

        # Mattermost nests the post as a JSON *string* inside the event
        # data, and does the same for the mention list.
        post = _decode_embedded(data.get("post"))
        if not isinstance(post, dict):
            return
        mentions = _decode_embedded(data.get("mentions"))

        await self._forward_post(
            seat,
            post=post,
            channel_type=str(data.get("channel_type") or ""),
            channel_name=str(data.get("channel_name") or ""),
            mentions=[str(m) for m in mentions] if isinstance(mentions, list) else [],
            replayed=False,
            sender_name=str(data.get("sender_name") or ""),
            event_name=event,
        )

    async def _forward_post(
        self,
        seat: _SeatState,
        *,
        post: dict[str, Any],
        channel_type: str,
        channel_name: str,
        mentions: list[str],
        replayed: bool,
        sender_name: str = "",
        event_name: str = "posted",
    ) -> bool:
        """Publish one post onto the inbound topic.

        Returns ``True`` when it was forwarded, ``False`` when the dedupe
        ring had already seen it.
        """
        post_id = str(post.get("id") or "")
        if not post_id or not seat.remember(post_id):
            return False

        create_at = int(post.get("update_at") or post.get("create_at") or 0)
        if create_at > seat.last_event_ms:
            seat.last_event_ms = create_at

        payload = {
            "body": {
                "event": event_name,
                "post": post,
                "channel_type": channel_type,
                "channel_name": channel_name,
                "mentions": mentions,
                "sender_name": sender_name,
                "bot_user_id": seat.user_id,
                "replayed": replayed,
            },
            "handle": seat.handle,
            "headers": {},
        }
        await self._publish(payload)
        return True

    async def _publish(self, payload: dict[str, Any]) -> None:
        if self._queue is None:
            return
        try:
            await self._queue.publish(
                INBOUND_TOPIC,
                Event(type="raw_webhook", source="mattermost", payload=payload),
            )
        except Exception as exc:
            # A publish failure must not tear down the socket — the next
            # message still deserves a chance to land.
            logger.warning("mattermost_publish_failed", error=str(exc))


def _decode_embedded(value: Any) -> Any:
    """Decode a field Mattermost nests as a JSON string.

    ``data.post`` and ``data.mentions`` arrive as serialised JSON inside
    the event payload rather than as objects.  Already-decoded values
    pass through so the backfill path (which reads real JSON from REST)
    and the socket path can share one forwarder.
    """
    if isinstance(value, (dict, list)):
        return value
    if not isinstance(value, str) or not value:
        return None
    try:
        return json.loads(value)
    except ValueError:
        return None


__all__ = [
    "MAX_BACKFILL_WINDOW_SECONDS",
    "RECONNECT_BACKOFF_SECONDS",
    "MattermostEventFleet",
]
