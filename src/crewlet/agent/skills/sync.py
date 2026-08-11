"""ToolSkillSyncWorker — Confluence → PromptSkillRegistry.

Consumes the authenticated httpx client from
:class:`ConfluenceTransport` and walks the Tool Skills space into the
in-memory :class:`~crewlet.agent.skills.PromptSkillRegistry`.

Two entry points:

- :meth:`run_initial_sync` — boot-time full populate. Walks every page
  in the configured Tool Skills space, decodes, populates the registry.
- :meth:`handle_page_event` — webhook-driven incremental update.
  Routed from the engine's existing
  :func:`ConfluenceTransport.set_index_callback` dispatch.

Malformed / over-cap pages are logged and skipped during the boot walk
(they simply don't get seeded) and EVICTED on the webhook path (a page
edited into a non-skill must not keep serving its last-good body) —
they never abort the walk or raise out of the webhook handler.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any
from urllib.parse import quote

from crewlet._logging import get_logger
from crewlet.agent.skills.confluence_codec import CodecDecodeError, decode_page
from crewlet.agent.skills.labels import SKILL_MARKER_LABEL
from crewlet.agent.skills.parser import SkillParseError, build_skill

if TYPE_CHECKING:
    from crewlet.agent.skills.registry import PromptSkillRegistry
    from crewlet.notifications.transports.confluence import ConfluenceTransport

logger = get_logger("agent.skills.sync")


class ToolSkillSyncWorker:
    """Populate a :class:`PromptSkillRegistry` from a Confluence space."""

    def __init__(
        self,
        *,
        transport: ConfluenceTransport,
        registry: PromptSkillRegistry,
        space_key: str,
        page_size: int = 50,
    ) -> None:
        self._transport = transport
        self._registry = registry
        # Normalised once, like every other space-key comparison in the
        # tree (build_cql, accessible_spaces, build_space_key_lead_map,
        # the notification exclusion set): Confluence stores space keys
        # uppercased, and the CQL below resolves the key server-side
        # case-insensitively — so a lowercased CREWLET_TOOL_SKILLS_SPACE
        # boots fine, and the webhook admission check must not then
        # evict every skill over the casing mismatch.
        self._space_key = (space_key or "").strip().upper()
        self._page_size = page_size

    @property
    def space_key(self) -> str:
        return self._space_key

    @property
    def _http_client(self) -> Any | None:
        return getattr(self._transport, "_http_client", None)

    @property
    def _rest_base(self) -> str:
        return getattr(self._transport, "_rest_base", "") or ""

    # ------------------------------------------------------------------
    # Boot-time full populate
    # ------------------------------------------------------------------

    async def run_initial_sync(self) -> int | None:
        """Walk the Tool Skills space; populate the registry.

        Returns the number of skills successfully loaded, or ``None``
        when the walk FAILED before completing — the registry is then
        left untouched (never seeded from partial rows) and the
        engine's ``_kick_tool_skill_resync`` retry loop can tell the
        failure apart from a legitimately empty space (``0``).  The
        worker never raises during boot.  A successful walk first
        builds a fresh-state list, then atomically replaces the
        registry contents (so dropped pages disappear).
        """
        if not self._space_key:
            logger.info("tool_skill_sync_no_space_configured")
            return 0
        client = self._http_client
        rest_base = self._rest_base
        if client is None or not rest_base:
            logger.warning(
                "tool_skill_sync_no_http_client",
                space_key=self._space_key,
            )
            return None

        # CQL filter: scope to the configured space and require the
        # shared ``crewlet-skill`` marker label.  Without the label
        # filter the walk would also pick up the space's auto-generated
        # home page and any non-skill content an operator happens to
        # park in the space — both fail to decode and produce noisy
        # ``tool_skill_sync_decode_failed`` logs on every boot.  The
        # import flow stamps every skill page with this marker label
        # (see :data:`SKILL_MARKER_LABEL`); the symmetry between
        # import-side stamp and sync-side filter is intentional.
        cql = f'space = "{self._space_key}" AND label = "{SKILL_MARKER_LABEL}"'
        encoded_cql = quote(cql, safe="")

        loaded: list = []
        start = 0
        while True:
            url = (
                f"{rest_base}/rest/api/content/search"
                f"?cql={encoded_cql}"
                f"&start={start}"
                f"&limit={self._page_size}"
                f"&expand=body.storage,version"
            )
            try:
                resp = await client.get(url)
            except Exception:
                # Incomplete walk — the registry keeps its current
                # contents (a wholesale seed from partial rows would
                # silently delete skills the walk didn't reach).
                logger.exception(
                    "tool_skill_sync_list_failed",
                    space_key=self._space_key,
                    start=start,
                )
                return None
            if resp.status_code != 200:
                logger.warning(
                    "tool_skill_sync_list_non_200",
                    space_key=self._space_key,
                    status=resp.status_code,
                    body_preview=(resp.text or "")[:200],
                )
                return None
            payload = resp.json()
            results = payload.get("results", [])
            if not results:
                break

            # ``expand=body.storage`` means the bodies arrived with the
            # search response — decoding is pure CPU, so a plain loop
            # (no concurrency machinery) is all there is to do.
            for page in results:
                skill = self._page_to_skill(page)
                if skill is not None:
                    loaded.append(skill)
            if len(results) < self._page_size:
                break
            start += self._page_size

        # Atomic replace: build the new state, then swap. Webhook
        # handlers that fire during the walk will simply land on the
        # new registry once it's populated.
        self._registry.seed(loaded)
        logger.info(
            "tool_skill_sync_initial_complete",
            space_key=self._space_key,
            loaded=len(loaded),
        )
        return len(loaded)

    # ------------------------------------------------------------------
    # Webhook-driven incremental
    # ------------------------------------------------------------------

    async def handle_page_event(
        self,
        *,
        page_id: str,
        event_kind: str,
    ) -> None:
        """Apply a single Confluence webhook event to the registry.

        ``event_kind`` is the engine's webhook-dispatcher tag (e.g.
        ``page_created`` / ``page_updated`` / ``page_trashed`` /
        ``page_removed``). Page-delete events evict by page id;
        otherwise re-fetch the page and apply the same admission
        predicate the boot walk's CQL encodes — configured space AND
        the ``crewlet-skill`` marker label AND a decodable skill body.
        A fetched page that fails the predicate is EVICTED, not
        skipped: a page moved out of the space, stripped of its
        marker, or edited into a non-skill must not keep serving its
        last-good body until restart.
        """
        if event_kind in {"page_trashed", "page_removed", "page_deleted"}:
            await self._registry.evict_by_page_id(page_id)
            return

        client = self._http_client
        rest_base = self._rest_base
        if client is None or not rest_base:
            logger.warning(
                "tool_skill_sync_no_http_client_for_webhook",
                page_id=page_id,
            )
            return

        url = (
            f"{rest_base}/rest/api/content/{page_id}"
            f"?expand=body.storage,version,space,metadata.labels"
        )
        try:
            resp = await client.get(url)
        except Exception:
            logger.exception("tool_skill_sync_fetch_failed", page_id=page_id)
            return
        if resp.status_code == 404:
            # Page was already deleted by the time we fetched.
            await self._registry.evict_by_page_id(page_id)
            return
        if resp.status_code != 200:
            logger.warning(
                "tool_skill_sync_fetch_non_200",
                page_id=page_id,
                status=resp.status_code,
            )
            return
        page = resp.json()
        if not self._admit_page(page):
            # Admission predicate failed — the page is no longer (or
            # never was) a Tool Skills page.  Harmless no-op when the
            # registry never held it.
            if await self._registry.evict_by_page_id(page_id):
                logger.info("tool_skill_sync_evicted_unadmitted", page_id=page_id)
            return
        skill = self._page_to_skill(page)
        if skill is None:
            # Space + label admitted the page but its body no longer
            # decodes into a valid skill — evict the stale entry
            # rather than silently serving the last-good body.
            if await self._registry.evict_by_page_id(page_id):
                logger.info("tool_skill_sync_evicted_undecodable", page_id=page_id)
            return
        await self._registry.upsert(skill)

    def _admit_page(self, page: dict[str, Any]) -> bool:
        """The webhook-path admission predicate.

        Mirrors the boot walk's CQL filter exactly (``space = "<TS>"
        AND label = "crewlet-skill"``): the index callback fires for
        every page webhook instance-wide, so without this check any
        decodable page anywhere would be upserted — and a page loaded
        by webhook without the marker label would silently vanish on
        the next restart's CQL-filtered walk.
        """
        # Uppercase both sides — the codebase-wide space-key comparison
        # convention (build_cql, accessible_spaces, the notification
        # exclusion set).  ``self._space_key`` is normalised at
        # construction; the page payload's key is canonically stored
        # uppercased but is normalised here too so a casing quirk can
        # never masquerade as a foreign space and evict a live skill.
        space_key = str(page.get("space", {}).get("key", "") or "").strip().upper()
        if space_key != self._space_key:
            logger.debug(
                "tool_skill_sync_foreign_space",
                page_id=str(page.get("id", "")),
                space_key=space_key,
            )
            return False
        labels = page.get("metadata", {}).get("labels", {}).get("results", [])
        label_names = {
            str(label.get("name", "")) for label in labels if isinstance(label, dict)
        }
        if SKILL_MARKER_LABEL not in label_names:
            logger.debug(
                "tool_skill_sync_missing_marker_label",
                page_id=str(page.get("id", "")),
            )
            return False
        return True

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _page_to_skill(self, page: dict[str, Any]) -> Any | None:
        """Decode + validate one Confluence page payload into a PromptSkill.

        Returns ``None`` and logs at debug/info level when the page
        isn't a valid skill (missing code macro, malformed YAML,
        over-cap body, etc.) — most "non-skill" pages in the space
        should never reach this in production but a defensive skip
        keeps boot robust against typos.
        """
        page_id = str(page.get("id", ""))
        storage = page.get("body", {}).get("storage", {}).get("value", "")
        version = int(page.get("version", {}).get("number", 0))
        if not storage:
            logger.debug("tool_skill_sync_empty_body", page_id=page_id)
            return None
        try:
            frontmatter, body = decode_page(storage)
        except CodecDecodeError as exc:
            logger.info(
                "tool_skill_sync_decode_failed",
                page_id=page_id,
                error=str(exc),
            )
            return None
        try:
            return build_skill(
                frontmatter,
                body,
                source_page_id=page_id,
                source_page_version=version,
            )
        except SkillParseError as exc:
            logger.info(
                "tool_skill_sync_invalid_skill",
                page_id=page_id,
                error=str(exc),
            )
            return None


__all__ = ["ToolSkillSyncWorker"]
