"""NotificationService — event-queue-driven notification hub.

Sits at the engine level. Subscribes to the EventQueue for both inbound
and outbound notification topics. Inbound notifications (published by
API webhook handlers) are resolved to agents and forwarded to their
inbox topics. Outbound notifications (published by agents) are
dispatched through the appropriate transport.

Transports are outbound-only — they provide ``start()``, ``stop()``,
and ``send()`` methods. Raw webhook payloads are received by the API
layer and parsed here via transport-specific logic.
"""

from __future__ import annotations

import json
import time
from typing import TYPE_CHECKING, Any

from crewlet._logging import get_logger
from crewlet.notifications.protocol import (
    InboundNotification,
    OutboundMessage,
    Transport,
)
from crewlet.telemetry import restore_context, tracer

if TYPE_CHECKING:
    from crewlet.events.types import Event
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.queue.protocol import EventQueue

logger = get_logger("notifications.service")

INBOUND_TOPIC = "crewlet.notifications.inbound"
OUTBOUND_TOPIC = "crewlet.notifications.outbound"
INBOUND_CONSUMER_GROUP = "notification-svc-inbound"
OUTBOUND_CONSUMER_GROUP = "notification-svc-outbound"

# Sources whose empty parses are routine BY DESIGN and whose parser
# logs its own skip reasons, so ``webhook_produced_no_notifications``
# is demoted to debug for them: Plane's ``project`` / ``cycle`` /
# ``module`` / ``user`` events and excluded skills-project pages all
# deliberately parse to nothing, and the Plane parser/transport logs
# each drop (``plane_webhook_ignored_event`` /
# ``plane_webhook_not_parsed`` / ``plane_webhook_no_recipients``).
# GitLab and Jira do NOT belong here: their parsers return empty
# WITHOUT logging (e.g. ``parse_gitlab_webhook``'s unknown
# ``object_kind`` and ``emoji`` branches), so for them the WARNING
# below is the only signal that an entirely unhandled event type is
# being discarded.
_QUIET_EMPTY_SOURCES: frozenset[str] = frozenset({"plane"})


