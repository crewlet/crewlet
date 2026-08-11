"""Tests for register_plane_accounts_from_org (REST GET /users/me/)."""

from __future__ import annotations

import httpx

from crewlet.config import (
    PlaneConfig,
    plane_token_from_mcp_env,
    register_plane_accounts_from_org,
)


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


def _patch_httpx(
    monkeypatch,
    token_to_user_id: dict[str, str],
    calls: list[str] | None = None,
):
    real = httpx.AsyncClient

    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/api/v1/users/me/"
        token = request.headers.get("X-API-Key", "")
        if calls is not None:
            calls.append(token)
        user_id = token_to_user_id.get(token)
        if user_id is None:
            return httpx.Response(401, json={"detail": "Invalid token"})
        return httpx.Response(200, json={"id": user_id, "email": "bot@x"})

    def factory(*args, **kwargs):
        kwargs["transport"] = httpx.MockTransport(handler)
        return real(*args, **kwargs)

    monkeypatch.setattr(httpx, "AsyncClient", factory)


def _cfg() -> PlaneConfig:
    return PlaneConfig(
        enabled=True, url="https://plane", workspace="acme", webhook_secret="s"
    )


async def test_registers_user_ids_lowercased(monkeypatch):
    # Plane payloads carry lowercase UUIDs; a mixed-case REST response is
    # normalised so webhook-side lookups always match.
    _patch_httpx(
        monkeypatch,
        {
            "plane_api_swe": "AB12CD34-0000-0000-0000-000000000001",
            "plane_api_fe": "ab12cd34-0000-0000-0000-000000000002",
        },
    )
    org = FakeOrg(
        [
            FakeRole(
                "Agent SWE", "agent-swe", {"plane": {"PLANE_API_KEY": "plane_api_swe"}}
            ),
            FakeRole(
                "Agent FE", "agent-fe", {"plane": {"PLANE_API_KEY": "plane_api_fe"}}
            ),
            FakeRole("Tech Lead", "tech-lead", {}),  # no creds → skipped
        ]
    )
    registry = FakeRegistry()

    count = await register_plane_accounts_from_org(registry, org, plane_config=_cfg())

    assert count == 2
    assert (
        "plane",
        "ab12cd34-0000-0000-0000-000000000001",
        "agent-swe",
    ) in registry.registered
    assert (
        "plane",
        "ab12cd34-0000-0000-0000-000000000002",
        "agent-fe",
    ) in registry.registered


async def test_unresolved_credential_skipped(monkeypatch):
    # An unset ${VAR} resolves empty → the seat is skipped BEFORE any REST
    # call is made (no MCP server would have started for it either).
    calls: list[str] = []
    _patch_httpx(monkeypatch, {"plane_api_good": "uuid-good"}, calls=calls)
    org = FakeOrg(
        [
            FakeRole("Good", "good", {"plane": {"PLANE_API_KEY": "plane_api_good"}}),
            FakeRole(
                "Bad",
                "bad",
                {"plane": {"PLANE_API_KEY": "${CREWLET_TEST_UNSET_PLANE_TOKEN}"}},
            ),
        ]
    )
    registry = FakeRegistry()

    count = await register_plane_accounts_from_org(registry, org, plane_config=_cfg())

    assert count == 1
    assert registry.registered == [("plane", "uuid-good", "good")]
    assert calls == ["plane_api_good"]  # no REST call for the unresolved seat


async def test_rejected_credential_not_registered(monkeypatch):
    # A token the instance rejects (401) → plane_identity_unresolved, skipped.
    _patch_httpx(monkeypatch, {"plane_api_good": "uuid-good"})
    org = FakeOrg(
        [
            FakeRole("Good", "good", {"plane": {"PLANE_API_KEY": "plane_api_good"}}),
            FakeRole("Bad", "bad", {"plane": {"PLANE_API_KEY": "plane_api_bad"}}),
        ]
    )
    registry = FakeRegistry()

    count = await register_plane_accounts_from_org(registry, org, plane_config=_cfg())

    assert count == 1
    assert registry.registered == [("plane", "uuid-good", "good")]


async def test_shared_account_not_registered(monkeypatch, caplog):
    # Two seats resolve to the SAME user UUID → ambiguous, register neither.
    _patch_httpx(
        monkeypatch, {"plane_api_a": "shared-uuid", "plane_api_b": "shared-uuid"}
    )
    org = FakeOrg(
        [
            FakeRole("A", "a", {"plane": {"PLANE_API_KEY": "plane_api_a"}}),
            FakeRole("B", "b", {"plane": {"PLANE_API_KEY": "plane_api_b"}}),
        ]
    )
    registry = FakeRegistry()

    count = await register_plane_accounts_from_org(registry, org, plane_config=_cfg())

    assert count == 0
    assert registry.registered == []
    assert "plane_account_shared" in caplog.text


async def test_only_roles_filter(monkeypatch):
    _patch_httpx(monkeypatch, {"plane_api_a": "uuid-a", "plane_api_b": "uuid-b"})
    org = FakeOrg(
        [
            FakeRole("A", "a", {"plane": {"PLANE_API_KEY": "plane_api_a"}}),
            FakeRole("B", "b", {"plane": {"PLANE_API_KEY": "plane_api_b"}}),
        ]
    )
    registry = FakeRegistry()

    count = await register_plane_accounts_from_org(
        registry, org, plane_config=_cfg(), only_roles={"A"}
    )

    assert count == 1
    assert registry.registered == [("plane", "uuid-a", "a")]


async def test_disabled_or_missing_config_returns_zero(monkeypatch):
    org = FakeOrg([FakeRole("A", "a", {"plane": {"PLANE_API_KEY": "t"}})])
    registry = FakeRegistry()

    assert await register_plane_accounts_from_org(registry, org, plane_config=None) == 0
    # A config without a url (e.g. never enabled) is equally inert.
    no_url = PlaneConfig()
    assert (
        await register_plane_accounts_from_org(registry, org, plane_config=no_url) == 0
    )
    assert registry.registered == []


# ── plane_token_from_mcp_env ──────────────────────────────────────────


def test_token_from_mcp_env_plane_api_key():
    assert plane_token_from_mcp_env({"PLANE_API_KEY": " plane_api_x "}) == "plane_api_x"


def test_token_from_mcp_env_header_forms():
    assert plane_token_from_mcp_env({"X-API-Key": "plane_api_h"}) == "plane_api_h"
    assert plane_token_from_mcp_env({"x-api-key": "plane_api_l"}) == "plane_api_l"


def test_token_from_mcp_env_absent():
    assert plane_token_from_mcp_env({}) == ""
    assert plane_token_from_mcp_env({"PLANE_BASE_URL": "https://p"}) == ""


def test_token_from_mcp_env_resolves_env_refs(monkeypatch):
    monkeypatch.setenv("CREWLET_TEST_PLANE_TOKEN", "plane_api_env")
    assert (
        plane_token_from_mcp_env({"PLANE_API_KEY": "${CREWLET_TEST_PLANE_TOKEN}"})
        == "plane_api_env"
    )
    monkeypatch.delenv("CREWLET_TEST_PLANE_TOKEN")
    assert (
        plane_token_from_mcp_env({"PLANE_API_KEY": "${CREWLET_TEST_PLANE_TOKEN}"}) == ""
    )
