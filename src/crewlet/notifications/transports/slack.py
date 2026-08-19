"""SlackTransport — outbound Slack messaging per agent.

Architecture:
- Each agent gets its own Slack app (bot token + signing secret)
- Outbound: agents primarily use Slack MCP tools (the ``slack`` server
  in ``mcp_servers``, korotovsky/slack-mcp-server; each agent's bot
  token comes from ``role.mcp_env.slack.SLACK_MCP_XOXB_TOKEN``).
  ``send()`` is available as a transport-agnostic fallback for
  outbound notifications.
- ``set_status()`` drives the "is thinking…" working indicator while a
  turn runs; the lifecycle lives in
  :mod:`crewlet.notifications.typing_status`
- HandleRegistry tracks channel → handle mappings
- Thread routing state is persisted in PostgreSQL

Inbound webhook handling lives in the API layer, which publishes
to the EventQueue. The NotificationService subscribes and routes.

Per-agent Slack credentials are registered at startup via
``register_app``::

    transport.register_app("engineer", SlackAppConfig(
        bot_token="xoxb-eng-...",
        signing_secret="secret-eng",
    ))
"""

from __future__ import annotations

import hashlib
import hmac
import time
from dataclasses import dataclass
from typing import Any

import httpx

from crewlet._logging import get_logger
from crewlet.db.chat_thread_follows import ChatThreadFollowRepository
from crewlet.notifications.handle import HandleRegistry
from crewlet.notifications.protocol import (
    InboundNotification,
    OutboundMessage,
)
from crewlet.notifications.transports.chat_threads import (
    ChatThreadTracker,
    ThreadFollowReason,
)
from crewlet.notifications.transports.slack_threads import (
    build_slack_thread_tracker,
    detect_follow_trigger,
)
from crewlet.notifications.typing_status import (
    REFRESH_INTERVAL_SECONDS,
    StatusPhrases,
    WorkingStatusDriver,
    WorkingStatusMode,
)

logger = get_logger("notifications.slack")

# Message subtypes that carry NEW user-visible message content and may
# wake an agent.  Slack reuses ``type: "message"`` for channel
# bookkeeping — edits, deletions, thread-reply counters, join/leave/
# topic system lines — and those envelopes have no top-level ``user``
# or ``text``; parsing them as messages produces notifications with an
# empty body and sender that wake agents into phantom "(empty)" turns.
_CONTENT_SUBTYPES = frozenset({"", "thread_broadcast", "file_share", "bot_message"})


def slack_message_skip_reason(event: dict[str, Any]) -> str:
    """Why a Slack ``message``-family event must NOT wake an agent.

    Returns ``""`` when the event is a real, newly posted message.
    Two discriminators, both required:

    - ``hidden: true`` marks bookkeeping deliveries: ``message_changed``
      (human edits AND Slack's own link-unfurl edits of a bot's
      message), ``message_deleted``, and ``message_replied`` thread
      bookkeeping — which Slack delivers WITHOUT its subtype (a
      documented Slack bug), so the ``hidden`` flag is the only
      reliable tell for that one.
    - The subtype allowlist (:data:`_CONTENT_SUBTYPES`) drops system
      lines (``channel_join``, ``channel_topic``, …) that do carry
      text but are *about* the channel, not addressed to anyone.

    ``app_mention`` events carry neither field and always pass.

    A module-level function rather than a method so the delivery
    decision has exactly one definition: a second parser for this source
    once existed in ``crewlet.notifications.sources`` and, being
    unreachable from the service, quietly drifted out of sync (it never
    set ``recipient_handle`` or the ``transport`` metadata key, so
    anything routed through it would have missed its agent and never
    raised a working indicator).  It was deleted rather than repaired.
    """
    if event.get("hidden"):
        return f"hidden bookkeeping event (subtype: {event.get('subtype') or 'none'})"
    subtype = event.get("subtype", "") or ""
    if subtype not in _CONTENT_SUBTYPES:
        return f"non-content message subtype: {subtype}"
    return ""


