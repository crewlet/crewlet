"""ConfluenceSearcher -- query-time Confluence CQL search.

The Confluence backend of the
:class:`~crewlet.knowledge.protocol.KnowledgeSearcher` seam.  Replaces
the synced pgvector ``doc_index``: instead of mirroring every
Confluence page into a local vector store, the engine queries
Confluence's REST search API (CQL) live at retrieval time.

The auxiliary LLM turns the turn's trigger context into a short text
query (see :mod:`crewlet.learning.relevant_knowledge`); this module
turns that into a CQL request and returns ranked
:class:`~crewlet.knowledge.protocol.KnowledgeHit` rows.

Auth model: the search authenticates as the *calling agent's* own
Atlassian user when the role carries per-agent credentials in
``mcp_env["atlassian"]`` -- so Confluence enforces page-level
permissions natively, and the engine needs no restricted-page
bookkeeping of its own.  Roles without per-agent credentials fall
back to the org admin/service account the
:class:`~crewlet.notifications.transports.confluence.ConfluenceTransport`
already holds.  A missing or malformed per-agent credential pair is
never an error -- it silently selects the admin path.

Space scoping is **optional**: when the org has configured
``knowledge.confluence_spaces`` the CQL is narrowed with ``space IN
(...)``.  When there are none *and* the role authenticates as its own
Atlassian user, the space filter is dropped entirely -- Confluence's own
ACLs bound what comes back, so the explicit allow-list is just an
optional focus mechanism.  (A unit's own ``confluence_space`` identity
is for routing / writes and never narrows reads.)
A credential-less role searches as the admin account (which sees the
whole instance), so for it an empty space set means *no search* rather
than reading everything the admin can.
"""

from __future__ import annotations

import re
import urllib.parse
from collections.abc import Sequence
from typing import TYPE_CHECKING, Any

from crewlet._logging import get_logger
from crewlet.config import resolve_env_vars
from crewlet.knowledge.accessibility import accessible_spaces
from crewlet.knowledge.protocol import KnowledgeHit

if TYPE_CHECKING:
    import httpx

    from crewlet.notifications.transports.confluence import ConfluenceTransport
    from crewlet.org.models import Organization, Role

logger = get_logger("knowledge.confluence_search")

# Confluence storage format wraps content in XHTML-like tags; strip
# them so the snippet embeds as plain text.  Permissive, not a
# security-critical sanitiser -- the content is already trusted.
_TAG_RE = re.compile(r"<[^>]+>")
_WHITESPACE_RE = re.compile(r"\s+")

# Cap on the aux-generated text fed into the CQL ``text ~`` clause.
# A pathologically long fragment is both a CQL risk and a sign the
# aux model misbehaved.
_MAX_QUERY_CHARS = 200

# Characters of stripped page body kept as the per-hit snippet.
_SNIPPET_CHARS = 200

# Extra rows fetched beyond the caller's ``limit`` so the ancestor
# post-filter (auto-draft exclusion) still leaves enough hits.
_OVERFETCH = 5


def _strip_storage_format(html: str) -> str:
    """Convert Confluence storage-format HTML to plain text."""
    if not html:
        return ""
    no_tags = _TAG_RE.sub(" ", html)
    return _WHITESPACE_RE.sub(" ", no_tags).strip()


def _snippet(text: str, limit: int = _SNIPPET_CHARS) -> str:
    """First sentence / line of ``text``, capped at ``limit`` chars."""
    text = (text or "").strip()
    if not text:
        return ""
    for boundary in ("\n", ". "):
        idx = text.find(boundary)
        if idx > 0:
            text = text[:idx].strip()
            break
    text = text.replace("\n", " ")
    if len(text) > limit:
        text = text[:limit] + "..."
    return text


def _escape_cql(value: str) -> str:
    """Escape a string for use inside a CQL double-quoted literal."""
    return value.replace("\\", "\\\\").replace('"', '\\"')


