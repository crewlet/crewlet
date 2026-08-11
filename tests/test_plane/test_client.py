"""Tests for the Plane REST client (httpx MockTransport)."""

from __future__ import annotations

import json as jsonlib

import httpx
import pytest

from crewlet.plane.client import PlaneClient, PlaneProvisionError

_TOKEN = "plane_api_supersecrettoken123"

# Pinned work-item subscriber-list fixture: each row is an IssueSubscriber
# through-model row — ``id`` is the subscription ROW's UUID (never a
# user id), ``subscriber`` is the user FK, and the user profile is
# embedded under ``member``.
SUBSCRIBER_ROWS = [
    {
        "id": "0e0e0e0e-0000-0000-0000-00000000000a",
        "subscriber": "11111111-1111-1111-1111-111111111111",
        "member": {
            "id": "11111111-1111-1111-1111-111111111111",
            "email": "swe@plane.test",
            "display_name": "Agent SWE",
        },
        "issue": "beefbeef-0000-0000-0000-000000000002",
        "project": "facefeed-0000-0000-0000-000000000001",
        "workspace": "ws-uuid",
        "created_at": "2026-07-26T10:00:00Z",
        "updated_at": "2026-07-26T10:00:00Z",
    },
    {
        "id": "0e0e0e0e-0000-0000-0000-00000000000b",
        "subscriber": "22222222-2222-2222-2222-222222222222",
        "member": {
            "id": "22222222-2222-2222-2222-222222222222",
            "email": "lead@plane.test",
            "display_name": "Tech Lead",
        },
        "issue": "beefbeef-0000-0000-0000-000000000002",
        "project": "facefeed-0000-0000-0000-000000000001",
        "workspace": "ws-uuid",
        "created_at": "2026-07-26T10:00:00Z",
        "updated_at": "2026-07-26T10:00:00Z",
    },
]


# Pinned page-object fixture (``PageAPISerializer``): the
# parent ref is ``parent`` (NOT ``parent_id``), ``external_id`` /
# ``external_source`` are exposed, and there is NO project ref — pages
# attach to projects through a through-table, the project lives in the
# URL only.  Engine code may rely ONLY on id, name, description_html,
# parent, external_id/external_source, access, archived_at,
# created_at, updated_at.
PAGE_OBJECT = {
    "id": "9a9a9a9a-0000-0000-0000-000000000001",
    "name": "Deploy Runbook",
    "description_html": "<p>How to deploy.</p>",
    "access": 0,
    "color": "",
    "is_locked": False,
    "archived_at": None,
    "view_props": {},
    "logo_props": {},
    "external_id": "doc:Deploy Runbook",
    "external_source": "crewlet",
    "owned_by": "11111111-1111-1111-1111-111111111111",
    "parent": None,
    "sort_order": 65535.0,
    "created_at": "2026-07-20T10:00:00Z",
    "updated_at": "2026-07-26T10:00:00Z",
    "created_by": "11111111-1111-1111-1111-111111111111",
    "updated_by": "11111111-1111-1111-1111-111111111111",
}

# Pinned search-row fixture (``PageSearchSerializer``): exactly
# {id, name, project_id, parent_id, updated_at, snippet} — the ONLY
# page shape carrying ``project_id`` / ``parent_id``.  Results are
# ordered by ``-updated_at`` (recency, not relevance).  Matching
# semantics: the query string is whitespace-tokenised
# and every token must match (AND), each case-insensitively against
# page name OR stripped body text — the engine sends the raw
# multi-keyword aux query verbatim and never rewrites it.
SEARCH_ROWS = [
    {
        "id": "9a9a9a9a-0000-0000-0000-000000000001",
        "name": "Deploy Runbook",
        "project_id": "facefeed-0000-0000-0000-000000000001",
        "parent_id": None,
        "updated_at": "2026-07-26T10:00:00Z",
        "snippet": "…how to deploy the payments service…",
    },
    {
        "id": "9a9a9a9a-0000-0000-0000-000000000002",
        "name": "Rollback Checklist",
        "project_id": "facefeed-0000-0000-0000-000000000001",
        "parent_id": "9a9a9a9a-0000-0000-0000-000000000001",
        "updated_at": "2026-07-25T10:00:00Z",
        "snippet": "",
    },
]

_PROJECT = "facefeed-0000-0000-0000-000000000001"
_PAGE = "9a9a9a9a-0000-0000-0000-000000000001"


def _client(handler) -> PlaneClient:
    c = PlaneClient("https://plane.test", _TOKEN, "testco")
    c._client = httpx.AsyncClient(
        base_url="https://plane.test/api/v1",
        headers={"X-API-Key": _TOKEN, "Accept": "application/json"},
        transport=httpx.MockTransport(handler),
    )
    return c


async def test_get_current_user_sends_x_api_key():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["auth"] = request.headers.get("X-API-Key")
        seen["path"] = request.url.path
        return httpx.Response(200, json={"id": "ABCD-1234", "email": "me@plane.test"})

    c = _client(handler)
    me = await c.get_current_user()
    await c.close()
    assert me["id"] == "ABCD-1234"
    assert seen["auth"] == _TOKEN
    assert seen["path"] == "/api/v1/users/me/"


