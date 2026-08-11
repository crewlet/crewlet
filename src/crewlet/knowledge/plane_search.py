"""PlaneSearcher -- query-time Plane page search.

The Plane backend of the
:class:`~crewlet.knowledge.protocol.KnowledgeSearcher` seam.  The
auxiliary LLM turns the turn's trigger context into a short text query
(see :mod:`crewlet.learning.relevant_knowledge`); this module sends it
to the fork's workspace page-search endpoint (which
tokenises server-side: AND across whitespace-split tokens against page
name and stripped body text) and maps the rows into
:class:`~crewlet.knowledge.protocol.KnowledgeHit`.

Because the server-side search is a strict conjunction, a many-keyword query over a
modest page corpus is near-guaranteed to match nothing even when its
leading terms name exactly the right document (observed live: a
7-keyword aux query yielded zero hits where its 3-token prefix hit).
The searcher therefore RELAXES on empty results: full token list
first, then the 4-token and 2-token prefixes
(:data:`_RELAX_PREFIXES`), stopping at the first non-empty result set.
Confluence needs no equivalent -- CQL ``siteSearch`` ranks over loose
matching server-side.

Auth model mirrors the Confluence searcher: the search authenticates
as the *calling agent's* own Plane user when the role carries
``mcp_env["plane"]["PLANE_API_KEY"]`` -- Plane then enforces project
membership and page access natively -- falling back to the engine's
shared read client.  A missing or unresolvable per-agent key silently
selects the engine path; a credential-less role on a token-less engine
searches nothing.

Project scoping is **optional**: ``knowledge.plane_projects``
identifiers narrow the search (resolved to project UUIDs through the
transport's shared cache).  Empty scope + self-authenticating role =>
unscoped search (Plane's own membership/access rules bound the hits);
empty scope + credential-less role => no search.  A non-empty scope
that resolves to ZERO project UUIDs also searches nothing -- silently
widening to unscoped would leak everything the engine account can see.

Two result exclusions:

* the **Tool Skills project** is dropped wholesale -- skill content
  reaches agents through the catalogue / ``load_tool_skill`` mechanism,
  and a skill page's leading YAML block would dominate any excerpt;
* **auto-drafts** are hidden by parent page: rows whose ``parent_id``
  matches the project's ``Auto-Drafted Skills`` page (resolved lazily
  per project, engine-client preferred).  When that lookup FAILS the
  :data:`~crewlet.knowledge.protocol.AUTO_DRAFT_TITLE_PREFIX` title
  backstop applies instead, so an outage hides drafts rather than
  leaking them; the filter is depth-1 (drafts are flat leaf pages by
  construction), unlike Confluence's full ancestor chain.

Both exclusions fail CLOSED, matching the transport's page-routing
rule: a row without a resolvable ``project_id`` is dropped, and when a
configured skills project cannot be resolved to a UUID only rows
positively attributable to a different project survive.  The scope and
the skills project resolve in ONE ``resolve_project_ids`` call per
search, so a token-less engine's per-agent fallback pays a single
``list_projects()`` walk per call -- and a failed resolution request
(the transport raises; distinct from "identifier absent") fails the
whole search closed to ``[]``.

Ranking note: the endpoint orders by ``-updated_at`` -- recency, not relevance
(unlike Confluence CQL).  The searcher preserves the server order and
truncates the overfetch window from the tail, i.e. it drops the OLDEST
rows of the window.
"""

from __future__ import annotations

import time
from collections.abc import Sequence
from typing import TYPE_CHECKING, Any

from crewlet._logging import get_logger
from crewlet.config import resolve_env_vars
from crewlet.knowledge.accessibility import accessible_projects
from crewlet.knowledge.protocol import (
    AUTO_DRAFT_TITLE_PREFIX,
    AUTO_DRAFTED_PARENT,
    KnowledgeHit,
)

if TYPE_CHECKING:
    from crewlet.notifications.transports.plane import PlaneTransport
    from crewlet.org.models import Organization, Role
    from crewlet.plane.client import PlaneClient

logger = get_logger("knowledge.plane_search")

# Cap on the aux-generated text sent as the search ``query`` param.  Same
# constant and rationale as the Confluence searcher's ``_MAX_QUERY_CHARS``:
# a pathologically long fragment is a sign the aux model misbehaved.
_MAX_QUERY_CHARS = 200

