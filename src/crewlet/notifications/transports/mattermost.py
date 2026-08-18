"""MattermostTransport — per-agent Mattermost identity and message parsing.

Architecture:

- Each agent gets its own Mattermost **bot account** and one personal
  access token.  That single credential drives everything: the inbound
  websocket, the outbound REST calls, and the Mattermost MCP server.
  There is no second secret because there is no inbound webhook to
  verify — see :class:`~crewlet.config.MattermostConfig`.
- **Inbound** events arrive from :mod:`crewlet.mattermost.events`, which
  holds the sockets and republishes each post onto the standard
  ``raw_webhook`` envelope; this transport parses that into an
  :class:`~crewlet.notifications.protocol.InboundNotification`.
- **Outbound**: agents primarily use Mattermost MCP tools (the
  ``mattermost`` server in ``mcp_servers``, credentialed from
  ``role.mcp_env.mattermost``).  ``send()`` is the transport-agnostic
  fallback.
- ``set_status()`` raises the composer typing indicator.  Mattermost's
  wording is fixed by the client, so this backend declares
  ``supports_status_text = False`` and the per-phase phrase pools go
  inert.

Where Mattermost is *better* than Slack for routing: the websocket event
carries a server-stamped ``channel_type`` and a server-computed
``mentions`` list of user ids, so DM detection and mention detection are
field reads rather than inference.  The regex grammar in
:mod:`.mattermost_threads` is only the fallback for reconnect-backfilled
posts, which come over REST without a mention list.
"""

from __future__ import annotations

import asyncio
import contextlib
from dataclasses import dataclass
from typing import Any

from crewlet._logging import get_logger
from crewlet.db.chat_thread_follows import ChatThreadFollowRepository
from crewlet.mattermost.client import (
    DEFAULT_TYPING_THROTTLE_MS,
    DIRECT_CHANNEL_TYPES,
    MattermostClient,
    MattermostError,
    normalize_base_url,
    site_urls_match,
    typing_throttle_from,
)
from crewlet.notifications.handle import HandleRegistry
from crewlet.notifications.protocol import (
    InboundNotification,
    OutboundMessage,
)
from crewlet.notifications.transports.chat_threads import (
    ChatThreadTracker,
    ThreadFollowReason,
)
from crewlet.notifications.transports.mattermost_threads import (
    build_mattermost_thread_tracker,
)
from crewlet.notifications.typing_status import (
    StatusPhrases,
    WorkingStatusDriver,
    WorkingStatusMode,
)

logger = get_logger("notifications.mattermost")

#: Fraction of ``TimeBetweenUserTypingUpdatesMilliseconds`` the heartbeat
#: re-asserts at.
#:
#: That setting is NOT a server-side throttle — the server accepts every
#: ``POST /users/me/typing`` it is given (``publishUserTyping`` has no
#: rate limit) — it is the interval Mattermost's own web client waits
#: between emitting typing events while a human types, and therefore the
#: rhythm every client is built around.  So the heartbeat belongs
#: *inside* the window, not just outside it: at 0.6 the indicator is
#: re-asserted every 3 s at the 5 s default, which means a single lost
#: call still lands before the previous assertion could lapse.  Slack's
#: driver makes the same trade (45 s inside a 120 s expiry).
_TYPING_REFRESH_FRACTION = 0.6

#: Floor for the derived heartbeat.  An operator who sets the throttle
#: absurdly low would otherwise turn a cosmetic indicator into a request
#: storm for the whole length of a turn.
_TYPING_REFRESH_FLOOR_SECONDS = 1.0


def _typing_refresh_interval(throttle_ms: float) -> float:
    """Heartbeat for a backend whose window is *throttle_ms* long."""
    return max(
        _TYPING_REFRESH_FLOOR_SECONDS,
        throttle_ms / 1000.0 * _TYPING_REFRESH_FRACTION,
    )