async def test_list_projects_follows_cursor_pagination():
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(dict(request.url.params))
        cursor = request.url.params.get("cursor")
        if cursor is None:
            return httpx.Response(
                200,
                json={
                    "results": [{"id": "p1", "identifier": "ENG"}],
                    "next_page_results": True,
                    "next_cursor": "100:1:0",
                },
            )
        return httpx.Response(
            200,
            json={
                "results": [{"id": "p2", "identifier": "PROD"}],
                "next_page_results": False,
                "next_cursor": "",
            },
        )

    c = _client(handler)
    projects = await c.list_projects()
    await c.close()
    assert [p["identifier"] for p in projects] == ["ENG", "PROD"]
    assert calls[0]["per_page"] == "100"
    assert calls[1]["cursor"] == "100:1:0"


async def test_constant_next_cursor_terminates_and_warns(caplog):
    """A server that echoes the same ``next_cursor`` forever (Plane
    encodes per_page/offset inside the cursor, so a fork rebase or an
    intermediary can plausibly do this) must terminate — the loop runs
    on the inbound-webhook hot path where an unbounded walk would stall
    all webhook processing."""
    calls: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(dict(request.url.params))
        return httpx.Response(
            200,
            json={
                "results": [{"id": f"p{len(calls)}"}],
                "next_page_results": True,
                "next_cursor": "100:1:0",
            },
        )

    c = _client(handler)
    projects = await c.list_projects()
    await c.close()
    # First page (no cursor) + exactly one page at the stuck cursor.
    assert len(calls) == 2
    assert [p["id"] for p in projects] == ["p1", "p2"]
    assert "plane_pagination_cursor_stalled" in caplog.text


async def test_pagination_hard_page_cap(caplog):
    """Even with an always-advancing cursor the walk stops at the hard
    page cap (50 pages × per_page=100 = 5000 rows — far above any real
    workspace project or subscriber list) and warns, naming the path."""
    calls: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(dict(request.url.params))
        return httpx.Response(
            200,
            json={
                "results": [{"id": f"p{len(calls)}"}],
                "next_page_results": True,
                "next_cursor": f"100:{len(calls)}:0",
            },
        )

    c = _client(handler)
    projects = await c.list_projects()
    await c.close()
    assert len(calls) == 50
    assert len(projects) == 50
    assert "plane_pagination_cap_reached" in caplog.text
    assert "/workspaces/testco/projects/" in caplog.text


async def test_bare_list_response_is_single_page():
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request.url.path)
        return httpx.Response(200, json=[{"id": "p1", "identifier": "ENG"}])

    c = _client(handler)
    projects = await c.list_projects()
    await c.close()
    assert projects == [{"id": "p1", "identifier": "ENG"}]
    assert len(calls) == 1
    assert calls[0] == "/api/v1/workspaces/testco/projects/"


async def test_list_issue_subscribers_member_embedded_fixture():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        return httpx.Response(200, json={"results": SUBSCRIBER_ROWS})

    c = _client(handler)
    rows = await c.list_issue_subscribers(
        "facefeed-0000-0000-0000-000000000001",
        "beefbeef-0000-0000-0000-000000000002",
    )
    await c.close()
    assert seen["path"] == (
        "/api/v1/workspaces/testco/projects/facefeed-0000-0000-0000-000000000001"
        "/work-items/beefbeef-0000-0000-0000-000000000002/subscribers/"
    )
    # The client returns the raw rows — extraction is the transport's
    # ``_subscriber_ids`` job (the row's own ``id`` is never a user id).
    assert [r["member"]["id"] for r in rows] == [
        "11111111-1111-1111-1111-111111111111",
        "22222222-2222-2222-2222-222222222222",
    ]


async def test_error_maps_to_provision_error():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(403, text="forbidden")

    c = _client(handler)
    with pytest.raises(PlaneProvisionError) as exc:
        await c.get_current_user()
    await c.close()
    assert exc.value.status == 403
    assert exc.value.method == "GET"
    assert exc.value.path == "/users/me/"
    assert "403" in str(exc.value)


async def test_error_never_leaks_the_token():
    def handler(request: httpx.Request) -> httpx.Response:
        # A proxy echoing request headers into the error body.
        return httpx.Response(400, text=f"bad key: {_TOKEN} rejected")

    c = _client(handler)
    with pytest.raises(PlaneProvisionError) as exc:
        await c.list_projects()
    await c.close()
    assert _TOKEN not in str(exc.value)
    assert "[REDACTED]" in str(exc.value)


async def test_close_is_idempotent():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={})

    c = _client(handler)
    await c.close()
    await c.close()
    assert c._client is None


# ---------------------------------------------------------------------------
# search_pages
# ---------------------------------------------------------------------------


async def test_search_pages_single_request_raw_query_and_params():
    """One request, raw multi-keyword query verbatim (the server tokenises
    server-side), comma-joined project UUIDs, per_page=min(limit, 100)
    — and NO cursor walk even when the envelope advertises more."""
    calls: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append({"path": request.url.path, "params": dict(request.url.params)})
        return httpx.Response(
            200,
            json={
                "results": SEARCH_ROWS,
                "next_page_results": True,
                "next_cursor": "20:1:0",
            },
        )

    c = _client(handler)
    rows = await c.search_pages(
        "latency spike rollback incident",
        projects=[_PROJECT, "facefeed-0000-0000-0000-000000000002"],
        limit=11,
    )
    await c.close()
    assert rows == SEARCH_ROWS
    assert len(calls) == 1  # a search never drains the cursor
    assert calls[0]["path"] == "/api/v1/workspaces/testco/pages/search/"
    params = calls[0]["params"]
    assert params["query"] == "latency spike rollback incident"
    assert params["projects"] == (f"{_PROJECT},facefeed-0000-0000-0000-000000000002")
    assert params["per_page"] == "11"