def slack_message_sender(event: dict[str, Any]) -> str:
    """Sender identity of a content-bearing message event.

    Human and bot-user messages carry ``user``; legacy ``bot_message``
    events (incoming webhooks, workflow bots) carry ``username`` and/or
    ``bot_id`` instead.
    """
    return event.get("user", "") or event.get("username", "") or event.get("bot_id", "")


def slack_message_text(event: dict[str, Any]) -> str:
    """User-visible text of a content-bearing message event.

    A file share posted without a comment has empty ``text`` but real
    content — render the file names so a genuine message never produces
    a blank notification body.
    """
    text = event.get("text", "") or ""
    if text:
        return text
    names = [
        str(f.get("name") or f.get("title") or "unnamed file")
        for f in event.get("files") or []
        if isinstance(f, dict)
    ]
    if names:
        return "(shared file: " + ", ".join(names) + ")"
    return ""


@dataclass
class SlackAppConfig:
    """Credentials and settings for one Slack app (one agent)."""

    bot_token: str = ""
    signing_secret: str = ""
    channel: str = ""
    """Default outbound channel for this agent."""


class SlackTransport:
    """Multi-app Slack transport — one Slack app per agent.

    Outbound-only: agents primarily use Slack MCP tools (the ``slack``
    server in ``mcp_servers``; each agent's bot token comes from
    ``role.mcp_env.slack``).  The ``send()`` method is available for
    outbound notifications that route through the notification service.

    Inbound webhook handling is done by the API layer, which publishes
    to the EventQueue for the NotificationService to consume.
    """

    name: str = "slack"

    # --- StatusPoster contract (see notifications.typing_status) ---
    status_backend: str = "slack"
    supports_status_text: bool = True
    """Slack's ``assistant.threads.setStatus`` renders free text, so the
    per-phase phrase pools are live on this backend."""
    status_refresh_interval: float = REFRESH_INTERVAL_SECONDS
    dm_channel_id_prefix: str = "D"
    """Slack channel ids beginning ``D`` are always DMs — the fallback
    that keeps the addressed check right even for metadata that reached
    the driver without a ``channel_type``."""

    def __init__(
        self,
        handle_registry: HandleRegistry | None = None,
        thread_routing: bool = True,
        thread_follow_repo: ChatThreadFollowRepository | None = None,
        typing_status_mode: WorkingStatusMode = WorkingStatusMode.ADDRESSED,
        typing_status_phrases: StatusPhrases | None = None,
    ) -> None:
        self._apps: dict[str, SlackAppConfig] = {}
        self._handle_registry = handle_registry
        self._client: httpx.AsyncClient | None = None
        self._thread_routing = thread_routing
        self._thread_tracker: ChatThreadTracker | None = (
            build_slack_thread_tracker(thread_follow_repo)
            if thread_follow_repo
            else None
        )
        # Working-status ("is thinking…") driver.  Owned here because the
        # per-agent bot tokens and the HTTP client live here, so its
        # heartbeats are torn down with the transport — a live config swap
        # that rebuilds the transport can't strand an indicator.
        self._typing_status = WorkingStatusDriver(
            self, typing_status_mode, phrases=typing_status_phrases
        )
        # Dedup ring: tracks recently processed (handle, channel, ts) to
        # avoid duplicate delivery when both message.channels and
        # app_mention are subscribed for the same Slack app.
        self._recent_events: dict[str, float] = {}
        self._dedup_ttl = 60.0
        self._last_dedup_prune = 0.0
        self.last_skip_reason: str = ""

    @property
    def apps(self) -> dict[str, SlackAppConfig]:
        """Registered per-agent Slack apps (handle → config)."""
        return dict(self._apps)

    @property
    def thread_tracker(self) -> ChatThreadTracker | None:
        """The thread-follow tracker (for external use / testing)."""
        return self._thread_tracker

    @property
    def typing_status(self) -> WorkingStatusDriver:
        """The working-status driver the TurnEngine opens sessions on."""
        return self._typing_status

    def register_app(self, handle: str, config: SlackAppConfig) -> None:
        """Register a per-agent Slack app.

        Args:
            handle: The agent handle (e.g. "engineer").
            config: Slack app credentials for this agent.
        """
        self._apps[handle] = config
        logger.info("slack_app_registered", handle=handle)

        # Auto-register the default channel in HandleRegistry
        if config.channel and self._handle_registry is not None:
            self._handle_registry.register_external_id("slack", config.channel, handle)

    def _get_app(self, handle: str) -> SlackAppConfig | None:
        """Get the Slack app config for an agent handle."""
        return self._apps.get(handle)

    def set_handle_registry(self, registry: HandleRegistry) -> None:
        """Set the handle registry for identity resolution."""
        self._handle_registry = registry

    async def start(self) -> None:
        """Start the transport — initialize HTTP client.

        Also fetches the bot user ID for each registered app via
        Slack's ``auth.test`` API and registers it in the
        HandleRegistry so ``lookup_colleague`` can return it.
        """
        self._client = httpx.AsyncClient()

        # Resolve bot user IDs for each registered app.
        for handle, config in self._apps.items():
            if not config.bot_token:
                continue
            try:
                resp = await self._client.post(
                    "https://slack.com/api/auth.test",
                    headers={"Authorization": f"Bearer {config.bot_token}"},
                )
                if resp.status_code == 200:
                    data = resp.json()
                    if data.get("ok"):
                        bot_user_id = data.get("user_id", "")
                        if bot_user_id and self._handle_registry is not None:
                            self._handle_registry.register_external_id(
                                "slack_bot", bot_user_id, handle
                            )
                            logger.info(
                                "slack_bot_user_id_resolved",
                                handle=handle,
                                bot_user_id=bot_user_id,
                            )
                    else:
                        logger.warning(
                            "slack_auth_test_failed",
                            handle=handle,
                            error=data.get("error", "unknown"),
                        )
                else:
                    logger.warning(
                        "slack_auth_test_http_error",
                        handle=handle,
                        status_code=resp.status_code,
                    )
            except Exception as exc:
                logger.warning(
                    "slack_auth_test_error",
                    handle=handle,
                    error=str(exc),
                )

        logger.info("transport_started", app_count=len(self._apps))

    async def stop(self) -> None:
        """Stop and clean up HTTP client."""
        try:
            # Clear live "is thinking…" indicators BEFORE the client
            # closes — the clearing calls go through it.
            await self._typing_status.stop()
        except Exception as exc:
            logger.warning("typing_status_stop_failed", error=str(exc))
        try:
            if self._client is not None:
                await self._client.aclose()
                self._client = None
        finally:
            self._recent_events.clear()
        logger.info("transport_stopped")

    async def send(self, message: OutboundMessage) -> bool:
        """Send a message to Slack via chat.postMessage.

        Uses the sending agent's own bot token.

        Resolves the target channel by trying (in order):
        1. ``reply_to_metadata["channel"]`` (reply to original channel)
        2. ``metadata["channel"]`` (explicit channel override)
        3. ``recipient`` field as a raw Slack channel/user ID (starts
           with ``C``, ``D``, ``U``, or ``#``)
        4. HandleRegistry lookup for the ``recipient`` as an agent handle
        5. Per-agent app's default channel
        6. HandleRegistry lookup for the sender's assigned channel
        """
        app = self._get_app(message.sender_handle)

        if app is None or not app.bot_token:
            logger.warning("no_app_for_handle", handle=message.sender_handle)
            return False

        # Resolve channel
        channel = message.reply_to_metadata.get("channel", "") or message.metadata.get(
            "channel", ""
        )
        if not channel:
            recipient = message.recipient
            if recipient and recipient[:1] in ("C", "D", "U", "#"):
                channel = recipient
            elif recipient and self._handle_registry is not None:
                channel = self._handle_registry.get_external_id("slack", recipient)
        if not channel:
            channel = app.channel
        if not channel and self._handle_registry is not None:
            channel = self._handle_registry.get_external_id(
                "slack", message.sender_handle
            )
        if not channel:
            logger.warning(
                "no_channel_resolved",
                handle=message.sender_handle,
                recipient=message.recipient,
                registry="set" if self._handle_registry else "none",
            )
            return False

        # Thread reply if we have a thread_ts
        thread_ts = message.reply_to_metadata.get("ts", "")

        payload: dict[str, Any] = {
            "channel": channel,
            "text": message.body,
        }
        if thread_ts:
            payload["thread_ts"] = thread_ts
            # Record outbound participation — auto-follow the thread
            if self._thread_routing and self._thread_tracker is not None:
                await self._thread_tracker.record_participation(
                    message.sender_handle,
                    channel,
                    thread_ts,
                )

        if self._client is None:
            logger.warning("send_before_start")
            return False

        try:
            resp = await self._client.post(
                "https://slack.com/api/chat.postMessage",
                json=payload,
                headers={
                    "Authorization": f"Bearer {app.bot_token}",
                    "Content-Type": "application/json",
                },
            )
            resp.raise_for_status()
            data = resp.json()
            if data.get("ok"):
                return True
            logger.error("slack_api_error", error=data.get("error", "unknown"))
            return False
        except Exception as exc:
            logger.error("slack_send_failed", error=str(exc))
            return False

    async def clear_status(self, handle: str, channel: str, thread_id: str) -> bool:
        """Take the working status down.

        Slack's own clear IS an empty status, so this is
        :meth:`set_status` with no text — spelled out because the
        protocol no longer overloads the payload for backends whose
        indicator has no text to empty.
        """
        return await self.set_status(handle, channel, thread_id, "")

    async def set_status(
        self,
        handle: str,
        channel: str,
        thread_id: str,
        status: str,
    ) -> bool:
        """Set (or clear) an agent's working status in a Slack thread.

        Wraps `assistant.threads.setStatus
        <https://docs.slack.dev/reference/methods/assistant.threads.setStatus/>`_,
        which renders "*<agent> is thinking…*" under the thread's
        composer — the closest thing Slack offers a bot to a typing
        indicator (there is no public ``user_typing`` API for granular
        apps).  Since March 2026 the method accepts the plain
        ``chat:write`` bot scope every Slack-enabled agent already holds,
        so this needs no extra scope and no app-manifest change.

        An empty ``status`` clears the indicator.  Slack also clears it
        by itself the moment the app posts into the thread, and expires
        any set status after 2 minutes — see
        :mod:`crewlet.notifications.typing_status` for the refresh
        lifecycle.

        Never raises: a failed call returns ``False`` and the status
        expires on Slack's side.  The caller is a cosmetic side-channel
        and must never fail an agent turn.
        """
        if not channel or not thread_id:
            return False
        app = self._get_app(handle)
        if app is None or not app.bot_token:
            return False
        if self._client is None:
            return False

        try:
            resp = await self._client.post(
                "https://slack.com/api/assistant.threads.setStatus",
                json={
                    "channel_id": channel,
                    "thread_ts": thread_id,
                    "status": status,
                },
                headers={
                    "Authorization": f"Bearer {app.bot_token}",
                    "Content-Type": "application/json",
                },
            )
            resp.raise_for_status()
            data = resp.json()
            if data.get("ok"):
                return True
            logger.warning(
                "slack_set_status_failed",
                handle=handle,
                channel=channel,
                error=data.get("error", "unknown"),
            )
            return False
        except Exception as exc:
            logger.warning("slack_set_status_error", handle=handle, error=str(exc))
            return False

    # --- Webhook parsing utilities (used by API layer) ---

    @staticmethod
    def verify_signature(
        body_raw: str | bytes,
        headers: dict[str, str],
        signing_secret: str,
    ) -> bool:
        """Verify the Slack request signature.

        Uses the provided signing secret to validate
        ``x-slack-signature`` against the request body and timestamp.
        Returns False if the signature is invalid or the timestamp is
        too old (> 5 minutes).

        Static because it depends on nothing but its arguments, and the
        API layer verifies at the webhook edge — before the payload is
        persisted or broadcast — without having a transport instance to
        hand.  One implementation, two callers: a second HMAC of the
        same signature is how one of them quietly stops matching.
        """
        if not signing_secret:
            return False

        timestamp = headers.get("x-slack-request-timestamp", "")
        signature = headers.get("x-slack-signature", "")

        if not timestamp or not signature:
            return False

        try:
            ts = int(timestamp)
        except ValueError:
            return False
        if abs(time.time() - ts) > 300:
            return False

        if isinstance(body_raw, str):
            body_raw = body_raw.encode("utf-8")

        sig_basestring = f"v0:{timestamp}:".encode() + body_raw
        my_signature = (
            "v0="
            + hmac.new(
                signing_secret.encode("utf-8"),
                sig_basestring,
                hashlib.sha256,
            ).hexdigest()
        )
        return hmac.compare_digest(my_signature, signature)

    async def handle_event(
        self,
        body: dict[str, Any],
        handle: str,
        headers: dict[str, str] | None = None,
        body_raw: str | bytes = b"",
    ) -> dict[str, Any] | InboundNotification | None:
        """Parse an incoming Slack Events API payload.

        Called by the API layer's ``/webhooks/slack/{handle}`` endpoint.
        Returns a response dict (for Slack URL verification), an
        ``InboundNotification`` (to be published to the queue), or None
        (if the event should be skipped).

        When a message is skipped, ``self.last_skip_reason`` is set to
        a human-readable explanation for tracing purposes.

        Args:
            body: Parsed JSON body.
            handle: Agent handle from the webhook URL path.
            headers: HTTP request headers (for signature verification).
            body_raw: Raw request body bytes (for signature verification).
        """
        self.last_skip_reason = ""

        # Reject events after transport is stopped
        if self._client is None:
            logger.warning("handle_event_after_stop", handle=handle)
            self.last_skip_reason = "transport stopped"
            return None

        app = self._apps.get(handle)
        if app is None:
            logger.warning("no_app_for_handle", handle=handle)
            self.last_skip_reason = f"no Slack app registered for {handle}"
            return None

        # URL verification challenge
        if body.get("type") == "url_verification":
            logger.debug("url_verification", handle=handle)
            return {"challenge": body.get("challenge", "")}

        # Verify webhook signature
        if not headers:
            logger.warning("no_headers_for_verification", handle=handle)
            self.last_skip_reason = "missing headers for signature verification"
            return None
        if not self.verify_signature(body_raw, headers, app.signing_secret):
            logger.warning("invalid_webhook_signature", handle=handle)
            self.last_skip_reason = "invalid webhook signature"
            return None

        if body.get("type") != "event_callback":
            self.last_skip_reason = f"ignored event type: {body.get('type', '?')}"
            return None

        event = body.get("event", {})
        event_type = event.get("type", "")

        is_app_mention = event_type == "app_mention"
        if event_type != "message" and not is_app_mention:
            logger.debug("ignoring_event_type", event_type=event_type, handle=handle)
            self.last_skip_reason = f"ignored Slack event type: {event_type}"
            return None

        # Bookkeeping ``message`` events (edits — including Slack's own
        # link-unfurl edits — deletions, thread-reply counters, channel
        # system lines) have no top-level ``user``/``text``; delivering
        # them wakes the agent with an empty notification.
        skip_reason = slack_message_skip_reason(event)
        if skip_reason:
            logger.debug(
                "skipping_bookkeeping_event",
                handle=handle,
                subtype=event.get("subtype", ""),
                hidden=bool(event.get("hidden")),
            )
            self.last_skip_reason = skip_reason
            return None

        # Skip own app messages (prevent loops)
        event_app_id = event.get("app_id")
        our_app_id = body.get("api_app_id")
        if event_app_id and our_app_id and event_app_id == our_app_id:
            if self._thread_routing and self._thread_tracker is not None:
                echo_thread_ts = event.get("thread_ts", "")
                echo_channel = event.get("channel", "")
                if echo_thread_ts and echo_channel:
                    await self._thread_tracker.record_participation(
                        handle,
                        echo_channel,
                        echo_thread_ts,
                    )
            logger.debug("skipping_own_app_message", app_id=event_app_id, handle=handle)
            self.last_skip_reason = f"own message from {handle} (loop prevention)"
            return None

        channel = event.get("channel", "")
        user = event.get("user", "")

        authorizations = body.get("authorizations", [])
        bot_user_id = ""
        if isinstance(authorizations, list) and authorizations:
            bot_user_id = authorizations[0].get("user_id", "")

        text = slack_message_text(event)
        msg_ts = event.get("ts", "")
        thread_ts = event.get("thread_ts", "")
        team = body.get("team_id", "")

        # --- Dedup ---
        dedup_key = f"{handle}:{channel}:{msg_ts}"
        now = time.monotonic()
        if dedup_key in self._recent_events:
            logger.debug("dedup_hit", handle=handle, channel=channel, ts=msg_ts)
            self.last_skip_reason = "duplicate event (already processed)"
            return None
        self._recent_events[dedup_key] = now
        if now - self._last_dedup_prune >= self._dedup_ttl:
            cutoff = now - self._dedup_ttl
            self._recent_events = {
                k: v for k, v in self._recent_events.items() if v > cutoff
            }
            self._last_dedup_prune = now

        # --- Thread routing ---
        follow_reason: ThreadFollowReason | None = None
        is_thread_reply = bool(thread_ts)
        is_following_thread = False
        tracker = self._thread_tracker

        if self._thread_routing and tracker is not None:
            if is_thread_reply:
                # Single DB check up front
                is_following_thread = await tracker.is_following(
                    handle, channel, thread_ts
                )

                if not is_following_thread:
                    # Not yet following — check if this message triggers a follow
                    if is_app_mention:
                        follow_reason = ThreadFollowReason.MENTION
                    else:
                        follow_reason = detect_follow_trigger(text, bot_user_id)

                    if follow_reason is not None:
                        await tracker.start_following(
                            handle, channel, thread_ts, follow_reason
                        )
                        is_following_thread = True

                if not is_following_thread:
                    logger.debug(
                        "skipping_thread_reply",
                        handle=handle,
                        channel=channel,
                        thread_ts=thread_ts,
                    )
                    self.last_skip_reason = (
                        f"thread reply in #{channel} — not following this thread"
                    )
                    return None
            else:
                if is_app_mention:
                    follow_reason = ThreadFollowReason.MENTION
                else:
                    follow_reason = detect_follow_trigger(text, bot_user_id)
                if follow_reason is not None:
                    await tracker.start_following(
                        handle, channel, msg_ts, follow_reason
                    )

        logger.info(
            "delivering_message",
            handle=handle,
            user=user,
            channel=channel,
            ts=thread_ts or msg_ts,
            team=team,
            text_length=len(text),
            thread="reply" if is_thread_reply else "top-level",
            following=follow_reason.value
            if follow_reason
            else ("existing" if is_following_thread else "n/a"),
        )

        return InboundNotification(
            source="slack",
            source_event_type=event_type,
            recipient_handle=handle,
            # ``user`` (the raw Slack user id) is empty for legacy
            # ``bot_message`` events — fall through to the bot's
            # username / bot_id so the sender is never blank.  The
            # ``user`` METADATA key below stays the raw id: the
            # learning subsystem resolves counterparty identity from
            # it and must not see display names there.
            sender=slack_message_sender(event),
            subject="Slack message",
            body=text,
            metadata={
                "channel": channel,
                # Slack's channel kind ("im" / "mpim" / "group" /
                # "channel").  Drives DM-level inbox coalescing (a DM
                # channel IS one conversation — see
                # ``SlackNotificationPrompt.conversation_key``) and the
                # learning subsystem's channel-kind derivation.
                # ``app_mention`` events omit the field, so fall back to
                # the channel-id prefix (``D…`` is always a DM) — without
                # this, the message/app_mention double-delivery dedup
                # race would non-deterministically key a mention-bearing
                # DM message thread-grained and split a burst in two.
                "channel_type": event.get("channel_type", "")
                or ("im" if channel.startswith("D") else ""),
                "ts": msg_ts,
                "thread_ts": thread_ts,
                "team": team,
                "user": user,
                "bot_user_id": bot_user_id,
                "transport": "slack",
                "thread_following": "true"
                if (is_thread_reply and is_following_thread)
                else "",
                "thread_follow_reason": follow_reason.value if follow_reason else "",
            },
        )