def mattermost_post_skip_reason(post: dict[str, Any]) -> str:
    """Why a Mattermost post must NOT wake an agent.

    Returns ``""`` for a real, user-authored message.  Mattermost marks
    channel bookkeeping — joins, leaves, header and purpose changes,
    channel renames — with a ``system_*`` post type.  Those carry text,
    but the text is *about* the channel rather than addressed to anyone,
    so delivering them produces turns triaging "user X joined the
    channel".
    """
    post_type = str(post.get("type") or "")
    if post_type.startswith("system_"):
        return f"system post type: {post_type}"
    if post.get("delete_at"):
        return "deleted post"
    return ""


def mattermost_post_text(post: dict[str, Any]) -> str:
    """User-visible text of a post.

    A file attached without a comment has an empty ``message`` but real
    content — render the attachment count so a genuine post never
    produces a blank notification body, mirroring what the Slack
    transport does for a comment-less file share.
    """
    message = str(post.get("message") or "")
    if message:
        return message
    file_ids = post.get("file_ids") or []
    if isinstance(file_ids, list) and file_ids:
        count = len(file_ids)
        noun = "file" if count == 1 else "files"
        return f"(shared {count} {noun})"
    return ""


def _is_channel_id(value: str) -> bool:
    """Whether *value* is a Mattermost id rather than a name.

    Mattermost ids are exactly 26 characters of ``[a-z0-9]``
    (``model.IsValidId``), which no channel name or username can be:
    names are capped at 64 characters but are the operator's own words,
    and a 26-character all-lowercase-alphanumeric name is vanishingly
    unlikely to collide by accident.
    """
    return len(value) == 26 and all(c.islower() or c.isdigit() for c in value)


@dataclass
class MattermostBotConfig:
    """Credentials and settings for one Mattermost bot (one agent)."""

    bot_token: str = ""
    username: str = ""
    channel: str = ""
    """Default outbound channel name for this agent."""


