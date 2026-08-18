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
import random
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

#: Proportional jitter added to each reconnect delay.  Every seat drops
#: at the same instant when the server restarts or the network blips, so
#: an unjittered schedule reconnects the whole fleet in lockstep — and
#: each reconnect is not one request but a backfill walking every channel
#: that seat is in.  A quarter of the delay is enough to smear the herd
#: without making the schedule unrecognisable in logs.
_BACKOFF_JITTER = 0.25

#: How long a connection must survive before the reconnect backoff is
#: allowed to drop back to the bottom of the schedule.  A socket the
#: server accepts and then closes immediately looks, to the loop, exactly
#: like a healthy connection that ended — so resetting on any clean
#: return turns a server that hangs up on sight into a one-per-second
#: reconnect storm that never escalates.  A minute is well past every
#: handshake this integration performs (connect, auth ack, identity read,
#: backfill) and well inside the 5-minute ceiling the schedule tops out
#: at, so a genuinely healthy socket is never mistaken for a flapping one.
_STABLE_CONNECTION_SECONDS = 60.0

#: Websocket keepalive.  Sized to the server's ACTUAL deadlines, which
#: are far longer than this once claimed: ``web_conn.go`` sets
#: ``pongWaitTime = 100s`` (the read deadline) and pings the client every
#: ``pingInterval = 60s``; Mattermost's own web client pings at 30 s.
#:
#: The point of pinging at all is to notice a half-open TCP connection —
#: the server's pings already keep its deadline fresh. A 10 s/20 s pair
#: meant any 20-second stall in this process (a backfill walking a
#: seat's channels, a GC pause, a burst of publishes) tore down a
#: perfectly healthy socket and re-ran the whole reconnect, backfill
#: included. 30 s/60 s detects a dead peer inside the server's own
#: 100 s window while tolerating a stall an order of magnitude longer.
_PING_INTERVAL = 30.0
_PING_TIMEOUT = 60.0

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
#:
#: ``post_edited`` is deliberately NOT here, matching the Slack transport,
#: which classes edits as bookkeeping for the same reason: an edit of a
#: message the agent has already triaged is not a new request, and waking
#: it again costs a full turn to re-answer what it answered.  Forwarding
#: edits was also incoherent in practice — the seat's de-duplication ring
#: dropped the edit of any post still in it and forwarded the edit of any
#: post that had aged out, so the behaviour was decided by how much
#: traffic had passed since the original.
_FORWARDED_EVENTS = frozenset({"posted"})


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
    #: ``time.monotonic()`` at the moment this connection started reading
    #: LIVE traffic — set once the handshake, identity read and backfill
    #: are behind it.  ``None`` until then, and reset on every attempt.
    live_since: float | None = field(default=None, repr=False)

    def already_seen(self, post_id: str) -> bool:
        """Whether this post has already been forwarded."""
        return post_id in self.seen_lookup

    def remember(self, post_id: str) -> bool:
        """Record *post_id*; ``False`` when it was already seen.

        Called only AFTER a successful publish: a post recorded here is
        one the fleet has committed to having delivered, and the
        reconnect backfill will never offer it again.
        """
        if post_id in self.seen_lookup:
            return False
        self.seen_lookup.add(post_id)
        self.seen_posts.append(post_id)
        if len(self.seen_posts) > _DEDUPE_RING:
            evicted = self.seen_posts.pop(0)
            self.seen_lookup.discard(evicted)
        return True


async def send_authentication_challenge(socket: Any, token: str) -> None:
    """Send the frame that authenticates a Mattermost websocket.

    Mattermost's websocket carries no credential in its handshake: the
    connection opens unauthenticated and the client proves itself in the
    first frame.  Shared with ``crewlet mattermost doctor``, which opens
    exactly this handshake to prove a seat's token really works — the
    check that separates "the token is syntactically fine" from "this
    seat will actually hear anything".
    """
    await socket.send(
        json.dumps(
            {
                "seq": _AUTH_SEQ,
                "action": "authentication_challenge",
                "data": {"token": token},
            }
        )
    )