def build_cql(
    terms: str, space_keys: list[str], *, allow_unscoped: bool = False
) -> str:
    """Build a CQL text query, optionally scoped to ``space_keys``.

    Shape::

        space IN ("ENG","GENERAL") AND type = page AND text ~ "<terms>"

    ``terms`` is capped and escaped; ``space_keys`` are uppercased and
    escaped.  When ``space_keys`` is empty the result depends on
    ``allow_unscoped``: ``True`` (the caller authenticates as its own
    Confluence user, so ACLs bound the result) drops the ``space IN``
    clause and returns ``type = page AND text ~ "<terms>"``; ``False``
    returns ``""`` so the caller skips the request rather than searching
    the admin's whole view.  Empty ``terms`` always returns ``""``.
    """
    terms = (terms or "").strip()[:_MAX_QUERY_CHARS]
    if not terms:
        return ""
    spaces = [k.strip().upper() for k in space_keys if k and k.strip()]
    if spaces:
        space_list = ",".join(f'"{_escape_cql(k)}"' for k in spaces)
        return (
            f'space IN ({space_list}) AND type = page AND text ~ "{_escape_cql(terms)}"'
        )
    if allow_unscoped:
        return f'type = page AND text ~ "{_escape_cql(terms)}"'
    return ""


def _resolve_atlassian_env(role: Role | None) -> dict[str, str]:
    """Resolve a role's ``mcp_env['atlassian']`` env (``${VAR}`` expanded).

    Returns ``{}`` for a missing role or on resolution failure -- callers
    treat that as "no per-agent Atlassian identity".
    """
    if role is None:
        return {}
    try:
        return resolve_env_vars(role.mcp_env.get("atlassian", {}) or {})
    except Exception:
        logger.exception(
            "confluence_search_mcp_env_resolve_failed",
            role=getattr(role, "name", ""),
        )
        return {}


def authenticates_as_self(role: Role | None) -> bool:
    """True when the role carries its own Confluence credentials.

    The query-time search then runs as that agent and Confluence enforces
    its page permissions natively -- so an explicit space allow-list is
    optional and an empty one means "search everything this agent can
    read" rather than "search nothing".
    """
    env = _resolve_atlassian_env(role)
    username = str(env.get("CONFLUENCE_USERNAME") or "").strip()
    api_token = str(env.get("CONFLUENCE_API_TOKEN") or "").strip()
    personal_token = str(env.get("CONFLUENCE_PERSONAL_TOKEN") or "").strip()
    return bool((username and api_token) or personal_token)


