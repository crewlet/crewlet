"""PlaneTransport — webhook transport (outbound ``send()`` unsupported).

Outbound Plane operations (work items, comments, pages) are handled by
MCP tools with per-agent service-account credentials via ``mcp_env``.
The ``send()`` method returns False (not supported).

Inbound webhook parsing is provided by ``handle_webhook()``: event
deduplication, defensive payload coercion, and the two-layer routing
model (directed targets from the payload; thread activity fans out to
the work item's subscribers via the engine read credential, degrading
to payload assignees without one).  It returns a list of
``InboundNotification`` objects for the NotificationService to route
to agent inboxes.

Routing (see ``docs/integrations/plane.md``):

1. ``issue`` create → assignees; unassigned — or assigned ONLY to
   unregistered outsiders — → project-lead fallback.
2. ``issue`` update on ``activity.field == "assignees"`` → the newly
   added assignee (directed).
3. ``issue`` update (any other field) → subscribers (the fork's
   work-item subscribers endpoint),
   degrading to assignees; ``issue`` delete → assignees directly (the
   subscribers endpoint cannot serve a deleted work item).
4. ``issue_comment`` → @-mentions (the fork's ``<mention-component>``
   markup; no registry ⇒ no mentions, fail closed), then subscribers.

Every directed fan-out (assignee, assignee_added, subscriber, mention)
is intersected with the registered Plane UUIDs (agents ∪ human seats)
at the single choke point in ``_directed_copies`` — ordinary workspace
humans must never produce copies the service cannot resolve.  The
subscriber-vs-assignee degrade decision in ``_thread_targets`` is made
on the PRE-intersection list: an all-outsider subscriber list is a
successful lookup, not a failed one.
5. ``intake_issue`` → project lead (triage).
6. ``page`` (the fork's page webhooks) → index callback ALWAYS first (tool-skills
   sync), then project lead; the Tool Skills project is excluded from
   routing, and a page whose project cannot be resolved fails closed.
7. ``project`` → no routing; feeds the id→identifier cache.
8. ``cycle`` / ``module`` / ``user`` / unknown → dropped at debug.

The trigger actor is excluded from every fan-out; every copy carries
``metadata.actor_account_id`` so the service's generic self-action
guard catches any fall-through.  Signature verification does NOT
happen here — the API layer verified ``X-Plane-Signature`` before
publishing (GitHub/GitLab pattern); ``headers`` / ``body_raw`` are
accepted for dispatch-signature parity and ignored.
"""

from __future__ import annotations

import re
import time
from collections.abc import Awaitable, Callable, Sequence
from html import unescape
from typing import TYPE_CHECKING, Any

from crewlet._logging import get_logger
from crewlet.notifications.protocol import InboundNotification, OutboundMessage

if TYPE_CHECKING:
    from crewlet.config import PlaneConfig
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.plane.client import PlaneClient

# Callback shape for the engine's page webhook → tool-skill re-index
# hook.  Fires for every ``page`` event BEFORE the excluded-projects
# check and the routing filters so the Tool Skills registry always
# reflects current state regardless of who triggered the change.
# The transport defines and fires the hook; the PlaneSkillSyncWorker
# registers a worker via ``set_index_callback``.
IndexCallback = Callable[[str, str], Awaitable[None]]
"""``async (event_type: str, page_id: str) -> None``."""

logger = get_logger("notifications.plane")

# Regex to extract mentioned user UUIDs from the fork's
# ``<mention-component>`` markup rendered into ``comment_html``:
# <mention-component id="…" entity_identifier="<user-uuid>"
#   entity_name="user_mention"></mention-component>
# ``entity_name`` is the entity TYPE discriminator (``user_mention``),
# never a display name — labels resolve via the HandleRegistry.
_MENTION_RE = re.compile(r'<mention-component[^>]*entity_identifier="([^"]+)"')
_MENTION_COMPONENT_RE = re.compile(r"<mention-component[^>]*>(?:</mention-component>)?")
_TAG_RE = re.compile(r"<[^>]+>")
_WS_RE = re.compile(r"\s+")

# Events the parser produces notifications for.  ``project`` feeds the
# id→identifier cache; ``cycle`` / ``module`` / ``cycle_issue`` /
# ``module_issue`` / ``user`` and anything unknown are dropped.
_CONTENT_EVENTS = frozenset({"issue", "issue_comment", "intake_issue", "page"})

#: Cache-miss floor for the project id→identifier refresh: at most one
#: full ``list_projects()`` call per minute bounds the worst case of an
#: unresolvable UUID under webhook bursts, while a freshly created
#: project still resolves within one tick.
_PROJECT_CACHE_REFETCH_INTERVAL = 60.0


def extract_mention_ids(html: str) -> list[str]:
    """``entity_identifier`` UUIDs from the fork's ``<mention-component>`` markup.

    Lowercased, order-preserving, deduped.  The routing choke point
    (``PlaneTransport._directed_copies``) intersects the result with the
    registered Plane UUIDs (agents ∪ human seats) so a comment
    mentioning outsiders never fans out.
    """
    if not html:
        return []
    out: list[str] = []
    seen: set[str] = set()
    for raw in _MENTION_RE.findall(html):
        mention_id = raw.lower()
        if mention_id and mention_id not in seen:
            seen.add(mention_id)
            out.append(mention_id)
    return out


