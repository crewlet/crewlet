"""Tests for the GitLab REST client (httpx MockTransport)."""

from __future__ import annotations

import json

import httpx
import pytest

from crewlet.gitlab.client import ACCESS_LEVELS, GitLabClient, GitLabProvisionError


def _client(handler) -> GitLabClient:
    c = GitLabClient("https://gitlab.example/api/v4", "op-token")
    c._client = httpx.AsyncClient(
        base_url="https://gitlab.example/api/v4",
        headers={"PRIVATE-TOKEN": "op-token"},
        transport=httpx.MockTransport(handler),
    )
    return c


async def test_get_current_user_sends_private_token():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["auth"] = request.headers.get("PRIVATE-TOKEN")
        seen["path"] = request.url.path
        return httpx.Response(200, json={"id": 1, "username": "op"})

    async with _client(handler) as c:
        me = await c.get_current_user()
    assert me["username"] == "op"
    assert seen["auth"] == "op-token"
    assert seen["path"].endswith("/user")


async def test_error_maps_to_provision_error():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(403, text="forbidden")

    async with _client(handler) as c:
        with pytest.raises(GitLabProvisionError) as exc:
            await c.get_current_user()
    assert exc.value.status == 403
    assert exc.value.method == "GET"


async def test_pagination_follows_x_next_page():
    def handler(request: httpx.Request) -> httpx.Response:
        page = request.url.params.get("page")
        if page == "1":
            return httpx.Response(
                200,
                json=[{"id": 1, "username": "a"}],
                headers={"x-next-page": "2"},
            )
        return httpx.Response(200, json=[{"id": 2, "username": "b"}], headers={})

    async with _client(handler) as c:
        accounts = await c.list_group_service_accounts("grp")
    assert [a["username"] for a in accounts] == ["a", "b"]


async def test_create_group_service_account_payload():
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["body"] = json.loads(request.content)
        captured["path"] = request.url.path
        return httpx.Response(201, json={"id": 99, "username": "agent-swe"})

    async with _client(handler) as c:
        acct = await c.create_group_service_account(
            "nimbus-hq", name="Agent SWE", username="agent-swe", email="a@b.co"
        )
    assert acct["id"] == 99
    assert captured["body"] == {
        "name": "Agent SWE",
        "username": "agent-swe",
        "email": "a@b.co",
    }
    assert "/groups/nimbus-hq/service_accounts" in captured["path"]


async def test_group_path_is_url_encoded():
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["path"] = request.url.raw_path.decode()
        return httpx.Response(200, json=[])

    async with _client(handler) as c:
        await c.list_group_service_accounts("acme/sub")
    assert "acme%2Fsub" in captured["path"]


async def test_add_group_member_treats_409_as_ok():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(409, json={"message": "already a member"})

    async with _client(handler) as c:
        # 409 is in the ok set → returns the body without raising.
        resp = await c.add_group_member("grp", 5, ACCESS_LEVELS["developer"])
    assert resp["message"] == "already a member"


async def test_create_token_includes_expiry_when_given():
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["body"] = json.loads(request.content)
        return httpx.Response(201, json={"id": 7, "token": "glpat-new"})

    async with _client(handler) as c:
        tok = await c.create_group_service_account_token(
            "grp",
            5,
            name="crewlet-provision:x",
            scopes=["api"],
            expires_at="2027-01-01",
        )
    assert tok["token"] == "glpat-new"
    assert captured["body"]["expires_at"] == "2027-01-01"
    assert captured["body"]["scopes"] == ["api"]


async def test_participants_endpoints():
    seen = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request.url.path)
        return httpx.Response(200, json=[{"username": "Agent-A"}, {"username": "b"}])

    async with _client(handler) as c:
        issue = await c.get_issue_participants(17, 4)
        mr = await c.get_merge_request_participants(17, 9)
    assert [u["username"] for u in issue] == ["Agent-A", "b"]
    assert [u["username"] for u in mr] == ["Agent-A", "b"]
    assert seen[0].endswith("/projects/17/issues/4/participants")
    assert seen[1].endswith("/projects/17/merge_requests/9/participants")


async def test_build_participants_fetcher(monkeypatch):
    from crewlet.gitlab.client import build_participants_fetcher

    def handler(request: httpx.Request) -> httpx.Response:
        assert request.headers.get("PRIVATE-TOKEN") == "read-token"
        if "merge_requests" in request.url.path:
            return httpx.Response(200, json=[{"username": "MR-User"}])
        return httpx.Response(200, json=[{"username": "Issue-User"}, {"no": "name"}])

    real = httpx.AsyncClient

    def factory(*args, **kwargs):
        kwargs["transport"] = httpx.MockTransport(handler)
        return real(*args, **kwargs)

    monkeypatch.setattr(httpx, "AsyncClient", factory)

    fetch = build_participants_fetcher("https://gl/api/v4", "read-token")
    assert await fetch(17, "merge_request", 9) == ["mr-user"]
    assert await fetch(17, "issue", 4) == ["issue-user"]