async def await_authentication(
    socket: Any,
    *,
    handle: str = "",
    timeout: float = _AUTH_TIMEOUT,
) -> list[Any]:
    """Block until the server acknowledges the authentication challenge.

    Mattermost answers ``authentication_challenge`` with a status frame
    carrying ``seq_reply``, and sends an unsolicited ``hello`` event once
    the connection is authenticated.  A **rejected** token gets
    ``status: FAIL`` and then a close.

    Without waiting for that, both outcomes look identical to a caller:
    ``connect()`` succeeds, the send succeeds, and the read loop ends
    immediately when the server hangs up.  That returns cleanly, which
    the fleet's seat loop would treat as a normal server-side close — so
    a revoked or mistyped token would reconnect forever, logging
    ``mattermost_ws_connected`` on every pass.  Raising
    :class:`MattermostAuthError` instead puts a bad token on the backoff
    schedule and names it in the log.

    Returns any non-auth frames read while waiting, so a post that
    arrives in the same batch as the ack is not dropped.
    """
    early: list[Any] = []
    while True:
        raw = await asyncio.wait_for(socket.recv(), timeout=timeout)
        try:
            frame = json.loads(raw)
        except (TypeError, ValueError):
            logger.debug("mattermost_ws_undecodable_frame", handle=handle)
            continue
        if not isinstance(frame, dict):
            continue
        if frame.get("seq_reply") == _AUTH_SEQ:
            status = str(frame.get("status") or "").upper()
            if status == "OK":
                return early
            error = frame.get("error")
            raise MattermostAuthError(
                f"websocket authentication rejected for {handle or 'seat'}: "
                f"{error or status or 'unknown error'}"
            )
        if frame.get("event") == "hello":
            # Success is also signalled unsolicited, and can land before
            # the status reply.
            return early
        early.append(raw)


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
            # reconnect picks the new one up.  Awaited, not just
            # cancelled: the displaced loop may be mid-request on the
            # client that is about to be closed underneath it.
            await self._cancel_seat(handle)
            await self._close_client(handle)
        self._seats[handle] = _SeatState(handle=handle, token=token)
        self._clients[handle] = MattermostClient(self._base_url, token)
        if self._running:
            self._start_seat(handle)

    async def unregister_seat(self, handle: str) -> None:
        """Drop a seat (role removed by a live config apply)."""
        await self._cancel_seat(handle)
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
        """Cancel every connection and release the HTTP clients.

        Every seat task is *awaited* after cancellation, not merely
        cancelled: a websocket whose task is still unwinding holds an
        open socket and, worse, may be mid-request on the HTTP client
        this method is about to close — which surfaces as
        ``RuntimeError: client has been closed`` from a task nobody is
        waiting on, long after ``stop()`` returned.
        """
        self._running = False
        tasks = [
            task
            for task in (self._cancel_task(handle) for handle in list(self._seats))
            if task is not None
        ]
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
        # A *finished* task is not a running connection.  The loop only
        # returns when the fleet is stopping, so a done task here means
        # something escaped it — treating that as "already started" would
        # leave the seat permanently deaf.
        if seat is None or (seat.task is not None and not seat.task.done()):
            return
        task = asyncio.create_task(
            self._seat_loop(seat), name=f"mattermost-ws-{handle}"
        )
        seat.task = task
        task.add_done_callback(lambda t: self._seat_task_finished(handle, t))

    def _seat_task_finished(self, handle: str, task: asyncio.Task[None]) -> None:
        """Log a seat loop that ended for any reason other than shutdown."""
        if task.cancelled() or not self._running:
            return
        exc = task.exception()
        logger.error(
            "mattermost_seat_loop_exited",
            handle=handle,
            error=str(exc) if exc else "returned",
        )

    def _cancel_task(self, handle: str) -> asyncio.Task[None] | None:
        """Cancel a seat's task and hand it back for awaiting."""
        seat = self._seats.get(handle)
        if seat is None or seat.task is None:
            return None
        task = seat.task
        task.cancel()
        seat.task = None
        return task

    async def _cancel_seat(self, handle: str) -> None:
        """Cancel a seat's task and wait for it to unwind."""
        task = self._cancel_task(handle)
        if task is None:
            return
        with contextlib.suppress(asyncio.CancelledError, Exception):
            await task

    # ----- the per-seat connection loop ----------------------------------

    async def _seat_loop(self, seat: _SeatState) -> None:
        """Connect, authenticate, read — forever, with backoff."""
        attempt = 0
        while self._running:
            seat.live_since = None
            try:
                await self._connect_once(seat)
            except asyncio.CancelledError:
                raise
            except MattermostAuthError as exc:
                # Never reaches the live-read phase, so it is never
                # "stable": a revoked token climbs the schedule instead of
                # reconnecting once a second forever.
                logger.error(
                    "mattermost_ws_auth_rejected",
                    handle=seat.handle,
                    attempt=attempt + 1,
                    error=str(exc),
                )
            except Exception as exc:
                logger.warning(
                    "mattermost_ws_connection_failed",
                    handle=seat.handle,
                    attempt=attempt + 1,
                    error=str(exc),
                )
            # Stability is judged on EVERY exit, not just a clean return.
            # Mattermost closes without a close frame, so an ordinary
            # disconnect after hours of healthy traffic surfaces here as
            # ``ConnectionClosedError`` — an exception. Resetting only on
            # the clean path would ratchet a perfectly healthy seat up to
            # the 5-minute ceiling and leave it there for the life of the
            # process.
            #
            # And it is timed from the LIVE READ, not from the start of
            # the attempt: the handshake, the identity read and a backfill
            # that issues one REST call per channel can themselves take a
            # minute, which would make a socket that died on its first
            # frame look like a connection that lasted.
            lived = (
                time.monotonic() - seat.live_since
                if seat.live_since is not None
                else 0.0
            )
            if lived >= _STABLE_CONNECTION_SECONDS:
                attempt = 0
            else:
                attempt += 1
                if seat.live_since is not None:
                    logger.warning(
                        "mattermost_ws_closed_early",
                        handle=seat.handle,
                        attempt=attempt,
                        seconds=round(lived, 2),
                    )
            if not self._running:
                return
            delay = RECONNECT_BACKOFF_SECONDS[
                min(attempt, len(RECONNECT_BACKOFF_SECONDS) - 1)
            ]
            await asyncio.sleep(delay * (1.0 + random.uniform(0.0, _BACKOFF_JITTER)))

    async def _connect_once(self, seat: _SeatState) -> None:
        """One connection's lifetime: auth, backfill the gap, then read."""
        import websockets

        async with websockets.connect(
            self._websocket_url,
            ping_interval=_PING_INTERVAL,
            ping_timeout=_PING_TIMEOUT,
            max_size=None,
        ) as socket:
            await send_authentication_challenge(socket, seat.token)
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
            await self._anchor_cursor(seat)
            for raw in early:
                await self._handle_frame(seat, raw)
            # From here on the connection is doing its job; how long it
            # lasts from this point is what tells the reconnect loop
            # whether this seat is healthy or flapping.
            seat.live_since = time.monotonic()
            async for raw in socket:
                await self._handle_frame(seat, raw)

    async def _await_authentication(self, seat: _SeatState, socket: Any) -> list[Any]:
        """Block until the server acknowledges the authentication challenge."""
        return await await_authentication(socket, handle=seat.handle)

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

        now_ms = await self._now_ms(seat)
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

    async def _anchor_cursor(self, seat: _SeatState) -> None:
        """Give a seat a reconnect floor even before its first post.

        Called after :meth:`_backfill` on every connect, so the first one
        still replays nothing.  Without it a connection that drops before
        the seat has seen a single message leaves the cursor at zero, and
        the next connect reads that as a first boot with no gap to cover —
        so everything said during the outage is dropped silently.  A
        seat that joins a quiet channel and reconnects an hour later is
        exactly that case.
        """
        if not seat.last_event_ms:
            seat.last_event_ms = await self._now_ms(seat)

    async def _now_ms(self, seat: _SeatState) -> int:
        """ "Now", on the clock the post timestamps are stamped by.

        The backfill window is a comparison between a server-stamped post
        timestamp and the present, so both sides have to come from the
        same clock; skew between the engine host and the Mattermost host
        would otherwise widen or silently truncate the window.  Falls
        back to the local clock when the server does not say.
        """
        client = self._clients.get(seat.handle)
        if client is not None:
            server_ms = await client.server_time_ms()
            if server_ms:
                return server_ms
        return int(time.time() * 1000)

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
        if not post_id or seat.already_seen(post_id):
            return False

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
        if not await self._publish(payload):
            # Nothing is recorded for a post that did not land: leaving
            # the cursor and the dedupe ring untouched is what lets the
            # next reconnect's backfill redeliver it. Advancing them
            # first meant a queue hiccup dropped the message permanently,
            # with the gap already marked as covered.
            return False

        seat.remember(post_id)
        # The cursor advances on CREATION time, matching what the
        # backfill selects on.  Taking it from ``update_at`` would let an
        # edit push the cursor past posts that had not arrived yet.
        create_at = int(post.get("create_at") or post.get("update_at") or 0)
        if create_at > seat.last_event_ms:
            seat.last_event_ms = create_at
        return True

    async def _publish(self, payload: dict[str, Any]) -> bool:
        """Publish one envelope; ``False`` when it did not land."""
        if self._queue is None:
            return False
        try:
            await self._queue.publish(
                INBOUND_TOPIC,
                Event(type="raw_webhook", source="mattermost", payload=payload),
            )
        except Exception as exc:
            # A publish failure must not tear down the socket — the next
            # message still deserves a chance to land — but it must not
            # be recorded as delivered either.
            logger.warning("mattermost_publish_failed", error=str(exc))
            return False
        return True


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
    "MattermostAuthError",
    "MattermostEventFleet",
    "await_authentication",
    "send_authentication_challenge",
]
