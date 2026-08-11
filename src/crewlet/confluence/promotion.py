"""ConfluencePromotionWriter — the Confluence promotion page writer.

Implements the consumer-owned
:class:`~crewlet.learning.skill_synthesizer.PromotionPageWriter` seam
over the started :class:`ConfluenceTransport`, reusing the generic
:mod:`crewlet.confluence.pages` helpers (``find_by_title`` +
``create_page``).

The draft body arrives as markdown and is rendered to real storage
XHTML via :func:`~crewlet.knowledge.markdown_docs.render_markdown`, so
the reviewing lead sees proper headings / lists instead of flattened
prose.
"""

from __future__ import annotations

from typing import Any

from crewlet._logging import get_logger
from crewlet.confluence.pages import create_page, find_by_title
from crewlet.knowledge.markdown_docs import render_markdown
from crewlet.learning.skill_synthesizer import resolve_unit_container_attr

logger = get_logger("confluence.promotion")


class ConfluencePromotionWriter:
    """Write promotion drafts as Confluence pages.

    Best-effort like every promotion path: any failure logs and
    returns ``None`` so the scheduler retries on its next tick.
    """

    backend = "confluence"

    def __init__(self, *, transport: Any) -> None:
        self._transport = transport

    def resolve_unit_container(self, org: Any, unit_id: str) -> str:
        """The unit's ``integrations.confluence.space`` identity."""
        return resolve_unit_container_attr(org, unit_id, "confluence_space")

    def missing_container_hint(self) -> str:
        return "set integrations.confluence.space on the unit"

    async def create_draft_page(
        self,
        *,
        container: str,
        parent_title: str,
        title: str,
        name: str,
        body_markdown: str,
    ) -> str | None:
        """Create the draft under ``parent_title`` in space ``container``.

        ``name`` (the stable kebab identity) is unused here: Confluence
        enforces per-space title uniqueness and 400s a repeat create of
        the same draft title, which ``create_page`` surfaces as
        ``ConfluencePublishError`` → ``None`` — that 4xx IS the
        Confluence cross-tick dedup (the Plane writer, whose backend
        has no title constraint, dedups on ``external_id="draft:<name>"``
        instead).

        The parent is resolved by exact title; when it doesn't exist
        the draft lands at the space root with a
        ``promotion_no_parent_page`` hint — a unit lead can pre-create
        the parent once, or move the draft under it later.  No labels
        are stamped: the ``## Relevant knowledge`` prefetch finds the
        page once a lead publishes it (moves it out of the
        ``Auto-Drafted Skills`` parent).
        """
        if self._transport is None or not container or not title:
            return None
        parent_id: str | None = None
        try:
            parent = await find_by_title(self._transport, container, parent_title)
        except Exception:
            logger.exception(
                "promotion_parent_lookup_failed",
                container=container,
                parent_title=parent_title,
            )
            parent = None
        if parent is not None:
            parent_id = str(parent.get("id") or "") or None
        if parent_id is None:
            logger.info(
                "promotion_no_parent_page",
                container=container,
                parent_title=parent_title,
                hint=(
                    "create the parent page once in Confluence so "
                    "auto-drafts land beneath it"
                ),
            )
        try:
            page_id = await create_page(
                self._transport,
                container,
                title,
                render_markdown(body_markdown),
                labels=[],
                parent_id=parent_id,
            )
        except Exception:
            logger.exception(
                "promotion_page_create_failed",
                backend=self.backend,
                container=container,
                title=title,
            )
            return None
        return page_id or None


__all__ = ["ConfluencePromotionWriter"]