async def test_search_pages_per_page_clamped_to_endpoint_range():
    """The search endpoint 400s outside [1, 100] — the client clamps rather than issuing
    a request that is guaranteed to fail."""
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request.url.params["per_page"])
        return httpx.Response(200, json={"results": []})

    c = _client(handler)
    await c.search_pages("q", limit=250)
    await c.search_pages("q", limit=0)
    await c.close()
    assert seen == ["100", "1"]


async def test_search_pages_omits_projects_param_when_unscoped():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["params"] = dict(request.url.params)
        return httpx.Response(200, json={"results": []})

    c = _client(handler)
    await c.search_pages("deploy")
    await c.close()
    assert "projects" not in seen["params"]


async def test_search_pages_tolerates_bare_list_and_bad_shapes():
    responses = iter(
        [
            httpx.Response(200, json=SEARCH_ROWS),  # bare list
            httpx.Response(200, json={"unexpected": True}),  # no results key
            httpx.Response(200, json="what"),  # not a container
        ]
    )

    def handler(request: httpx.Request) -> httpx.Response:
        return next(responses)

    c = _client(handler)
    assert await c.search_pages("q") == SEARCH_ROWS
    assert await c.search_pages("q") == []
    assert await c.search_pages("q") == []
    await c.close()


# ---------------------------------------------------------------------------
# list_pages — strict cursor walk
# ---------------------------------------------------------------------------


async def test_list_pages_cursor_walk_and_params():
    calls: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append({"path": request.url.path, "params": dict(request.url.params)})
        if request.url.params.get("cursor") is None:
            return httpx.Response(
                200,
                json={
                    "results": [dict(PAGE_OBJECT)],
                    "next_page_results": True,
                    "next_cursor": "100:1:0",
                },
            )
        return httpx.Response(
            200,
            json={
                "results": [dict(PAGE_OBJECT, id="page-2")],
                "next_page_results": False,
                "next_cursor": "",
            },
        )

    c = _client(handler)
    rows = await c.list_pages(
        _PROJECT,
        search="Runbook",
        fields=["id", "name", "description_html"],
        page_type="all",
    )
    await c.close()
    assert [r["id"] for r in rows] == [PAGE_OBJECT["id"], "page-2"]
    assert calls[0]["path"] == f"/api/v1/workspaces/testco/projects/{_PROJECT}/pages/"
    params = calls[0]["params"]
    assert params["search"] == "Runbook"
    assert params["fields"] == "id,name,description_html"
    assert params["type"] == "all"
    assert params["per_page"] == "100"
    assert calls[1]["params"]["cursor"] == "100:1:0"


async def test_list_pages_strict_raises_on_stalled_cursor():
    """An incomplete enumeration must raise, never silently truncate —
    a partial page list would wipe registry entries or mis-target prune."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "results": [dict(PAGE_OBJECT)],
                "next_page_results": True,
                "next_cursor": "100:1:0",
            },
        )

    c = _client(handler)
    with pytest.raises(PlaneProvisionError, match="stalled"):
        await c.list_pages(_PROJECT)
    await c.close()


async def test_list_pages_strict_raises_on_page_cap():
    calls: list[int] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(1)
        return httpx.Response(
            200,
            json={
                "results": [dict(PAGE_OBJECT, id=f"p{len(calls)}")],
                "next_page_results": True,
                "next_cursor": f"100:{len(calls)}:0",
            },
        )

    c = _client(handler)
    with pytest.raises(PlaneProvisionError, match="cap"):
        await c.list_pages(_PROJECT)
    await c.close()
    assert len(calls) == 50


async def test_list_pages_strict_raises_on_missing_cursor():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "results": [dict(PAGE_OBJECT)],
                "next_page_results": True,
                "next_cursor": "",
            },
        )

    c = _client(handler)
    with pytest.raises(PlaneProvisionError, match="no cursor"):
        await c.list_pages(_PROJECT)
    await c.close()


async def test_list_projects_stays_tolerant_of_incomplete_walks():
    """The non-strict default keeps the webhook-hot-path behavior:
    a stalled walk returns the rows collected so far."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "results": [{"id": "p1", "identifier": "ENG"}],
                "next_page_results": True,
                "next_cursor": "100:1:0",
            },
        )

    c = _client(handler)
    rows = await c.list_projects()
    await c.close()
    assert len(rows) == 2  # first page + one page at the stuck cursor


# ---------------------------------------------------------------------------
# pages: get / create / update
# ---------------------------------------------------------------------------


async def test_get_page_path_and_object():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["method"] = request.method
        return httpx.Response(200, json=PAGE_OBJECT)

    c = _client(handler)
    page = await c.get_page(_PROJECT, _PAGE)
    await c.close()
    assert seen["method"] == "GET"
    assert seen["path"] == (
        f"/api/v1/workspaces/testco/projects/{_PROJECT}/pages/{_PAGE}/"
    )
    # The pinned page shape: ``parent`` (not parent_id), no project ref.
    assert page == PAGE_OBJECT
    assert "parent" in page
    assert "parent_id" not in page
    assert "project" not in page and "project_id" not in page


