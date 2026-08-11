"""Tests for register_gitlab_accounts_from_org (REST GET /user)."""

from __future__ import annotations

import httpx

from crewlet.config import GitLabConfig, register_gitlab_accounts_from_org


class FakeRole:
    def __init__(self, name, handle, mcp_env):
        self.name = name
        self._handle = handle
        self.mcp_env = mcp_env

    def get_handle(self):
        return self._handle


class FakeOrg:
    def __init__(self, roles):
        self._roles = roles

    def all_roles(self):
        return self._roles


class FakeRegistry:
    def __init__(self):
        self.registered: list[tuple[str, str, str]] = []

    def register_external_id(self, transport, external_id, handle):
        self.registered.append((transport, external_id, handle))


def _patch_httpx(monkeypatch, token_to_username: dict[str, str]):
    real = httpx.AsyncClient

    def handler(request: httpx.Request) -> httpx.Response:
        token = request.headers.get("PRIVATE-TOKEN", "")
        username = token_to_username.get(token)
        if username is None:
            return httpx.Response(401, json={"message": "401 Unauthorized"})
        return httpx.Response(200, json={"id": 1, "username": username})

    def factory(*args, **kwargs):
        kwargs["transport"] = httpx.MockTransport(handler)
        return real(*args, **kwargs)

    monkeypatch.setattr(httpx, "AsyncClient", factory)


async def test_registers_usernames_lowercased(monkeypatch):
    _patch_httpx(
        monkeypatch,
        {"glpat-swe": "Agent-SWE", "glpat-fe": "agent-fe"},
    )
    org = FakeOrg(
        [
            FakeRole(
                "Agent SWE", "agent-swe", {"gitlab": {"Private-Token": "glpat-swe"}}
            ),
            FakeRole("Agent FE", "agent-fe", {"gitlab": {"Private-Token": "glpat-fe"}}),
            FakeRole("Tech Lead", "tech-lead", {}),  # no creds → skipped
        ]
    )
    registry = FakeRegistry()
    cfg = GitLabConfig(enabled=True, url="https://gl", signing_secret="s")

    count = await register_gitlab_accounts_from_org(registry, org, gitlab_config=cfg)

    assert count == 2
    assert ("gitlab", "agent-swe", "agent-swe") in registry.registered
    assert ("gitlab", "agent-fe", "agent-fe") in registry.registered


async def test_unresolved_credential_skipped(monkeypatch):
    # Token that the fake server rejects (401) → not registered.
    _patch_httpx(monkeypatch, {"glpat-good": "good-agent"})
    org = FakeOrg(
        [
            FakeRole("Good", "good", {"gitlab": {"Private-Token": "glpat-good"}}),
            FakeRole("Bad", "bad", {"gitlab": {"Private-Token": "glpat-bad"}}),
        ]
    )
    registry = FakeRegistry()
    cfg = GitLabConfig(enabled=True, url="https://gl", signing_secret="s")

    count = await register_gitlab_accounts_from_org(registry, org, gitlab_config=cfg)

    assert count == 1
    assert registry.registered == [("gitlab", "good-agent", "good")]


async def test_shared_account_not_registered(monkeypatch):
    # Two seats resolve to the SAME username → ambiguous, register neither.
    _patch_httpx(monkeypatch, {"glpat-a": "shared", "glpat-b": "shared"})
    org = FakeOrg(
        [
            FakeRole("A", "a", {"gitlab": {"Private-Token": "glpat-a"}}),
            FakeRole("B", "b", {"gitlab": {"Private-Token": "glpat-b"}}),
        ]
    )
    registry = FakeRegistry()
    cfg = GitLabConfig(enabled=True, url="https://gl", signing_secret="s")

    count = await register_gitlab_accounts_from_org(registry, org, gitlab_config=cfg)

    assert count == 0
    assert registry.registered == []


async def test_only_roles_filter(monkeypatch):
    _patch_httpx(monkeypatch, {"glpat-a": "agent-a", "glpat-b": "agent-b"})
    org = FakeOrg(
        [
            FakeRole("A", "a", {"gitlab": {"Private-Token": "glpat-a"}}),
            FakeRole("B", "b", {"gitlab": {"Private-Token": "glpat-b"}}),
        ]
    )
    registry = FakeRegistry()
    cfg = GitLabConfig(enabled=True, url="https://gl", signing_secret="s")

    count = await register_gitlab_accounts_from_org(
        registry, org, gitlab_config=cfg, only_roles={"A"}
    )

    assert count == 1
    assert registry.registered == [("gitlab", "agent-a", "a")]
