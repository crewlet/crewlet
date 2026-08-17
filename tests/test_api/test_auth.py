"""Tests for the API bearer-token middleware."""

from __future__ import annotations

import json
import os
from collections.abc import Awaitable, Callable
from unittest.mock import AsyncMock, MagicMock

import pytest
from starlette.testclient import TestClient

from crewlet.api.auth import TokenLoadError, load_tokens
from crewlet.config import ApiAuthConfig, ApiAuthTokenConfig, ApiConfig, BootstrapConfig
from crewlet.events.types import Event
from tests.test_api.helpers import create_app


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


# ---------------------------------------------------------------------------
# Full-surface auth
# ---------------------------------------------------------------------------


class TestFullSurfaceAuth:
    """Auth used to guard only ``/config/*``, which left ``/events``,
    ``/agents/{id}/memory`` and ``/ws/stream`` serving full LLM
    transcripts — prompts, tool arguments, diary entries — to anyone who
    could reach the port."""

    @staticmethod
    def _bootstrap(**auth: object):
        from crewlet.config import (
            ApiAuthConfig,
            ApiAuthTokenConfig,
            ApiConfig,
            BootstrapConfig,
        )

        tokens = [ApiAuthTokenConfig(id="founder", token="s3cret")]
        return BootstrapConfig(
            api=ApiConfig(auth=ApiAuthConfig(tokens=tokens, **auth))  # type: ignore[arg-type]
        )

    def _client(self, **auth: object):
        from starlette.testclient import TestClient

        from tests.test_api.helpers import create_app

        app = create_app(MagicMock(), bootstrap=self._bootstrap(**auth))
        app.state.configured = True
        return TestClient(app, raise_server_exceptions=False)

    @pytest.mark.parametrize("path", ["/agents", "/events", "/stream/snapshot"])
    def test_data_routes_require_a_token(self, path: str) -> None:
        assert self._client().get(path).status_code == 401

    @pytest.mark.parametrize("path", ["/agents", "/events", "/stream/snapshot"])
    def test_data_routes_pass_with_a_token(self, path: str) -> None:
        resp = self._client().get(path, headers={"Authorization": "Bearer s3cret"})
        assert resp.status_code != 401

    @pytest.mark.parametrize("path", ["/health", "/ready"])
    def test_probes_stay_open(self, path: str) -> None:
        """An orchestrator has no token, and a liveness check that 401s is
        a liveness check that fails."""
        assert self._client().get(path).status_code in (200, 503)

    def test_dashboard_shell_stays_open(self) -> None:
        """The page that prompts for the token cannot itself require one.
        It ships no data — every byte it renders comes from an
        authenticated fetch."""
        assert self._client().get("/dashboard").status_code == 200

    def test_webhooks_stay_open_to_the_middleware(self) -> None:
        """They authenticate by provider HMAC, which is stronger than a
        shared bearer token — so they must not also 401."""
        resp = self._client().post("/webhooks/jira", json={"webhookEvent": "x"})
        assert resp.status_code != 401

    def test_anonymous_read_opens_reads_but_not_writes(self) -> None:
        client = self._client(allow_anonymous_read=True)
        assert client.get("/agents").status_code != 401
        # /config is never eligible: reading it exposes the whole company
        # document, and writing it changes the company.
        assert client.get("/config").status_code == 401
        assert client.put("/config", content=b"name: x").status_code == 401

    def test_disabled_opens_everything(self) -> None:
        client = self._client(disabled=True)
        assert client.get("/agents").status_code != 401
        assert client.get("/events").status_code != 401


class TestAuthDecisionIsRequired:
    """Serving the API requires an explicit decision — silence used to
    mean 'open'."""

    @staticmethod
    def _bootstrap(**auth: object):
        from crewlet.config import ApiAuthConfig, ApiConfig, BootstrapConfig

        return BootstrapConfig(api=ApiConfig(auth=ApiAuthConfig(**auth)))  # type: ignore[arg-type]

    def test_no_tokens_and_no_decision_refuses_to_start(self) -> None:
        from crewlet.api.auth import TokenLoadError, load_tokens

        with pytest.raises(TokenLoadError, match="explicit decision"):
            load_tokens(self._bootstrap())

    @pytest.mark.parametrize("decision", ["disabled", "allow_anonymous_read"])
    def test_an_explicit_decision_is_accepted(self, decision: str) -> None:
        from crewlet.api.auth import load_tokens

        assert load_tokens(self._bootstrap(**{decision: True})) == {}


class TestWebSocketAuth:
    """``BaseHTTPMiddleware`` never sees a WebSocket scope, so the stream —
    the single richest endpoint — has to check for itself."""

    @staticmethod
    def _app(**auth: object):
        from crewlet.config import (
            ApiAuthConfig,
            ApiAuthTokenConfig,
            ApiConfig,
            BootstrapConfig,
        )
        from tests.test_api.helpers import create_app

        bootstrap = BootstrapConfig(
            api=ApiConfig(
                auth=ApiAuthConfig(
                    tokens=[ApiAuthTokenConfig(id="f", token="s3cret")],
                    **auth,  # type: ignore[arg-type]
                )
            )
        )
        app = create_app(MagicMock(), bootstrap=bootstrap)
        app.state.configured = True
        return app

    def test_handshake_without_a_token_is_rejected(self) -> None:
        from starlette.testclient import TestClient
        from starlette.websockets import WebSocketDisconnect

        with TestClient(self._app()) as client:  # noqa: SIM117
            with pytest.raises(WebSocketDisconnect):
                with client.websocket_connect("/ws/stream") as ws:
                    ws.receive_text()

    def test_handshake_with_a_query_token_is_accepted(self) -> None:
        """Browsers cannot set headers on a WebSocket constructor, so the
        query form has to work."""
        from starlette.testclient import TestClient

        with (
            TestClient(self._app()) as client,
            client.websocket_connect("/ws/stream?token=s3cret") as ws,
        ):
            assert json.loads(ws.receive_text())["kind"] == "snapshot"

    def test_handshake_with_a_header_token_is_accepted(self) -> None:
        from starlette.testclient import TestClient

        with (
            TestClient(self._app()) as client,
            client.websocket_connect(
                "/ws/stream", headers={"Authorization": "Bearer s3cret"}
            ) as ws,
        ):
            assert json.loads(ws.receive_text())["kind"] == "snapshot"

    def test_a_wrong_token_is_rejected(self) -> None:
        from starlette.testclient import TestClient
        from starlette.websockets import WebSocketDisconnect

        with TestClient(self._app()) as client:  # noqa: SIM117
            with pytest.raises(WebSocketDisconnect):
                with client.websocket_connect("/ws/stream?token=nope") as ws:
                    ws.receive_text()
