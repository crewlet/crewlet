"""PlanePromotionWriter — the Plane promotion page writer.

Implements the consumer-owned
:class:`~crewlet.learning.skill_synthesizer.PromotionPageWriter` seam
over the started
:class:`~crewlet.notifications.transports.plane.PlaneTransport`'s
public client seam (``engine_client`` + ``resolve_project_ids``).

Drafts land under the unit project's ``Auto-Drafted Skills`` parent
page, which this writer ENSURES EXISTS (creating it on first use):
on Plane the Plan-phase search hides drafts by parent-id match
(:class:`~crewlet.knowledge.plane_search.PlaneSearcher`), so a draft
created at the project root would only be caught by the
``[Auto-draft] `` title backstop — the parent page is load-bearing,
not cosmetic.  Everything the writer creates is explicitly
``access=public`` (a private Plane page is invisible to every
non-owner on both the pages-CRUD and page-search read paths).

**Cross-tick dedup.**  The scheduler re-clusters the same persisted
``synthesized_skills`` rows every tick, so the writer is the only
layer that stops one converging cluster from piling up an identical
draft per tick.  Confluence gets this for free (a repeat create 400s
on the duplicate title); Plane enforces no title uniqueness, so this
writer stamps every page it creates with the fork's external-identity
pair (``external_source="crewlet"`` + ``external_id``) — drafts as
``draft:<name>`` (see :func:`draft_external_id`), the parent as
``doc:<parent title>`` (the importer's doc convention) — checks for
an existing page before creating (external id first, exact title as
the fallback for unmarked drafts missing the stamp), and returns the existing
page id instead of creating another.  The fork's project-wide 409 on
a duplicate pair is the hard backstop for anything the lookup missed
(a lookup/create race, a retitled draft).
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from crewlet._logging import get_logger
from crewlet.knowledge.markdown_docs import render_markdown
from crewlet.learning.skill_synthesizer import resolve_unit_container_attr
from crewlet.plane.client import PAGE_ACCESS_PUBLIC, PlaneProvisionError

if TYPE_CHECKING:
    from crewlet.notifications.transports.plane import PlaneTransport
    from crewlet.plane.client import PlaneClient

logger = get_logger("plane.promotion")

_EXTERNAL_SOURCE = "crewlet"
"""The engine's ``external_source`` marker (see
:data:`crewlet.plane.import_cli.EXTERNAL_SOURCE`; duplicated here as a
plain string so the promotion path doesn't import the CLI module)."""

_DEDUP_FIELDS = ["id", "name", "external_id", "external_source"]
"""``fields=`` projection for the pre-create dedup lookup — identity
columns only, bodies never fetched."""

_PARENT_PAGE_BODY = (
    "<p>Auto-drafted skills awaiting review. Move a page out of this "
    "parent to publish it into the team knowledge base.</p>"
)
"""Body for a freshly created ``Auto-Drafted Skills`` parent page —
tells a reviewing lead what the container is and what the publish
gesture is."""


def draft_external_id(name: str) -> str:
    """The ``external_id`` for one auto-draft page: ``draft:<name>``.

    Keyed on the LLM-picked kebab-case skill NAME, not the display
    title: the title carries the cosmetic ``[Auto-draft] `` prefix a
    lead may strip (and the constant itself could change), while the
    name is the cluster's stable identity — the same value the
    scheduler dedups against in ``existing_skill_names``.  The
    ``draft:`` namespace keeps these pages disjoint from the
    importer's ``skill:<key>`` / ``doc:<title>`` identities, so a
    published draft can later be re-imported as a doc without an
    identity collision.
    """
    return f"draft:{name}"