# The fork rejects a query above 16 DISTINCT case-folded tokens with a 400 --
# it never truncates server-side -- and the search's error path treats
# that as "no hits".  Pre-trim to the cap so a pathological aux query
# degrades to a real (leading-tokens) search instead of a guaranteed
# empty knowledge block.
_MAX_QUERY_TOKENS = 16

# AND-relaxation ladder (see the module docstring): the full
# token list first, then these leading-prefix lengths.  At most three
# HTTP searches bound the Plan-phase latency; 4- and 2-token prefixes
# are the meaningfully looser conjunctions -- a single token would match
# half the corpus and recency-rank it, which reads as noise, not
# knowledge.  Prefixes (not tail-drops from the middle) because the aux
# prompt's examples lead with the topical terms and trail with generic
# qualifiers.
_RELAX_PREFIXES = (4, 2)

# Extra rows fetched beyond the caller's ``limit`` so the post-filters
# (skills project, auto-draft parent) still leave enough hits.  Because
# the endpoint orders by recency, the overfetch window extends toward OLDER pages
# and truncation drops the oldest of the window.
_OVERFETCH = 5

# Miss floor for the per-project ``Auto-Drafted Skills`` parent-page
# lookup: a failed or empty lookup is retried at most once a minute per
# project.  Same value and rationale as the transport's
# ``_PROJECT_CACHE_REFETCH_INTERVAL`` -- bounds the worst case under
# result bursts while a freshly created parent page still resolves
# within one tick.  A FOUND parent id is cached for the searcher's
# lifetime (page ids are stable; searchers are rebuilt on refresh).
_MISS_REFETCH_INTERVAL = 60.0


def _resolve_plane_env(role: Role | None) -> dict[str, str]:
    """Resolve a role's ``mcp_env['plane']`` env (``${VAR}`` expanded).

    Returns ``{}`` for a missing role or on resolution failure -- callers
    treat that as "no per-agent Plane identity".
    """
    if role is None:
        return {}
    try:
        return resolve_env_vars(role.mcp_env.get("plane", {}) or {})
    except Exception:
        logger.exception(
            "plane_search_mcp_env_resolve_failed",
            role=getattr(role, "name", ""),
        )
        return {}


def authenticates_as_self(role: Role | None) -> bool:
    """True when the role carries its own Plane API key.

    The query-time search then runs as that agent and Plane enforces
    project membership + page access natively -- so an explicit project
    allow-list is optional and an empty one means "search everything
    this agent can read" rather than "search nothing".
    """
    env = _resolve_plane_env(role)
    return bool(str(env.get("PLANE_API_KEY") or "").strip())


