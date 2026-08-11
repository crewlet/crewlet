"""ConfluenceTransport — webhook transport (outbound ``send()`` unsupported).

Outbound Confluence operations (creating/updating pages, comments) are
handled by MCP tools with per-agent credentials via ``mcp_env``.
The ``send()`` method returns False (not supported).

Inbound webhook parsing is provided by ``handle_webhook()`` which
performs signature verification (HMAC-SHA256 for Data Center),
event deduplication, self-ignore, and
multi-strategy routing.  It returns a list of ``InboundNotification``
objects for the NotificationService to route to agent inboxes.

Routing strategy:
1. **Watchers** — fetch page watchers via REST API (agents who edit
   a page are auto-added as watchers by Confluence, including the
   page creator).
2. **@mentions** — agents mentioned in comment body via API-inserted
   markup, auto-added as page watchers for future events.
3. **Space leads** — fan out to all unit leads for the space.
4. **Standard resolution** — fallback.

Note: Confluence does NOT auto-add commenters as watchers.  When an
agent comments on a page via MCP tools, the engine should separately
call ``add_watcher()`` so future events on that page route to them.

Note: The Confluence UI does not allow @mentioning service accounts,
but the API can insert mention markup.  When an agent creates a
comment via MCP tools that includes ``<ri:user ri:account-id="..."/>``
markup, the transport extracts these mentions, routes to the mentioned
agents, and auto-adds them as page watchers so they receive future
events on that page.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import re
import time
from collections.abc import Awaitable, Callable
from typing import TYPE_CHECKING, Any

from crewlet._logging import get_logger
from crewlet.notifications.protocol import InboundNotification, OutboundMessage

if TYPE_CHECKING:
    import httpx

    from crewlet.config import ConfluenceConfig
    from crewlet.notifications.handle import HandleRegistry

# Callback shape for the engine's webhook -> tool-skill re-index hook.
# Fires for every page-shape webhook (page_*, comment_*, label_*)
# BEFORE the self-ignore + routing filters so the Tool Skills registry
# always reflects current state regardless of who triggered the change.
IndexCallback = Callable[[str, str], Awaitable[None]]
"""``async (event_type: str, page_id: str) -> None``."""

logger = get_logger("notifications.confluence")

# Regex to extract @mention account IDs from Confluence storage format.
# Cloud: <ri:user ri:account-id="abc123" />
# Data Center: <ri:user ri:userkey="JIRAUSER10001" />
_MENTION_ACCOUNT_RE = re.compile(r'ri:account-id="([^"]+)"')
_MENTION_USERKEY_RE = re.compile(r'ri:userkey="([^"]+)"')


def _extract_mention_ids(html: str) -> list[str]:
    """Extract @mentioned user account IDs from Confluence storage format.

    The Confluence UI does not allow mentioning service accounts,
    but the API can insert mention markup.  Agents commenting via
    MCP tools can include ``<ri:user ri:account-id="..."/>`` to
    direct a notification to another agent.
    """
    ids = _MENTION_ACCOUNT_RE.findall(html)
    ids.extend(_MENTION_USERKEY_RE.findall(html))
    return ids


def parse_confluence_webhook(
    body: dict[str, Any],
) -> InboundNotification | None:
    """Parse a Confluence webhook payload into a notification.

    Handles events: page_created, page_updated, page_removed,
    comment_created, comment_updated, label_added, label_removed.

    Confluence Cloud (via Forge app) and Data Center webhooks use
    slightly different payload shapes.  This parser handles both.
    """
    # Cloud webhooks use "event" key; Data Center uses
    # "webhookEvent" or just top-level event name.
    event_type = (
        body.get("event") or body.get("webhookEvent") or body.get("eventType", "")
    )
    if not event_type:
        logger.debug("confluence_webhook_no_event_type")
        return None

    # Extract page data — Cloud nests under "page", DC may use
    # "content" or "page".
    page = body.get("page") or body.get("content") or {}
    space = page.get("space") or body.get("space") or {}
    comment = body.get("comment") or {}

    # User who triggered the event
    user_data = body.get("userAccountId") or body.get("user") or {}
    if isinstance(user_data, str):
        # Cloud sends userAccountId as a plain string
        sender = ""
        sender_account_id = user_data
    elif isinstance(user_data, dict):
        sender = user_data.get("displayName", "")
        sender_account_id = user_data.get("accountId", "")
    else:
        sender = ""
        sender_account_id = ""

    page_id = str(page.get("id", ""))
    page_title = page.get("title", "")
    space_key = space.get("key", "")
    space_name = space.get("name", "")

    # Build body text
    body_text = ""
    if "comment" in event_type and comment:
        # Comment events — extract comment body
        comment_body = comment.get("body") or {}
        logger.debug(
            "confluence_comment_body_extraction",
            has_body=bool(comment_body),
            body_type=type(comment_body).__name__,
            body_keys=(
                list(comment_body.keys()) if isinstance(comment_body, dict) else []
            ),
            comment_keys=list(comment.keys()),
        )
        if isinstance(comment_body, dict):
            # Cloud uses {"storage": {"value": "..."}}
            storage = comment_body.get("storage") or comment_body.get("view") or {}
            body_text = storage.get("value", "")
        elif isinstance(comment_body, str):
            body_text = comment_body
    elif page:
        # Page events — include version message if available
        version = page.get("version") or {}
        body_text = version.get("message", "")

    # Build subject
    if page_title:
        subject = f"[{space_key}] {page_title}" if space_key else page_title
    else:
        subject = f"Confluence {event_type}"

    # Extract @mention IDs for routing metadata
    mention_ids = _extract_mention_ids(body_text) if body_text else []

    metadata: dict[str, str] = {
        "page_id": page_id,
        "page_title": page_title,
        "space_key": space_key,
        "space_name": space_name,
        "event_type": event_type,
        "actor_account_id": sender_account_id,
        "actor_name": sender,
        "timestamp": str(body.get("timestamp", "")),
    }

    if mention_ids:
        metadata["mention_ids"] = ",".join(mention_ids)

    if comment:
        metadata["comment_id"] = str(comment.get("id", ""))

    logger.debug(
        "confluence_webhook_parsed",
        event_type=event_type,
        page_id=page_id,
        space_key=space_key,
        sender=sender,
        mention_count=len(mention_ids),
    )

    return InboundNotification(
        source="confluence",
        source_event_type=event_type,
        sender=sender,
        subject=subject,
        body=body_text,
        metadata=metadata,
    )


class ConfluenceTransport:
    """Confluence transport with admin API access and webhook parsing.

    Outbound Confluence operations are handled by MCP tools —
    ``send()`` is a no-op that returns False. The transport provides
    ``handle_webhook()`` for inbound webhook processing and
    ``fetch_watchers()`` / ``add_watcher()`` for page watcher
    management via the Confluence REST API.
    """

    name: str = "confluence"

    def __init__(self, config: ConfluenceConfig) -> None:
        self._webhook_secret = config.webhook_secret
        self._confluence_url = config.base_url.rstrip("/")
        # REST API base:
        # - URLs ending with /wiki are used as-is (Cloud site URLs)
        # - Atlassian Cloud gateway URLs need a /wiki prefix
        # - Data Center URLs are used as-is (REST is at /rest/api)
        is_cloud_gateway = "://api.atlassian.com" in self._confluence_url
        if self._confluence_url.endswith("/wiki"):
            self._rest_base = self._confluence_url
        elif is_cloud_gateway:
            self._rest_base = f"{self._confluence_url}/wiki"
        else:
            self._rest_base = self._confluence_url
        self._shareable_base_url = config.shareable_base_url.rstrip("/")
        self._confluence_email = config.email
        self._confluence_token = config.token
        self._http_client: Any | None = None  # httpx.AsyncClient
        self._handle_registry: HandleRegistry | None = None
        self._processed_events: dict[str, float] = {}
        self._dedup_ttl = 300.0  # 5 minutes
        self._space_key_leads: dict[str, list[str]] = {}
        self._notification_excluded_spaces: set[str] = set()
        self._index_callback: IndexCallback | None = None

    def set_handle_registry(self, registry: HandleRegistry) -> None:
        """Set the handle registry for self-ignore checks."""
        self._handle_registry = registry

    def set_index_callback(self, callback: IndexCallback | None) -> None:
        """Wire the tool-skill re-index hook.

        ``callback`` runs for every page-shape webhook BEFORE the
        notification self-ignore filter so the Tool Skills registry
        reflects every change regardless of who triggered it.  Pass
        ``None`` to disable it without tearing the transport down.
        """
        self._index_callback = callback

    def set_space_key_leads(self, mapping: dict[str, list[str]]) -> None:
        """Set the space key to unit lead handle mapping.

        Multiple leads can be mapped to the same space key — each
        receives a copy of the webhook notification.

        Args:
            mapping: ``{confluence_space: [lead_handle, ...]}`` (see
                :func:`crewlet.config.build_space_key_lead_map`)
        """
        self._space_key_leads = {k.upper(): v for k, v in mapping.items()}
        if mapping:
            logger.info(
                "space_key_lead_mapping_configured",
                mapping=", ".join(f"{k}→[{', '.join(v)}]" for k, v in mapping.items()),
            )

    def set_notification_excluded_spaces(self, keys: list[str]) -> None:
        """Mark spaces whose webhooks must not produce notifications.

        Engine-managed spaces (e.g. the Tool Skills space, which feeds
        the in-memory ``PromptSkillRegistry`` via the index callback)
        have no human or agent recipient by design.  Without this set,
        page edits in those spaces fall through ``handle_webhook``'s
        standard-resolution path, surface as ``notification_undeliverable``
        warnings in ``NotificationService``, and emit spurious
        ``notification_skipped`` events.

        The index callback still fires for excluded spaces — only the
        notification-routing path is short-circuited.
        """
        self._notification_excluded_spaces = {k.upper() for k in keys if k}
        if self._notification_excluded_spaces:
            logger.info(
                "notification_excluded_spaces_configured",
                spaces=", ".join(sorted(self._notification_excluded_spaces)),
            )

    async def start(self) -> None:
        """Start the transport and create the Confluence API client."""
        import httpx

        if self._confluence_email:
            auth_str = f"{self._confluence_email}:{self._confluence_token}"
            encoded = base64.b64encode(auth_str.encode()).decode()
            auth_header = f"Basic {encoded}"
        else:
            auth_header = f"Bearer {self._confluence_token}"

        self._http_client = httpx.AsyncClient(
            base_url=self._confluence_url,
            headers={
                "Authorization": auth_header,
                "Accept": "application/json",
            },
            timeout=10.0,
        )
        logger.info(
            "confluence_transport_started",
            url=self._confluence_url,
            auth="basic" if self._confluence_email else "bearer",
        )

    def new_user_client(self, username: str, token: str) -> httpx.AsyncClient:
        """Build a per-call HTTP client authed as a specific Atlassian user.

        Used by
        :class:`~crewlet.knowledge.confluence_search.ConfluenceSearcher`
        to run query-time CQL searches as the calling agent's own user,
        so Confluence enforces that agent's page-level permissions.
        Mirrors the admin client built in :meth:`start` -- Basic auth
        when ``username`` is set (Cloud), Bearer otherwise (Data Center
        PAT).  The caller owns the returned client and must close it.
        """
        import httpx

        if username:
            encoded = base64.b64encode(f"{username}:{token}".encode()).decode()
            auth_header = f"Basic {encoded}"
        else:
            auth_header = f"Bearer {token}"
        return httpx.AsyncClient(
            base_url=self._confluence_url,
            headers={"Authorization": auth_header, "Accept": "application/json"},
            timeout=10.0,
        )

    async def stop(self) -> None:
        """Stop and clean up resources."""
        self._processed_events.clear()
        if self._http_client is not None:
            await self._http_client.aclose()
            self._http_client = None
        logger.info("confluence_transport_stopped")

    async def send(self, message: OutboundMessage) -> bool:
        """Not supported — outbound Confluence operations use MCP tools."""
        logger.warning("confluence_outbound_not_supported")
        return False

    # --- REST API helpers ---

    async def fetch_watchers(self, page_id: str) -> list[str]:
        """Fetch watcher account IDs for a Confluence page via REST API.

        Confluence auto-adds users as watchers when they edit a page.
        This is the primary routing mechanism — similar to Jira's
        watcher-based routing.

        Note: Confluence does NOT auto-add commenters as watchers.
        Use ``add_watcher()`` to explicitly watch a page after
        commenting.
        """
        if self._http_client is None:
            logger.warning("fetch_watchers_no_http_client")
            return []

        try:
            # Cloud: /wiki/rest/api/content/{id}/notification/child-created
            # This returns users watching for child content (comments).
            # We also check /notification/created for page-level watchers.
            watchers: list[str] = []
            for notif_type in ("created", "child-created"):
                url = (
                    f"{self._rest_base}"
                    f"/rest/api/content/{page_id}/notification/{notif_type}"
                )
                resp = await self._http_client.get(url)
                if resp.status_code == 200:
                    data = resp.json()
                    results = data.get("results", [])
                    for entry in results:
                        user = entry.get("watcher") or {}
                        account_id = user.get("accountId", "")
                        if account_id and account_id not in watchers:
                            watchers.append(account_id)
            return watchers
        except Exception as exc:
            logger.warning("fetch_watchers_failed", page_id=page_id, error=str(exc))
            return []

    async def add_watcher(self, page_id: str, account_id: str) -> bool:
        """Add a user as a watcher on a Confluence page.

        Call this after an agent comments on a page so they receive
        future webhook events for that page.  Confluence does NOT
        auto-add commenters as watchers (unlike page editors).

        Returns True if successful.
        """
        if self._http_client is None:
            logger.warning("add_watcher_no_http_client")
            return False

        try:
            url = f"{self._rest_base}/rest/api/content/{page_id}/notification/created"
            resp = await self._http_client.post(
                url,
                json={"watcher": {"accountId": account_id}},
            )
            if resp.status_code in (200, 204):
                logger.debug(
                    "watcher_added",
                    page_id=page_id,
                    account_id=account_id,
                )
                return True
            logger.warning(
                "add_watcher_failed",
                page_id=page_id,
                account_id=account_id,
                status=resp.status_code,
            )
            return False
        except Exception as exc:
            logger.warning(
                "add_watcher_error",
                page_id=page_id,
                account_id=account_id,
                error=str(exc),
            )
            return False

    async def fetch_comment_body(self, comment_id: str) -> str:
        """Fetch comment body HTML via REST API.

        Forge webhook payloads do not include the comment body
        (only metadata like id, type, title, etc.).  This method
        fetches the full comment content so we can extract @mentions.

        Returns the storage-format HTML string, or "" on failure.
        """
        if self._http_client is None:
            logger.warning("fetch_comment_body_no_http_client")
            return ""

        try:
            url = f"{self._rest_base}/rest/api/content/{comment_id}?expand=body.storage"
            resp = await self._http_client.get(url)
            if resp.status_code == 200:
                data = resp.json()
                body_val = data.get("body", {}).get("storage", {}).get("value", "")
                logger.debug(
                    "comment_body_fetched",
                    comment_id=comment_id,
                    body_length=len(body_val),
                )
                return body_val
            logger.warning(
                "fetch_comment_body_failed",
                comment_id=comment_id,
                status=resp.status_code,
            )
            return ""
        except Exception as exc:
            logger.warning(
                "fetch_comment_body_error",
                comment_id=comment_id,
                error=str(exc),
            )
            return ""

    # --- Webhook parsing utilities (used by NotificationService) ---

    def verify_webhook(self, body_raw: str | bytes, headers: dict[str, str]) -> bool:
        """Verify webhook signature (HMAC-SHA256 for Data Center).

        If no webhook_secret is configured, returns True (no-op).
        Cloud webhooks arrive via the Forge app and do not use this
        method.
        """
        if not self._webhook_secret:
            return True

        signature = headers.get("x-hub-signature", "")
        if not signature:
            return False

        if isinstance(body_raw, str):
            body_raw = body_raw.encode("utf-8")

        expected = (
            "sha256="
            + hmac.new(
                self._webhook_secret.encode("utf-8"),
                body_raw,
                hashlib.sha256,
            ).hexdigest()
        )
        return hmac.compare_digest(expected, signature)

    def _dedup_event(self, body: dict[str, Any]) -> bool:
        """Check if this webhook event was already processed.

        Returns True if the event is a duplicate (should be skipped).
        Uses timestamp + page ID + event type as dedup key.
        """
        timestamp = str(body.get("timestamp", ""))
        page = body.get("page") or body.get("content") or {}
        page_id = str(page.get("id", ""))
        event_type = (
            body.get("event") or body.get("webhookEvent") or body.get("eventType", "")
        )
        dedup_key = f"{timestamp}:{page_id}:{event_type}"

        now = time.monotonic()

        # Prune old entries
        stale = [
            k for k, t in self._processed_events.items() if now - t > self._dedup_ttl
        ]
        for k in stale:
            del self._processed_events[k]

        if dedup_key in self._processed_events:
            logger.debug("duplicate_confluence_event_skipped", dedup_key=dedup_key)
            return True

        self._processed_events[dedup_key] = now
        return False

    def _resolve_account_id(self, account_id: str) -> str | None:
        """Try to resolve an Atlassian account ID to an agent handle."""
        if not account_id or self._handle_registry is None:
            return None
        agent = self._handle_registry.resolve_external_id("confluence", account_id)
        return agent.handle if agent is not None else None

    async def handle_webhook(
        self,
        body: dict[str, Any],
        headers: dict[str, str] | None = None,
        body_raw: str | bytes = b"",
    ) -> list[InboundNotification]:
        """Process an incoming Confluence webhook payload.

        This method handles both native Confluence/Data Center webhook
        payloads and Confluence Cloud events delivered via the Forge
        app after they have been normalized into a Confluence webhook
        shape. Forge-originated Cloud events do not use the Data Center
        HMAC signature verification path.

        Routing strategy (ordered by specificity):
        1. **Watchers** — fetch page watchers via REST API. Agents
           who edit pages are auto-added as watchers by Confluence.
           Page creators are already watchers so no separate step
           is needed for them.
        2. **@mentions** — agents mentioned in comment body via API-
           inserted markup. Mentioned agents are auto-added as page
           watchers so they receive future events.
        3. **Space leads** → fan out to all unit leads for the space,
           excluding the trigger user.
        4. **Standard resolution** → return for generic resolution.

        Every routing step excludes the user who triggered the event —
        an agent is never woken by a webhook describing its own action.
        If watchers and/or mentions are found (steps 1-2), space
        leads are NOT notified.  If the only candidate recipient is the
        trigger user (the sole watcher, or the sole space lead), the
        event is dropped — it does NOT fall through to a later step.
        """
        # Verify HMAC-SHA256 signature (Data Center).
        if self._webhook_secret:
            if not headers:
                logger.warning("webhook_no_headers_for_signature")
                return []
            if not self.verify_webhook(body_raw, headers):
                logger.warning("webhook_invalid_signature")
                return []

        # Log raw webhook body for debugging routing issues
        comment = body.get("comment") or {}
        logger.debug(
            "confluence_webhook_body_received",
            webhook_event=body.get("event") or body.get("webhookEvent", ""),
            page_id=str((body.get("page") or body.get("content") or {}).get("id", "")),
            comment_id=str(comment.get("id", "")),
            has_comment_body=bool(comment.get("body")),
            trigger_user=body.get("userAccountId", ""),
            body_keys=sorted(body.keys()),
        )

        # Deduplicate
        if self._dedup_event(body):
            return []

        # Identify trigger user — used to exclude them from routing
        # (they already know about their own action). We do NOT drop
        # the entire webhook here because other agents (watchers) may
        # need to receive it.
        trigger_account_id = ""
        user_data = body.get("userAccountId") or body.get("user") or {}
        if isinstance(user_data, str):
            trigger_account_id = user_data
        elif isinstance(user_data, dict):
            trigger_account_id = user_data.get("accountId", "")

        # Parse the webhook payload
        notification = parse_confluence_webhook(body)
        if notification is None:
            return []

        # Fire the tool-skill re-index hook before any routing.  The
        # Tool Skills sync cares about every page change, including
        # self-edits; the self-ignore filter further down only
        # suppresses the *notification* path, not the registry.
        if self._index_callback is not None:
            page_id_for_index = notification.metadata.get("page_id", "")
            event_type_for_index = notification.metadata.get("event_type", "")
            if page_id_for_index and event_type_for_index:
                try:
                    await self._index_callback(event_type_for_index, page_id_for_index)
                except Exception:
                    # Indexing must never break notification routing.
                    logger.exception(
                        "confluence_index_callback_failed",
                        event_type=event_type_for_index,
                        page_id=page_id_for_index,
                    )

        # Short-circuit for engine-managed spaces (e.g. the Tool Skills
        # space).  The index callback above already applied the change
        # to the in-memory registry; there is no human or agent recipient
        # for these events, so feeding them to the notification router
        # would surface as ``notification_undeliverable`` + a spurious
        # ``notification_skipped`` event.
        space_key_for_excl = notification.metadata.get("space_key", "")
        if (
            space_key_for_excl
            and space_key_for_excl.upper() in self._notification_excluded_spaces
        ):
            logger.debug(
                "notification_skipped_excluded_space",
                space_key=space_key_for_excl,
                page_id=notification.metadata.get("page_id", ""),
                event_type=notification.metadata.get("event_type", ""),
            )
            return []

        # Forge webhooks don't include comment body — fetch via API
        # so we can extract @mentions for routing.
        comment_id = notification.metadata.get("comment_id", "")
        if comment_id and not notification.body and self._http_client is not None:
            fetched_body = await self.fetch_comment_body(comment_id)
            if fetched_body:
                notification.body = fetched_body
                # Re-extract mentions from the fetched body
                mention_ids = _extract_mention_ids(fetched_body)
                if mention_ids:
                    notification.metadata["mention_ids"] = ",".join(mention_ids)
                    logger.info(
                        "mentions_extracted_from_fetched_body",
                        comment_id=comment_id,
                        mention_count=len(mention_ids),
                        mention_ids=",".join(mention_ids),
                    )

        # Inject shareable page URL so prompts can link to it.
        page_id = notification.metadata.get("page_id", "")
        space_key = notification.metadata.get("space_key", "")
        if self._shareable_base_url and page_id:
            notification.metadata["page_url"] = (
                f"{self._shareable_base_url}/spaces/{space_key}/pages/{page_id}"
                if space_key
                else f"{self._shareable_base_url}/pages/{page_id}"
            )

        # --- Routing ---
        results: list[InboundNotification] = []
        delivered_handles: set[str] = set()

        # Resolve trigger user's handle once — skip them during
        # routing since they already know about their own action.
        trigger_handle = self._resolve_account_id(trigger_account_id)

        # 1. Watchers — fetch from Confluence REST API.
        # Agents who edit pages are auto-added as watchers.
        watcher_ids = await self.fetch_watchers(page_id) if page_id else []
        trigger_is_watcher = False
        for wid in watcher_ids:
            handle = self._resolve_account_id(wid)
            if not handle:
                continue
            if handle == trigger_handle:
                trigger_is_watcher = True
                continue
            if handle not in delivered_handles:
                n = notification.model_copy(deep=True)
                n.recipient_handle = handle
                n.metadata["routed_via"] = "watcher"
                results.append(n)
                delivered_handles.add(handle)

        # 2. @mentions — agents mentioned in comment body.
        # The Confluence UI can't mention service accounts, but the
        # API can insert mention markup.  When found, route to the
        # mentioned agent and auto-add them as a page watcher.
        mention_ids_raw = notification.metadata.get("mention_ids", "")
        if mention_ids_raw:
            for mid in mention_ids_raw.split(","):
                mid = mid.strip()
                handle = self._resolve_account_id(mid)
                if (
                    handle
                    and handle != trigger_handle
                    and handle not in delivered_handles
                ):
                    n = notification.model_copy(deep=True)
                    n.recipient_handle = handle
                    n.metadata["routed_via"] = "mention"
                    results.append(n)
                    delivered_handles.add(handle)
                    # Auto-follow: add mentioned agent as page watcher
                    # so they receive future events on this page.
                    if page_id:
                        await self.add_watcher(page_id, mid)

        # If specific recipients were found (steps 1-2), skip space leads.
        # Trigger user was already excluded from the loops above.
        if delivered_handles:
            logger.info(
                "routing_to_specific_recipients",
                space_key=space_key,
                handles=", ".join(delivered_handles),
            )
            return results

        # If the trigger user was a watcher but no other watchers were
        # delivered, do NOT fall through to space leads. The trigger
        # user already knows about their own action.
        if trigger_is_watcher and not delivered_handles:
            logger.info(
                "all_watchers_excluded_as_trigger",
                space_key=space_key,
                page_id=page_id,
                trigger_handle=trigger_handle,
                watcher_count=len(watcher_ids),
            )
            return []

        # 3. Space leads — fan out to all unit leads for the space,
        #    EXCLUDING the trigger user (same self-ignore invariant the
        #    watcher and @mention steps above already apply).  An agent
        #    that leads the space it just acted in must never be notified
        #    about its own action: a lead commenting on a page in its own
        #    space (e.g. the CEO on the leadership space) is the default
        #    space-lead recipient for the resulting webhook, so without
        #    this exclusion it re-triggers itself and posts another
        #    acknowledgement comment every cycle — an endless
        #    self-notification loop.
        if space_key:
            lead_handles = self._space_key_leads.get(space_key.upper(), [])
            if lead_handles:
                for handle in lead_handles:
                    if handle == trigger_handle:
                        continue
                    if handle in delivered_handles:
                        continue
                    n = notification.model_copy(deep=True)
                    n.recipient_handle = handle
                    n.metadata["routed_via"] = "space_lead"
                    results.append(n)
                    delivered_handles.add(handle)
                if results:
                    logger.info(
                        "routing_to_space_leads",
                        space_key=space_key,
                        lead_handles=", ".join(delivered_handles),
                    )
                    return results
                # Every mapped lead was the trigger user — drop the
                # event.  Do NOT fall through to standard resolution:
                # there is no other recipient, and the trigger already
                # knows about its own action (mirrors the
                # ``trigger_is_watcher`` drop above).
                logger.info(
                    "space_leads_all_excluded_as_trigger",
                    space_key=space_key,
                    page_id=page_id,
                    trigger_handle=trigger_handle,
                )
                return []

        # 4. No specific routing — return for standard resolution.
        logger.info(
            "standard_resolution_fallback",
            space_key=space_key,
            page_id=page_id,
        )
        return [notification]