async def test_create_page_post_body_full():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["method"] = request.method
        seen["auth"] = request.headers.get("X-API-Key")
        seen["body"] = jsonlib.loads(request.content)
        return httpx.Response(201, json=PAGE_OBJECT)

    c = _client(handler)
    page = await c.create_page(
        _PROJECT,
        name="Deploy Runbook",
        description_html="<p>How to deploy.</p>",
        parent_id="9a9a9a9a-0000-0000-0000-00000000000f",
        external_id="doc:Deploy Runbook",
        external_source="crewlet",
    )
    await c.close()
    assert page == PAGE_OBJECT
    assert seen["method"] == "POST"
    assert seen["auth"] == _TOKEN
    assert seen["path"] == f"/api/v1/workspaces/testco/projects/{_PROJECT}/pages/"
    # The write field is ``parent``; access is sent
    # explicitly as public; the external identity pair rides along.
    assert seen["body"] == {
        "name": "Deploy Runbook",
        "description_html": "<p>How to deploy.</p>",
        "access": 0,
        "parent": "9a9a9a9a-0000-0000-0000-00000000000f",
        "external_id": "doc:Deploy Runbook",
        "external_source": "crewlet",
    }


async def test_create_page_minimal_body_omits_optional_fields():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = jsonlib.loads(request.content)
        return httpx.Response(201, json=PAGE_OBJECT)

    c = _client(handler)
    await c.create_page(_PROJECT, name="Home", description_html="<p></p>")
    await c.close()
    assert seen["body"] == {
        "name": "Home",
        "description_html": "<p></p>",
        "access": 0,
    }


async def test_update_page_sends_only_provided_fields():
    bodies: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        bodies.append(jsonlib.loads(request.content))
        return httpx.Response(200, json=PAGE_OBJECT)

    c = _client(handler)
    await c.update_page(_PROJECT, _PAGE, name="Renamed")
    await c.update_page(
        _PROJECT,
        _PAGE,
        description_html="<p>v2</p>",
        external_id="skill:deploy",
        external_source="crewlet",
    )
    # ``parent_id`` distinguishes omitted / uuid / explicit None.
    await c.update_page(_PROJECT, _PAGE, parent_id="9a9a" + "0" * 28)
    await c.update_page(_PROJECT, _PAGE, parent_id=None)
    await c.close()
    assert bodies[0] == {"name": "Renamed"}
    assert bodies[1] == {
        "description_html": "<p>v2</p>",
        "external_id": "skill:deploy",
        "external_source": "crewlet",
    }
    assert bodies[2] == {"parent": "9a9a" + "0" * 28}
    assert bodies[3] == {"parent": None}


async def test_update_page_patch_path():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        return httpx.Response(200, json=PAGE_OBJECT)

    c = _client(handler)
    await c.update_page(_PROJECT, _PAGE, name="X")
    await c.close()
    assert seen["method"] == "PATCH"
    assert seen["path"] == (
        f"/api/v1/workspaces/testco/projects/{_PROJECT}/pages/{_PAGE}/"
    )


# ---------------------------------------------------------------------------
# pages: archive / delete (archive-first precondition)
# ---------------------------------------------------------------------------


async def test_archive_page_posts_and_returns_archived_at():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        return httpx.Response(200, json={"archived_at": "2026-07-26"})

    c = _client(handler)
    result = await c.archive_page(_PROJECT, _PAGE)
    await c.close()
    assert seen["method"] == "POST"
    assert seen["path"] == (
        f"/api/v1/workspaces/testco/projects/{_PROJECT}/pages/{_PAGE}/archive/"
    )
    assert result == {"archived_at": "2026-07-26"}


async def test_unarchive_page_deletes_the_archive_route():
    """Restore = DELETE on the same fork endpoint archive POSTs to
    (``PageArchiveAPIEndpoint``) — the prune rollback for a page whose
    delete was refused after a successful archive."""
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        return httpx.Response(204)

    c = _client(handler)
    assert await c.unarchive_page(_PROJECT, _PAGE) is None
    await c.close()
    assert seen["method"] == "DELETE"
    assert seen["path"] == (
        f"/api/v1/workspaces/testco/projects/{_PROJECT}/pages/{_PAGE}/archive/"
    )


async def test_delete_page_204():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        return httpx.Response(204)

    c = _client(handler)
    assert await c.delete_page(_PROJECT, _PAGE) is None
    await c.close()
    assert seen["method"] == "DELETE"
    assert seen["path"] == (
        f"/api/v1/workspaces/testco/projects/{_PROJECT}/pages/{_PAGE}/"
    )