class MattermostTransport:
    """Multi-bot Mattermost transport — one bot account per agent.

    Inbound events are produced by :class:`.mattermost.events.MattermostEventFleet`
    and parsed here; outbound messaging is primarily via MCP tools, with
    ``send()`` as the fallback.
    """

    name: str = "mattermost"

    # --- StatusPoster contract (see notifications.typing_status) ---
    status_backend: str = "mattermost"
    supports_status_text: bool = False
    """Mattermost's composer indicator has a fixed, client-supplied
    wording.  The engine can raise it but cannot say anything with it, so
    the per-phase phrase pools are inert on this backend."""
    dm_channel_id_prefix: str = ""
    """Empty deliberately: Mattermost channel ids are opaque 26-char
    strings, so a prefix test would mark arbitrary public channels as
    DMs.  ``channel_type`` is authoritative here and always present."""

    def __init__(
        self,
        base_url: str,
        team: str,
        handle_registry: HandleRegistry | None = None,
        thread_routing: bool = True,
        thread_follow_repo: ChatThreadFollowRepository | None = None,
        typing_status_mode: WorkingStatusMode = WorkingStatusMode.OFF,
        typing_status_phrases: StatusPhrases | None = None,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._team = team
        self._bots: dict[str, MattermostBotConfig] = {}
        self._clients: dict[str, MattermostClient] = {}
        self._user_ids: dict[str, str] = {}
        self._handle_registry = handle_registry
        self._thread_routing = thread_routing
        self._thread_tracker: ChatThreadTracker | None = (
            build_mattermost_thread_tracker(thread_follow_repo)
            if thread_follow_repo
            else None
        )
        # Derived from the server's own throttle at ``start()``; the
        # module default only covers a server that refuses the config
        # read.  See ``MattermostClient.typing_throttle_ms``.
        self.status_refresh_interval: float = _typing_refresh_interval(
            DEFAULT_TYPING_THROTTLE_MS
        )
        self._typing_status = WorkingStatusDriver(
            self, typing_status_mode, phrases=typing_status_phrases
        )
        # The inbound websocket fleet is owned HERE rather than by the
        # engine, for the same reason the typing driver is: its
        # connections are per-agent credentials this transport already
        # holds, so a live config swap that rebuilds the transport tears
        # the sockets down and brings them back with it, instead of
        # stranding a fleet authenticated with rotated tokens.
        self._fleet: Any = None
        self._event_queue: Any = None
        self._started = False
        self.last_skip_reason: str = ""
        # Resolution caches. Channel names and DM channels are stable
        # for the life of a transport, and the alternative is a lookup
        # per outbound message.
        self._team_id: str = ""
        self._channel_ids: dict[str, str] = {}
        self._dm_channels: dict[tuple[str, str], str] = {}

    @property
    def bots(self) -> dict[str, MattermostBotConfig]:
        """Registered per-agent bots (handle → config)."""
        return dict(self._bots)

    @property
    def thread_tracker(self) -> ChatThreadTracker | None:
        """The thread-follow tracker (for external use / testing)."""
        return self._thread_tracker

    @property
    def typing_status(self) -> WorkingStatusDriver:
        """The working-status driver the TurnEngine opens sessions on."""
        return self._typing_status

    def set_handle_registry(self, registry: HandleRegistry) -> None:
        """Set the handle registry for identity resolution."""
        self._handle_registry = registry

    def set_event_queue(self, queue: Any) -> None:
        """Give the transport the queue its websocket fleet publishes on.

        Must be called before :meth:`start`; without it the transport
        still parses and sends, but nothing wakes an agent, because
        nothing is listening to Mattermost.
        """
        self._event_queue = queue

    @property
    def fleet(self) -> Any:
        """The websocket fleet, once started (for tests / diagnostics)."""
        return self._fleet

    def register_bot(self, handle: str, config: MattermostBotConfig) -> None:
        """Register a per-agent Mattermost bot.

        Re-registering a handle whose token is unchanged is a no-op, so a
        routine re-seed costs nothing; a CHANGED token replaces the
        client and schedules the displaced one for closing, because
        dropping an httpx client on the floor leaks its connection pool
        for the life of the process.
        """
        existing = self._bots.get(handle)
        if existing is not None and existing.bot_token == config.bot_token:
            self._bots[handle] = config
        else:
            displaced = self._clients.get(handle)
            self._bots[handle] = config
            self._clients[handle] = MattermostClient(self._base_url, config.bot_token)
            if displaced is not None:
                self._retire_client(displaced, handle)
            logger.info("mattermost_bot_registered", handle=handle)
        if config.username and self._handle_registry is not None:
            self._handle_registry.register_external_id(
                "mattermost", config.username, handle
            )

    def _retire_client(self, client: MattermostClient, handle: str) -> None:
        """Close a displaced client without blocking the caller.

        ``register_bot`` is synchronous (it is called from config-apply
        code paths that are not), so the close is scheduled rather than
        awaited; the client is already unreachable by then.
        """

        async def _close() -> None:
            try:
                await client.close()
            except Exception as exc:
                logger.warning(
                    "mattermost_client_close_failed", handle=handle, error=str(exc)
                )

        with contextlib.suppress(RuntimeError):  # no loop: sync/CLI context
            asyncio.get_running_loop().create_task(_close())

    async def unregister_bot(self, handle: str) -> None:
        """Release a departed seat's bot — socket, client and identities.

        The mirror image of :meth:`register_bot` plus the fleet's seat
        registration.  Mattermost is the one transport holding a LIVE
        outbound connection per seat, so a decommissioned role that is
        merely forgotten keeps a websocket authenticated with its
        personal access token, publishing events onto the inbound topic
        under a handle that no longer resolves to an agent.
        """
        bot = self._bots.pop(handle, None)
        user_id = self._user_ids.pop(handle, "")
        client = self._clients.pop(handle, None)
        if self._fleet is not None:
            try:
                await self._fleet.unregister_seat(handle)
            except Exception as exc:
                logger.warning(
                    "mattermost_seat_unregister_failed", handle=handle, error=str(exc)
                )
        if client is not None:
            try:
                await client.close()
            except Exception as exc:
                logger.warning(
                    "mattermost_client_close_failed", handle=handle, error=str(exc)
                )
        if self._handle_registry is not None:
            # Both namespaces: payloads identify a poster by user id,
            # humans and the MCP tools address a bot by username.
            if user_id:
                self._handle_registry.unregister_external_id(
                    "mattermost_bot", user_id, expected=handle
                )
            if bot is not None and bot.username:
                self._handle_registry.unregister_external_id(
                    "mattermost", bot.username, expected=handle
                )
        logger.info("mattermost_bot_unregistered", handle=handle)

    def user_id_for(self, handle: str) -> str:
        """The bot's own Mattermost user id, once resolved."""
        return self._user_ids.get(handle, "")

    async def start(self) -> None:
        """Resolve each bot's user id and the server's typing cadence."""
        for handle, client in self._clients.items():
            try:
                me = await client.me()
            except MattermostError as exc:
                logger.warning("mattermost_auth_failed", handle=handle, error=str(exc))
                continue
            user_id = str((me or {}).get("id") or "")
            username = str((me or {}).get("username") or "")
            if not user_id:
                continue
            self._user_ids[handle] = user_id
            if self._handle_registry is not None:
                # Two namespaces, same seat: payloads identify a poster by
                # user id, while humans and the MCP tools address a bot by
                # username.
                self._handle_registry.register_external_id(
                    "mattermost_bot", user_id, handle
                )
                if username:
                    self._handle_registry.register_external_id(
                        "mattermost", username, handle
                    )
            logger.info(
                "mattermost_bot_identity_resolved",
                handle=handle,
                user_id=user_id,
                username=username,
            )

        # One read, from any bot's client — both settings below are
        # server-wide, not per-user.
        for client in self._clients.values():
            config = await client.client_config()
            self.status_refresh_interval = _typing_refresh_interval(
                typing_throttle_from(config)
            )
            self._warn_on_site_url_mismatch(config)
            break
        self._started = True

        # Open the inbound sockets last: identities are resolved by now,
        # so the first event a seat receives can already be attributed
        # and own-message-suppressed.
        await self._start_fleet()

        logger.info(
            "transport_started",
            bot_count=len(self._bots),
            typing_refresh_seconds=round(self.status_refresh_interval, 2),
        )

    async def _start_fleet(self) -> None:
        """Build and start the websocket fleet for every registered bot."""
        if self._event_queue is None:
            logger.warning(
                "mattermost_fleet_not_started_no_queue",
                bots=len(self._bots),
            )
            return
        from crewlet.mattermost.client import websocket_url
        from crewlet.mattermost.events import MattermostEventFleet

        self._fleet = MattermostEventFleet(
            base_url=self._base_url,
            websocket_url=websocket_url(self._base_url),
            team=self._team,
            event_queue=self._event_queue,
        )
        for handle, bot in self._bots.items():
            await self._fleet.register_seat(handle, bot.bot_token)
        await self._fleet.start()

    async def stop(self) -> None:
        """Close the sockets, clear live indicators, release the clients."""
        self._started = False
        if self._fleet is not None:
            try:
                await self._fleet.stop()
            except Exception as exc:
                logger.warning("mattermost_fleet_stop_failed", error=str(exc))
            self._fleet = None
        try:
            await self._typing_status.stop()
        except Exception as exc:
            logger.warning("typing_status_stop_failed", error=str(exc))
        for client in self._clients.values():
            try:
                await client.close()
            except Exception as exc:
                logger.warning("mattermost_client_close_failed", error=str(exc))
        logger.info("transport_stopped")

    def _warn_on_site_url_mismatch(self, config: dict[str, Any]) -> None:
        """Warn when the server does not know the address it is reached on.

        ``ServiceSettings.SiteURL`` is the address Mattermost hands to
        every browser and every plugin, which build their absolute URLs
        from it.  When it disagrees with the address this engine — and,
        almost always, the humans — reach the server on, the server keeps
        answering REST calls perfectly while the web app's live feed and
        its plugin requests point at a host the reader's machine cannot
        resolve.  Nothing errors; readers simply refresh to see replies.

        That is the one Mattermost misconfiguration with no symptom the
        engine would otherwise surface, so it is named here rather than
        left for someone to find in a browser console.
        """
        reported = str(config.get("SiteURL") or "")
        if not reported or site_urls_match(self._base_url, reported):
            return
        logger.warning(
            "mattermost_site_url_mismatch",
            configured=self._base_url,
            site_url=normalize_base_url(reported),
            impact=(
                "browsers and plugins build absolute URLs from the server's "
                "SiteURL, so the web app's live updates and its plugin "
                "requests target that address instead of this one"
            ),
            fix=(
                "set ServiceSettings.SiteURL (MM_SERVICESETTINGS_SITEURL) to "
                "the address people reach Mattermost on, then re-run "
                "`crewlet mattermost doctor`"
            ),
        )

    # ----- outbound ------------------------------------------------------

    async def send(self, message: OutboundMessage) -> bool:
        """Post a message as the sending agent's own bot.

        Everything the API takes here is a channel **id**, and most of
        what a caller has is not one — a recipient handle, a channel
        name from config, a person's username — so each form is resolved
        rather than passed through and hoped for.
        """
        bot = self._bots.get(message.sender_handle)
        client = self._clients.get(message.sender_handle)
        if bot is None or client is None or not bot.bot_token:
            logger.warning("no_bot_for_handle", handle=message.sender_handle)
            return False

        channel = await self._resolve_channel(message, bot, client)
        if not channel:
            logger.warning(
                "no_channel_resolved",
                handle=message.sender_handle,
                recipient=message.recipient,
            )
            return False

        # ``thread_anchor`` is the root for a reply and the message's own
        # id for a top-level trigger, which is exactly what a reply must
        # be posted under: the agent is told to answer in a thread, and
        # answering at top level instead both contradicts that and skips
        # the participation record that subscribes it to the human's
        # response.  ``thread_ts`` remains the fallback for metadata
        # produced before the anchor existed.
        root_id = message.reply_to_metadata.get(
            "thread_anchor", ""
        ) or message.reply_to_metadata.get("thread_ts", "")

        try:
            await client.create_post(channel, message.body, root_id=root_id)
        except MattermostError as exc:
            logger.error("mattermost_send_failed", error=str(exc))
            return False

        if root_id and self._thread_routing and self._thread_tracker is not None:
            await self._thread_tracker.record_participation(
                message.sender_handle, channel, root_id
            )
        return True

    async def _resolve_channel(
        self,
        message: OutboundMessage,
        bot: MattermostBotConfig,
        client: MattermostClient,
    ) -> str:
        """The channel **id** to post *message* into, or ``""``.

        In order: the id carried by the trigger, the recipient when it
        already is an id, a DM with the recipient (resolved handle →
        username → user id → direct channel), then the agent's
        configured default channel, which is a NAME and has to be looked
        up in the team.
        """
        carried = message.reply_to_metadata.get("channel", "") or message.metadata.get(
            "channel", ""
        )
        if carried:
            return carried

        recipient = message.recipient
        if recipient:
            if _is_channel_id(recipient):
                return recipient
            username = ""
            if self._handle_registry is not None:
                username = self._handle_registry.get_external_id(
                    "mattermost", recipient
                )
            dm = await self._direct_channel(
                message.sender_handle, client, username or recipient
            )
            if dm:
                return dm

        if bot.channel:
            return await self._channel_id_for_name(client, bot.channel)
        return ""

    async def _direct_channel(
        self, sender_handle: str, client: MattermostClient, username: str
    ) -> str:
        """Open (or fetch) the DM channel between this bot and *username*.

        A username is not a channel id — posting one as though it were is
        rejected by the server, which is how every fallback DM to a
        colleague or to a human seat used to fail.
        """
        own_id = self._user_ids.get(sender_handle, "")
        if not own_id or not username:
            return ""
        cache_key = (sender_handle, username)
        cached = self._dm_channels.get(cache_key)
        if cached:
            return cached
        try:
            other = await client.get_user_by_username(username.lstrip("@"))
            other_id = str((other or {}).get("id") or "")
            if not other_id:
                logger.warning("mattermost_recipient_unknown", username=username)
                return ""
            channel = await client.create_direct_channel(own_id, other_id)
        except MattermostError as exc:
            logger.warning(
                "mattermost_dm_channel_failed", username=username, error=str(exc)
            )
            return ""
        channel_id = str((channel or {}).get("id") or "")
        if channel_id:
            self._dm_channels[cache_key] = channel_id
        return channel_id

    async def _channel_id_for_name(self, client: MattermostClient, name: str) -> str:
        """Resolve a configured channel NAME to its id, once per name."""
        if _is_channel_id(name):
            return name
        cached = self._channel_ids.get(name)
        if cached:
            return cached
        team_id = await self._resolve_team_id(client)
        if not team_id:
            return ""
        try:
            channel = await client.get_channel_by_name(team_id, name)
        except MattermostError as exc:
            logger.warning(
                "mattermost_channel_lookup_failed", channel=name, error=str(exc)
            )
            return ""
        channel_id = str((channel or {}).get("id") or "")
        if channel_id:
            self._channel_ids[name] = channel_id
        else:
            logger.warning("mattermost_channel_unknown", channel=name, team=self._team)
        return channel_id

    async def _resolve_team_id(self, client: MattermostClient) -> str:
        """The configured team's id, resolved once."""
        if self._team_id:
            return self._team_id
        try:
            team = await client.get_team_by_name(self._team)
        except MattermostError as exc:
            logger.warning(
                "mattermost_team_lookup_failed", team=self._team, error=str(exc)
            )
            return ""
        self._team_id = str((team or {}).get("id") or "")
        if not self._team_id:
            logger.warning("mattermost_team_unknown", team=self._team)
        return self._team_id

    async def clear_status(self, handle: str, channel: str, thread_id: str) -> bool:
        """No-op: Mattermost's typing indicator has no clear operation.

        It expires on its own once the heartbeat stops, which is why this
        backend needs the protocol's explicit clear rather than the
        empty-status convention — under that convention "raise" and
        "clear" arrived as the same call on a backend that renders no
        text, and the only safe reading was to refuse both.  Which is
        what happened: the indicator could never be raised at all.
        """
        return False

    async def set_status(
        self,
        handle: str,
        channel: str,
        thread_id: str,
        status: str,
    ) -> bool:
        """Raise the composer typing indicator.

        ``status`` is ignored — Mattermost's indicator wording is the
        client's, which is why this transport declares
        ``supports_status_text = False``.  Raising it is the whole
        vocabulary; :meth:`clear_status` is the other half.

        Never raises: a failed call returns ``False`` and the indicator
        lapses on its own.
        """
        if not channel:
            return False
        client = self._clients.get(handle)
        if client is None or not self._started:
            return False
        try:
            # ``thread_id`` scopes the indicator to the thread when the
            # trigger was a reply; for a top-level message it is the
            # message's own id, which is also the thread the agent will
            # reply under.
            #
            # The deadline is this driver's own heartbeat: a call still in
            # flight when the next beat is due is raising an indicator
            # that beat is about to raise anyway, so waiting on it only
            # holds a pool connection the agent's real calls need.
            await client.publish_typing(
                channel,
                parent_id=thread_id,
                timeout=self.status_refresh_interval,
            )
        except MattermostError as exc:
            logger.warning("mattermost_typing_failed", handle=handle, error=str(exc))
            return False
        except Exception as exc:
            logger.warning("mattermost_typing_error", handle=handle, error=str(exc))
            return False
        return True

    # ----- inbound -------------------------------------------------------

    async def handle_event(
        self,
        body: dict[str, Any],
        handle: str,
    ) -> InboundNotification | None:
        """Parse one fleet-published post into an InboundNotification.

        Returns ``None`` when the post must not wake the agent, setting
        ``last_skip_reason`` to why.
        """
        self.last_skip_reason = ""

        if handle not in self._bots:
            self.last_skip_reason = f"no Mattermost bot registered for {handle}"
            return None

        post = body.get("post") or {}
        if not isinstance(post, dict) or not post.get("id"):
            self.last_skip_reason = "event carried no post"
            return None

        skip = mattermost_post_skip_reason(post)
        if skip:
            self.last_skip_reason = skip
            return None

        channel = str(post.get("channel_id") or "")
        post_id = str(post.get("id") or "")
        root_id = str(post.get("root_id") or "")
        sender_id = str(post.get("user_id") or "")
        own_user_id = self._own_user_id(handle, body)
        channel_type = str(body.get("channel_type") or "")
        text = mattermost_post_text(post)

        # Own-message suppression.  Still record participation: replying
        # in a thread is how a human follows it, and an agent's own reply
        # should subscribe it to what comes back.
        if own_user_id and sender_id == own_user_id:
            if root_id and self._thread_routing and self._thread_tracker is not None:
                await self._thread_tracker.record_participation(
                    handle, channel, root_id
                )
            self.last_skip_reason = f"own message from {handle} (loop prevention)"
            return None

        if not text:
            self.last_skip_reason = "post carried no user-visible content"
            return None

        # --- thread routing ---
        thread_anchor = root_id or post_id
        is_thread_reply = bool(root_id)
        follow_reason: ThreadFollowReason | None = None
        is_following_thread = False
        tracker = self._thread_tracker

        if self._thread_routing and tracker is not None:
            if is_thread_reply:
                is_following_thread = await tracker.is_following(
                    handle, channel, root_id
                )
                if not is_following_thread:
                    follow_reason = self._detect_follow(
                        body, text, handle, channel_type, own_user_id
                    )
                    if follow_reason is not None:
                        await tracker.start_following(
                            handle, channel, root_id, follow_reason
                        )
                        is_following_thread = True
                if not is_following_thread:
                    self.last_skip_reason = (
                        f"thread reply in {channel} — not following this thread"
                    )
                    return None
            else:
                follow_reason = self._detect_follow(
                    body, text, handle, channel_type, own_user_id
                )
                if follow_reason is not None:
                    await tracker.start_following(
                        handle, channel, post_id, follow_reason
                    )

        logger.info(
            "delivering_message",
            handle=handle,
            user=sender_id,
            channel=channel,
            channel_type=channel_type,
            thread="reply" if is_thread_reply else "top-level",
            text_length=len(text),
            replayed=bool(body.get("replayed")),
            following=follow_reason.value
            if follow_reason
            else ("existing" if is_following_thread else "n/a"),
        )

        return InboundNotification(
            source="mattermost",
            source_event_type=str(body.get("event") or "posted"),
            recipient_handle=handle,
            sender=str(body.get("sender_name") or "").lstrip("@") or sender_id,
            subject="Mattermost message",
            body=text,
            metadata={
                "channel": channel,
                # Mattermost's single-letter channel type: O(pen),
                # P(rivate), D(irect), G(roup DM).  Server-stamped and
                # always present, which is why this backend needs no
                # channel-id-prefix heuristic.
                "channel_type": channel_type,
                "channel_name": str(body.get("channel_name") or ""),
                "ts": post_id,
                # ``thread_ts`` rather than ``thread_id`` for the metadata
                # key: the coalescer, the typing-status resolver and the
                # sandbox round-trip all read this name across every chat
                # backend, so the key stays stable and only its VALUE is
                # backend-shaped.
                "thread_ts": root_id,
                "thread_anchor": thread_anchor,
                "user": sender_id,
                "bot_user_id": own_user_id,
                # Mattermost addresses a bot by NAME, not id, so the
                # prompt needs both: the id to recognise its own posts in
                # a thread, the username to write a mention that renders.
                "bot_username": self._bots[handle].username,
                "transport": "mattermost",
                "replayed": "true" if body.get("replayed") else "",
                "thread_following": "true"
                if (is_thread_reply and is_following_thread)
                else "",
                "thread_follow_reason": follow_reason.value if follow_reason else "",
            },
        )

    def _own_user_id(self, handle: str, body: dict[str, Any]) -> str:
        """This seat's own Mattermost user id, however it can be learned.

        ``start()`` resolves every bot's id up front, but that read can
        fail (a server blip, a token rotated between provisioning and
        boot) and a bot registered by a live config apply is never in
        that map at all.  An empty id here is not cosmetic: it disables
        own-message suppression, and an agent that cannot recognise its
        own posts answers itself — one inbound message per reply, forever,
        at one LLM turn each.

        So fall back to the id the websocket fleet stamps on every event
        it publishes.  The fleet resolved it from ``/users/me`` with this
        seat's own token, which is exactly what ``start()`` does, and
        caching it here repairs the map for every later event.
        """
        known = self._user_ids.get(handle, "")
        if known:
            return known
        stamped = str(body.get("bot_user_id") or "")
        if stamped:
            self._user_ids[handle] = stamped
            logger.info(
                "mattermost_identity_recovered_from_event",
                handle=handle,
                user_id=stamped,
            )
        return stamped

    def _detect_follow(
        self,
        body: dict[str, Any],
        text: str,
        handle: str,
        channel_type: str,
        own_user_id: str,
    ) -> ThreadFollowReason | None:
        """Why this post should make the agent follow its thread.

        The two signals answer different questions, and using either for
        the other's job gets it wrong.

        **Whether** this bot is a target is the server's answer.  Its
        ``mentions`` list is rewritten per connection
        (``addMentionsBroadcastHook``): the field is present only when
        this connection's user was mentioned, and its value is then
        always exactly ``[own_user_id]``.  So it is exact — it resolves
        ``@all`` / ``@channel`` / ``@here`` against real membership, and
        catches group and keyword mentions no regex could — but it can
        never say a bot was *not* the target while naming someone else.
        The old "mentions list without me ⇒ collective" branch was
        therefore unreachable except when this seat's own id was unknown,
        in which case it fired on every mention of anybody.

        **Why** is the message text's answer, and only the text's.  A
        bare ``@channel`` expands into every member's id, so by the list
        alone a broadcast is indistinguishable from being named — and
        treating a broadcast as a personal address is exactly what the
        working-status modes say must not happen.

        A DM short-circuits both: there is nobody else the message could
        be for.
        """
        if channel_type in DIRECT_CHANNEL_TYPES:
            return ThreadFollowReason.MENTION

        tracker = self._thread_tracker
        username = self._bots[handle].username
        by_text = (
            tracker.detect_follow_trigger(text, username)
            if tracker is not None
            else None
        )
        mentions = body.get("mentions")

        if isinstance(mentions, list) and mentions and own_user_id:
            if own_user_id not in {str(m) for m in mentions}:
                # Only reachable when this seat's id is stale; the server
                # says someone else was the target.
                return None
            # Named in the text outranks being swept up by a broadcast;
            # a group or keyword mention the text cannot show still
            # counts as being addressed.
            return by_text or ThreadFollowReason.MENTION

        # No usable list: a reconnect-backfilled post (read over REST,
        # which carries none) or an unresolved identity.
        return by_text


__all__ = [
    "MattermostBotConfig",
    "MattermostTransport",
    "mattermost_post_skip_reason",
    "mattermost_post_text",
]