def _strip_html(html: str, registry: HandleRegistry | None = None) -> str:
    """Flatten ``comment_html`` / ``description_html`` to prompt text.

    Mention components render as ``@{party label}`` — the mentioned
    UUID resolved through the HandleRegistry (``entity_name`` is the
    node-type discriminator ``user_mention``, never a display name);
    unknown UUIDs fall back to ``@{uuid}``, which the prompt's
    ``_party_label`` pass can still rewrite.  Remaining tags collapse
    to whitespace.  Deliberately lossy (the consumer is an LLM).
    """
    if not html:
        return ""

    def _mention_label(match: re.Match[str]) -> str:
        id_match = _MENTION_RE.search(match.group(0))
        mention_id = id_match.group(1).lower() if id_match else ""
        if not mention_id:
            return ""
        label = ""
        if registry is not None:
            try:
                party = registry.resolve_party_external("plane", mention_id)
            except Exception:
                party = None
            if party is not None:
                label = party.name or party.handle
        return f"@{label or mention_id}"

    text = _MENTION_COMPONENT_RE.sub(_mention_label, html)
    text = _TAG_RE.sub(" ", text)
    text = _WS_RE.sub(" ", text).strip()
    return unescape(text)


# ── defensive payload coercions ──
# CE serializer shapes are not contractual across fork rebases: FK
# fields arrive as UUID strings or expanded dicts, actor as a UUID or a
# serialized user, and the serializers disagree on ``project`` vs
# ``project_id``.  Every extraction goes through these.


def _ref_id(value: Any) -> str:
    """Coerce a FK field (UUID string OR expanded ``{"id": …}`` dict) to
    a lowercased UUID string; ``""`` for anything else."""
    if isinstance(value, dict):
        value = value.get("id")
    if value is None:
        return ""
    return str(value).lower()


def _ids(value: Any) -> list[str]:
    """Coerce ``data.assignees`` — UUID strings OR user dicts — to
    lowercased UUID strings.

    NOT for subscriber responses: those are through-model rows whose
    ``id`` is the subscription row, see :func:`_subscriber_ids`.
    """
    out: list[str] = []
    if not isinstance(value, list):
        return out
    for item in value:
        if isinstance(item, dict):
            item = item.get("id")
        if item:
            out.append(str(item).lower())
    return out


def _subscriber_ids(rows: Any) -> list[str]:
    """User UUIDs from a work-item subscribers response.

    Each row is an ``IssueSubscriber`` through-model row — its ``id``
    is the subscription row's UUID, NEVER any user's id, so this reads
    only the embedded ``member`` profile / ``subscriber`` FK.  A
    non-empty response that yields nothing here must be treated by the
    caller as a FAILED lookup (degrade to assignees), not an empty
    subscriber set.
    """
    out: list[str] = []
    if not isinstance(rows, list):
        return out
    for row in rows:
        if isinstance(row, str):
            if row:
                out.append(row.lower())
            continue
        if not isinstance(row, dict):
            continue
        member = (
            row.get("member") or row.get("subscriber") or row.get("subscriber_detail")
        )
        uid = (member.get("id") if isinstance(member, dict) else member) or row.get(
            "subscriber_id"
        )
        if uid:
            out.append(str(uid).lower())
        else:
            logger.warning("plane_subscriber_shape_unrecognized", keys=sorted(row))
    return out


def _actor_id(activity: Any) -> str:
    """The trigger actor's user UUID (lowercased) from ``activity.actor``,
    which CE serializes as either a bare UUID string or a user dict."""
    if not isinstance(activity, dict):
        return ""
    actor = activity.get("actor")
    if isinstance(actor, dict):
        return str(actor.get("id") or "").lower()
    if actor is None:
        return ""
    return str(actor).lower()


def _actor_name(activity: Any) -> str:
    """The trigger actor's display name when the payload carries one."""
    if not isinstance(activity, dict):
        return ""
    actor = activity.get("actor")
    if isinstance(actor, dict):
        return str(actor.get("display_name") or actor.get("first_name") or "")
    return ""


def _project_id(data: dict[str, Any]) -> str:
    """The scalar project FK — CE serializers disagree on ``project`` vs
    ``project_id`` and on expansion, so accept both shapes."""
    return _ref_id(data.get("project_id") or data.get("project"))


def _page_project_id(data: dict[str, Any]) -> str:
    """Project ref for a page payload.

    Pages are workspace-scoped with a ``projects`` M2M in current
    Plane, so accept the scalar FK shapes AND the first entry of a
    list; ``""`` when nothing resolves — the page path then fails
    closed (index callback fires, routing does not).
    """
    ref = data.get("project_id") or data.get("project")
    if not ref:
        candidates = data.get("projects") or data.get("project_ids")
        if isinstance(candidates, list) and candidates:
            ref = candidates[0]
    return _ref_id(ref)