async def test_delete_live_page_maps_the_400_precondition():
    """Pinned B2 fork behavior: DELETE on an unarchived page 400s —
    callers must archive first."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            400, json={"error": "The page should be archived before deleting"}
        )

    c = _client(handler)
    with pytest.raises(PlaneProvisionError) as exc:
        await c.delete_page(_PROJECT, _PAGE)
    await c.close()
    assert exc.value.status == 400
    assert "archived" in str(exc.value)


async def test_page_write_error_never_leaks_the_token():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(409, text=f"conflict for {_TOKEN}")

    c = _client(handler)
    with pytest.raises(PlaneProvisionError) as exc:
        await c.create_page(_PROJECT, name="X", description_html="<p></p>")
    await c.close()
    assert _TOKEN not in str(exc.value)
    assert "[REDACTED]" in str(exc.value)
    assert exc.value.status == 409


# ---------------------------------------------------------------------------
# Provisioning: service accounts
# ---------------------------------------------------------------------------

# Pinned create-response fixture (``ServiceAccountSerializer``):
# ``id`` is the service account's USER uuid, ``role`` comes back as the
# INT (20 admin / 15 member / 5 guest) even though the request sends the
# STRING choice, and ``token`` is the plaintext first API token —
# returned exactly once, never retrievable again.
SERVICE_ACCOUNT_CREATED = {
    "id": "5a5a5a5a-0000-0000-0000-000000000001",
    "username": "agent-swe",
    "email": "svc_9d89245e45164385a00e42ff6056cbce@service.plane.local",
    "display_name": "Agent SWE",
    "role": 15,
    "workspace": "abcdabcd-0000-0000-0000-000000000001",
    "token": "plane_api_freshly_minted_once",
}

# Pinned token-list rows (``ServiceAccountTokenSerializer``):
# metadata only — the secret ``token`` value is structurally NOT a field
# on the list serializer, so it can never appear here.  Revoked tokens
# are omitted from the list; rotated-away ones remain is_active=false.
SERVICE_ACCOUNT_TOKEN_ROWS = [
    {
        "id": "7c7c7c7c-0000-0000-0000-000000000001",
        "label": "crewlet-provision:agent-swe",
        "description": "",
        "is_active": True,
        "is_service": True,
        "user_type": 1,
        "created_at": "2026-07-20T10:00:00Z",
        "updated_at": "2026-07-20T10:00:00Z",
        "expired_at": "2027-07-19T10:00:00Z",
        "last_used": None,
    },
    {
        "id": "7c7c7c7c-0000-0000-0000-000000000002",
        "label": "crewlet-provision:agent-swe",
        "description": "",
        "is_active": False,
        "is_service": True,
        "user_type": 1,
        "created_at": "2026-01-01T10:00:00Z",
        "updated_at": "2026-07-20T10:00:00Z",
        "expired_at": None,
        "last_used": "2026-07-19T10:00:00Z",
    },
]

# Pinned mint/rotate response (``ServiceAccountTokenCreatedSerializer``)
# — the ONLY token shape carrying the plaintext value, shown once.
SERVICE_ACCOUNT_TOKEN_CREATED = {
    "id": "7c7c7c7c-0000-0000-0000-000000000003",
    "label": "crewlet-provision:agent-swe",
    "is_active": True,
    "created_at": "2026-07-27T10:00:00Z",
    "expired_at": None,
    "token": "plane_api_rotated_value_once",
}

_USER = "5a5a5a5a-0000-0000-0000-000000000001"
_TOKEN_ID = "7c7c7c7c-0000-0000-0000-000000000001"


async def test_create_service_account_full_body_and_echo():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        seen["body"] = jsonlib.loads(request.content)
        return httpx.Response(201, json=SERVICE_ACCOUNT_CREATED)

    c = _client(handler)
    created = await c.create_service_account(
        name="crewlet-provision:agent-swe",
        role="member",
        username="agent-swe",
        display_name="Agent SWE",
        description="managed by crewlet",
    )
    await c.close()
    assert created == SERVICE_ACCOUNT_CREATED
    assert seen["method"] == "POST"
    assert seen["path"] == "/api/v1/workspaces/testco/service-accounts/"
    # role is the STRING choice and is always sent (the API default is
    # `admin` — omission would silently privilege-escalate).
    assert seen["body"] == {
        "name": "crewlet-provision:agent-swe",
        "role": "member",
        "username": "agent-swe",
        "display_name": "Agent SWE",
        "description": "managed by crewlet",
    }


async def test_create_service_account_minimal_body_omits_optional_fields():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = jsonlib.loads(request.content)
        return httpx.Response(201, json=SERVICE_ACCOUNT_CREATED)

    c = _client(handler)
    await c.create_service_account(name="Bot", role="admin")
    await c.close()
    assert seen["body"] == {"name": "Bot", "role": "admin"}


async def test_create_service_account_username_conflict_409():
    """A taken username → 409 with a machine-readable code,
    never silently mutated."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            409,
            json={
                "error": "A user with this username already exists.",
                "code": "USERNAME_ALREADY_EXISTS",
            },
        )

    c = _client(handler)
    with pytest.raises(PlaneProvisionError) as exc:
        await c.create_service_account(name="Bot", role="member", username="taken")
    await c.close()
    assert exc.value.status == 409
    assert "USERNAME_ALREADY_EXISTS" in str(exc.value)


async def test_delete_service_account_204():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        return httpx.Response(204)

    c = _client(handler)
    assert await c.delete_service_account(_USER) is None
    await c.close()
    assert seen["method"] == "DELETE"
    assert seen["path"] == f"/api/v1/workspaces/testco/service-accounts/{_USER}/"


async def test_list_service_account_tokens_never_carry_the_value():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        return httpx.Response(200, json={"results": SERVICE_ACCOUNT_TOKEN_ROWS})

    c = _client(handler)
    rows = await c.list_service_account_tokens(_USER)
    await c.close()
    assert seen["path"] == (
        f"/api/v1/workspaces/testco/service-accounts/{_USER}/tokens/"
    )
    assert rows == SERVICE_ACCOUNT_TOKEN_ROWS
    assert all("token" not in row for row in rows)