class PlanePromotionWriter:
    """Write promotion drafts as Plane pages.

    Best-effort like every promotion path: any failure logs and
    returns ``None`` so the scheduler retries on its next tick.
    """

    backend = "plane"

    def __init__(self, *, transport: PlaneTransport) -> None:
        self._transport = transport

    def resolve_unit_container(self, org: Any, unit_id: str) -> str:
        """The unit's ``integrations.plane.project`` identity."""
        return resolve_unit_container_attr(org, unit_id, "plane_project")

    def missing_container_hint(self) -> str:
        return "set integrations.plane.project on the unit"

    async def create_draft_page(
        self,
        *,
        container: str,
        parent_title: str,
        title: str,
        name: str,
        body_markdown: str,
    ) -> str | None:
        """Create — or find — the draft for ``name`` in ``container``.

        ``container`` is a project *identifier* (e.g. ``ENG``), resolved
        to its UUID through the transport's shared cache.  A page
        already carrying this draft's identity (``draft:<name>``
        external id, or the exact title for an unmarked draft)
        is returned as-is — the cross-tick dedup — otherwise the draft
        is created under the ensured ``Auto-Drafted Skills`` parent
        with the identity stamped, and a create-time 409 (the fork's
        uniqueness backstop: a race, or a draft a lead retitled) logs
        and returns ``None`` so the tick no-ops exactly like
        Confluence's duplicate-title 400.

        Only when the parent can be neither found nor created does the
        draft land at the project root with a ``promotion_no_parent_page``
        hint.  NOTE the honest coverage of that fallback: the searcher's
        ``[Auto-draft] `` title backstop applies only while the
        searcher's OWN parent lookup is failing (the plausible
        correlated outage) — once the backend recovers, a root-level
        draft has no excluding parent and IS served into search results
        until a lead reviews it or moves it under the parent.
        """
        client = self._transport.engine_client
        if client is None:
            logger.info(
                "promotion_backend_unavailable",
                backend=self.backend,
                container=container,
                reason="no_engine_client",
            )
            return None
        if not container or not title or not name:
            return None
        try:
            mapping = await self._transport.resolve_project_ids([container])
        except Exception:
            logger.exception(
                "promotion_container_resolve_failed",
                backend=self.backend,
                container=container,
            )
            return None
        project_uuid = mapping.get(container.strip().upper(), "")
        if not project_uuid:
            logger.warning(
                "promotion_container_unresolved",
                backend=self.backend,
                container=container,
                hint="create the Plane project (identifier above) or fix "
                "the unit's integrations.plane.project",
            )
            return None

        external_id = draft_external_id(name)
        existing = await self._find_existing_draft(
            client, project_uuid, external_id=external_id, title=title
        )
        if existing:
            logger.info(
                "promotion_draft_exists",
                backend=self.backend,
                container=container,
                title=title,
                page_id=existing,
            )
            return existing

        parent_id = await self._ensure_parent(client, project_uuid, parent_title)
        try:
            page = await client.create_page(
                project_uuid,
                name=title,
                description_html=render_markdown(body_markdown),
                parent_id=parent_id,
                external_id=external_id,
                external_source=_EXTERNAL_SOURCE,
                access=PAGE_ACCESS_PUBLIC,
            )
        except PlaneProvisionError as exc:
            if exc.status == 409:
                # The identity already exists somewhere the title
                # lookup couldn't see (a retitled or archived draft, or
                # a concurrent create) — already drafted, so no-op like
                # Confluence's duplicate-title 400.
                logger.info(
                    "promotion_draft_conflict",
                    backend=self.backend,
                    container=container,
                    external_id=external_id,
                    error=str(exc),
                )
                return None
            logger.exception(
                "promotion_page_create_failed",
                backend=self.backend,
                container=container,
                title=title,
            )
            return None
        except Exception:
            logger.exception(
                "promotion_page_create_failed",
                backend=self.backend,
                container=container,
                title=title,
            )
            return None
        return str(page.get("id") or "") or None

    async def _find_existing_draft(
        self,
        client: PlaneClient,
        project_uuid: str,
        *,
        external_id: str,
        title: str,
    ) -> str:
        """Cross-tick dedup lookup: this draft's page id, or ``""``.

        One narrow ``list_pages(search=<title>)`` (name-``icontains``)
        enumeration, matched by external identity first — rename-stable
        for stamped drafts — with an exact-title fallback that adopts
        drafts created before the identity stamp existed.  A FAILED
        lookup returns ``""`` and lets the create proceed: with the
        identity stamped, the fork's 409 on a duplicate pair already
        guarantees a stamped draft cannot be duplicated, so blocking
        the whole tick on a lookup outage would only cost drafts.
        """
        try:
            rows = await client.list_pages(
                project_uuid, search=title, fields=_DEDUP_FIELDS
            )
        except Exception as exc:
            logger.warning(
                "promotion_draft_lookup_failed",
                project_id=project_uuid,
                title=title,
                error=str(exc),
            )
            return ""
        by_title = ""
        for page in rows:
            if not isinstance(page, dict):
                continue
            page_id = str(page.get("id") or "")
            if not page_id:
                continue
            if (
                str(page.get("external_source") or "") == _EXTERNAL_SOURCE
                and str(page.get("external_id") or "") == external_id
            ):
                return page_id
            if not by_title and str(page.get("name") or "") == title:
                by_title = page_id
        return by_title

    async def _ensure_parent(
        self, client: PlaneClient, project_uuid: str, parent_title: str
    ) -> str | None:
        """Resolve — or create — the ``Auto-Drafted Skills`` parent page.

        Lookup is ``list_pages(search=…)`` (name-``icontains``) plus an
        exact-name post-filter.  A FAILED lookup returns ``None``
        without creating: creating on a failed lookup could duplicate
        an existing parent.  The create stamps the importer's doc
        identity (``doc:<parent title>``) so the fork's 409 closes the
        lookup/create race — on a 409 the parent evidently exists (or
        an archived page holds the identity), so the lookup is retried
        once before giving up.
        """
        if not parent_title:
            return None
        parent_id = await self._lookup_parent(client, project_uuid, parent_title)
        if parent_id is None:
            self._log_no_parent(project_uuid, parent_title)
            return None
        if parent_id:
            return parent_id
        try:
            created = await client.create_page(
                project_uuid,
                name=parent_title,
                description_html=_PARENT_PAGE_BODY,
                external_id=f"doc:{parent_title}",
                external_source=_EXTERNAL_SOURCE,
                access=PAGE_ACCESS_PUBLIC,
            )
        except PlaneProvisionError as exc:
            if exc.status == 409:
                # Someone (a concurrent tick, an importer run, or an
                # archived page) already owns the identity — re-look
                # once now that we know it exists.
                retry = await self._lookup_parent(client, project_uuid, parent_title)
                if retry:
                    return retry
            logger.exception(
                "promotion_parent_create_failed",
                project_id=project_uuid,
                parent_title=parent_title,
            )
            self._log_no_parent(project_uuid, parent_title)
            return None
        except Exception:
            logger.exception(
                "promotion_parent_create_failed",
                project_id=project_uuid,
                parent_title=parent_title,
            )
            self._log_no_parent(project_uuid, parent_title)
            return None
        parent_id = str(created.get("id") or "") or None
        if parent_id:
            logger.info(
                "promotion_parent_created",
                project_id=project_uuid,
                parent_title=parent_title,
                page_id=parent_id,
            )
        else:
            self._log_no_parent(project_uuid, parent_title)
        return parent_id

    @staticmethod
    async def _lookup_parent(
        client: PlaneClient, project_uuid: str, parent_title: str
    ) -> str | None:
        """Exact-name parent lookup.

        Returns the page id, ``""`` when the lookup SUCCEEDED and found
        nothing (safe to create), or ``None`` when the lookup FAILED
        (must not create).
        """
        try:
            pages = await client.list_pages(
                project_uuid, search=parent_title, fields=["id", "name"]
            )
        except Exception:
            logger.exception(
                "promotion_parent_lookup_failed",
                project_id=project_uuid,
                parent_title=parent_title,
            )
            return None
        for page in pages:
            if not isinstance(page, dict):
                continue
            if str(page.get("name") or "") != parent_title:
                continue
            page_id = str(page.get("id") or "")
            if page_id:
                return page_id
        return ""

    @staticmethod
    def _log_no_parent(project_uuid: str, parent_title: str) -> None:
        logger.info(
            "promotion_no_parent_page",
            project_id=project_uuid,
            parent_title=parent_title,
            hint=(
                "the draft lands at the project root; the searcher's "
                "[Auto-draft] title backstop hides it ONLY while the "
                "searcher's own parent lookup is failing too — once the "
                "backend recovers a root-level draft surfaces in search, "
                "so review it or move it under the parent promptly"
            ),
        )


__all__ = ["PlanePromotionWriter", "draft_external_id"]
