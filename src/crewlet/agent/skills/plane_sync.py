"""PlaneSkillSyncWorker — Plane Tool Skills project → PromptSkillRegistry.

The Plane analog of :class:`~crewlet.agent.skills.sync.ToolSkillSyncWorker`.
Consumes the shared engine read client through the
:class:`~crewlet.notifications.transports.plane.PlaneTransport` public
client seam and keeps the in-memory
:class:`~crewlet.agent.skills.PromptSkillRegistry` in step with the
Tool Skills project.

Two entry points:

- :meth:`run_initial_sync` — boot-time full populate.  ONE strict
  ``list_pages`` enumeration of the skills project carrying
  ``description_html`` (the fork's list rows serialize the body, so no
  per-page fetch fan-out); an INCOMPLETE enumeration never seeds the registry —
  :meth:`~crewlet.plane.client.PlaneClient.list_pages` raises instead
  of returning partial rows, because ``registry.seed`` is a wholesale
  replace and a truncated walk would silently delete skills.  A failed
  walk returns ``None`` (registry untouched) so the engine's bounded
  retry can distinguish it from a legitimately empty project (``0``).
- :meth:`handle_page_event` — webhook-driven incremental update, fired
  by the engine's index-callback closure registered on
  :meth:`PlaneTransport.set_index_callback`.  The fork's page webhooks
  (including 60 s-debounced content-persist updates) carry the
  action vocabulary ``created`` / ``updated`` / ``deleted`` (a
  duplicate emits ``created``); the worker treats any non-delete as
  fetch + re-admit, so an unknown future action degrades to a
  harmless refetch.

Admission predicate (identical for both entry points): the page lives
in the skills project — enumerated for boot, enforced for webhooks by
the fork's project-scoped GET 404ing on foreign pages — AND its
``description_html`` decodes into a valid skill (leading YAML code
block with a ``trigger``).  A fetched page failing the predicate is
EVICTED, not skipped: an archived page (Plane's "remove" gesture —
delete requires archive first), a page moved to another project, or a
page edited into a non-skill must not keep serving its last-good body.
The ``external_source == "crewlet"`` marker the importer stamps does
not admit by itself (an unparseable page has no skill to admit); it
promotes the decode-failure log to ``info`` — an engine-authored page
that stops decoding is an operator-visible defect, while a stray
operator page (or the project home page) skips at ``debug``.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from crewlet._logging import get_logger
from crewlet.agent.skills.parser import SkillParseError, build_skill
from crewlet.plane.client import PlaneProvisionError
from crewlet.plane.plane_codec import CodecDecodeError, decode_page

if TYPE_CHECKING:
    from crewlet.agent.skills.models import PromptSkill
    from crewlet.agent.skills.registry import PromptSkillRegistry
    from crewlet.notifications.transports.plane import PlaneTransport
    from crewlet.plane.client import PlaneClient

logger = get_logger("agent.skills.plane_sync")

SYNC_PAGE_FIELDS = ["id", "name", "external_id", "external_source", "description_html"]
"""``fields=`` projection for the boot walk's single enumeration.

Everything :func:`page_to_skill` reads and nothing more — the body
rides along on the list rows (the fork's list uses the same serializer
as retrieve), which is what collapses the boot walk to ONE paginated
request.  Shared with ``crewlet plane resync``
(:func:`crewlet.plane.import_cli.sync_skills_registry`).
"""

_CREWLET_EXTERNAL_SOURCE = "crewlet"
"""The importer's ``external_source`` marker (see
:data:`crewlet.plane.import_cli.EXTERNAL_SOURCE`; duplicated here as a
plain string so the sync path doesn't import the CLI module)."""


def page_to_skill(page: dict[str, Any]) -> PromptSkill | None:
    """Decode one Plane page object into a :class:`PromptSkill`.

    Returns ``None`` when the page isn't a valid skill (no body, no
    leading YAML code block, no ``trigger``, over-cap body, invalid
    trigger/phase values).  Pages carrying the importer's
    ``external_source == "crewlet"`` marker log their failure at
    ``info`` (they *should* decode); anything else skips at ``debug``
    (the project home page and stray operator pages are expected
    non-skills — skill pages identify themselves).

    ``source_page_version`` is stamped ``0``: Plane has no integer
    page version (its pages API exposes ``updated_at`` only).
    """
    page_id = str(page.get("id") or "")
    crewlet_marked = str(page.get("external_source") or "") == _CREWLET_EXTERNAL_SOURCE
    description_html = str(page.get("description_html") or "")
    if not description_html:
        logger.debug("tool_skill_sync_empty_body", page_id=page_id)
        return None
    try:
        frontmatter, body = decode_page(description_html)
    except CodecDecodeError as exc:
        log = logger.info if crewlet_marked else logger.debug
        log(
            "tool_skill_sync_decode_failed",
            page_id=page_id,
            crewlet_marked=crewlet_marked,
            error=str(exc),
        )
        return None
    if "trigger" not in frontmatter:
        log = logger.info if crewlet_marked else logger.debug
        log("tool_skill_sync_no_trigger", page_id=page_id)
        return None
    try:
        return build_skill(
            frontmatter,
            body,
            source_page_id=page_id,
            source_page_version=0,
        )
    except SkillParseError as exc:
        logger.info(
            "tool_skill_sync_invalid_skill",
            page_id=page_id,
            error=str(exc),
        )
        return None


