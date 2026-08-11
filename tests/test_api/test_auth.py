"""Tests for the ``/config/*`` bearer-token middleware."""

from __future__ import annotations

import os
from collections.abc import Awaitable, Callable
from unittest.mock import AsyncMock, MagicMock

import pytest
from starlette.testclient import TestClient

from crewlet.api.app import create_app
from crewlet.api.auth import TokenLoadError, load_tokens
from crewlet.config import ApiAuthConfig, ApiAuthTokenConfig, ApiConfig, BootstrapConfig
from crewlet.events.types import Event


class _MockQueue:
    async def publish(self, topic: str, event: Event) -> None: ...
    async def subscribe(
        self,
        topic: str,
        group: str,
        handler: Callable[[Event], Awaitable[None]],
    ) -> None: ...
    async def start(self) -> None: ...
    async def stop(self) -> None: ...


def _bootstrap_with_tokens(*pairs: tuple[str, str]) -> BootstrapConfig:
    return BootstrapConfig(
        api=ApiConfig(
            auth=ApiAuthConfig(
                tokens=[ApiAuthTokenConfig(id=i, token=t) for i, t in pairs]
            )
        ),
    )


def _store_mock() -> MagicMock:
    """Minimal CompanyConfigStore stub."""
    s = MagicMock()
    s.get_active = AsyncMock(return_value=None)
    s.list_revisions = AsyncMock(return_value=[])
    s.get_revision = AsyncMock(return_value=None)
    s.has_any = AsyncMock(return_value=False)
    return s


# ── load_tokens ─────────────────────────────────────────────────────


def test_load_tokens_returns_id_to_token_map() -> None:
    bootstrap = _bootstrap_with_tokens(("founder", "f-secret"), ("ops", "o-secret"))
    result = load_tokens(bootstrap)
    assert result == {"founder": "f-secret", "ops": "o-secret"}


def test_load_tokens_disabled_returns_empty(caplog) -> None:
    bootstrap = BootstrapConfig(api=ApiConfig(auth=ApiAuthConfig(disabled=True)))
    assert load_tokens(bootstrap) == {}


def test_load_tokens_empty_list_fatal() -> None:
    bootstrap = BootstrapConfig()
    with pytest.raises(TokenLoadError, match="empty"):
        load_tokens(bootstrap)


def test_load_tokens_empty_resolved_token_fatal() -> None:
    bootstrap = _bootstrap_with_tokens(("ci", ""))
    with pytest.raises(TokenLoadError, match="empty string"):
        load_tokens(bootstrap)


def test_load_tokens_duplicate_id_fatal() -> None:
    bootstrap = _bootstrap_with_tokens(("ci", "a"), ("ci", "b"))
    with pytest.raises(TokenLoadError, match="Duplicate"):
        load_tokens(bootstrap)


def test_load_tokens_rejects_anonymous_id() -> None:
    """``"anonymous"`` is the sentinel used when auth.disabled=True;
    allowing a real token with this id would collide audit attribution
    between disabled-mode requests and real operator writes."""
    bootstrap = _bootstrap_with_tokens(("anonymous", "leaked-token"))
    with pytest.raises(TokenLoadError, match="reserved"):
        load_tokens(bootstrap)


def test_load_bootstrap_resolves_env_var_tokens(tmp_path) -> None:
    """End-to-end: env-resolved tokens flow through to load_tokens."""
    os.environ["AUTH_TEST_TOKEN"] = "resolved-token-xyz"
    try:
        from crewlet.config import load_bootstrap_config

        cfg = tmp_path / "config.yaml"
        cfg.write_text(
            "api:\n"
            "  auth:\n"
            "    tokens:\n"
            "      - id: t1\n"
            '        token: "${AUTH_TEST_TOKEN}"\n'
        )
        bootstrap = load_bootstrap_config(cfg)
        assert load_tokens(bootstrap) == {"t1": "resolved-token-xyz"}
    finally:
        del os.environ["AUTH_TEST_TOKEN"]


# ── middleware behaviour ────────────────────────────────────────────


def _make_app_with_auth(*token_pairs: tuple[str, str]):
    return create_app(
        event_queue=_MockQueue(),
        bootstrap=_bootstrap_with_tokens(*token_pairs),
        company_config_store=_store_mock(),
    )


def test_middleware_allows_unguarded_routes() -> None:
    app = _make_app_with_auth(("founder", "secret"))
    client = TestClient(app)
    # /health is not under /config and must not require auth.
    resp = client.get("/health")
    assert resp.status_code == 200


def test_middleware_rejects_missing_auth_header() -> None:
    app = _make_app_with_auth(("founder", "secret"))
    client = TestClient(app)
    resp = client.get("/config")
    assert resp.status_code == 401
    assert resp.json() == {"error": "invalid_token"}


def test_middleware_rejects_wrong_token() -> None:
    app = _make_app_with_auth(("founder", "secret"))
    client = TestClient(app)
    resp = client.get("/config", headers={"Authorization": "Bearer wrong"})
    assert resp.status_code == 401


def test_middleware_rejects_wrong_scheme() -> None:
    app = _make_app_with_auth(("founder", "secret"))
    client = TestClient(app)
    resp = client.get("/config", headers={"Authorization": "Basic dXNlcjpwYXNz"})
    assert resp.status_code == 401


def test_middleware_accepts_valid_token() -> None:
    app = _make_app_with_auth(("founder", "secret-1"))
    client = TestClient(app)
    # No active revision → 404 from the route; middleware passed.
    resp = client.get("/config", headers={"Authorization": "Bearer secret-1"})
    assert resp.status_code == 404
    assert resp.json() == {"error": "no_active_revision"}


def test_middleware_accepts_any_when_disabled() -> None:
    bootstrap = BootstrapConfig(api=ApiConfig(auth=ApiAuthConfig(disabled=True)))
    app = create_app(
        event_queue=_MockQueue(),
        bootstrap=bootstrap,
        company_config_store=_store_mock(),
    )
    client = TestClient(app)
    resp = client.get("/config")
    # Auth bypass kicks in; route returns 404 (unconfigured).
    assert resp.status_code == 404


def test_middleware_constant_time_compare_works_for_multiple_tokens() -> None:
    """Both tokens must work; the iteration is constant-time per check."""
    app = _make_app_with_auth(("alice", "alice-token"), ("bob", "bob-token"))
    client = TestClient(app)
    assert (
        client.get(
            "/config", headers={"Authorization": "Bearer alice-token"}
        ).status_code
        == 404
    )
    assert (
        client.get("/config", headers={"Authorization": "Bearer bob-token"}).status_code
        == 404
    )
