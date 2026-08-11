"""Tests for the per-entity ``/config/*`` CRUD routes."""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from datetime import UTC, datetime
from unittest.mock import AsyncMock, MagicMock
from uuid import uuid4

import pytest
from starlette.testclient import TestClient

from crewlet.api.app import create_app
from crewlet.config import ApiAuthConfig, ApiAuthTokenConfig, ApiConfig, BootstrapConfig
from crewlet.db.company_config import CompanyConfigRevision
from crewlet.events.types import Event


class _MockQueue:
    def __init__(self) -> None:
        self.published: list[tuple[str, Event]] = []

    async def publish(self, topic: str, event: Event) -> None:
        self.published.append((topic, event))

    async def subscribe(
        self,
        topic: str,
        group: str,
        handler: Callable[[Event], Awaitable[None]],
    ) -> None: ...

    async def start(self) -> None: ...
    async def stop(self) -> None: ...


def _bootstrap() -> BootstrapConfig:
    return BootstrapConfig(
        api=ApiConfig(
            auth=ApiAuthConfig(tokens=[ApiAuthTokenConfig(id="t1", token="s1")])
        ),
    )


def _rev(payload: dict) -> CompanyConfigRevision:
    return CompanyConfigRevision(
        revision_id=uuid4(),
        parent_revision_id=None,
        created_at=datetime.now(UTC),
        created_by="alice",
        source="api",
        summary="test",
        payload=payload,
        is_active=True,
        activated_at=datetime.now(UTC),
    )


@pytest.fixture
def store() -> MagicMock:
    s = MagicMock()
    s.get_active = AsyncMock(return_value=None)
    s.insert_active = AsyncMock(return_value=uuid4())
    return s


@pytest.fixture
def client(store: MagicMock) -> TestClient:
    app = create_app(
        event_queue=_MockQueue(),
        bootstrap=_bootstrap(),
        company_config_store=store,
    )
    return TestClient(app)


_AUTH = {"Authorization": "Bearer s1"}


# ── Unconfigured-state behaviour ───────────────────────────────────


def test_put_identity_unconfigured_returns_409(client: TestClient) -> None:
    resp = client.put("/config/identity", json={"mission": "x"}, headers=_AUTH)
    assert resp.status_code == 409
    assert resp.json()["error"] == "company_not_initialised"


def test_put_role_unconfigured_returns_409(client: TestClient) -> None:
    resp = client.put("/config/roles/x", json={"goal": "x"}, headers=_AUTH)
    assert resp.status_code == 409


def test_post_role_unconfigured_returns_409(client: TestClient) -> None:
    resp = client.post("/config/roles", json={"name": "X", "goal": "y"}, headers=_AUTH)
    assert resp.status_code == 409


# ── /config/identity (PUT) ─────────────────────────────────────────