class NotificationService:
    """Central notification service for the Crewlet engine.

    Subscribes to ``crewlet.notifications.inbound`` and
    ``crewlet.notifications.outbound`` topics on the EventQueue.

    - Inbound: resolves the recipient via HandleRegistry, publishes to
      the agent's inbox topic (``crewlet.agent.{handle}.inbox``).
    - Outbound: dispatches through the appropriate transport.
    """

    def __init__(
        self,
        event_queue: EventQueue,
        transports: dict[str, Transport],
        handle_registry: HandleRegistry,
        rate_limit: int = 0,
    ) -> None:
        self._event_queue = event_queue
        self._transports = transports
        self._handle_registry = handle_registry
        self._rate_limit = rate_limit
        self._running = False
        self._subscribed = False

        # GitLab webhook config (url + optional read token), set by the
        # engine via set_gitlab_config — enables participants-based
        # routing in _parse_gitlab.  None = payload-only routing.
        self._gitlab_config: Any = None

        # Rate limiting: max notifications per second per agent (0 = unlimited)
        # {agent_id: [timestamp, ...]}
        self._rate_tracker: dict[str, list[float]] = {}

    def set_gitlab_config(self, gitlab_config: Any) -> None:
        """Wire (or clear) the GitLab integration config.

        Called by the engine at start and on live integrations diffs —
        same pattern as ``set_handle_registry`` on transports.  When the
        config carries a read ``token``, ``_parse_gitlab`` routes thread
        activity to the issue/MR *participants* (GitLab's own
        notification reach); without one it degrades to payload-derived
        targets.
        """
        self._gitlab_config = gitlab_config

    @property
    def transports(self) -> dict[str, Transport]:
        return dict(self._transports)

    @transports.setter
    def transports(self, value: dict[str, Transport]) -> None:
        # Live-config rewrites swap the transports map on the running
        # service (engine ``_apply_notification_transports_live`` /
        # ``_rollback``).  Transport lifecycle (start/stop) is owned by
        # the engine; the setter is a pure storage replacement that
        # defensively copies so the caller can't mutate it afterwards.
        self._transports = dict(value)

    @property
    def rate_limit(self) -> int:
        return self._rate_limit

    @rate_limit.setter
    def rate_limit(self, value: int) -> None:
        # Live-config scalar updates push the new per-agent rate cap
        # onto the running service (engine ``_apply_scalars_diff``).
        # ``_check_rate_limit`` reads ``self._rate_limit`` on every
        # send, so the swap takes effect on the next notification with
        # no re-subscription.
        self._rate_limit = value

    @property
    def handle_registry(self) -> HandleRegistry:
        return self._handle_registry

    async def start(self) -> None:
        """Subscribe to notification topics and start all transports."""
        if self._running:
            return
        self._running = True

        if not self._subscribed:
            await self._event_queue.subscribe(
                INBOUND_TOPIC, INBOUND_CONSUMER_GROUP, self._handle_inbound
            )
            await self._event_queue.subscribe(
                OUTBOUND_TOPIC, OUTBOUND_CONSUMER_GROUP, self._handle_outbound
            )
            self._subscribed = True

        for transport in self._transports.values():
            try:
                await transport.start()
                logger.info("transport_started", transport=transport.name)
            except Exception as exc:
                logger.error(
                    "transport_start_failed",
                    transport=transport.name,
                    error=str(exc),
                )

        logger.info(
            "notification_service_started",
            transport_count=len(self._transports),
        )

    async def stop(self) -> None:
        """Stop all transports."""
        if not self._running:
            return

        # Gate handlers immediately before tearing down transports
        self._running = False

        for transport in self._transports.values():
            try:
                await transport.stop()
            except Exception as exc:
                logger.error(
                    "transport_stop_error",
                    transport=transport.name,
                    error=str(exc),
                )

        self._rate_tracker.clear()
        logger.info("notification_service_stopped")

    async def _handle_inbound(self, event: Event) -> None:
        """Route an inbound notification to the target agent's inbox.

        Accepts two event formats:
        - ``raw_webhook``: published by the standalone API process.
          Contains ``body``, ``headers``, and ``body_raw_b64``.
          Parsed here via transport-specific logic.
        - ``inbound_notification``: already-parsed InboundNotification.

        Resolution order for recipient:
        1. Handle-based (direct handle lookup)
        2. Email-based (plus-address parsing or direct email match)
        3. External ID (e.g. Jira accountId) — uses the
           ``assignee_account_id`` metadata field with the notification
           source as transport name.
        """
        if not self._running:
            return

        # Restore OTel context from the incoming event so all
        # downstream events (ExternalNotification, etc.) share the
        # same trace that started at the webhook handler.
        otel_ctx = restore_context(event.trace_id, event.span_id)
        span_name = f"notification.{event.type}"

        with tracer.start_as_current_span(span_name, context=otel_ctx):
            await self._handle_inbound_inner(event)

    def _resolve_human_recipient(self, notification: InboundNotification):
        """Resolve the notification's recipient to a human seat, if any.

        Mirrors the agent resolution order in ``_handle_inbound_inner``
        (handle → email → the same external-ID candidates, including
        the lowercased-email fallback) over the party API, evaluated
        lazily — the first human hit returns.  Runs only after agent
        resolution already failed, so any party found here is either
        a human seat or nothing.
        """
        registry = self._handle_registry
        meta = notification.metadata

        def _candidates():
            if notification.recipient_handle:
                yield registry.resolve_party(notification.recipient_handle)
            if notification.recipient_email:
                yield registry.resolve_party_email(notification.recipient_email)
            for ext_key in (
                meta.get("assignee_account_id", ""),
                meta.get("github_login", ""),
                meta.get("gitlab_username", ""),
                meta.get("plane_user_id", ""),
                notification.recipient_email.lower()
                if notification.recipient_email
                else "",
            ):
                if ext_key:
                    yield registry.resolve_party_external(notification.source, ext_key)

        for party in _candidates():
            if party is not None and party.is_human:
                return party
        return None

    async def _record_skip(
        self,
        source: str,
        handle: str,
        reason: str,
    ) -> None:
        """Write a notification_skipped event for traceability."""
        from crewlet.events.types import NotificationSkipped

        event = NotificationSkipped(
            source=f"notification_service.{source}",
            handle=handle,
            reason=reason,
            notification_source=source,
        )
        if self._event_queue is not None:
            await self._event_queue.publish(
                "crewlet.events.notification_skipped", event
            )

    async def _handle_inbound_inner(self, event: Event) -> None:
        """Inner handler, runs inside an OTel span."""
        if event.type == "raw_webhook":
            logger.info(
                "raw_webhook_received",
                source=event.source,
                event_id=str(event.id),
            )
            await self._parse_and_route_webhook(event)
            return

        try:
            notification = InboundNotification(**event.payload)
        except (TypeError, ValueError) as exc:
            logger.error(
                "invalid_inbound_payload",
                event_id=str(event.id),
                error=str(exc),
            )
            return

        handle = notification.recipient_handle
        agent = self._handle_registry.resolve_handle(handle) if handle else None

        if agent is None and notification.recipient_email:
            agent = self._handle_registry.resolve_email_address(
                notification.recipient_email
            )

        if agent is None:
            # External ID lookup using values populated during engine
            # startup (Jira/GitHub/GitLab/Plane account registration) or
            # by the webhook parser. Resolution order:
            #   1. assignee_account_id  (Jira accountId)
            #   2. github_login         (GitHub username)
            #   3. gitlab_username      (GitLab username)
            #   4. plane_user_id        (Plane user UUID)
            #   5. recipient_email      (lowercased fallback)
            meta = notification.metadata
            ext_candidates: list[tuple[str, str]] = [
                (notification.source, meta.get("assignee_account_id", "")),
                (notification.source, meta.get("github_login", "")),
                (notification.source, meta.get("gitlab_username", "")),
                (notification.source, meta.get("plane_user_id", "")),
                (
                    notification.source,
                    notification.recipient_email.lower()
                    if notification.recipient_email
                    else "",
                ),
            ]
            for source, ext_key in ext_candidates:
                if not ext_key:
                    continue
                agent = self._handle_registry.resolve_external_id(source, ext_key)
                if agent is not None:
                    break

        if agent is None:
            # A human seat as recipient is EXPECTED, not an error: the
            # external tool (Jira, Slack, …) already notified the
            # person natively — the engine never forwards
            # external-surface events to humans.  Record the skip at
            # info level for traceability and stop quietly.
            human = self._resolve_human_recipient(notification)
            if human is not None:
                logger.info(
                    "notification_recipient_is_human",
                    source=notification.source,
                    seat=human.name,
                    handle=human.handle,
                )
                await self._record_skip(
                    notification.source,
                    human.handle,
                    "recipient is a human seat — notified natively by "
                    "the external tool",
                )
                return
            logger.warning(
                "notification_undeliverable",
                source=notification.source,
                handle=notification.recipient_handle,
                email=notification.recipient_email,
            )
            await self._record_skip(
                notification.source,
                notification.recipient_handle,
                f"no agent found for handle={notification.recipient_handle} "
                f"email={notification.recipient_email}",
            )
            return

        # Self-action guard: an agent must never be woken by a
        # notification describing its own action.  ``JiraTransport``
        # already excludes the trigger user from watcher fan-out, but
        # a webhook can fall through to standard resolution (here)
        # via ``assignee_account_id`` when the watcher API is empty
        # or stale -- and that path lacks any self-check.  Without
        # this guard, an agent assigned to its own issue gets a
        # webhook for every comment IT posts and re-comments forever
        # (observed: PM, POC-482, 28× same comment).
        actor_account_id = notification.metadata.get("actor_account_id", "")
        if actor_account_id:
            agent_ext_id = self._handle_registry.get_external_id(
                notification.source, agent.handle
            )
            if agent_ext_id and agent_ext_id == actor_account_id:
                logger.info(
                    "notification_self_action_skipped",
                    source=notification.source,
                    agent_handle=agent.handle,
                    actor_account_id=actor_account_id,
                    event_type=notification.source_event_type,
                )
                await self._record_skip(
                    notification.source,
                    agent.handle,
                    "self-action: agent's own webhook",
                )
                return

        if not self._check_rate_limit(agent.id_str):
            logger.warning(
                "rate_limit_exceeded",
                agent_id=agent.id_str,
                source=notification.source,
            )
            await self._record_skip(
                notification.source,
                agent.handle,
                f"rate limit exceeded for {agent.handle}",
            )
            return

        inbox_topic = f"crewlet.agent.{agent.handle}.inbox"

        from crewlet.events.types import ExternalNotification
        from crewlet.notifications.notification_prompts import (
            build_notification_prompt,
            notification_requires_recon,
        )

        enriched_body = build_notification_prompt(
            notification, handle_registry=self._handle_registry
        )
        # Whether the enriched body is a pointer (webhook naming a
        # thing-that-changed) vs. self-contained context.  Rides the
        # event so the Plan-phase prefetches can skip aux-LLM filtering
        # on a trigger too thin to filter against.
        context_requires_recon = notification_requires_recon(notification)

        # Forward delegation bookkeeping from the inbound notification
        # metadata so the woken agent's TurnEngine can enforce the
        # depth cap.  Most webhooks won't set these; when
        # present they come from an in-process event or a future
        # cross-process header.  Webhook metadata values are strings
        # of arbitrary shape, so we safe-parse and fall back to
        # defaults rather than aborting a notification on a single
        # malformed field.
        raw_depth = notification.metadata.get("delegation_depth", 0) or 0
        try:
            delegation_depth = int(raw_depth)
        except (TypeError, ValueError):
            logger.warning(
                "delegation_depth_parse_failed",
                raw=raw_depth,
                source=notification.source,
            )
            delegation_depth = 0
        parent_turn_id = str(notification.metadata.get("parent_turn_id", "") or "")
        # ``InboundNotification.metadata`` is ``dict[str, str]`` (Pydantic
        # coerces / rejects anything else at construction time), so a
        # producer carrying ``delegation_chain`` across a webhook
        # boundary MUST encode it as a JSON array string.  Anything that
        # doesn't decode to a list falls back to an empty chain rather
        # than aborting routing; the delegation-depth cap still enforces
        # the invariant downstream.
        delegation_chain: list[str] = []
        chain_raw = notification.metadata.get("delegation_chain", "") or ""
        if chain_raw:
            try:
                parsed = json.loads(chain_raw)
            except (TypeError, ValueError):
                logger.warning(
                    "delegation_chain_parse_failed",
                    raw=chain_raw,
                    source=notification.source,
                )
                parsed = None
            if isinstance(parsed, list):
                # Drop ``None`` and empty strings but keep falsy-but-
                # valid values like ``0`` -- producers that encode
                # numeric IDs must not lose them to a ``if x`` filter.
                delegation_chain = [str(x) for x in parsed if x is not None and x != ""]

        inbox_event = ExternalNotification(
            source=f"notification_service.{notification.source}",
            notification_source=notification.source,
            source_event_type=notification.source_event_type,
            recipient_email=notification.recipient_email,
            agent_id=agent.id_str,
            sender=notification.sender,
            subject=notification.subject,
            body=enriched_body,
            salient_body=notification.body,
            metadata=notification.metadata,
            context_requires_recon=context_requires_recon,
            delegation_depth=delegation_depth,
            parent_turn_id=parent_turn_id,
            delegation_chain=delegation_chain,
        )
        await self._event_queue.publish(inbox_topic, inbox_event)

        logger.info(
            "notification_routed",
            source=notification.source,
            handle=agent.handle,
            agent_id=agent.id_str,
            inbox_topic=inbox_topic,
        )

    async def _parse_and_route_webhook(self, event: Event) -> None:
        """Parse a raw webhook from the API and route the results.

        Raw webhooks arrive with ``body`` (parsed JSON), ``headers``,
        and ``body_raw_b64`` (base64-encoded original bytes for HMAC
        verification).  The ``event.source`` identifies the transport
        (jira, slack, github).
        """
        import base64

        payload = event.payload
        body: dict = payload.get("body", {})
        headers: dict = payload.get("headers", {})
        raw_b64 = payload.get("body_raw_b64", "")
        source = event.source

        notifications: list[InboundNotification] = []

        try:
            body_raw: bytes = base64.b64decode(raw_b64) if raw_b64 else b""
            if source == "jira":
                notifications = await self._parse_jira(body, headers, body_raw)
            elif source == "confluence":
                notifications = await self._parse_confluence(body, headers, body_raw)
            elif source == "slack":
                handle = payload.get("handle", "")
                notifications = await self._parse_slack(body, handle, headers, body_raw)
            elif source == "github":
                notifications = await self._parse_github(body, headers)
            elif source == "gitlab":
                notifications = await self._parse_gitlab(body, headers)
            elif source == "plane":
                notifications = await self._parse_plane(body, headers, body_raw)
            else:
                logger.warning("unknown_webhook_source", source=source)
                return
        except Exception as exc:
            logger.exception("webhook_parse_failed", source=source, error=str(exc))
            return

        if not notifications:
            # WARNING by default: for most sources the parser returns
            # empty silently, so this line is the only clue an
            # unhandled event type (e.g. a newly-enabled GitLab
            # ``wiki_page`` hook) is being discarded.  Only sources in
            # ``_QUIET_EMPTY_SOURCES`` — whose empty results are by
            # design and self-logged — drop to debug.
            if source in _QUIET_EMPTY_SOURCES:
                logger.debug("webhook_produced_no_notifications", source=source)
            else:
                logger.warning("webhook_produced_no_notifications", source=source)
            return

        logger.info(
            "webhook_parsed",
            source=source,
            notification_count=len(notifications),
        )

        # Route each parsed notification through _handle_inbound.
        from crewlet.events.types import Event as EvtType

        for notification in notifications:
            await self._handle_inbound(
                EvtType(
                    type="inbound_notification",
                    payload=notification.model_dump(),
                )
            )

    async def _parse_jira(
        self,
        body: dict,
        headers: dict,
        body_raw: bytes,
    ) -> list[InboundNotification]:
        from crewlet.notifications.transports.jira import JiraTransport

        transport = self._transports.get("jira")
        if isinstance(transport, JiraTransport):
            return await transport.handle_webhook(
                body=body, headers=headers, body_raw=body_raw
            )
        from crewlet.notifications.sources import parse_jira_webhook

        n = await parse_jira_webhook("jira", body, headers)
        return [n] if n is not None else []

    async def _parse_confluence(
        self,
        body: dict,
        headers: dict,
        body_raw: bytes,
    ) -> list[InboundNotification]:
        from crewlet.notifications.transports.confluence import ConfluenceTransport

        transport = self._transports.get("confluence")
        if isinstance(transport, ConfluenceTransport):
            return await transport.handle_webhook(
                body=body, headers=headers, body_raw=body_raw
            )
        from crewlet.notifications.transports.confluence import (
            parse_confluence_webhook,
        )

        n = parse_confluence_webhook(body)
        return [n] if n is not None else []

    async def _parse_slack(
        self,
        body: dict,
        handle: str,
        headers: dict,
        body_raw: bytes,
    ) -> list[InboundNotification]:
        from crewlet.notifications.transports.slack import SlackTransport

        transport = self._transports.get("slack")
        if isinstance(transport, SlackTransport):
            result = await transport.handle_event(
                body=body,
                handle=handle,
                headers=headers,
                body_raw=body_raw,
            )
            if isinstance(result, InboundNotification):
                return [result]
            # dict = Slack challenge (already handled by API)
            if isinstance(result, dict):
                return []
            # None = skipped — record the decision for traceability
            skip_reason = transport.last_skip_reason
            if skip_reason:
                logger.info(
                    "slack_webhook_skipped",
                    handle=handle,
                    reason=skip_reason,
                )
                await self._record_skip("slack", handle, skip_reason)
            return []
        logger.warning("slack_transport_not_configured")
        return []

    async def _parse_github(
        self,
        body: dict,
        headers: dict,
    ) -> list[InboundNotification]:

        from crewlet.notifications.sources import parse_github_webhook

        n = await parse_github_webhook("github", body, headers)
        return [n] if n is not None else []

    async def _parse_gitlab(
        self,
        body: dict,
        headers: dict,
    ) -> list[InboundNotification]:
        from crewlet.notifications.sources import parse_gitlab_webhook

        # Intersect @mentions and participants against the parties we can
        # actually route to (agents ∪ human seats), so a comment
        # mentioning outsiders never fans out.
        known = self._handle_registry.known_external_ids("gitlab")

        # Participants-based routing needs a read credential
        # (integrations.gitlab.token); without one the parser degrades to
        # payload-derived targets.
        fetcher = None
        cfg = self._gitlab_config
        if cfg is not None and getattr(cfg, "token", "") and getattr(cfg, "url", ""):
            from crewlet.gitlab.client import build_participants_fetcher

            fetcher = build_participants_fetcher(cfg.api_base, cfg.token)

        return await parse_gitlab_webhook(
            "gitlab",
            body,
            headers,
            known_usernames=known,
            participants_fetcher=fetcher,
        )

    async def _parse_plane(
        self,
        body: dict,
        headers: dict,
        body_raw: bytes,
    ) -> list[InboundNotification]:
        from crewlet.notifications.transports.plane import PlaneTransport

        # The transport owns its enrichment client (built from the
        # resolved PlaneConfig by ``build_notification_transports``, the
        # Confluence pattern) — no service-side config store needed.
        transport = self._transports.get("plane")
        if isinstance(transport, PlaneTransport):
            return await transport.handle_webhook(
                body=body, headers=headers, body_raw=body_raw
            )
        from crewlet.notifications.transports.plane import parse_plane_webhook

        n = parse_plane_webhook(body)
        return [n] if n is not None else []

    async def _handle_outbound(self, event: Event) -> None:
        """Dispatch an outbound message through the appropriate transport."""
        if not self._running:
            return

        try:
            message = OutboundMessage(**event.payload)
        except Exception as exc:
            logger.error(
                "invalid_outbound_payload",
                event_id=str(event.id),
                event_type=event.type,
                payload_keys=list(event.payload.keys()) if event.payload else [],
                error=str(exc),
            )
            return

        transport = self._transports.get(message.transport)
        if transport is None:
            logger.warning(
                "unknown_transport",
                transport=message.transport,
                sender_handle=message.sender_handle,
                recipient=message.recipient,
            )
            return

        try:
            result = await transport.send(message)
            if result:
                logger.info(
                    "message_sent",
                    transport=message.transport,
                    sender_handle=message.sender_handle,
                    recipient=message.recipient,
                )
            else:
                logger.error(
                    "transport_send_failed",
                    transport=message.transport,
                    sender_handle=message.sender_handle,
                )
        except Exception as exc:
            logger.error(
                "transport_send_error",
                transport=message.transport,
                error=str(exc),
            )

    def _check_rate_limit(self, agent_id: str) -> bool:
        """Check if the agent has exceeded the rate limit.

        Uses a 1-second sliding window. Returns True if the
        notification should be processed.
        """
        if self._rate_limit <= 0:
            return True

        now = time.monotonic()
        window = 1.0

        if agent_id not in self._rate_tracker:
            self._rate_tracker[agent_id] = []

        timestamps = self._rate_tracker[agent_id]
        # Prune old entries
        timestamps[:] = [t for t in timestamps if now - t < window]

        if len(timestamps) >= self._rate_limit:
            return False

        timestamps.append(now)
        return True