async def test_list_service_account_tokens_strict_raises_on_stalled_cursor():
    """Rotation targets a row from this list — targeting on a silently
    truncated enumeration is corruption, so the walk is strict."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "results": SERVICE_ACCOUNT_TOKEN_ROWS,
                "next_page_results": True,
                "next_cursor": "100:1:0",
            },
        )

    c = _client(handler)
    with pytest.raises(PlaneProvisionError, match="stalled"):
        await c.list_service_account_tokens(_USER)
    await c.close()


async def test_create_service_account_token_body_with_and_without_expiry():
    from datetime import UTC, datetime

    bodies: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        bodies.append(jsonlib.loads(request.content))
        return httpx.Response(201, json=SERVICE_ACCOUNT_TOKEN_CREATED)

    c = _client(handler)
    minted = await c.create_service_account_token(
        _USER,
        label="crewlet-provision:agent-swe",
        expired_at=datetime(2027, 7, 27, tzinfo=UTC),
    )
    await c.create_service_account_token(_USER, label="lbl")
    await c.close()
    assert minted == SERVICE_ACCOUNT_TOKEN_CREATED
    assert bodies[0] == {
        "label": "crewlet-provision:agent-swe",
        "description": "",
        "expired_at": "2027-07-27T00:00:00+00:00",
    }
    # Omitted expiry ⇒ the key is ABSENT (Plane: the token never expires).
    assert bodies[1] == {"label": "lbl", "description": ""}


async def test_rotate_token_three_state_expired_at_body_encoding():
    """Pinned rotate semantics: key omitted ⇒ inherit the source
    token's expiry; explicit null ⇒ never expire; ISO datetime ⇒ that
    instant.  Asserted on the RAW request body."""
    from datetime import UTC, datetime

    bodies: list[dict] = []
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        bodies.append(jsonlib.loads(request.content))
        return httpx.Response(201, json=SERVICE_ACCOUNT_TOKEN_CREATED)

    c = _client(handler)
    rotated = await c.rotate_service_account_token(_USER, _TOKEN_ID)
    await c.rotate_service_account_token(_USER, _TOKEN_ID, expired_at=None)
    await c.rotate_service_account_token(
        _USER, _TOKEN_ID, expired_at=datetime(2027, 7, 27, tzinfo=UTC)
    )
    await c.close()
    assert rotated == SERVICE_ACCOUNT_TOKEN_CREATED
    assert seen["method"] == "POST"
    assert seen["path"] == (
        f"/api/v1/workspaces/testco/service-accounts/{_USER}/tokens/{_TOKEN_ID}/rotate/"
    )
    assert bodies[0] == {}  # omitted ⇒ key absent ⇒ inherit
    assert bodies[1] == {"expired_at": None}  # explicit null ⇒ never expire
    assert bodies[2] == {"expired_at": "2027-07-27T00:00:00+00:00"}


async def test_rotate_token_409_not_active_raises():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            409,
            json={
                "error": "The token is not active and cannot be rotated.",
                "code": "TOKEN_NOT_ACTIVE",
            },
        )

    c = _client(handler)
    with pytest.raises(PlaneProvisionError) as exc:
        await c.rotate_service_account_token(_USER, _TOKEN_ID, expired_at=None)
    await c.close()
    assert exc.value.status == 409


async def test_revoke_token_delete_204():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        return httpx.Response(204)

    c = _client(handler)
    assert await c.revoke_service_account_token(_USER, _TOKEN_ID) is None
    await c.close()
    assert seen["method"] == "DELETE"
    assert seen["path"] == (
        f"/api/v1/workspaces/testco/service-accounts/{_USER}/tokens/{_TOKEN_ID}/"
    )


async def test_probe_workspace_returns_status_without_raising():
    """The workspace-slug probe: GET on the membership-scoped projects
    list — 200 slug good, 403/404 slug unknown or invisible (run after
    /users/me has proven the credential authenticates)."""
    statuses = iter([200, 403, 404])
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        status = next(statuses)
        if status == 200:
            return httpx.Response(200, json={"results": []})
        return httpx.Response(status, text="probe")

    c = _client(handler)
    assert await c.probe_workspace() == 200
    assert await c.probe_workspace() == 403
    assert await c.probe_workspace() == 404
    await c.close()
    assert seen["method"] == "GET"
    assert seen["path"] == "/api/v1/workspaces/testco/projects/"


async def test_raw_error_bodies_are_capped_but_crafted_messages_are_not():
    """The 300-char cap guards against megabyte proxy error bodies and
    applies ONLY where untrusted data enters (_request); a caller-crafted
    PlaneProvisionError message is never truncated — the B8 409
    remediation depends on its tail surviving any username length."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(502, text="x" * 5000)

    c = _client(handler)
    with pytest.raises(PlaneProvisionError) as exc:
        await c.get_current_user()
    await c.close()
    assert len(str(exc.value)) < 400  # prefix + capped 300-char body

    crafted = PlaneProvisionError("y" * 1000, method="POST", path="/p", status=409)
    assert "y" * 1000 in str(crafted)


async def test_probe_service_accounts_returns_status_without_raising():
    """The B5 presence probe: GET on the POST-only create route — 405
    route present, 404 absent (stock CE), 403 not a workspace admin."""
    statuses = iter([405, 404, 403])
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        return httpx.Response(next(statuses), text="probe")

    c = _client(handler)
    assert await c.probe_service_accounts() == 405
    assert await c.probe_service_accounts() == 404
    assert await c.probe_service_accounts() == 403
    await c.close()
    assert seen["method"] == "GET"
    assert seen["path"] == "/api/v1/workspaces/testco/service-accounts/"


async def test_probe_service_account_tokens_patches_the_zero_uuid_route():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        return httpx.Response(405, text="Method Not Allowed")

    c = _client(handler)
    assert await c.probe_service_account_tokens() == 405
    await c.close()
    assert seen["method"] == "PATCH"
    assert seen["path"] == (
        "/api/v1/workspaces/testco/service-accounts/"
        "00000000-0000-0000-0000-000000000000/tokens/"
    )


# ---------------------------------------------------------------------------
# Provisioning: workspace members / project members / projects (CE stock)
# ---------------------------------------------------------------------------

