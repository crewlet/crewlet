"""Backend-neutral knowledge-search seam (``KnowledgeSearcher``).

The Plan-phase ``## Relevant knowledge`` prefetch talks to the team
knowledge base through this protocol.  Each backend --
:class:`~crewlet.knowledge.confluence_search.ConfluenceSearcher`
(Confluence CQL) and the Plane page searcher (``plane_search``, same
protocol) -- implements it; the engine constructs exactly one, selected
by which integration block is configured.

Contract semantics every backend MUST honor:

- **Scope lives behind the seam.**  ``search()`` derives its container
  scope from ``org`` (:func:`~crewlet.knowledge.accessibility.accessible_spaces`
  / :func:`~crewlet.knowledge.accessibility.accessible_projects`);
  callers pass a role, a plain-text query, and ancestor-title
  exclusions -- never CQL fragments, space keys, or project lists.
  ``org`` is a **per-call** parameter, so live config edits to the
  ``knowledge.*`` scope flow through with no engine refresh hook.
- **Unscoped-vs-nothing is enforced inside ``search()``**: empty scope
  + self-authenticating role => unscoped search (the backend's own
  ACLs bound the hits); empty scope + credential-less role => ``[]``.
- **``can_search`` is a cheap, no-I/O pre-gate**:
  ``bool(scope) or authenticates_as_self(role)``.  Its only job is
  letting the prefetch skip the aux-LLM query-generation call when the
  search is a guaranteed no-op.  ``authenticates_as_self`` deliberately
  stays a module-level function per backend rather than a protocol
  method: exposing self-auth alone would force the caller to also
  fetch the scope list to reproduce the gate, leaking the container
  vocabulary the seam exists to hide.
- **Best-effort**: ``search()`` never raises; every failure path
  returns ``[]`` and the prefetch degrades to an empty block.
- ``exclude_ancestors`` drops hits whose ancestor/parent chain matches
  any listed title.  The prefetch defaults it to
  ``[AUTO_DRAFTED_PARENT]``; ``[]`` disables the exclusion.
"""

from __future__ import annotations

from collections.abc import Sequence
from typing import TYPE_CHECKING, Protocol, runtime_checkable

from pydantic import BaseModel, Field

if TYPE_CHECKING:
    from crewlet.org.models import Organization, Role

AUTO_DRAFTED_PARENT = "Auto-Drafted Skills"
"""Canonical parent-page title for unreviewed auto-drafted skills.

:class:`~crewlet.learning.skill_synthesizer.PromotionSynthesizer` posts
every promotion draft under a page with this title in the unit's
container; a unit lead publishes a draft by moving it out.  Searchers
exclude pages under this parent when the caller lists it in
``exclude_ancestors`` (the prefetch's default), so other agents never
follow an unvetted LLM-proposed procedure during the review window.
Single-sourced here -- both the prefetch and the synthesizer import it.
"""

AUTO_DRAFT_TITLE_PREFIX = "[Auto-draft] "
"""Title prefix the promotion path stamps on every auto-drafted page.

The primary hiding mechanism is the :data:`AUTO_DRAFTED_PARENT`
ancestor exclusion; this prefix is the fail-closed backstop for
backends whose parent lookup can fail (a lookup outage then hides
drafts rather than leaking them).  A lead who moves a draft out of the
parent without renaming it still publishes it -- renaming is optional.
Single-sourced here -- searcher backends and the synthesizer import it.
"""


class KnowledgeHit(BaseModel):
    """One ranked page hit from a knowledge-base search."""

    title: str = ""
    url: str = ""
    """Shareable human URL; ``""`` when unbuildable."""
    container: str = ""
    """Confluence space key / Plane project identifier."""
    page_id: str = ""
    snippet: str = ""
    """First sentence, plain text, <= 200 chars; may be ``""``."""
    ancestors: list[str] = Field(default_factory=list)
    """Ancestor page titles; ``[]`` on backends without ancestor chains."""


@runtime_checkable
class KnowledgeSearcher(Protocol):
    """Query-time knowledge-base search over a role's accessible scope.

    See the module docstring for the semantics every implementation
    must honor (scope behind the seam, unscoped-vs-nothing, cheap
    ``can_search``, best-effort ``search``).
    """

    def can_search(self, *, role: Role | None, org: Organization | None) -> bool:
        """Cheap, no-I/O pre-gate: could ``search()`` possibly hit anything?"""
        ...

    async def search(
        self,
        *,
        query: str,
        role: Role | None,
        org: Organization | None,
        limit: int = 8,
        exclude_ancestors: Sequence[str] | None = None,
    ) -> list[KnowledgeHit]:
        """Search the knowledge base; return up to ``limit`` ranked hits.

        Best-effort: never raises; every failure path returns ``[]``.
        """
        ...