def test_put_identity_updates_field(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev({"name": "Acme", "mission": "old"})
    resp = client.put(
        "/config/identity",
        json={"mission": "shipping faster"},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    body = resp.json()
    assert "Updated identity" in body["summary"]
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["mission"] == "shipping faster"
    assert payload_arg["name"] == "Acme"  # untouched


# ── /config/turn-engine (PUT) ──────────────────────────────────────


def test_put_turn_engine_updates_settings(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev({"name": "Acme"})
    resp = client.put(
        "/config/turn-engine",
        json={"max_iterations": 7},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["turn_engine"]["max_iterations"] == 7


# ── /config/budgets (PUT) ──────────────────────────────────────────


def test_put_budgets_updates_token_budget(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev({"name": "Acme", "token_budget": 100})
    resp = client.put(
        "/config/budgets",
        json={"token_budget": 500},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["token_budget"] == 500


# ── /config/llm-providers ──────────────────────────────────────────


def test_list_llm_providers(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "providers": {
                "llm": {
                    "default": {"type": "openai", "model": "gpt-4o"},
                    "cheap": {"type": "openai", "model": "gpt-4o-mini"},
                }
            },
        }
    )
    resp = client.get("/config/llm-providers", headers=_AUTH)
    assert resp.status_code == 200
    assert resp.json() == ["cheap", "default"]


def test_get_llm_provider_404(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev({"name": "Acme"})
    resp = client.get("/config/llm-providers/missing", headers=_AUTH)
    assert resp.status_code == 404


def test_put_llm_provider_upserts(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev({"name": "Acme"})
    resp = client.put(
        "/config/llm-providers/default",
        json={"type": "openai", "model": "gpt-4o-mini"},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["providers"]["llm"]["default"]["model"] == "gpt-4o-mini"


def test_delete_llm_provider_404(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev({"name": "Acme"})
    resp = client.delete("/config/llm-providers/missing", headers=_AUTH)
    assert resp.status_code == 404


# ── /config/roles ──────────────────────────────────────────────────


def test_post_role_appends_root_role(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev({"name": "Acme", "roles": []})
    resp = client.post(
        "/config/roles",
        json={"name": "Designer", "goal": "ship pixels"},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    body = resp.json()
    assert "Added role 'Designer'" in body["summary"]
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["roles"][0]["name"] == "Designer"


def test_post_role_missing_name_returns_400(
    client: TestClient, store: MagicMock
) -> None:
    store.get_active.return_value = _rev({"name": "Acme", "roles": []})
    resp = client.post("/config/roles", json={"goal": "x"}, headers=_AUTH)
    assert resp.status_code == 400


def test_post_role_duplicate_handle_rejected(
    client: TestClient, store: MagicMock
) -> None:
    """A second role whose handle collides with an existing one must be
    rejected — duplicates derive the same agent id and route ambiguously."""
    store.get_active.return_value = _rev(
        {"name": "Acme", "roles": [{"name": "Designer"}]}
    )
    resp = client.post("/config/roles", json={"name": "Designer"}, headers=_AUTH)
    assert resp.status_code == 400
    store.insert_active.assert_not_called()


def test_post_role_duplicate_handle_in_unit_rejected(
    client: TestClient, store: MagicMock
) -> None:
    """The collision check walks units too, not just root roles."""
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "roles": [],
            "units": [{"name": "Eng", "roles": [{"name": "Sarah Chen"}]}],
        }
    )
    # "Sarah Chen" slugifies to "sarah-chen" — the canonical handle the
    # nested role already owns.
    resp = client.post("/config/roles", json={"name": "Sarah Chen"}, headers=_AUTH)
    assert resp.status_code == 400


def test_post_role_handle_uses_canonical_slugify(
    client: TestClient, store: MagicMock
) -> None:
    """A punctuated name collides with the engine's ``slugify`` handle —
    ``"QA/Test"`` → ``"qa-test"`` (slugified), never ``"qa/test"``."""
    store.get_active.return_value = _rev(
        {"name": "Acme", "roles": [{"handle": "qa-test", "name": "QA Tester"}]}
    )
    resp = client.post("/config/roles", json={"name": "QA/Test"}, headers=_AUTH)
    assert resp.status_code == 400


def test_put_role_404_when_not_found(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev({"name": "Acme", "roles": []})
    resp = client.put("/config/roles/nobody", json={"goal": "x"}, headers=_AUTH)
    assert resp.status_code == 404


def test_put_role_updates_existing(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "roles": [
                {"name": "Engineer", "goal": "old goal"},
            ],
        }
    )
    resp = client.put(
        "/config/roles/engineer",
        json={"goal": "new goal"},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    body = resp.json()
    assert "Updated role 'Engineer'" in body["summary"]
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["roles"][0]["goal"] == "new goal"


def test_delete_role_removes_existing(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "roles": [
                {"name": "Engineer", "goal": "x"},
                {"name": "Designer", "goal": "y"},
            ],
        }
    )
    resp = client.delete("/config/roles/engineer", headers=_AUTH)
    assert resp.status_code == 201
    body = resp.json()
    assert "Removed role 'Engineer'" in body["summary"]
    payload_arg = store.insert_active.call_args.args[0]
    assert len(payload_arg["roles"]) == 1
    assert payload_arg["roles"][0]["name"] == "Designer"


def test_list_roles(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "roles": [{"name": "Engineer"}],
            "units": [
                {
                    "name": "Eng",
                    "type": "team",
                    "roles": [{"name": "Designer"}],
                }
            ],
        }
    )
    resp = client.get("/config/roles", headers=_AUTH)
    assert resp.status_code == 200
    assert set(resp.json()) == {"engineer", "designer"}


# ── Auth on entity routes ─────────────────────────────────────────


def test_entity_route_requires_auth(client: TestClient) -> None:
    resp = client.put("/config/identity", json={"mission": "x"})
    assert resp.status_code == 401


# ── /config/embeddings (PUT) ───────────────────────────────────────


def test_put_embeddings_updates(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev({"name": "Acme"})
    resp = client.put(
        "/config/embeddings",
        json={"type": "openai", "model": "text-embedding-3-small"},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["providers"]["embeddings"]["model"] == "text-embedding-3-small"


# ── /config/learning (PUT) ─────────────────────────────────────────


def test_put_learning_updates(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev({"name": "Acme"})
    resp = client.put(
        "/config/learning",
        json={"enabled": False},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["learning"]["enabled"] is False


# ── /config/integrations/{kind} ────────────────────────────────────


def test_put_integration_unknown_kind_404(client: TestClient) -> None:
    resp = client.put(
        "/config/integrations/notion",
        json={"url": "x"},
        headers=_AUTH,
    )
    assert resp.status_code == 404


def test_put_integration_jira_writes_nested(
    client: TestClient, store: MagicMock
) -> None:
    """PUT /config/integrations/jira lands under ``integrations.jira``."""
    store.get_active.return_value = _rev({"name": "Acme"})
    resp = client.put(
        "/config/integrations/jira",
        json={"url": "https://x.atlassian.net", "token": "${T}"},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["integrations"]["jira"]["url"] == "https://x.atlassian.net"


def test_put_integration_gitlab_writes_nested(
    client: TestClient, store: MagicMock
) -> None:
    """PUT /config/integrations/gitlab is an allowed kind and lands under
    ``integrations.gitlab``."""
    store.get_active.return_value = _rev({"name": "Acme"})
    resp = client.put(
        "/config/integrations/gitlab",
        json={"enabled": True, "url": "https://gitlab.com", "signing_secret": "${S}"},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["integrations"]["gitlab"]["url"] == "https://gitlab.com"


def test_put_integration_plane_writes_nested(
    client: TestClient, store: MagicMock
) -> None:
    """PUT /config/integrations/plane is an allowed kind and lands under
    ``integrations.plane``."""
    store.get_active.return_value = _rev({"name": "Acme"})
    resp = client.put(
        "/config/integrations/plane",
        json={
            "enabled": True,
            "url": "https://plane.example",
            "workspace": "acme",
            "webhook_secret": "${S}",
        },
        headers=_AUTH,
    )
    assert resp.status_code == 201
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["integrations"]["plane"]["url"] == "https://plane.example"


def test_put_integration_preserves_sibling_integrations(
    client: TestClient, store: MagicMock
) -> None:
    """Replacing one integration block must not clobber the others."""
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "integrations": {
                "confluence": {"url": "https://x.atlassian.net/wiki", "token": "${T}"}
            },
        }
    )
    resp = client.put(
        "/config/integrations/jira",
        json={"url": "https://x.atlassian.net", "token": "${T}"},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    integrations = store.insert_active.call_args.args[0]["integrations"]
    assert "jira" in integrations
    assert "confluence" in integrations  # sibling not clobbered


# ── /config/mcp-servers ────────────────────────────────────────────


def test_post_mcp_server_appends(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev({"name": "Acme"})
    resp = client.post(
        "/config/mcp-servers",
        json={"name": "calculator", "command": "uvx", "args": ["mcp-calc"]},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    body = resp.json()
    assert "Added MCP server 'calculator'" in body["summary"]
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["mcp_servers"][0]["name"] == "calculator"


def test_post_mcp_server_duplicate_409(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "mcp_servers": [{"name": "calc", "command": "x", "args": []}],
        }
    )
    resp = client.post(
        "/config/mcp-servers",
        json={"name": "calc", "command": "y", "args": []},
        headers=_AUTH,
    )
    assert resp.status_code == 400


def test_delete_mcp_server_removes(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "mcp_servers": [
                {"name": "calc", "command": "uvx", "args": []},
                {"name": "calendar", "command": "uvx", "args": []},
            ],
        }
    )
    resp = client.delete("/config/mcp-servers/calc", headers=_AUTH)
    assert resp.status_code == 201
    body = resp.json()
    assert "Removed MCP server 'calc'" in body["summary"]
    payload_arg = store.insert_active.call_args.args[0]
    assert len(payload_arg["mcp_servers"]) == 1
    assert payload_arg["mcp_servers"][0]["name"] == "calendar"


# ── /config/audit ──────────────────────────────────────────────────


def test_config_audit_returns_revisions(client: TestClient, store: MagicMock) -> None:
    rev = _rev({"name": "Acme"})
    store.list_revisions = AsyncMock(return_value=[rev])
    resp = client.get("/config/audit", headers=_AUTH)
    assert resp.status_code == 200
    body = resp.json()
    assert "revisions" in body
    assert len(body["revisions"]) == 1
    assert "revision_id" in body["revisions"][0]


def test_config_audit_limit_validation(client: TestClient) -> None:
    resp = client.get("/config/audit?limit=999", headers=_AUTH)
    assert resp.status_code == 400


# ── /config/units (CRUD) ───────────────────────────────────────────


def test_list_units(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "units": [
                {"name": "Engineering", "type": "team"},
                {
                    "name": "Product",
                    "type": "department",
                    "children": [{"name": "Design", "type": "team"}],
                },
            ],
        }
    )
    resp = client.get("/config/units", headers=_AUTH)
    assert resp.status_code == 200
    assert set(resp.json()) == {"Engineering", "Product", "Design"}


def test_get_unit_walks_nested_children(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "units": [
                {
                    "name": "Tech",
                    "type": "department",
                    "children": [
                        {"name": "Platform", "type": "team", "lead": "Tech Lead"}
                    ],
                }
            ],
        }
    )
    resp = client.get("/config/units/Platform", headers=_AUTH)
    assert resp.status_code == 200
    assert resp.json()["lead"] == "Tech Lead"


def test_post_unit_appends_root_unit(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev({"name": "Acme", "units": []})
    resp = client.post(
        "/config/units",
        json={"name": "Marketing", "type": "team"},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["units"][0]["name"] == "Marketing"


def test_post_unit_duplicate_rejected(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "units": [{"name": "Marketing", "type": "team"}],
        }
    )
    resp = client.post(
        "/config/units",
        json={"name": "Marketing", "type": "team"},
        headers=_AUTH,
    )
    assert resp.status_code == 400


def test_put_unit_merge_update(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "units": [{"name": "Engineering", "type": "team", "lead": "Old"}],
        }
    )
    resp = client.put(
        "/config/units/Engineering",
        json={"lead": "New Lead"},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["units"][0]["lead"] == "New Lead"
    assert payload_arg["units"][0]["type"] == "team"  # untouched


def test_delete_unit_removes(client: TestClient, store: MagicMock) -> None:
    store.get_active.return_value = _rev(
        {
            "name": "Acme",
            "units": [
                {"name": "Engineering", "type": "team"},
                {"name": "Product", "type": "team"},
            ],
        }
    )
    resp = client.delete("/config/units/Engineering", headers=_AUTH)
    assert resp.status_code == 201
    payload_arg = store.insert_active.call_args.args[0]
    assert len(payload_arg["units"]) == 1
    assert payload_arg["units"][0]["name"] == "Product"