# Pinned CE-preview members fixture (``api/views/member.py``
# ``WorkspaceMemberAPIEndpoint``): a BARE array of FLAT user dicts —
# ``UserLiteSerializer(member).data`` with the workspace ``role`` int
# appended by the view, so ``row["id"]`` IS the member's user UUID
# (there is NO through-row indirection here, unlike the issue-subscriber
# rows above).  Stock ``UserLiteSerializer`` has NO ``username`` field;
# ``username`` / ``is_bot`` / ``bot_type`` are fork-added member fields
# the provisioner hard-requires.
MEMBER_ROWS = [
    {
        "id": "11111111-1111-1111-1111-111111111111",
        "first_name": "Founding",
        "last_name": "Human",
        "email": "founder@plane.test",
        "avatar": "",
        "avatar_url": None,
        "display_name": "Founding Human",
        "role": 20,
        # fork-added member fields ↓
        "username": "founder",
        "is_bot": False,
        "bot_type": None,
    },
    {
        "id": "5a5a5a5a-0000-0000-0000-000000000001",
        "first_name": "crewlet-provision:agent-swe",
        "last_name": "",
        "email": "svc_9d89245e45164385a00e42ff6056cbce@service.plane.local",
        "avatar": "",
        "avatar_url": None,
        "display_name": "Agent SWE",
        "role": 15,
        # fork-added member fields ↓
        "username": "agent-swe",
        "is_bot": True,
        "bot_type": "SERVICE",
    },
]


async def test_list_workspace_members_flat_rows_bare_array():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        return httpx.Response(200, json=MEMBER_ROWS)

    c = _client(handler)
    rows = await c.list_workspace_members()
    await c.close()
    assert seen["path"] == "/api/v1/workspaces/testco/members/"
    assert rows == MEMBER_ROWS
    # The flat-row contract: row["id"] IS the user UUID (no `member` key).
    assert rows[1]["id"] == "5a5a5a5a-0000-0000-0000-000000000001"
    assert all("member" not in row for row in rows)


async def test_list_workspace_members_strict_raises_on_stalled_cursor():
    """M3: the reconcile keys create-vs-exists and decommission targeting
    off this enumeration — a truncated walk must raise, never truncate."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "results": [dict(MEMBER_ROWS[0])],
                "next_page_results": True,
                "next_cursor": "100:1:0",
            },
        )

    c = _client(handler)
    with pytest.raises(PlaneProvisionError, match="stalled"):
        await c.list_workspace_members()
    await c.close()


async def test_add_project_member_body_and_created_row():
    """Pinned CE-preview shape: writable fields are `member` (the user
    UUID — a PrimaryKeyRelatedField) and `role` (the INT); the created
    row's `id` is the ProjectMember ROW pk, not a user id."""
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        seen["body"] = jsonlib.loads(request.content)
        return httpx.Response(
            201,
            json={
                "id": "3b3b3b3b-0000-0000-0000-000000000001",
                "member": _USER,
                "role": 15,
            },
        )

    c = _client(handler)
    created = await c.add_project_member(_PROJECT, member_id=_USER, role=15)
    await c.close()
    assert seen["method"] == "POST"
    assert seen["path"] == (f"/api/v1/workspaces/testco/projects/{_PROJECT}/members/")
    assert seen["body"] == {"member": _USER, "role": 15}
    assert created == {
        "id": "3b3b3b3b-0000-0000-0000-000000000001",
        "member": _USER,
        "role": 15,
    }


async def test_add_project_member_unambiguous_duplicate_maps_to_none():
    """A 4xx whose body unambiguously names an existing/duplicate
    membership (fork-side message improvements) reads as 'exists'."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(409, json={"error": "Member already exists"})

    c = _client(handler)
    assert await c.add_project_member(_PROJECT, member_id=_USER, role=15) is None
    await c.close()


async def test_add_project_member_generic_integrity_400_raises():
    """Pinned CE-preview duplicate semantics: a duplicate add violates
    ProjectMember's project+member unique constraint → IntegrityError →
    BaseAPIView's GENERIC 400 {"error": "The payload is not valid"}.
    The SAME body fires for any other IntegrityError from the view
    (e.g. ProjectMember.save()'s ProjectUserProperty creation), so the
    client RAISES it — callers must confirm the membership against the
    project's member rows before treating it as a duplicate (the
    provisioner does exactly that)."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(400, json={"error": "The payload is not valid"})

    c = _client(handler)
    with pytest.raises(PlaneProvisionError) as exc:
        await c.add_project_member(_PROJECT, member_id=_USER, role=15)
    await c.close()
    assert exc.value.status == 400
    assert "payload is not valid" in str(exc.value)