class PlaneSearcher:
    """Query-time Plane page search over a role's accessible projects.

    Implements the :class:`~crewlet.knowledge.protocol.KnowledgeSearcher`
    protocol.  Constructed once in :meth:`crewlet.engine.Engine.start`
    whenever a :class:`~crewlet.notifications.transports.plane.PlaneTransport`
    is configured; ``skills_project`` is the engine's Tool Skills
    project identifier (``""`` disables that exclusion, consistent with
    sync being disabled).
    """

    def __init__(self, *, transport: PlaneTransport, skills_project: str = "") -> None:
        self._transport = transport
        self._skills_project = (skills_project or "").strip()
        # Per-project ``Auto-Drafted Skills`` parent-page cache: a found
        # page id lives for the searcher's lifetime; misses (absent or
        # failed lookups) are retried behind ``_MISS_REFETCH_INTERVAL``
        # as ``{project_uuid: (monotonic_at, lookup_ok)}``.
        self._draft_parent_ids: dict[str, str] = {}
        self._draft_parent_misses: dict[str, tuple[float, bool]] = {}

    # ── protocol surface ──

    def can_search(self, *, role: Role | None, org: Organization | None) -> bool:
        """Cheap, no-I/O pre-gate: could :meth:`search` hit anything?

        ``True`` when the org has a configured project scope or the
        role authenticates as its own Plane user (unscoped, ACL-bound
        search).  ``False`` -- including on a scope-resolution failure
        -- means a search is a guaranteed no-op, so the caller can skip
        the aux-LLM query-generation call entirely.
        """
        projects = self._scope(org)
        if projects is None:
            return False
        return bool(projects) or authenticates_as_self(role)

    async def search(
        self,
        *,
        query: str,
        role: Role | None,
        org: Organization | None,
        limit: int = 8,
        exclude_ancestors: Sequence[str] | None = None,
    ) -> list[KnowledgeHit]:
        """Search Plane pages; return up to ``limit`` recency-ranked hits.

        The project scope is derived from ``org``
        (:func:`~crewlet.knowledge.accessibility.accessible_projects`);
        an empty scope searches unscoped for a self-authenticating role
        (Plane membership/access bound the hits) and nothing for a
        credential-less one.  A non-empty scope resolving to zero
        project UUIDs searches nothing -- never silently widens.  Rows
        in the skills project are dropped, and when
        :data:`~crewlet.knowledge.protocol.AUTO_DRAFTED_PARENT` is
        listed in ``exclude_ancestors`` rows parented under each
        project's ``Auto-Drafted Skills`` page are dropped too (title
        backstop when the parent lookup fails).

        Best-effort: any failure -- empty inputs, no client, a
        transport/HTTP error -- returns ``[]``, and the Plan-phase
        prefetch degrades to an empty knowledge block.
        """
        projects = self._scope(org)
        if projects is None:
            return []
        if not projects and not authenticates_as_self(role):
            return []
        query = (query or "").strip()[:_MAX_QUERY_CHARS]
        if not query:
            return []

        client, auth_label, owns = self._resolve_client(role)
        if client is None:
            # A credential-less role on a token-less engine searches
            # nothing -- the Confluence admin-client-missing path.
            logger.debug("plane_search_no_client", auth=auth_label)
            return []
        logger.debug("plane_search_request", auth=auth_label)
        try:
            # ONE resolve for the scope AND the skills project.  On a
            # token-less engine ``resolve_project_ids`` falls back to a
            # full per-agent ``list_projects()`` walk that is never
            # written to the shared cache — so the two lookups must
            # share a single fetch per search call, not one each.  A
            # raised resolution failure lands in the except below and
            # fails the whole search closed ([] — never an unfiltered
            # result set).
            wanted = list(projects)
            if self._skills_project:
                wanted.append(self._skills_project)
            mapping: dict[str, str] = {}
            if wanted:
                mapping = await self._transport.resolve_project_ids(
                    wanted, client=client
                )
            project_uuids = [mapping[p] for p in projects if p in mapping]
            if projects and not project_uuids:
                # Fail closed: an unresolvable scope must not widen
                # to everything the credential can see.
                logger.warning(
                    "plane_search_projects_unresolved",
                    projects=",".join(projects),
                )
                return []
            skills_uuid = ""
            if self._skills_project:
                skills_uuid = mapping.get(
                    self._skills_project.strip().upper(), ""
                ).lower()
            rows = await self._search_relaxed(
                client,
                query,
                projects=project_uuids or None,
                limit=int(limit) + _OVERFETCH,
            )
            return await self._filter_and_map(
                rows, client, int(limit), exclude_ancestors, skills_uuid=skills_uuid
            )
        except Exception:
            logger.exception("plane_search_failed", auth=auth_label)
            return []
        finally:
            if owns:
                await client.close()

    # ── internals ──

    async def _search_relaxed(
        self,
        client: PlaneClient,
        query: str,
        *,
        projects: list[str] | None,
        limit: int,
    ) -> list[dict[str, Any]]:
        """Run the page search, relaxing the AND-conjunction on zero hits.

        Attempts, in order: the full token list (pre-trimmed to the server's
        16-distinct-token cap), then each leading prefix in
        :data:`_RELAX_PREFIXES` that is actually shorter than the
        previous attempt.  Returns the first non-empty result set; a
        dry run through the whole ladder returns ``[]``.
        """
        tokens = query.split()
        seen: set[str] = set()
        trimmed: list[str] = []
        for token in tokens:
            folded = token.lower()
            if folded not in seen:
                if len(seen) >= _MAX_QUERY_TOKENS:
                    break
                seen.add(folded)
            trimmed.append(token)
        if len(trimmed) < len(tokens):
            logger.debug(
                "plane_search_query_trimmed",
                original_tokens=len(tokens),
                sent_tokens=len(trimmed),
            )
        attempts = [trimmed]
        attempts.extend(trimmed[:n] for n in _RELAX_PREFIXES if len(trimmed) > n)
        for i, attempt in enumerate(attempts):
            rows = await client.search_pages(
                " ".join(attempt), projects=projects, limit=limit
            )
            if rows:
                if i:
                    logger.debug(
                        "plane_search_relaxed",
                        query=query,
                        used=" ".join(attempt),
                    )
                return rows
        return []

    def _scope(self, org: Organization | None) -> list[str] | None:
        """Sorted org-wide project scope; ``None`` when resolution failed.

        ``None`` (as opposed to ``[]``) means the org object was
        present but unreadable -- both entry points then fail closed
        (no search) rather than falling through to an unscoped search.
        """
        if org is None:
            return []
        try:
            return sorted(accessible_projects(org))
        except Exception:
            logger.exception("plane_search_scope_failed")
            return None

    def _resolve_client(
        self, role: Role | None
    ) -> tuple[PlaneClient | None, str, bool]:
        """Pick the Plane client for this search.

        Returns ``(client, auth_label, owns_client)``.  A per-agent
        ``PLANE_API_KEY`` in the role's ``plane`` mcp_env wins -- the
        search then runs as that agent and Plane enforces its
        membership/access rules -- otherwise the transport's shared
        engine client is used.  ``owns_client`` is ``True`` only for
        the per-agent client, which the caller must close.
        """
        env = _resolve_plane_env(role)
        key = str(env.get("PLANE_API_KEY") or "").strip()
        if key:
            return self._transport.new_user_client(key), "per_agent", True
        return self._transport.engine_client, "engine", False

    async def _filter_and_map(
        self,
        rows: list[dict[str, Any]],
        client: PlaneClient,
        limit: int,
        exclude_ancestors: Sequence[str] | None,
        *,
        skills_uuid: str,
    ) -> list[KnowledgeHit]:
        """Post-filter search rows and map them to :class:`KnowledgeHit`.

        ``skills_uuid`` is the pre-resolved skills-project UUID
        (lowercased; ``""`` when unconfigured OR unresolvable — the
        caller resolves it in the same ``resolve_project_ids`` call as
        the scope, so a token-less engine pays one fallback walk per
        search, not two).  Server order (``-updated_at``) is preserved;
        truncation to ``limit`` drops the oldest rows of the overfetch
        window.

        Both exclusions FAIL CLOSED, the same rule the transport's page
        routing follows: a row whose project cannot be established is
        dropped, never served unchecked.

        * A row without a ``project_id`` is dropped (debug log) — it
          can be cleared against neither the skills-project nor the
          auto-draft exclusion, and its URL/container are unbuildable
          anyway.  (Not reachable through the real fork, whose search
          serializer always populates ``project_id`` — defensive.)
        * When the skills project is CONFIGURED but its UUID did not
          resolve, the UUID comparison cannot clear any row, so only
          rows positively identified (via the shared cache) as a
          DIFFERENT project are kept; the rest drop.  An outage hides
          content rather than leaking Tool Skills YAML into prompts.
        """
        exclude = {a.strip() for a in (exclude_ancestors or []) if a and a.strip()}
        exclude_drafts = AUTO_DRAFTED_PARENT in exclude
        skills_unresolved = bool(self._skills_project) and not skills_uuid
        if skills_unresolved and rows:
            logger.warning(
                "plane_search_skills_project_unresolved",
                project=self._skills_project,
                hint="rows not positively attributable to another project "
                "are dropped until the identifier resolves",
            )
        skills_identifier = self._skills_project.strip().upper()

        out: list[KnowledgeHit] = []
        for row in rows:
            if not isinstance(row, dict):
                continue
            page_id = str(row.get("id") or "")
            if not page_id:
                continue
            project_uuid = str(row.get("project_id") or "").lower()
            if not project_uuid:
                logger.debug("plane_search_row_no_project", page_id=page_id)
                continue
            if skills_uuid and project_uuid == skills_uuid:
                continue
            container = self._transport.cached_project_identifier(project_uuid)
            if skills_unresolved and (
                not container or container.strip().upper() == skills_identifier
            ):
                logger.debug(
                    "plane_search_row_dropped_skills_unresolved",
                    page_id=page_id,
                    project_id=project_uuid,
                )
                continue
            if exclude_drafts and await self._is_draft(row, project_uuid, client):
                continue
            out.append(
                KnowledgeHit(
                    title=str(row.get("name") or ""),
                    url=self._page_url(project_uuid, page_id),
                    container=container,
                    page_id=page_id,
                    snippet=str(row.get("snippet") or ""),
                    ancestors=[],
                )
            )
            if len(out) >= limit:
                break
        return out

    async def _is_draft(
        self, row: dict[str, Any], project_uuid: str, client: PlaneClient
    ) -> bool:
        """Is this row an unpublished auto-draft that must be hidden?

        Primary mechanism: the row's ``parent_id`` matches the
        project's ``Auto-Drafted Skills`` page.  Fail-closed backstop
        when that lookup FAILED for the project: the
        :data:`AUTO_DRAFT_TITLE_PREFIX` title match -- an outage hides
        drafts rather than leaking them, while a lead who moved a draft
        out without renaming it still publishes it (the backstop never
        applies when the lookup worked).
        """
        if not project_uuid:
            # Defensive: ``_filter_and_map`` drops project-less rows
            # before this check.  Should one ever arrive, the parent
            # lookup is impossible — fail closed via the title backstop
            # rather than serving a potential draft.
            return str(row.get("name") or "").startswith(AUTO_DRAFT_TITLE_PREFIX)
        parent_id, lookup_ok = await self._draft_parent(client, project_uuid)
        if lookup_ok:
            row_parent = str(row.get("parent_id") or "").lower()
            return bool(parent_id) and row_parent == parent_id
        return str(row.get("name") or "").startswith(AUTO_DRAFT_TITLE_PREFIX)

    async def _draft_parent(
        self, client: PlaneClient, project_uuid: str
    ) -> tuple[str, bool]:
        """Resolve a project's ``Auto-Drafted Skills`` page id.

        Returns ``(parent_page_id_or_empty, lookup_ok)``.  The lookup
        runs with the ENGINE client when available (per-agent
        membership must not decide what gets hidden), degrading to the
        active search client; ``list_pages(search=…)`` is a
        name-``icontains`` filter, so an exact-name post-filter picks
        the page.  Found ids are cached for the searcher's lifetime;
        misses (absent or failed) retry behind the 60 s floor and reuse
        the last outcome in between.
        """
        cached = self._draft_parent_ids.get(project_uuid, "")
        if cached:
            return cached, True
        now = time.monotonic()
        miss = self._draft_parent_misses.get(project_uuid)
        if miss is not None and now - miss[0] < _MISS_REFETCH_INTERVAL:
            return "", miss[1]
        lookup_client = self._transport.engine_client or client
        try:
            pages = await lookup_client.list_pages(
                project_uuid,
                search=AUTO_DRAFTED_PARENT,
                fields=["id", "name"],
            )
        except Exception as exc:
            logger.warning(
                "plane_search_draft_parent_lookup_failed",
                project_id=project_uuid,
                error=str(exc),
            )
            self._draft_parent_misses[project_uuid] = (now, False)
            return "", False
        for page in pages:
            if not isinstance(page, dict):
                continue
            if str(page.get("name") or "") != AUTO_DRAFTED_PARENT:
                continue
            page_id = str(page.get("id") or "").lower()
            if page_id:
                self._draft_parent_ids[project_uuid] = page_id
                self._draft_parent_misses.pop(project_uuid, None)
                return page_id, True
        # A successful lookup that found no parent page: nothing to
        # exclude by parent (and no backstop -- the lookup worked).
        self._draft_parent_misses[project_uuid] = (now, True)
        return "", True

    def _page_url(self, project_uuid: str, page_id: str) -> str:
        """Shareable page URL -- the exact shape the transport builds for
        webhook notifications; ``""`` when the project is unknown."""
        base = (self._transport.base_url or "").rstrip("/")
        workspace = self._transport.workspace
        if not base or not workspace or not project_uuid:
            return ""
        return f"{base}/{workspace}/projects/{project_uuid}/pages/{page_id}"
