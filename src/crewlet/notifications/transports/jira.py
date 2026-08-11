"""JiraTransport — outbound-only Jira transport with webhook parsing.

Outbound Jira operations (comments, transitions, assignments) are
handled by MCP tools with per-agent credentials via ``mcp_env``.
The ``send()`` method returns False (not supported).

Inbound webhook parsing is provided by ``handle_webhook()`` which
performs signature verification, event deduplication, self-ignore,
and watcher-based routing. It returns a list of ``InboundNotification``
objects (one per resolved recipient) for the API layer to publish to
the EventQueue.

The transport requires a ``JiraConfig`` (from the top-level ``jira:``
section in the company YAML) which provides the admin / service account
credentials for the Jira REST API.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import time
from typing import TYPE_CHECKING, Any

from crewlet._logging import get_logger
from crewlet.notifications.protocol import InboundNotification, OutboundMessage
from crewlet.notifications.sources import parse_jira_webhook

if TYPE_CHECKING:
    from crewlet.config import JiraConfig
    from crewlet.notifications.handle import HandleRegistry

logger = get_logger("notifications.jira")


class JiraTransport:
    """Jira transport with admin API access and webhook parsing.

    Outbound Jira operations are handled by MCP tools — ``send()``
    is a no-op that returns False. The transport provides
    ``fetch_watchers()`` for org-level API calls and
    ``handle_webhook()`` for inbound webhook processing.
    """

    name: str = "jira"

    def __init__(self, config: JiraConfig) -> None:
        self._webhook_secret = config.webhook_secret
        self._jira_url = config.base_url.rstrip("/")
        self._jira_email = config.email
        self._jira_token = config.token
        self._http_client: Any | None = None  # httpx.AsyncClient
        self._handle_registry: HandleRegistry | None = None
        self._processed_events: dict[str, float] = {}
        self._dedup_ttl = 300.0  # 5 minutes
        self._project_key_leads: dict[str, str] = {}

    def set_handle_registry(self, registry: HandleRegistry) -> None:
        """Set the handle registry for self-ignore checks."""
        self._handle_registry = registry

    def set_project_key_leads(self, mapping: dict[str, str]) -> None:
        """Set the project key to team lead handle mapping.

        Used for fallback routing when a newly created ticket has no
        assignee and no watchers.

        Args:
            mapping: ``{JIRA_PROJECT_KEY: lead_handle}``
        """
        self._project_key_leads = {k.upper(): v for k, v in mapping.items()}
        if mapping:
            logger.info(
                "project_key_lead_mapping_configured",
                mapping=", ".join(f"{k}→{v}" for k, v in mapping.items()),
            )

    async def start(self) -> None:
        """Start the transport and create the Jira API client."""
        import httpx

        if self._jira_email:
            auth_str = f"{self._jira_email}:{self._jira_token}"
            encoded = base64.b64encode(auth_str.encode()).decode()
            auth_header = f"Basic {encoded}"
        else:
            auth_header = f"Bearer {self._jira_token}"

        self._http_client = httpx.AsyncClient(
            base_url=self._jira_url,
            headers={
                "Authorization": auth_header,
                "Accept": "application/json",
            },
            timeout=10.0,
        )
        logger.info(
            "jira_transport_started",
            url=self._jira_url,
            auth="basic" if self._jira_email else "bearer",
        )

    async def stop(self) -> None:
        """Stop and clean up resources."""
        self._processed_events.clear()
        if self._http_client is not None:
            await self._http_client.aclose()
            self._http_client = None
        logger.info("jira_transport_stopped")

    async def send(self, message: OutboundMessage) -> bool:
        """Not supported — outbound Jira operations use MCP tools."""
        logger.warning("jira_outbound_not_supported")
        return False

    # --- Watcher fetching ---

    async def fetch_watchers(self, issue_key: str) -> list[str]:
        """Fetch watcher account IDs for a Jira issue via REST API."""
        if self._http_client is None:
            logger.warning("fetch_watchers_no_http_client")
            return []

        try:
            url = f"{self._jira_url}/rest/api/3/issue/{issue_key}/watchers"
            resp = await self._http_client.get(url)
            resp.raise_for_status()
            data = resp.json()
            watchers = data.get("watchers", [])
            return [w["accountId"] for w in watchers if w.get("accountId")]
        except Exception as exc:
            logger.warning("fetch_watchers_failed", issue_key=issue_key, error=str(exc))
            return []

    # --- Webhook parsing utilities (used by API layer) ---

    def verify_webhook(self, body_raw: str | bytes, headers: dict[str, str]) -> bool:
        """Verify Jira webhook signature (HMAC-SHA256).

        If no webhook_secret is configured, returns True (no-op).
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
        Uses timestamp + issue key + event type as dedup key.
        """
        timestamp = str(body.get("timestamp", ""))
        issue = body.get("issue", {})
        issue_key = issue.get("key", "")
        event_type = body.get("webhookEvent", "")
        dedup_key = f"{timestamp}:{issue_key}:{event_type}"

        now = time.monotonic()

        # Prune old entries
        stale = [
            k for k, t in self._processed_events.items() if now - t > self._dedup_ttl
        ]
        for k in stale:
            del self._processed_events[k]

        if dedup_key in self._processed_events:
            logger.debug("duplicate_jira_event_skipped", dedup_key=dedup_key)
            return True

        self._processed_events[dedup_key] = now
        return False

    async def handle_webhook(
        self,
        body: dict[str, Any],
        headers: dict[str, str] | None = None,
        body_raw: str | bytes = b"",
    ) -> list[InboundNotification]:
        """Process an incoming Jira webhook payload.

        Performs signature verification, deduplication, and
        watcher-based routing. The trigger user is excluded from
        routing (they already know about their own action) but
        the event is still delivered to other watchers/assignee.

        Routing strategy:
        1. Fetch ticket watchers from Jira REST API.
        2. Create a notification for each agent matching a watcher's
           account ID.
        3. If the assignee is not already a watcher, also create one
           for the assignee.
        4. If no watchers or assignee resolved, fall back to the
           team lead for the project key.
        5. If none of the above matched, return the original
           notification for standard resolution (email, handle, etc.).
        """
        # Verify HMAC-SHA256 signature (Data Center webhooks).
        # Cloud webhooks arrive via the Forge app route instead.
        if self._webhook_secret:
            if not headers:
                logger.warning("webhook_no_headers_for_signature")
                return []
            if not self.verify_webhook(body_raw, headers):
                logger.warning("webhook_invalid_signature")
                return []

        # Deduplicate
        if self._dedup_event(body):
            return []

        # Identify trigger user — used to exclude them from the
        # "meaningful recipients" check below.  We do NOT drop the
        # entire webhook because other agents (watchers, assignee)
        # may need to receive it.
        trigger_user = body.get("user", {}) or {}
        if not trigger_user:
            comment = body.get("comment", {}) or {}
            trigger_user = comment.get("author", {}) or {}
        trigger_account_id = trigger_user.get("accountId", "")

        # Parse via existing Jira webhook parser
        notification = await parse_jira_webhook("jira", body, headers or {})
        if notification is None:
            return []

        issue_key = notification.metadata.get("issue_key", "")
        assignee_id = notification.metadata.get("assignee_account_id", "")
        project_key = notification.metadata.get("project", "")

        # Fetch watchers from Jira REST API
        watcher_ids = await self.fetch_watchers(issue_key) if issue_key else []

        if watcher_ids:
            logger.debug(
                "watchers_resolved",
                issue_key=issue_key,
                count=len(watcher_ids),
            )

        results: list[InboundNotification] = []
        delivered_ids: set[str] = set()

        # Route to each watcher (skip trigger user — they already know)
        for watcher_id in watcher_ids:
            if watcher_id == trigger_account_id:
                continue
            watcher_notif = notification.model_copy(deep=True)
            watcher_notif.metadata["assignee_account_id"] = watcher_id
            watcher_notif.metadata["routed_via"] = "watcher"
            logger.info(
                "routing_to_watcher",
                issue_key=issue_key,
                watcher_id=watcher_id,
            )
            results.append(watcher_notif)
            delivered_ids.add(watcher_id)

        # If assignee exists, wasn't already delivered, and isn't trigger
        if (
            assignee_id
            and assignee_id != trigger_account_id
            and assignee_id not in delivered_ids
        ):
            logger.info(
                "routing_to_assignee",
                issue_key=issue_key,
                assignee_id=assignee_id,
            )
            results.append(notification)
            delivered_ids.add(assignee_id)

        # Route to anyone @-mentioned in the comment body.  Jira's
        # watcher list does NOT auto-include mentioned users, so a
        # non-watcher who got @'d would never get the notification
        # otherwise.  Mentions live in the ADF tree of
        # ``body.comment.body``; walk it for ``mention`` nodes.
        from crewlet.notifications.sources import extract_adf_mention_ids

        comment = body.get("comment") or {}
        for mention_id in extract_adf_mention_ids(comment.get("body")):
            if mention_id == trigger_account_id:
                continue
            if mention_id in delivered_ids:
                continue
            mention_notif = notification.model_copy(deep=True)
            mention_notif.metadata["assignee_account_id"] = mention_id
            mention_notif.metadata["routed_via"] = "mention"
            logger.info(
                "routing_to_mentioned_user",
                issue_key=issue_key,
                mentioned_account_id=mention_id,
            )
            results.append(mention_notif)
            delivered_ids.add(mention_id)

        # Fallback: route to project lead when the ticket has no meaningful
        # recipient.  Jira auto-adds the creator as a watcher, so if the
        # only person we delivered to is the event trigger user (creator),
        # the ticket is effectively unrouted — the creator already knows.
        meaningful_ids = delivered_ids - {trigger_account_id}
        if not meaningful_ids and project_key:
            lead_handle = self._project_key_leads.get(project_key.upper())
            if lead_handle:
                lead_notif = notification.model_copy(deep=True)
                lead_notif.recipient_handle = lead_handle
                lead_notif.metadata["routed_via"] = "project_lead_fallback"
                logger.info(
                    "routing_to_project_lead",
                    issue_key=issue_key,
                    lead_handle=lead_handle,
                    project_key=project_key,
                )
                results.append(lead_notif)
                delivered_ids.add(f"lead:{lead_handle}")
            else:
                logger.warning(
                    "no_lead_for_project_key",
                    project_key=project_key,
                    issue_key=issue_key,
                )

        # If nothing was delivered, return the original notification
        # for standard resolution (email/handle fallback).
        if not delivered_ids:
            logger.info(
                "standard_resolution_fallback",
                issue_key=issue_key,
                handle=notification.recipient_handle,
                email=notification.recipient_email,
            )
            results.append(notification)

        return results