def _build_base_notification(
    body: dict[str, Any],
    *,
    project_identifier: str = "",
    base_url: str = "",
    registry: HandleRegistry | None = None,
) -> InboundNotification | None:
    """Assemble the unrouted base notification for one content event.

    ``project_identifier`` comes from the transport's id→identifier
    cache (``""`` when unresolvable — the module-level fallback has no
    cache); ``base_url`` enables shareable-URL construction; ``registry``
    resolves mention labels during body flattening.  Metadata contract:
    every routing key except ``plane_user_id`` / ``routed_via``, which only
    routed copies carry.
    """
    event = str(body.get("event") or "")
    action = str(body.get("action") or "")
    if event not in _CONTENT_EVENTS:
        return None
    data = body.get("data")
    if not isinstance(data, dict):
        return None
    activity = body.get("activity") or {}

    event_type = f"{event}.{action}" if action else event
    actor_id = _actor_id(activity)
    actor_name = _actor_name(activity)
    workspace = str(body.get("workspace_slug") or "")
    timestamp = str(data.get("updated_at") or data.get("created_at") or "")
    project_id = _page_project_id(data) if event == "page" else _project_id(data)

    metadata: dict[str, str] = {
        "actor_account_id": actor_id,
        "actor_name": actor_name,
        "event_type": event_type,
        "project": project_identifier,
        "project_id": project_id,
        "workspace": workspace,
        "timestamp": timestamp,
    }

    name = str(data.get("name") or "")
    body_text = ""
    entity_url = ""

    if event == "page":
        page_id = _ref_id(data.get("id"))
        metadata["page_id"] = page_id
        if base_url and workspace and project_id and page_id:
            entity_url = f"{base_url}/{workspace}/projects/{project_id}/pages/{page_id}"
    else:
        if event == "issue":
            issue_id = _ref_id(data.get("id"))
        else:
            # issue_comment / intake_issue: ``data.id`` is the comment /
            # intake ROW — the work item lives under ``data.issue``.  No
            # ``data.id`` fallback: a comment/intake-row id in
            # ``issue_id`` produces a URL that 404s and a doomed
            # subscriber call, strictly worse than an absent pointer.
            issue_id = _ref_id(data.get("issue"))
        if issue_id:
            metadata["issue_id"] = issue_id
            if base_url and workspace and project_id:
                entity_url = (
                    f"{base_url}/{workspace}/projects/{project_id}/issues/{issue_id}"
                )
        if event == "issue_comment":
            metadata["comment_id"] = _ref_id(data.get("id"))
            comment_html = str(data.get("comment_html") or "")
            body_text = _strip_html(comment_html, registry)
            mention_ids = extract_mention_ids(comment_html)
            if mention_ids:
                metadata["mention_ids"] = ",".join(mention_ids)
        elif event == "issue" and action == "created":
            body_text = _strip_html(str(data.get("description_html") or ""), registry)
        if event in ("issue", "issue_comment"):
            # ``ENG-42`` — display-only (subject line, prompts).  The
            # conversation key stays the issue UUID: ``sequence_id``
            # only rides the issue payload and the identifier needs a
            # warm cache, so keying coalescing on this would split one
            # work item into two partitions.
            sequence_id = data.get("sequence_id")
            metadata["work_item_key"] = (
                f"{project_identifier}-{sequence_id}"
                if project_identifier and sequence_id not in (None, "")
                else ""
            )

    if entity_url:
        metadata["url"] = entity_url

    work_item_key = metadata.get("work_item_key", "")
    if name and work_item_key:
        subject = f"[{work_item_key}] {name}"
    elif name:
        subject = f"[{project_identifier or workspace}] {name}"
    else:
        subject = f"Plane {event_type}"

    logger.debug(
        "plane_webhook_parsed",
        event_type=event_type,
        project=project_identifier,
        project_id=project_id,
        actor=actor_id,
    )

    return InboundNotification(
        source="plane",
        source_event_type=event_type,
        sender=actor_name or actor_id,
        subject=subject,
        body=body_text,
        metadata=metadata,
    )


def parse_plane_webhook(body: dict[str, Any]) -> InboundNotification | None:
    """Base parse of one CE v1 payload into a single (unrouted) notification.

    The confluence-style no-transport fallback —
    ``NotificationService._parse_plane`` uses it when no
    ``PlaneTransport`` is configured.  No fan-out, no dedupe, no
    enrichment: the project id→identifier cache and shareable URLs need
    the transport, so ``metadata.project`` / ``url`` stay empty here.
    Non-content events (``project``, ``cycle``, ``module``, ``user``, …)
    return ``None`` — they never carry a routable recipient.
    """
    if not isinstance(body, dict):
        return None
    notification = _build_base_notification(body)
    if notification is None:
        logger.debug(
            "plane_webhook_not_parsed", webhook_event=str(body.get("event") or "")
        )
    return notification