class ConfluenceSearcher:
    """Query-time Confluence CQL search over a role's accessible spaces.

    Implements the :class:`~crewlet.knowledge.protocol.KnowledgeSearcher`
    protocol.  Constructed once in :meth:`crewlet.engine.Engine.start`
    whenever a :class:`ConfluenceTransport` is configured -- it needs
    neither a database nor an embedding provider.
    """

    def __init__(self, *, transport: ConfluenceTransport) -> None:
        self._transport = transport

    def can_search(self, *, role: Role | None, org: Organization | None) -> bool:
        """Cheap, no-I/O pre-gate: could :meth:`search` hit anything?

        ``True`` when the org has a configured space scope or the role
        authenticates as its own Confluence user (unscoped, ACL-bound
        search).  ``False`` -- including on a scope-resolution failure
        -- means a search is a guaranteed no-op, so the caller can skip
        the aux-LLM query-generation call entirely.
        """
        spaces = self._scope(org)
        if spaces is None:
            return False
        return bool(spaces) or authenticates_as_self(role)

    def _scope(self, org: Organization | None) -> list[str] | None:
        """Sorted org-wide space scope; ``None`` when resolution failed.

        ``None`` (as opposed to ``[]``) means the org object was
        present but unreadable -- both entry points then fail closed
        (no search) rather than falling through to an unscoped / admin
        search.
        """
        if org is None:
            return []
        try:
            return sorted(accessible_spaces(org))
        except Exception:
            logger.exception("confluence_search_scope_failed")
            return None

    async def search(
        self,
        *,
        query: str,
        role: Role | None,
        org: Organization | None,
        limit: int = 8,
        exclude_ancestors: Sequence[str] | None = None,
    ) -> list[KnowledgeHit]:
        """Search Confluence; return up to ``limit`` ranked page hits.

        The space scope is derived from ``org``
        (:func:`~crewlet.knowledge.accessibility.accessible_spaces`);
        an empty scope searches unscoped for a self-authenticating role
        (Confluence ACLs bound the hits) and nothing for a
        credential-less one.  Pages whose ancestor titles intersect
        ``exclude_ancestors`` are dropped (auto-draft suppression).

        Best-effort: any failure -- empty inputs, no HTTP client, a
        non-200 response, a transport/parse exception -- returns
        ``[]``, and the Plan-phase prefetch degrades to an empty
        knowledge block.
        """
        space_keys = self._scope(org)
        if space_keys is None:
            return []
        cql = build_cql(
            query,
            space_keys,
            allow_unscoped=not space_keys and authenticates_as_self(role),
        )
        if not cql:
            return []
        rest_base = getattr(self._transport, "_rest_base", "") or ""
        if not rest_base:
            logger.debug("confluence_search_no_rest_base")
            return []
        url = (
            f"{rest_base}/rest/api/content/search"
            f"?cql={urllib.parse.quote(cql)}"
            f"&limit={int(limit) + _OVERFETCH}"
            f"&expand=body.storage,space,ancestors"
        )
        resp = await self._get(role, url)
        if resp is None:
            return []
        if resp.status_code != 200:
            logger.warning(
                "confluence_search_non_200",
                status=resp.status_code,
                body_preview=(resp.text or "")[:200],
            )
            return []
        try:
            payload = resp.json()
        except Exception:
            logger.exception("confluence_search_bad_json")
            return []
        return self._parse_results(payload, limit, exclude_ancestors)

    async def _get(self, role: Role | None, url: str) -> Any | None:
        """Issue the GET, authed per-agent when possible, else admin."""
        client, auth_label, owns = self._resolve_client(role)
        if client is None:
            logger.debug("confluence_search_no_client", auth=auth_label)
            return None
        logger.debug("confluence_search_request", auth=auth_label)
        try:
            return await client.get(url)
        except Exception:
            logger.exception("confluence_search_request_failed", auth=auth_label)
            return None
        finally:
            if owns:
                await client.aclose()

    def _resolve_client(
        self, role: Role | None
    ) -> tuple[httpx.AsyncClient | None, str, bool]:
        """Pick the HTTP client for this search.

        Returns ``(client, auth_label, owns_client)``.  Per-agent
        credentials in the role's ``atlassian`` mcp_env win -- the
        search then runs as that agent and Confluence enforces its
        page permissions -- otherwise the transport's shared admin
        client is used.  ``owns_client`` is ``True`` only for the
        per-agent client, which the caller must close.
        """
        env = _resolve_atlassian_env(role)
        username = str(env.get("CONFLUENCE_USERNAME") or "").strip()
        api_token = str(env.get("CONFLUENCE_API_TOKEN") or "").strip()
        personal_token = str(env.get("CONFLUENCE_PERSONAL_TOKEN") or "").strip()
        if username and api_token:
            client = self._transport.new_user_client(username, api_token)
            return client, "per_agent_cloud", True
        if personal_token:
            client = self._transport.new_user_client("", personal_token)
            return client, "per_agent_dc", True
        return getattr(self._transport, "_http_client", None), "admin", False

    def _parse_results(
        self,
        payload: dict[str, Any],
        limit: int,
        exclude_ancestors: Sequence[str] | None,
    ) -> list[KnowledgeHit]:
        """Map a ``content/search`` payload to ranked hits."""
        exclude = {a.strip() for a in (exclude_ancestors or []) if a and a.strip()}
        out: list[KnowledgeHit] = []
        for page in payload.get("results", []) or []:
            page_id = str(page.get("id") or "")
            if not page_id:
                continue
            ancestors = [
                str(a.get("title") or "")
                for a in (page.get("ancestors") or [])
                if a.get("title")
            ]
            if exclude and exclude.intersection(ancestors):
                continue
            space_key = str((page.get("space") or {}).get("key") or "")
            storage = (page.get("body") or {}).get("storage") or {}
            snippet = _snippet(_strip_storage_format(str(storage.get("value") or "")))
            out.append(
                KnowledgeHit(
                    title=str(page.get("title") or ""),
                    url=self._page_url(space_key, page_id),
                    container=space_key,
                    page_id=page_id,
                    snippet=snippet,
                    ancestors=ancestors,
                )
            )
            if len(out) >= int(limit):
                break
        return out

    def _page_url(self, space_key: str, page_id: str) -> str:
        """Build a shareable page URL from the transport's site URL."""
        base = (getattr(self._transport, "_shareable_base_url", "") or "").rstrip("/")
        if not base:
            return ""
        if space_key:
            return f"{base}/spaces/{space_key}/pages/{page_id}"
        return f"{base}/pages/{page_id}"
