"""Async REST client for the subset of the Plane fork's public API.

Thin wrapper over ``httpx`` with ``X-API-Key`` auth and Plane's cursor
pagination.  Covers the **read side** the ``PlaneTransport``
uses for webhook enrichment (work-item subscriber lookups, the
project id→identifier map, credential probing), the page
surface — pages CRUD (list/get/create/update/archive/delete)
and workspace page search — consumed by the knowledge
searcher, the import CLI, and the Tool Skills sync worker, and the
provisioning write endpoints (service accounts, token lifecycle,
members, webhooks) the ``crewlet plane provision`` reconcile drives.
No SDK dependency.  See ``docs/integrations/plane.md``.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

import httpx

from crewlet._logging import get_logger

logger = get_logger("plane.client")

# Hard ceiling on cursor pagination: 50 pages × ``per_page=100`` =
# 5 000 rows, far above any real workspace project list, page list, or
# work-item subscriber list.  The loop runs on the inbound-webhook hot
# path, where an unbounded walk against a malfunctioning server would
# stall ALL webhook processing (the queue awaits handlers
# sequentially), so a finite cap is the backstop behind the
# cursor-progress check.
_MAX_LIST_PAGES = 50

# The page-search endpoint's advertised ``per_page`` ceiling;
# values above it are rejected with a 400, values below 1 likewise, so
# ``search_pages`` clamps into [1, 100] rather than issuing a request
# that is guaranteed to fail.
_SEARCH_MAX_PER_PAGE = 100

# Plane ``Page.access`` values (the fork exposes the integer field).
# Engine-created pages are always PUBLIC and say so explicitly rather
# than relying on the model default: a private page is invisible to
# every non-owner on both the pages-CRUD and page-search read paths, so
# an engine-authored page that defaulted private would vanish for every
# agent but the
# service account that wrote it.
PAGE_ACCESS_PUBLIC = 0
PAGE_ACCESS_PRIVATE = 1


class _UnsetType:
    """Sentinel distinguishing "parameter not provided" from ``None``.

    ``update_page`` sends only the fields the caller provided; for
    ``parent_id`` an explicit ``None`` is itself meaningful (detach the
    page to the project root), so ``None`` cannot double as the
    "not provided" marker.
    """

    def __repr__(self) -> str:  # pragma: no cover - debug repr
        return "<UNSET>"


_UNSET = _UnsetType()


class PlaneProvisionError(Exception):
    """A Plane API call failed.

    Carries the method + path + status so callers (the webhook
    transport, the provisioner) can report exactly which
    endpoint rejected the credential.  The message never includes the
    token — the client scrubs it before raising.  Untrusted raw response
    bodies are capped where they enter (:meth:`PlaneClient._request`),
    NOT here — a caller-crafted remediation message (e.g. the
    username-conflict copy) must never be truncated, whatever its
    length.
    """

    def __init__(
        self,
        message: str,
        *,
        method: str = "",
        path: str = "",
        status: int = 0,
    ) -> None:
        self.method = method
        self.path = path
        self.status = status
        prefix = f"{method} {path} → {status}: " if method else ""
        super().__init__(f"{prefix}{message}")


class PlaneClient:
    """Minimal async Plane REST client.

    ``base_url`` is the fork instance URL (e.g. ``https://plane.example``,
    NOT the ``/api/v1`` base); ``workspace`` is the workspace slug every
    resource path the integration operates on is scoped under.  Auth is
    the ``X-API-Key`` personal / service-account token header.  The
    underlying ``httpx.AsyncClient`` is created lazily on first request
    and released via :meth:`close`.
    """

    def __init__(
        self,
        base_url: str,
        token: str,
        workspace: str,
        *,
        timeout: float = 15.0,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._token = token
        self._workspace = workspace
        self._timeout = timeout
        self._client: httpx.AsyncClient | None = None

    @property
    def workspace(self) -> str:
        """The workspace slug every resource path is scoped under."""
        return self._workspace

    def _http(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(
                base_url=f"{self._base_url}/api/v1",
                headers={"X-API-Key": self._token, "Accept": "application/json"},
                timeout=self._timeout,
            )
        return self._client

    async def close(self) -> None:
        """Close the underlying HTTP client (idempotent)."""
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    # ── low-level ──

    async def _request(
        self,
        method: str,
        path: str,
        *,
        params: dict[str, Any] | None = None,
        json: dict[str, Any] | None = None,
    ) -> Any:
        resp = await self._http().request(method, path, params=params, json=json)
        if resp.status_code < 200 or resp.status_code >= 300:
            message = resp.text
            if self._token:
                # The error message must never leak the credential —
                # e.g. a proxy echoing request headers into an error body.
                message = message.replace(self._token, "[REDACTED]")
            raise PlaneProvisionError(
                # Cap the UNTRUSTED raw body here, at the boundary it
                # enters — a proxy can echo megabytes of HTML.  Crafted
                # messages built by callers are bounded by construction
                # and are never capped (see PlaneProvisionError).
                message[:300],
                method=method,
                path=path,
                status=resp.status_code,
            )
        if not resp.content:
            return None
        try:
            return resp.json()
        except ValueError:
            return None

    async def _get_all(
        self,
        path: str,
        params: dict[str, Any] | None = None,
        *,
        strict: bool = False,
    ) -> list[dict[str, Any]]:
        """GET every page of a cursor-paginated list endpoint.

        Plane's cursor envelope is a dict carrying ``results`` (list) +
        ``next_page_results`` (bool) + ``next_cursor`` (str); the loop
        re-requests with ``cursor=next_cursor`` and ``per_page=100``.
        A bare-list response is a single page and is returned as-is.

        Termination is guaranteed even against a malfunctioning server:
        a ``next_cursor`` that repeats the previous one is a stalled
        walk (Plane encodes per_page/offset inside the cursor, so a fork
        rebase or an intermediary can plausibly echo it) and breaks the
        loop, and ``_MAX_LIST_PAGES`` caps the total page count.

        ``strict`` is the completeness contract: ``False`` (the
        webhook-hot-path default) returns the rows collected so far
        with a warning on any incomplete walk; ``True`` raises
        :class:`PlaneProvisionError` instead — for callers whose result
        must be the COMPLETE enumeration (the Tool Skills boot walk
        seeds a wholesale registry replace, the importer's prune
        decides deletions), a silently truncated list is corruption,
        never a degradation.
        """
        out: list[dict[str, Any]] = []
        page_params: dict[str, Any] = {**(params or {}), "per_page": 100}
        last_cursor = ""
        for _ in range(_MAX_LIST_PAGES):
            data = await self._request("GET", path, params=page_params)
            if isinstance(data, list):
                out.extend(data)
                return out
            if not isinstance(data, dict):
                if strict:
                    raise PlaneProvisionError(
                        "unrecognized list envelope", method="GET", path=path
                    )
                return out
            results = data.get("results")
            if isinstance(results, list):
                out.extend(results)
            elif strict:
                raise PlaneProvisionError(
                    "list envelope carries no results", method="GET", path=path
                )
            if not data.get("next_page_results"):
                return out
            cursor = str(data.get("next_cursor") or "")
            if not cursor:
                if strict:
                    raise PlaneProvisionError(
                        f"more pages advertised but no cursor after {len(out)} rows",
                        method="GET",
                        path=path,
                    )
                return out
            if cursor == last_cursor:
                logger.warning(
                    "plane_pagination_cursor_stalled",
                    path=path,
                    cursor=cursor,
                    rows=len(out),
                )
                if strict:
                    raise PlaneProvisionError(
                        f"pagination cursor stalled after {len(out)} rows",
                        method="GET",
                        path=path,
                    )
                return out
            last_cursor = cursor
            page_params["cursor"] = cursor
        logger.warning(
            "plane_pagination_cap_reached",
            path=path,
            pages=_MAX_LIST_PAGES,
            rows=len(out),
        )
        if strict:
            raise PlaneProvisionError(
                f"pagination page cap reached after {len(out)} rows",
                method="GET",
                path=path,
            )
        return out

    # ── identity ──

    async def get_current_user(self) -> dict[str, Any]:
        """``GET /users/me/`` — the authenticated token's user.

        The one resource path that is NOT workspace-scoped; used for
        credential probing and per-seat identity resolution.
        """
        data = await self._request("GET", "/users/me/")
        return data if isinstance(data, dict) else {}

    # ── projects ──

    async def list_projects(self, *, strict: bool = False) -> list[dict[str, Any]]:
        """``GET /workspaces/{slug}/projects/`` — all projects (paginated).

        Feeds the transport's project id→identifier cache (webhook
        payloads carry project UUIDs while config speaks identifiers).
        ``strict`` keeps the tolerant walk as the webhook-hot-path
        default; the provisioner passes ``strict=True`` because a
        silently truncated project list would mis-classify an existing
        project as missing and (with ``--create-projects``) attempt a
        duplicate create.
        """
        return await self._get_all(
            f"/workspaces/{self._workspace}/projects/", strict=strict
        )

    # ── work-item subscribers ──

    async def list_issue_subscribers(
        self, project_id: str, issue_id: str
    ) -> list[dict[str, Any]]:
        """``GET .../work-items/{issue_id}/subscribers/`` (fork endpoint).

        Each row is an ``IssueSubscriber`` through-model row with the
        subscriber's user profile embedded under ``member`` — the row's
        own ``id`` is the subscription row, never a user id (see
        ``crewlet.notifications.transports.plane._subscriber_ids``).
        """
        return await self._get_all(
            f"/workspaces/{self._workspace}/projects/{project_id}"
            f"/work-items/{issue_id}/subscribers/"
        )

    # ── pages (fork pages CRUD + page search) ──
    #
    # Two distinct pinned row shapes — nothing may mix them up:
    #
    # * The **page object** (``PageAPISerializer``): ``{id, name,
    #   description_html, access, color, is_locked, archived_at,
    #   view_props, logo_props, external_id, external_source, owned_by,
    #   parent, sort_order, created_at, updated_at, created_by,
    #   updated_by}`` — the parent ref is ``parent`` (NOT ``parent_id``)
    #   and there is NO project ref (pages attach to projects through a
    #   through-table; the project lives in the URL, never the row).
    # * The **search row** (``PageSearchSerializer``): ``{id, name,
    #   project_id, parent_id, updated_at, snippet}`` — the only shape
    #   that carries ``project_id`` / ``parent_id``.

    async def search_pages(
        self,
        query: str,
        *,
        projects: list[str] | None = None,
        limit: int = 100,
    ) -> list[dict[str, Any]]:
        """``GET /workspaces/{slug}/pages/search/`` (fork endpoint).

        ``query`` is sent VERBATIM — the fork tokenises it server-side
        (tokenised AND search: AND across whitespace-split tokens, each matched
        case-insensitively against page name and stripped body text),
        so a multi-keyword aux-LLM query needs no client-side rewriting.
        ``projects`` is an optional list of project **UUIDs**
        (identifiers 400), sent comma-joined; results are ordered by
        ``-updated_at`` (recency, not relevance) and exclude archived
        pages.

        A SINGLE request — no cursor walk.  Search consumers want the
        top of the result window; draining every result page through
        ``_get_all`` would turn one bounded call into an unbounded
        crawl.  ``per_page`` is ``limit`` clamped into the endpoint's
        accepted ``[1, 100]`` range, and the cursor envelope
        (``{results, next_page_results, next_cursor}``) is unwrapped
        here (a bare-list response is tolerated as a single page).
        """
        params: dict[str, Any] = {
            "query": query,
            "per_page": max(1, min(int(limit), _SEARCH_MAX_PER_PAGE)),
        }
        if projects:
            params["projects"] = ",".join(projects)
        data = await self._request(
            "GET", f"/workspaces/{self._workspace}/pages/search/", params=params
        )
        if isinstance(data, list):
            return data
        if isinstance(data, dict):
            results = data.get("results")
            if isinstance(results, list):
                return results
        return []

    async def list_pages(
        self,
        project_id: str,
        *,
        search: str | None = None,
        fields: list[str] | None = None,
        page_type: str | None = None,
    ) -> list[dict[str, Any]]:
        """``GET …/projects/{project_id}/pages/`` — full cursor walk.

        ``search`` is a name-``icontains`` filter (cloud-docs
        semantics); callers needing an exact title MUST post-filter.
        ``fields`` restricts the serialized columns (comma-joined; e.g.
        ``["id", "name", "external_id", "external_source",
        "description_html"]`` lets the boot walk and the importer do ONE
        enumeration instead of a per-page ``get_page`` fan-out).
        ``page_type`` maps to the pages API's ``type`` filter (``all`` / ``public``
        / ``private`` / ``archived``; the server default excludes
        archived pages).

        Always a **strict** walk: every consumer of a page enumeration
        (registry seeding, importer indexing, prune, the searcher's
        draft-parent lookup) needs the COMPLETE list or an error —
        a silently truncated enumeration would wipe registry entries or
        mis-target prune — so an incomplete walk raises
        :class:`PlaneProvisionError` instead of returning partial rows.
        """
        params: dict[str, Any] = {}
        if search:
            params["search"] = search
        if fields:
            params["fields"] = ",".join(fields)
        if page_type:
            params["type"] = page_type
        return await self._get_all(
            f"/workspaces/{self._workspace}/projects/{project_id}/pages/",
            params,
            strict=True,
        )

    async def get_page(self, project_id: str, page_id: str) -> dict[str, Any]:
        """``GET …/pages/{page_id}/`` — the full page object.

        Includes ``description_html``.  404s for a page outside
        ``project_id`` (the queryset is project-scoped), which is how
        the sync worker scopes workspace-wide page webhooks for free.
        """
        data = await self._request(
            "GET",
            f"/workspaces/{self._workspace}/projects/{project_id}/pages/{page_id}/",
        )
        return data if isinstance(data, dict) else {}

    async def create_page(
        self,
        project_id: str,
        *,
        name: str,
        description_html: str,
        parent_id: str | None = None,
        external_id: str | None = None,
        external_source: str | None = None,
        access: int = PAGE_ACCESS_PUBLIC,
    ) -> dict[str, Any]:
        """``POST …/pages/`` — create a page; returns the created object.

        ``parent_id`` is sent as ``{"parent": <uuid>}`` (the pages API's write
        field name).  ``external_id`` / ``external_source`` are the
        idempotency identity for integration-authored pages
        (project-wide unique; the fork 409s on a duplicate pair, naming
        the conflicting page id when visible) — the two must be sent
        together, the serializer rejects half a pair.  ``access`` is
        sent explicitly and defaults public (see
        :data:`PAGE_ACCESS_PUBLIC`).
        """
        body: dict[str, Any] = {
            "name": name,
            "description_html": description_html,
            "access": access,
        }
        if parent_id:
            body["parent"] = parent_id
        if external_id is not None:
            body["external_id"] = external_id
        if external_source is not None:
            body["external_source"] = external_source
        data = await self._request(
            "POST",
            f"/workspaces/{self._workspace}/projects/{project_id}/pages/",
            json=body,
        )
        return data if isinstance(data, dict) else {}

    async def update_page(
        self,
        project_id: str,
        page_id: str,
        *,
        name: str | None = None,
        description_html: str | None = None,
        parent_id: str | None | _UnsetType = _UNSET,
        external_id: str | None = None,
        external_source: str | None = None,
    ) -> dict[str, Any]:
        """``PATCH …/pages/{page_id}/`` — partial update.

        Only provided fields are sent.  ``parent_id`` distinguishes
        three states: omitted (unchanged), a UUID (reparent — sent as
        ``{"parent": <uuid>}``), and an explicit ``None`` (detach to
        the project root).  ``external_id`` / ``external_source`` move
        together (same pair contract as :meth:`create_page`; the fork
        409s when the new identity collides).  Locked and archived
        pages reject updates with a 400.
        """
        body: dict[str, Any] = {}
        if name is not None:
            body["name"] = name
        if description_html is not None:
            body["description_html"] = description_html
        if not isinstance(parent_id, _UnsetType):
            body["parent"] = parent_id
        if external_id is not None:
            body["external_id"] = external_id
        if external_source is not None:
            body["external_source"] = external_source
        data = await self._request(
            "PATCH",
            f"/workspaces/{self._workspace}/projects/{project_id}/pages/{page_id}/",
            json=body,
        )
        return data if isinstance(data, dict) else {}

    async def archive_page(self, project_id: str, page_id: str) -> dict[str, Any]:
        """``POST …/pages/{page_id}/archive/`` — archive a page.

        Archiving CASCADES to every descendant page (the fork's
        recursive CTE), so this is safe for flat skill pages and must
        not be pointed at a doc tree whose children should survive.
        Returns the response body (``{"archived_at": "<date>"}``).
        Archive-then-delete is the only deletion path — see
        :meth:`delete_page`.
        """
        data = await self._request(
            "POST",
            f"/workspaces/{self._workspace}/projects/{project_id}"
            f"/pages/{page_id}/archive/",
        )
        return data if isinstance(data, dict) else {}

    async def unarchive_page(self, project_id: str, page_id: str) -> None:
        """``DELETE …/pages/{page_id}/archive/`` — restore a page.

        The same fork endpoint ``archive_page`` POSTs to, routed for
        DELETE (``PageArchiveAPIEndpoint``); restoring cascades to the
        descendants the archive took down.  Used to roll back a
        half-applied prune: when :meth:`archive_page` succeeded but
        :meth:`delete_page` was refused (owner-or-admin-only 403), the
        page must not be left archived — an archived page is invisible
        to the importer's enumeration while its ``external_id`` still
        409s every future create, a permanent-conflict trap.
        """
        await self._request(
            "DELETE",
            f"/workspaces/{self._workspace}/projects/{project_id}"
            f"/pages/{page_id}/archive/",
        )

    async def delete_page(self, project_id: str, page_id: str) -> None:
        """``DELETE …/pages/{page_id}/`` — permanently delete (204).

        PRECONDITION: the page must already be archived — the fork
        400s (``"The page should be archived before deleting"``) on a
        live page, so callers run :meth:`archive_page` first.  Deletion
        is additionally owner-or-project-admin only (403 otherwise);
        prune-style callers isolate per-page failures rather than
        aborting the run on a human-owned page.
        """
        await self._request(
            "DELETE",
            f"/workspaces/{self._workspace}/projects/{project_id}/pages/{page_id}/",
        )

    # ── provisioning: service accounts ──
    #
    # Pinned shapes (fork ``pr9399`` — ``service_account`` serializers,
    # views, urls):
    #
    # * Create request: ``name`` (required, ≤255 — doubles as the first
    #   API token's label), ``role`` (STRING choice ``admin|member|
    #   guest``; the API default is ``admin`` while crewlet's config
    #   default is ``member``, so callers must ALWAYS pass it
    #   explicitly), optional ``username`` (globally unique, ≤128; a
    #   taken name → 409 ``USERNAME_ALREADY_EXISTS``, never silently
    #   mutated), ``display_name`` (≤255, falls back to ``name``),
    #   ``description`` (stored on the token).  There is NO ``email``
    #   field on the HTTP endpoint.
    # * Create response 201: ``{id (user UUID), username, email,
    #   display_name, role (INT: 20 admin / 15 member / 5 guest),
    #   workspace, token}`` — ``token`` is the plaintext first API
    #   token, returned exactly once (redacted from Plane's activity
    #   log).
    # * Token rows from the list endpoint NEVER carry the token value.
    # * DELETE cascade: deactivates ALL the account's tokens, removes
    #   its project + workspace memberships, deactivates the user (row
    #   preserved for attribution); 400 ``NOT_A_SERVICE_ACCOUNT`` for a
    #   human/other bot (the guard that makes decommission human-safe);
    #   404 for unknown/already-decommissioned (idempotent).

    async def create_service_account(
        self,
        *,
        name: str,
        role: str,
        username: str | None = None,
        display_name: str | None = None,
        description: str | None = None,
    ) -> dict[str, Any]:
        """``POST /workspaces/{slug}/service-accounts/`` — 201.

        ``role`` is the STRING choice and is required here even though
        the API has a default: the API default is ``admin`` (silently
        privilege-escalating for an omitting caller), so this client
        never omits it.  The response echoes the effective identity —
        callers that supplied ``username`` must verify the echo, since
        an instance with only the upstream service-accounts API
        ignores caller-chosen identity
        fields and generates synthetic ``svc_<uuid>`` values instead.
        """
        body: dict[str, Any] = {"name": name, "role": role}
        if username is not None:
            body["username"] = username
        if display_name is not None:
            body["display_name"] = display_name
        if description is not None:
            body["description"] = description
        data = await self._request(
            "POST", f"/workspaces/{self._workspace}/service-accounts/", json=body
        )
        return data if isinstance(data, dict) else {}

    async def delete_service_account(self, user_id: str) -> None:
        """``DELETE …/service-accounts/{user_id}/`` — 204.

        Decommission: cascades token deactivation + membership removal
        and deactivates the user (row preserved for attribution).
        Raises with ``status == 400`` (``NOT_A_SERVICE_ACCOUNT``) for a
        human or non-service bot and ``404`` for an unknown or
        already-decommissioned account (idempotent re-runs).
        """
        await self._request(
            "DELETE", f"/workspaces/{self._workspace}/service-accounts/{user_id}/"
        )

    async def list_service_account_tokens(self, user_id: str) -> list[dict[str, Any]]:
        """``GET …/service-accounts/{user_id}/tokens/`` — all rows.

        Cursor-paginated rows ``{id, label, description, is_active,
        is_service, user_type, created_at, updated_at, expired_at,
        last_used}`` — NEVER the token value.  Revoked tokens are
        omitted; rotated-away ones remain with ``is_active: false``.
        The walk is strict: rotation targets a row from this list, and
        targeting on a silently truncated list is corruption.
        """
        return await self._get_all(
            f"/workspaces/{self._workspace}/service-accounts/{user_id}/tokens/",
            strict=True,
        )

    async def create_service_account_token(
        self,
        user_id: str,
        *,
        label: str,
        description: str = "",
        expired_at: datetime | None = None,
    ) -> dict[str, Any]:
        """``POST …/service-accounts/{user_id}/tokens/`` — 201.

        Response ``{id, label, is_active, created_at, expired_at,
        token}`` — the plaintext ``token`` is returned exactly once.
        ``expired_at=None`` omits the key: an omitted expiry means the
        token NEVER expires (Plane semantics — not GitLab's
        instance-default), and an explicit JSON ``null`` means the same
        on this endpoint, so omission is the canonical spelling.
        """
        body: dict[str, Any] = {"label": label, "description": description}
        if expired_at is not None:
            body["expired_at"] = expired_at.isoformat()
        data = await self._request(
            "POST",
            f"/workspaces/{self._workspace}/service-accounts/{user_id}/tokens/",
            json=body,
        )
        return data if isinstance(data, dict) else {}

    async def rotate_service_account_token(
        self,
        user_id: str,
        token_id: str,
        *,
        expired_at: datetime | None | _UnsetType = _UNSET,
    ) -> dict[str, Any]:
        """``POST …/tokens/{token_id}/rotate/`` — 201, value once.

        Atomically mints a replacement (inheriting label + description)
        and deactivates the source.  ``expired_at`` is three-state,
        exactly as the fork's ``OmittableDateTimeField`` pins it:
        omitted (:data:`_UNSET`) ⇒ the key is absent from the body and
        the replacement INHERITS the source token's expiry; ``None`` ⇒
        an explicit JSON ``null``, the replacement never expires; a
        ``datetime`` ⇒ that (future) instant.  Raises with ``409``
        (``TOKEN_NOT_ACTIVE``) when the source is not active and
        ``400`` (``SOURCE_TOKEN_EXPIRY_ELAPSED``) when inheriting an
        already-elapsed expiry.
        """
        body: dict[str, Any] = {}
        if not isinstance(expired_at, _UnsetType):
            body["expired_at"] = (
                expired_at.isoformat() if expired_at is not None else None
            )
        data = await self._request(
            "POST",
            f"/workspaces/{self._workspace}/service-accounts/{user_id}"
            f"/tokens/{token_id}/rotate/",
            json=body,
        )
        return data if isinstance(data, dict) else {}

    async def revoke_service_account_token(self, user_id: str, token_id: str) -> None:
        """``DELETE …/tokens/{token_id}/`` — 204 (soft-delete)."""
        await self._request(
            "DELETE",
            f"/workspaces/{self._workspace}/service-accounts/{user_id}"
            f"/tokens/{token_id}/",
        )

    async def probe_workspace(self) -> int:
        """Workspace-slug probe for the credential preflight (B5 step 0).

        ``GET /workspaces/{slug}/projects/`` is the cheapest
        workspace-scoped, NON-admin route: any member of the workspace
        gets 200 (possibly empty), while a mistyped/unknown slug can
        never satisfy Plane's slug-filtered permission classes and
        yields 403 (or 404) *whatever* the token's real permissions are
        — so, run AFTER ``get_current_user`` has proven the credential
        authenticates, this distinguishes a bad
        ``integrations.plane.workspace`` from a bad credential and lets
        a later admin-gated 403 be attributed to permission alone.
        Returns the status code instead of raising.
        """
        try:
            await self._request("GET", f"/workspaces/{self._workspace}/projects/")
        except PlaneProvisionError as exc:
            return exc.status
        return 200

    async def probe_service_accounts(self) -> int:
        """Capability probe for the service-account create route (B5).

        Issues ``GET`` against a route the fork registers POST-only, so
        the status is a clean presence signal: **405** ⇒ route present
        (Django resolves the URL, DRF checks permissions, THEN rejects
        the method); **404** ⇒ no service-accounts API at all (stock
        CE); **403** ⇒ present but the operator is not a workspace
        admin (DRF permissions run before the method rejection, so 403
        also proves presence).  Returns the status code instead of
        raising so the provisioner can run its whole decision procedure
        BEFORE any mutation.
        """
        try:
            await self._request(
                "GET", f"/workspaces/{self._workspace}/service-accounts/"
            )
        except PlaneProvisionError as exc:
            return exc.status
        return 200

    async def probe_service_account_tokens(self) -> int:
        """Capability probe for the token-lifecycle route family.

        Issues ``PATCH`` (a method no token route allows) against the
        zero-UUID token collection, so no real account is ever touched:
        **405** ⇒ token-lifecycle routes present; **404** ⇒ absent (stock
        CE or service-accounts-only); **403** ⇒ present, operator not a
        workspace admin.  Returns the status code instead of raising.
        """
        try:
            await self._request(
                "PATCH",
                f"/workspaces/{self._workspace}/service-accounts/"
                "00000000-0000-0000-0000-000000000000/tokens/",
            )
        except PlaneProvisionError as exc:
            return exc.status
        return 200

    # ── provisioning: workspace members / projects (CE stock) ──

    async def list_workspace_members(self) -> list[dict[str, Any]]:
        """``GET /workspaces/{slug}/members/`` — every workspace member.

        CE serves a BARE array (no cursor envelope) of FLAT user dicts:
        ``UserLiteSerializer(member).data`` with the workspace ``role``
        int appended by the view — so ``row["id"]`` IS the member's
        user UUID (no through-row indirection here, unlike
        :meth:`list_issue_subscribers`).  Stock ``UserLiteSerializer``
        carries NO ``username``; the fork appends ``username`` /
        ``is_bot`` / ``bot_type`` to each row — the provisioner
        hard-requires those and preflight-detects their absence.  Bot
        members ARE listed on this public endpoint (only the internal
        app API hides them).  Requires workspace admin.  The walk is
        strict: create-vs-exists decisions and decommission targeting
        key off this enumeration, so a truncated list is corruption.
        """
        return await self._get_all(
            f"/workspaces/{self._workspace}/members/", strict=True
        )

    async def add_project_member(
        self, project_id: str, *, member_id: str, role: int
    ) -> dict[str, Any] | None:
        """``POST …/projects/{project_id}/members/`` — add a member.

        Pinned from CE ``preview`` (``api/serializers/member.py``
        ``ProjectMemberSerializer`` + ``api/views/member.py``): writable
        fields are ``member`` (an EXISTING workspace member's user
        UUID — "Member not found in workspace"/``INVALID_MEMBER``
        otherwise) and ``role`` (the INT 20/15/5, validated against the
        ROLE enum).  Permission is project-admin.  Returns the created
        ``{id (ProjectMember row pk), member, role}`` — or ``None``
        when a 4xx UNAMBIGUOUSLY names an existing/duplicate membership
        (fork-side message improvements).  A duplicate add on stock CE
        instead violates ``ProjectMember``'s project+member unique
        constraint, which ``BaseAPIView`` maps to the GENERIC 400
        ``{"error": "The payload is not valid"}`` — generic because
        *any* ``IntegrityError`` from the same view (e.g.
        ``ProjectMember.save()``'s ``ProjectUserProperty`` creation)
        produces the identical body, so that 400 is RAISED for the
        caller to confirm against the project's member rows before
        treating it as "exists" (the provisioner does exactly that).
        Anything else raises.
        """
        try:
            data = await self._request(
                "POST",
                f"/workspaces/{self._workspace}/projects/{project_id}/members/",
                json={"member": member_id, "role": role},
            )
        except PlaneProvisionError as exc:
            message = str(exc).lower()
            duplicate = exc.status in (400, 409) and (
                "already exist" in message or "duplicate" in message
            )
            if duplicate:
                return None
            raise
        return data if isinstance(data, dict) else {}

    async def list_project_members_lite(self, project_id: str) -> list[dict[str, Any]]:
        """``GET …/projects/{project_id}/project-members-lite/`` — rows.

        Pinned from CE ``preview`` (``ProjectMemberLiteAPISerializer``):
        cursor-paginated FLAT rows ``{id, first_name, last_name, email,
        avatar, avatar_url, display_name, role, is_active, is_bot}``
        where ``id`` is the member's USER uuid (``source=member.id`` —
        NOT the ``ProjectMember`` row pk) and ``role`` is the
        PROJECT-level role int.  The only public surface exposing
        per-project roles; the provisioner uses it for its
        project-role drift note.  Requires project membership — a
        workspace admin who is not a member of the project gets 403,
        which callers isolate.
        """
        return await self._get_all(
            f"/workspaces/{self._workspace}/projects/{project_id}"
            f"/project-members-lite/",
            strict=True,
        )

    async def create_project(self, *, name: str, identifier: str) -> dict[str, Any]:
        """``POST /workspaces/{slug}/projects/`` — create a project (201).

        Body ``{"name", "identifier"}`` → 201 ``{"id", …}`` (pinned by
        the fork's service-account contract tests, which create projects exactly
        this way).  The creating credential becomes the project's
        admin.  A duplicate identifier is rejected by the server
        (surfacing as a raised error, never a silent overwrite).
        """
        data = await self._request(
            "POST",
            f"/workspaces/{self._workspace}/projects/",
            json={"name": name, "identifier": identifier},
        )
        return data if isinstance(data, dict) else {}

    # ── provisioning: workspace webhooks ──
    #
    # Pinned shapes (``pr9398``): ``secret_key`` (``plane_wh_…``) is
    # server-generated, read-only as input (a supplied value is
    # discarded), and returned ONLY in the create 201 — list, retrieve
    # and PATCH responses use the Lite serializer, which is structurally
    # the full shape minus the secret.  Duplicate URL per workspace →
    # 409 ``{"error": "URL already exists for the workspace"}`` on both
    # POST and PATCH.  Entity toggles are booleans (``project``,
    # ``issue``, ``module``, ``cycle``, ``issue_comment`` — plus
    # ``page`` from the fork's webhook serializer; on a server without
    # page-webhook support DRF silently drops the unknown field, so callers must verify
    # the echo).

    async def list_webhooks(self) -> list[dict[str, Any]]:
        """``GET /workspaces/{slug}/webhooks/`` — a BARE JSON array.

        ``WebhookLiteSerializer`` rows — never includes ``secret_key``
        (the secret is single-show at creation, structurally excluded
        from every read).
        """
        data = await self._request("GET", f"/workspaces/{self._workspace}/webhooks/")
        return data if isinstance(data, list) else []

    async def create_webhook(self, *, url: str, **toggles: Any) -> dict[str, Any]:
        """``POST /workspaces/{slug}/webhooks/`` — create (201).

        The ONLY response that ever carries ``secret_key`` — callers
        must capture it immediately, it cannot be re-read.  ``toggles``
        are the entity flags (``project`` / ``issue`` / ``module`` /
        ``cycle`` / ``issue_comment`` / ``page``) plus ``is_active``.
        A server without page-webhook support silently drops ``page``
        (DRF ignores unknown
        fields), so callers verify the echo.  Duplicate URL → raises
        with ``status == 409``; SSRF-guarded targets → 400 unless the
        instance sets the fork's ``WEBHOOK_ALLOW_PRIVATE_URLS=1``.
        """
        data = await self._request(
            "POST",
            f"/workspaces/{self._workspace}/webhooks/",
            json={"url": url, **toggles},
        )
        return data if isinstance(data, dict) else {}

    async def update_webhook(self, webhook_id: str, **fields: Any) -> dict[str, Any]:
        """``PATCH …/webhooks/{pk}/`` — partial update (200, Lite shape).

        The response never carries ``secret_key`` and the field can
        never be written — secret rotation is delete + recreate.  A
        changed URL is re-validated (409 on conflict, SSRF guard).
        """
        data = await self._request(
            "PATCH",
            f"/workspaces/{self._workspace}/webhooks/{webhook_id}/",
            json=fields,
        )
        return data if isinstance(data, dict) else {}

    async def delete_webhook(self, webhook_id: str) -> None:
        """``DELETE …/webhooks/{pk}/`` — 204."""
        await self._request(
            "DELETE", f"/workspaces/{self._workspace}/webhooks/{webhook_id}/"
        )