class PlaneTransport:
    """Plane transport with an engine read client and webhook routing.

    Outbound Plane operations are handled by MCP tools — ``send()`` is
    a no-op that returns False.  ``handle_webhook()`` performs dedup,
    parsing, and the two-layer routing; the transport owns the
    enrichment client (subscriber lookups + project list), the project
    id→identifier cache, the tool-skills index callback, the excluded
    skills project, and the project lead map.
    """

    name: str = "plane"

    def __init__(self, config: PlaneConfig) -> None:
        self._plane_url = config.url.rstrip("/")
        self._workspace = config.workspace
        self._token = config.token
        self._client: PlaneClient | None = None
        self._handle_registry: HandleRegistry | None = None
        self._processed_events: dict[str, float] = {}
        # Covers queue redelivery + operator replay — NOT Plane's own
        # webhook retries: CE's backoff starts at ~600 s (past this
        # TTL), and retries only fire when the API layer failed to
        # return 200, i.e. when the process holding this in-memory map
        # was down anyway.  Same constant as the Jira / Confluence
        # transports.
        self._dedup_ttl = 300.0  # 5 minutes
        self._project_leads: dict[str, str] = {}
        self._notification_excluded_projects: set[str] = set()
        self._index_callback: IndexCallback | None = None
        # Project id→identifier cache (identifier kept as authored;
        # lookups uppercase for the lead map / excluded set).  Seeded
        # opportunistically from ``project`` webhook payloads, refreshed
        # via ``list_projects()`` on a miss (client + refetch floor).
        # ``-inf`` ⇒ the first miss always refetches, regardless of
        # where ``time.monotonic()``'s epoch sits.
        self._project_names: dict[str, str] = {}
        self._project_cache_fetched_at: float = float("-inf")

    # ── engine wiring ──

    def set_handle_registry(self, registry: HandleRegistry) -> None:
        """Set the handle registry for mention / self-ignore resolution."""
        self._handle_registry = registry

    def set_project_leads(self, mapping: dict[str, str]) -> None:
        """Set the project identifier → unit lead handle mapping.

        Used for fallback routing: unassigned new work items, intake
        triage, and page events all wake the owning unit's lead.

        Args:
            mapping: ``{PLANE_PROJECT_IDENTIFIER: lead_handle}`` (see
                :func:`crewlet.config.build_plane_project_lead_map`)
        """
        self._project_leads = {k.upper(): v for k, v in mapping.items()}
        if mapping:
            logger.info(
                "plane_project_lead_mapping_configured",
                mapping=", ".join(f"{k}→{v}" for k, v in mapping.items()),
            )

    def set_notification_excluded_projects(self, keys: list[str]) -> None:
        """Mark projects whose webhooks must not produce notifications.

        Engine-managed projects (the Tool Skills project, which feeds
        the in-memory ``PromptSkillRegistry`` via the index callback)
        have no human or agent recipient by design.  Without this set,
        page edits there would fall through to lead routing and surface
        as ``notification_undeliverable`` warnings plus spurious
        ``notification_skipped`` events.

        The index callback still fires for excluded projects — only the
        notification-routing path is short-circuited.
        """
        self._notification_excluded_projects = {k.upper() for k in keys if k}
        if self._notification_excluded_projects:
            logger.info(
                "plane_notification_excluded_projects_configured",
                projects=", ".join(sorted(self._notification_excluded_projects)),
            )

    def set_index_callback(self, callback: IndexCallback | None) -> None:
        """Wire the page → tool-skill re-index hook.

        ``callback`` runs for every ``page`` webhook BEFORE the
        excluded-projects check and the routing filters, so the Tool
        Skills registry reflects every change regardless of who
        triggered it.  Pass ``None`` to disable without tearing the
        transport down.  The
        ``PlaneSkillSyncWorker`` registers here.
        """
        self._index_callback = callback

    # ── public client seam ──
    #
    # The knowledge searcher, the Tool Skills sync worker, and the
    # promotion writer consume the transport through these accessors —
    # never through the ``_client`` / ``_plane_url`` / ``_workspace``
    # privates (the getattr-duck-typing wart the Confluence searcher
    # carries is deliberately not reproduced here).

    def new_user_client(self, token: str) -> PlaneClient:
        """Build a per-agent read client for ``token``.

        Same instance URL and workspace as the transport, the agent's
        own API key — Plane then enforces project membership and page
        access natively.  The CALLER owns the client and must close it.
        """
        from crewlet.plane.client import PlaneClient

        return PlaneClient(self._plane_url, token, self._workspace)

    @property
    def engine_client(self) -> PlaneClient | None:
        """The shared engine read client; ``None`` without an engine token."""
        return self._client

    @property
    def workspace(self) -> str:
        """The workspace slug every resource path is scoped under."""
        return self._workspace

    @property
    def base_url(self) -> str:
        """The Plane instance URL (no trailing slash, no ``/api/v1``)."""
        return self._plane_url

    # ── lifecycle ──

    async def start(self) -> None:
        """Start the transport; create the read client when a token exists."""
        if self._token:
            from crewlet.plane.client import PlaneClient

            self._client = PlaneClient(self._plane_url, self._token, self._workspace)
        else:
            # Routing quality degrades without the engine credential:
            # no subscriber fan-out, and the project id→identifier cache
            # can only learn from ``project`` webhook payloads (after a
            # restart, lead-fallback and ``ENG-42`` keys stay empty
            # until the next project event).
            logger.warning("plane_engine_token_missing", url=self._plane_url)
        logger.info(
            "plane_transport_started",
            url=self._plane_url,
            workspace=self._workspace,
            enrichment="client" if self._client is not None else "degraded",
        )

    async def stop(self) -> None:
        """Stop and clean up resources."""
        self._processed_events.clear()
        self._project_names.clear()
        self._project_cache_fetched_at = float("-inf")
        if self._client is not None:
            await self._client.close()
            self._client = None
        logger.info("plane_transport_stopped")

    async def send(self, message: OutboundMessage) -> bool:
        """Not supported — outbound Plane operations use MCP tools."""
        logger.warning("plane_outbound_not_supported")
        return False

    # ── dedup ──

    def _dedup_event(self, body: dict[str, Any]) -> bool:
        """True when this delivery was already processed (skip it).

        CE payloads carry no stable event id (``X-Plane-Delivery`` is
        per-attempt), so the key is the event coordinates PLUS the
        activity discriminator: CE fires one webhook per changed field
        with an identical ``data`` snapshot, so a bulk edit is N
        deliveries differing only in ``activity`` — without the
        discriminator the one directed ``assignees`` route could be
        dropped as a duplicate of a ``state`` change.
        """
        data = body.get("data")
        if not isinstance(data, dict):
            data = {}
        activity = body.get("activity")
        if not isinstance(activity, dict):
            activity = {}
        dedup_key = ":".join(
            (
                str(body.get("event") or ""),
                str(body.get("action") or ""),
                str(data.get("id") or ""),
                str(data.get("updated_at") or ""),
                str(activity.get("field") or ""),
                str(activity.get("new_identifier") or ""),
                str(activity.get("old_identifier") or ""),
                str(activity.get("new_value") or "")[:64],
            )
        )

        now = time.monotonic()

        # Prune old entries
        stale = [
            k for k, t in self._processed_events.items() if now - t > self._dedup_ttl
        ]
        for k in stale:
            del self._processed_events[k]

        if dedup_key in self._processed_events:
            logger.debug("duplicate_plane_event_skipped", dedup_key=dedup_key)
            return True

        self._processed_events[dedup_key] = now
        return False

    # ── project id→identifier cache ──

    def _seed_project_cache(self, data: dict[str, Any], action: str) -> None:
        """Upsert / evict the id→identifier cache from a ``project`` payload."""
        project_id = _ref_id(data.get("id"))
        if not project_id:
            return
        if action == "deleted":
            self._project_names.pop(project_id, None)
            return
        identifier = str(data.get("identifier") or "")
        if identifier:
            self._project_names[project_id] = identifier

    @staticmethod
    def _project_rows_to_map(projects: list[dict[str, Any]]) -> dict[str, str]:
        """Extract an id→identifier mapping from ``list_projects()`` rows."""
        out: dict[str, str] = {}
        for project in projects:
            if not isinstance(project, dict):
                continue
            pid = _ref_id(project.get("id"))
            identifier = str(project.get("identifier") or "")
            if pid and identifier:
                out[pid] = identifier
        return out

    async def _refresh_project_cache(self) -> bool:
        """One engine-client ``list_projects()`` refresh, merge-only.

        Runs only when the engine client exists and the refetch floor
        (``_PROJECT_CACHE_REFETCH_INTERVAL``) has passed.  The floor is
        burned only on a SUCCESSFUL fetch: a successful-but-unresolvable
        lookup cannot hammer the API under a webhook burst, while a
        FAILED request (Plane still booting in the compose stack, a
        transient network error) stays retryable immediately — burning
        the floor on failure turned the ordinary boot race into 60 s of
        guaranteed-empty resolutions.  A failed request RAISES (after a
        ``plane_project_list_failed`` warning) so callers can tell
        "the refresh itself failed" apart from "identifier genuinely
        absent"; best-effort callers catch it (see
        :meth:`_project_identifier`).  The refresh MERGES into the
        cache and never evicts: a transiently short server response
        must not forget identifiers learned from webhook payloads.
        Returns ``True`` only when a refresh actually ran and succeeded.
        """
        if self._client is None:
            return False
        now = time.monotonic()
        if now - self._project_cache_fetched_at < _PROJECT_CACHE_REFETCH_INTERVAL:
            return False
        try:
            projects = await self._client.list_projects()
        except Exception as exc:
            logger.warning("plane_project_list_failed", error=str(exc))
            raise
        self._project_cache_fetched_at = now
        self._project_names.update(self._project_rows_to_map(projects))
        return True

    async def _project_identifier(self, project_id: str) -> str:
        """Resolve a project UUID to its identifier (e.g. ``ENG``).

        Cache hit → no REST.  On a miss, one full ``list_projects()``
        refresh runs via :meth:`_refresh_project_cache` (engine client
        + refetch floor).  Still missing → ``""`` (logged once per
        refresh, not per event); the consequences are deliberate: no
        lead fallback (never a misroute), empty ``metadata.project`` /
        ``work_item_key``, subject falls back to the workspace slug.
        """
        if not project_id:
            return ""
        cached = self._project_names.get(project_id, "")
        if cached:
            return cached
        try:
            refreshed = await self._refresh_project_cache()
        except Exception:
            # Routing is best-effort: a failed refresh degrades to the
            # unresolved consequences below (already logged by the
            # refresh) and stays retryable on the next event.
            return ""
        if not refreshed:
            return ""
        resolved = self._project_names.get(project_id, "")
        if not resolved:
            logger.warning("plane_project_unresolved", project_id=project_id)
        return resolved

    def cached_project_identifier(self, project_id: str) -> str:
        """Cache-only project UUID → identifier read (``""`` on a miss).

        For hit rendering on the search path: no REST call, no floor
        burn.  The searcher's earlier :meth:`resolve_project_ids` call
        warms the cache for every scoped project; an unscoped hit whose
        project the cache has never seen degrades to an empty container
        label rather than costing a lookup per row.
        """
        return self._project_names.get((project_id or "").lower(), "")

    @staticmethod
    def _cached_ids_for(wanted: set[str], names: dict[str, str]) -> dict[str, str]:
        """Reverse-resolve ``wanted`` (UPPERCASED identifiers) against an
        id→identifier mapping; unresolved identifiers are absent."""
        out: dict[str, str] = {}
        for pid, identifier in names.items():
            key = identifier.strip().upper()
            if key in wanted and key not in out:
                out[key] = pid
        return out

    async def resolve_project_ids(
        self,
        identifiers: Sequence[str],
        *,
        client: PlaneClient | None = None,
    ) -> dict[str, str]:
        """Resolve project identifiers (e.g. ``ENG``) to project UUIDs.

        The one identifier→UUID primitive every page consumer (the
        knowledge searcher, the Tool Skills sync worker, the promotion
        writer) shares.  Case-insensitive: inputs are matched against
        the cached authored identifiers case-folded, and the returned
        mapping is keyed by the UPPERCASED identifier.  Backed by the
        shared id→identifier cache — a full cache hit costs no REST —
        with one engine-client refresh behind the shared refetch floor
        on a miss (merge-only, see :meth:`_refresh_project_cache`).

        Without an engine token the optional per-agent ``client``
        performs the refetch instead, and its result is NOT written to
        the shared cache: ``list_projects`` is membership-scoped, so
        one agent's partial view must never poison another agent's
        resolution (the fallback fetch is per-call and unfloored — it
        writes nothing shared, and the caller's own request already
        bounds it).

        Two distinct outcomes callers MUST tell apart:

        * **Identifier absent** — the refresh ran (or the floor made a
          recent successful one authoritative) and the identifier is
          genuinely not in the workspace: the identifier is simply
          absent from the returned mapping.  A configuration problem.
        * **Refresh failed** — the ``list_projects()`` request itself
          errored (either client): the call RAISES instead of returning
          an empty mapping, because "Plane was unreachable" must never
          masquerade as "the project does not exist" (the Tool Skills
          boot walk once wiped its registry over exactly that
          conflation).  Best-effort consumers catch and degrade; the
          sync worker treats it as a retryable walk failure.
        """
        wanted = {i.strip().upper() for i in identifiers if i and i.strip()}
        if not wanted:
            return {}
        resolved = self._cached_ids_for(wanted, self._project_names)
        if len(resolved) == len(wanted):
            return resolved
        if self._client is not None:
            await self._refresh_project_cache()
            return self._cached_ids_for(wanted, self._project_names)
        if client is None:
            return resolved
        try:
            rows = await client.list_projects()
        except Exception as exc:
            logger.warning("plane_project_list_failed", error=str(exc))
            raise
        merged = dict(self._project_names)
        merged.update(self._project_rows_to_map(rows))
        return self._cached_ids_for(wanted, merged)

    # ── webhook entry point ──

    async def handle_webhook(
        self,
        body: dict[str, Any],
        headers: dict[str, str] | None = None,
        body_raw: str | bytes = b"",
    ) -> list[InboundNotification]:
        """Process one incoming Plane webhook payload.

        One notification per recipient — first (highest-signal) reason
        wins, deduped by target UUID, the trigger actor excluded from
        every fan-out.  Directed copies carry the target under
        ``metadata.plane_user_id``; lead copies carry
        ``recipient_handle``.  When no recipient survives the filters
        the list is empty — Plane copies always name an explicit
        target, so nothing falls through to standard resolution.
        """
        if not isinstance(body, dict):
            return []

        event = str(body.get("event") or "")
        action = str(body.get("action") or "")
        data = body.get("data")
        if not isinstance(data, dict):
            data = {}

        # Deduplicate first — the API layer already verified the
        # signature, so nothing precedes this.
        if self._dedup_event(body):
            return []

        # Opportunistic cache seeding: an expanded project FK carries
        # id + identifier — free resolution, no REST call.
        project_ref = data.get("project")
        if isinstance(project_ref, dict):
            pid = _ref_id(project_ref.get("id"))
            identifier = str(project_ref.get("identifier") or "")
            if pid and identifier:
                self._project_names[pid] = identifier

        if event == "project":
            # No agent routing — the payload feeds the id→identifier
            # cache (create/update upsert, delete evict).
            self._seed_project_cache(data, action)
            return []

        if event not in _CONTENT_EVENTS:
            # cycle / module / cycle_issue / module_issue stay off on
            # the provisioned webhook, but CE may still deliver ``user``
            # events — drop at debug so operators can see the volume.
            # A membership-change event worth reacting to belongs on
            # the identity-registration path, not this routing table.
            logger.debug(
                "plane_webhook_ignored_event", webhook_event=event, action=action
            )
            return []

        if event == "page":
            return await self._route_page(body, data, action)
        if event == "issue":
            return await self._route_issue(body, data, action)
        if event == "issue_comment":
            return await self._route_comment(body, data, action)
        return await self._route_intake(body, data)

    # ── per-event routing ──

    async def _route_issue(
        self, body: dict[str, Any], data: dict[str, Any], action: str
    ) -> list[InboundNotification]:
        activity = body.get("activity")
        if not isinstance(activity, dict):
            activity = {}
        actor_id = _actor_id(activity)
        project_id = _project_id(data)
        identifier = await self._project_identifier(project_id)
        base = self._base_notification(body, identifier)
        if base is None:
            return []
        issue_id = base.metadata.get("issue_id", "")

        if action == "created":
            assignees = _ids(data.get("assignees"))
            if not assignees:
                # The "new ticket wakes the lead" flow.
                return self._lead_copies(
                    base, identifier, actor_id, "project_lead_fallback"
                )
            copies = self._directed_copies(
                base, [(uid, "assignee") for uid in assignees], actor_id
            )
            if not copies and any(uid and uid != actor_id for uid in assignees):
                # Every non-actor assignee was an unregistered outsider
                # (the registry intersection in ``_directed_copies``
                # dropped them all): the ticket was handed entirely to
                # humans outside the org chart, so it still wakes the
                # owning lead — a new ticket must never vanish.  A
                # purely self-assigned create stays silent: the actor
                # claimed the work.
                return self._lead_copies(
                    base, identifier, actor_id, "project_lead_fallback"
                )
            return copies
        if action == "updated":
            if str(activity.get("field") or "") == "assignees":
                # Directed: the newly ADDED assignee.  Removal events
                # (``new_identifier`` empty) wake nobody.
                new_id = str(activity.get("new_identifier") or "").lower()
                if not new_id:
                    return []
                targets = [(new_id, "assignee_added")]
            else:
                targets = await self._thread_targets(project_id, issue_id, data)
        elif action == "deleted":
            # The subscribers endpoint cannot serve a deleted work item
            # — route straight to the payload assignees, no REST call.
            targets = [(uid, "assignee") for uid in _ids(data.get("assignees"))]
        else:
            logger.debug(
                "plane_webhook_ignored_event", webhook_event="issue", action=action
            )
            return []

        return self._directed_copies(base, targets, actor_id)

    async def _route_comment(
        self, body: dict[str, Any], data: dict[str, Any], action: str
    ) -> list[InboundNotification]:
        if action == "deleted":
            # No content to act on — Plane's comment-delete payload
            # carries no body, so routing it is pure noise.
            return []
        if action not in ("created", "updated"):
            logger.debug(
                "plane_webhook_ignored_event",
                webhook_event="issue_comment",
                action=action,
            )
            return []
        activity = body.get("activity")
        if not isinstance(activity, dict):
            activity = {}
        actor_id = _actor_id(activity)
        project_id = _project_id(data)
        identifier = await self._project_identifier(project_id)
        base = self._base_notification(body, identifier)
        if base is None:
            return []
        issue_id = base.metadata.get("issue_id", "")

        # 1. @-mentions — a directed ask.  The routing contract intersects
        # mentions with the registered Plane UUIDs unconditionally: with
        # no registry the known set is empty, so no mention routes (fail
        # closed); with one, the intersection happens at the single
        # choke point in ``_directed_copies``.
        mention_ids = (
            extract_mention_ids(str(data.get("comment_html") or ""))
            if self._handle_registry is not None
            else []
        )
        targets: list[tuple[str, str]] = [(m, "mention") for m in mention_ids]

        # 2. Thread activity — subscribers, degrading to assignees.
        targets.extend(await self._thread_targets(project_id, issue_id, data))
        return self._directed_copies(base, targets, actor_id)

    async def _route_intake(
        self, body: dict[str, Any], data: dict[str, Any]
    ) -> list[InboundNotification]:
        # Intake is Plane's unassigned-inbound surface — triage falls
        # to the project lead for any action.
        activity = body.get("activity")
        if not isinstance(activity, dict):
            activity = {}
        actor_id = _actor_id(activity)
        identifier = await self._project_identifier(_project_id(data))
        base = self._base_notification(body, identifier)
        if base is None:
            return []
        return self._lead_copies(base, identifier, actor_id, "intake_triage")

    async def _route_page(
        self, body: dict[str, Any], data: dict[str, Any], action: str
    ) -> list[InboundNotification]:
        event_type = f"page.{action}" if action else "page"
        page_id = _ref_id(data.get("id"))

        # 1. Index callback ALWAYS first — the Tool Skills sync cares
        # about every page change, including self-edits and excluded
        # projects; indexing must never break notification routing.
        if self._index_callback is not None and page_id:
            try:
                await self._index_callback(event_type, page_id)
            except Exception:
                logger.exception(
                    "plane_index_callback_failed",
                    event_type=event_type,
                    page_id=page_id,
                )

        # 2. Fail closed on an unresolvable project: the fork's read-only
        # PageSerializer carries no scalar project FK today, and
        # routing a page whose project is unknown would recreate the
        # undeliverable-notification storm the exclusion set exists to
        # prevent.
        project_id = _page_project_id(data)
        if not project_id:
            logger.warning(
                "plane_page_project_unresolved",
                page_id=page_id,
                event_type=event_type,
            )
            return []
        identifier = await self._project_identifier(project_id)

        # 3. Excluded projects (the Tool Skills project): the callback
        # above already applied the change — only routing stops here.
        if identifier and identifier.upper() in self._notification_excluded_projects:
            logger.debug(
                "plane_notification_skipped_excluded_project",
                project=identifier,
                page_id=page_id,
                event_type=event_type,
            )
            return []

        # 4. Project lead only.  CE pages have no subscription model and
        # no comments — the accepted degradation: page events wake
        # the owning unit's lead, and page discussion happens on work
        # items where the full routing applies.
        activity = body.get("activity")
        if not isinstance(activity, dict):
            activity = {}
        actor_id = _actor_id(activity)
        base = self._base_notification(body, identifier)
        if base is None:
            return []
        return self._lead_copies(base, identifier, actor_id, "page_project_lead")

    # ── routing helpers ──

    def _base_notification(
        self, body: dict[str, Any], project_identifier: str
    ) -> InboundNotification | None:
        return _build_base_notification(
            body,
            project_identifier=project_identifier,
            base_url=self._plane_url,
            registry=self._handle_registry,
        )

    async def _thread_targets(
        self, project_id: str, issue_id: str, data: dict[str, Any]
    ) -> list[tuple[str, str]]:
        """Subscriber fan-out targets, degrading to payload assignees.

        Degrades when the client is missing (no engine token), the
        lookup raises, the response is empty, or a non-empty response
        yields no extractable user ids — an unrecognized row shape must
        not suppress the assignee fallback.
        """
        subscriber_ids: list[str] = []
        if self._client is not None and project_id and issue_id:
            try:
                rows = await self._client.list_issue_subscribers(project_id, issue_id)
            except Exception as exc:
                logger.warning(
                    "plane_subscriber_fetch_failed",
                    project_id=project_id,
                    issue_id=issue_id,
                    error=str(exc),
                )
                rows = []
            subscriber_ids = _subscriber_ids(rows)
        if subscriber_ids:
            return [(uid, "subscriber") for uid in subscriber_ids]
        return [(uid, "assignee") for uid in _ids(data.get("assignees"))]

    def _directed_copies(
        self,
        base: InboundNotification,
        targets: list[tuple[str, str]],
        actor_id: str,
    ) -> list[InboundNotification]:
        """One copy per target UUID — first reason wins, actor excluded,
        unregistered outsiders dropped.

        The single choke point where every directed fan-out (assignee,
        assignee_added, subscriber, mention) is intersected with the
        parties the engine can route to (``known_external_ids("plane")``,
        agents ∪ registered human seats): an ordinary workspace human
        must never receive a copy the service cannot resolve — each one
        would surface as a ``notification_undeliverable`` warning plus a
        spurious ``notification_skipped`` event.  Without a registry the
        pass-through is deliberate: engine wiring always sets one, and
        the degraded bare transport must not go silent.

        Directed copies set ``metadata.plane_user_id`` (and never
        ``recipient_handle``); the service resolves the UUID through
        its external-ID candidates.
        """
        known = (
            self._handle_registry.known_external_ids("plane")
            if self._handle_registry is not None
            else None
        )
        results: list[InboundNotification] = []
        delivered: set[str] = set()
        dropped = 0
        for uid, routed_via in targets:
            if not uid or uid == actor_id or uid in delivered:
                continue
            delivered.add(uid)
            if known is not None and uid not in known:
                dropped += 1
                continue
            n = base.model_copy(deep=True)
            n.metadata["plane_user_id"] = uid
            n.metadata["routed_via"] = routed_via
            results.append(n)
        if dropped:
            logger.debug(
                "plane_unregistered_targets_dropped",
                dropped=dropped,
                event_type=base.metadata.get("event_type", ""),
            )
        if not results:
            # INFO, not debug: a verified event whose every target fell
            # away is the diagnosable end of the delivery chain ("webhook
            # arrived, nobody woke up") — at debug an operator sees
            # ``raw_webhook_received`` and then silence.  Low volume: it
            # fires once per zero-recipient event, and in an agent
            # workspace those are the anomaly.  ``unregistered_dropped``
            # says whether targets existed but were unknown to the
            # registry (boot-time identity registration is then the place
            # to look — ``plane_account_mapped`` lists what registered).
            logger.info(
                "plane_webhook_no_recipients",
                event_type=base.metadata.get("event_type", ""),
                unregistered_dropped=dropped,
            )
        return results

    def _lead_copies(
        self,
        base: InboundNotification,
        identifier: str,
        actor_id: str,
        routed_via: str,
    ) -> list[InboundNotification]:
        """The project-lead fallback copy (unassigned create / intake /
        page events).

        An unresolvable identifier or unmapped project produces NO
        notification — never a misroute.  Lead copies set
        ``recipient_handle`` (and never ``plane_user_id``).
        """
        lead = self._project_leads.get(identifier.upper()) if identifier else ""
        if not lead:
            logger.debug(
                "plane_webhook_no_recipients",
                event_type=base.metadata.get("event_type", ""),
                project=identifier,
            )
            return []
        if lead == self._actor_handle(actor_id):
            # The lead's own action must never re-trigger the lead —
            # the endless self-notification loop the Confluence
            # space-lead exclusion exists to prevent.
            logger.debug(
                "plane_lead_excluded_as_trigger",
                lead_handle=lead,
                event_type=base.metadata.get("event_type", ""),
            )
            return []
        n = base.model_copy(deep=True)
        n.recipient_handle = lead
        n.metadata["routed_via"] = routed_via
        logger.info(
            "plane_routing_to_project_lead",
            lead_handle=lead,
            project=identifier,
            routed_via=routed_via,
        )
        return [n]

    def _actor_handle(self, actor_id: str) -> str:
        """Resolve the trigger actor's UUID to a party handle (``""``
        when unknown or no registry)."""
        if not actor_id or self._handle_registry is None:
            return ""
        try:
            party = self._handle_registry.resolve_party_external("plane", actor_id)
        except Exception:
            return ""
        return party.handle if party is not None else ""