class PlaneSkillSyncWorker:
    """Populate a :class:`PromptSkillRegistry` from a Plane project."""

    def __init__(
        self,
        *,
        transport: PlaneTransport,
        registry: PromptSkillRegistry,
        project: str,
    ) -> None:
        self._transport = transport
        self._registry = registry
        self._project = project

    @property
    def project(self) -> str:
        return self._project

    # ------------------------------------------------------------------
    # Boot-time full populate
    # ------------------------------------------------------------------

    async def run_initial_sync(self) -> int | None:
        """Walk the Tool Skills project; populate the registry.

        Returns the number of skills successfully loaded, or ``None``
        when the walk FAILED and the registry was left untouched — the
        two must be distinguishable so the engine's
        ``_kick_tool_skill_resync`` retry loop knows whether the walk
        landed (a genuinely empty project seeds an empty registry and
        returns ``0``; that is success, not failure).  Never raises.

        ``None`` (walk failed, registry untouched, caller may retry):
        no engine client, the project-identifier resolution request
        errored (``resolve_project_ids`` raises on a failed refresh —
        distinct from "identifier absent"), the identifier resolved to
        nothing, or the strict ``list_pages`` enumeration failed
        (pagination cap, cursor stall, transport error) — a wholesale
        ``seed`` from partial rows would silently delete skills the
        walk didn't reach.  The pages API's list default excludes
        archived pages, so archived skills drop out on the next
        successful walk.
        """
        if not self._project:
            logger.info("tool_skill_sync_no_project_configured")
            return 0
        client = self._transport.engine_client
        if client is None:
            logger.warning(
                "tool_skill_sync_no_client",
                project=self._project,
            )
            return None

        try:
            uuid = await self._project_uuid(client)
            if not uuid:
                # The resolution request SUCCEEDED (a failed one raises)
                # and the identifier is genuinely absent — an operator
                # configuration problem, reported as such.  Still a
                # failed walk: the registry must keep what it has.
                logger.warning("tool_skill_project_not_found", project=self._project)
                return None
            pages = await client.list_pages(uuid, fields=SYNC_PAGE_FIELDS)
        except Exception:
            # Strict enumeration failed (or the resolution request did)
            # — leave the registry exactly as it was.
            logger.exception(
                "tool_skill_sync_list_failed",
                project=self._project,
            )
            return None

        loaded = [
            skill
            for page in pages
            if isinstance(page, dict) and (skill := page_to_skill(page)) is not None
        ]
        # Atomic replace: build the new state, then swap.  Webhook
        # handlers that fire during the walk simply land on the new
        # registry once it's populated.
        self._registry.seed(loaded)
        logger.info(
            "tool_skill_sync_initial_complete",
            project=self._project,
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
        """Apply a single Plane page webhook event to the registry.

        ``event_kind`` is the transport's ``f"page.{action}"`` tag with
        the fork's ``created`` / ``updated`` / ``deleted`` vocabulary.
        A delete action evicts by page id; every other action — known
        or future — fetches the page through the fork's project-scoped
        GET and re-runs the admission predicate:

        - **404** ⇒ evict: covers both "already deleted" and "page
          belongs to another project" (the index callback fires for
          every page webhook workspace-wide; the scoped queryset is the
          project filter).  A no-op when the registry never held it.
        - **archived** (non-null ``archived_at``) ⇒ evict: archive is
          the operator's "remove" gesture — Plane's delete requires
          archive first, and the retrieve path still serves archived
          content.
        - **decode/parse failure** ⇒ evict: a page edited into a
          non-skill must not keep serving its last-good body.
        - otherwise ⇒ upsert.  Idempotent by construction, so the
          fork's debounced ``page.updated`` bursts collapse into
          repeated upserts of the same content.
        """
        action = (event_kind or "").rsplit(".", 1)[-1].lower()
        if action in {"delete", "deleted"}:
            await self._registry.evict_by_page_id(page_id)
            return

        client = self._transport.engine_client
        if client is None:
            logger.warning(
                "tool_skill_sync_no_client_for_webhook",
                page_id=page_id,
            )
            return
        try:
            uuid = await self._project_uuid(client)
        except Exception:
            logger.exception(
                "tool_skill_sync_project_resolve_failed",
                page_id=page_id,
                project=self._project,
            )
            return
        if not uuid:
            logger.warning("tool_skill_project_not_found", project=self._project)
            return

        try:
            page = await client.get_page(uuid, page_id)
        except PlaneProvisionError as exc:
            if exc.status == 404:
                # Deleted in the interim, or a foreign-project page —
                # either way it must not be (stay) in the registry.
                await self._registry.evict_by_page_id(page_id)
                return
            logger.warning(
                "tool_skill_sync_fetch_failed",
                page_id=page_id,
                status=exc.status,
                error=str(exc),
            )
            return
        except Exception:
            logger.exception("tool_skill_sync_fetch_failed", page_id=page_id)
            return

        if page.get("archived_at"):
            if await self._registry.evict_by_page_id(page_id):
                logger.info("tool_skill_sync_evicted_archived", page_id=page_id)
            return
        skill = page_to_skill(page)
        if skill is None:
            if await self._registry.evict_by_page_id(page_id):
                logger.info("tool_skill_sync_evicted_undecodable", page_id=page_id)
            return
        await self._registry.upsert(skill)

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    async def _project_uuid(self, client: PlaneClient) -> str:
        """Resolve the skills-project identifier to its UUID.

        Goes through the transport's shared identifier→UUID cache
        (:meth:`PlaneTransport.resolve_project_ids` — case-insensitive,
        engine-client-backed, 60 s miss floor), passing the engine
        client for the token-less-engine fallback path.  ``""`` on a
        miss.
        """
        mapping = await self._transport.resolve_project_ids(
            [self._project], client=client
        )
        return mapping.get(self._project.strip().upper(), "")


__all__ = ["SYNC_PAGE_FIELDS", "PlaneSkillSyncWorker", "page_to_skill"]