async def test_add_project_member_other_400s_still_raise():
    """Serializer-level rejections (INVALID_MEMBER / INVALID_ROLE) use
    distinct field-error bodies and must surface, never read as
    'exists'."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(400, json={"member": ["Member not found in workspace"]})

    c = _client(handler)
    with pytest.raises(PlaneProvisionError) as exc:
        await c.add_project_member(_PROJECT, member_id=_USER, role=15)
    await c.close()
    assert exc.value.status == 400


async def test_list_project_members_lite_rows():
    """Pinned CE-preview ``ProjectMemberLiteAPISerializer`` rows: `id`
    is the member's USER uuid (source=member.id — NOT the ProjectMember
    row pk) and `role` is the PROJECT-level role int."""
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        return httpx.Response(
            200,
            json={
                "results": [
                    {
                        "id": _USER,
                        "first_name": "crewlet-provision:agent-swe",
                        "last_name": "",
                        "email": "svc_x@service.plane.local",
                        "avatar": "",
                        "avatar_url": None,
                        "display_name": "Agent SWE",
                        "role": 15,
                        "is_active": True,
                        "is_bot": True,
                    }
                ]
            },
        )

    c = _client(handler)
    rows = await c.list_project_members_lite(_PROJECT)
    await c.close()
    assert seen["path"] == (
        f"/api/v1/workspaces/testco/projects/{_PROJECT}/project-members-lite/"
    )
    assert rows[0]["id"] == _USER
    assert rows[0]["role"] == 15


async def test_create_project_body_and_201():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        seen["body"] = jsonlib.loads(request.content)
        return httpx.Response(201, json={"id": _PROJECT, "identifier": "ENG"})

    c = _client(handler)
    created = await c.create_project(name="ENG", identifier="ENG")
    await c.close()
    assert seen["method"] == "POST"
    assert seen["path"] == "/api/v1/workspaces/testco/projects/"
    assert seen["body"] == {"name": "ENG", "identifier": "ENG"}
    assert created["id"] == _PROJECT


# ---------------------------------------------------------------------------
# Provisioning: workspace webhooks
# ---------------------------------------------------------------------------

# Pinned create-response fixture (full ``WebhookSerializer``):
# the ONLY response ever carrying ``secret_key`` (server-generated,
# ``plane_wh_`` prefix, single-show).  ``page`` is a fork-added event
# toggle — a server without it silently drops the field.
WEBHOOK_CREATED = {
    "id": "8d8d8d8d-0000-0000-0000-000000000001",
    "url": "https://engine.test/webhooks/plane",
    "is_active": True,
    "secret_key": "plane_wh_generated_secret_once",
    "project": True,
    "issue": True,
    "module": False,
    "cycle": False,
    "issue_comment": True,
    "page": True,
    "created_at": "2026-07-27T10:00:00Z",
    "updated_at": "2026-07-27T10:00:00Z",
}

# Pinned list/PATCH shape (``WebhookLiteSerializer``): the full
# shape minus ``secret_key`` — the exclusion is structural (the Lite
# field list is derived from the full one), so a secret can never leak
# from a read.
WEBHOOK_LITE = {k: v for k, v in WEBHOOK_CREATED.items() if k != "secret_key"}


async def test_create_webhook_body_and_single_show_secret():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        seen["body"] = jsonlib.loads(request.content)
        return httpx.Response(201, json=WEBHOOK_CREATED)

    c = _client(handler)
    created = await c.create_webhook(
        url="https://engine.test/webhooks/plane",
        is_active=True,
        project=True,
        issue=True,
        issue_comment=True,
        page=True,
        cycle=False,
        module=False,
    )
    await c.close()
    assert seen["method"] == "POST"
    assert seen["path"] == "/api/v1/workspaces/testco/webhooks/"
    assert seen["body"] == {
        "url": "https://engine.test/webhooks/plane",
        "is_active": True,
        "project": True,
        "issue": True,
        "issue_comment": True,
        "page": True,
        "cycle": False,
        "module": False,
    }
    assert created["secret_key"] == "plane_wh_generated_secret_once"


async def test_list_webhooks_bare_array_without_secret():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        return httpx.Response(200, json=[WEBHOOK_LITE])

    c = _client(handler)
    hooks = await c.list_webhooks()
    await c.close()
    assert seen["path"] == "/api/v1/workspaces/testco/webhooks/"
    assert hooks == [WEBHOOK_LITE]
    assert "secret_key" not in hooks[0]


async def test_update_webhook_patch_partial_body_lite_response():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        seen["body"] = jsonlib.loads(request.content)
        return httpx.Response(200, json=WEBHOOK_LITE)

    c = _client(handler)
    updated = await c.update_webhook(WEBHOOK_CREATED["id"], is_active=True, page=True)
    await c.close()
    assert seen["method"] == "PATCH"
    assert seen["path"] == (
        f"/api/v1/workspaces/testco/webhooks/{WEBHOOK_CREATED['id']}/"
    )
    assert seen["body"] == {"is_active": True, "page": True}
    assert "secret_key" not in updated


async def test_create_webhook_duplicate_url_409():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            409, json={"error": "URL already exists for the workspace"}
        )

    c = _client(handler)
    with pytest.raises(PlaneProvisionError) as exc:
        await c.create_webhook(url="https://engine.test/webhooks/plane")
    await c.close()
    assert exc.value.status == 409


async def test_delete_webhook_204():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["path"] = request.url.path
        return httpx.Response(204)

    c = _client(handler)
    assert await c.delete_webhook(WEBHOOK_CREATED["id"]) is None
    await c.close()
    assert seen["method"] == "DELETE"
    assert seen["path"] == (
        f"/api/v1/workspaces/testco/webhooks/{WEBHOOK_CREATED['id']}/"
    )


async def test_list_projects_strict_opt_in_raises_on_stalled_cursor():
    """M3: the additive keyword-only `strict` lets the provisioner demand
    a complete project list (a truncated one would mis-classify an
    existing project as missing and attempt a duplicate create) while
    the webhook hot path keeps the tolerant default."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "results": [{"id": "p1", "identifier": "ENG"}],
                "next_page_results": True,
                "next_cursor": "100:1:0",
            },
        )

    c = _client(handler)
    with pytest.raises(PlaneProvisionError, match="stalled"):
        await c.list_projects(strict=True)
    await c.close()
