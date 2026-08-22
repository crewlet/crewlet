"""Tests for the API bearer-token middleware."""

from __future__ import annotations

import json
import os
from collections.abc import Awaitable, Callable
from unittest.mock import AsyncMock, MagicMock

import pytest
from starlette.testclient import TestClient

from crewlet.api.auth import (
    ApiAuthMiddleware,
    TokenLoadError,
    load_tokens,
    requires_token,
)
from crewlet.api.config_routes import build_config_routes
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


def test_load_tokens_empty_list_serves_reads_only() -> None:
    """No tokens is a legitimate posture, not a misconfiguration.

    Reads serve; writes and ``/config`` are refused outright, because no
    token can ever match an empty map. A deployment that never writes
    config therefore has no credential to manage — strictly safer than
    being made to mint one it will not use.
    """
    assert load_tokens(BootstrapConfig()) == {}


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
    """What ``allow_anonymous_read: false`` actually closes.

    Auth is a policy over the whole surface, not a ``/config/*`` prefix:
    ``/events``, ``/agents/{id}/memory`` and ``/ws/stream`` serve full
    LLM transcripts — prompts, tool arguments, diary entries — so an
    operator who turns reads off has to get all three, not the two
    somebody remembered to list.
    """

    @staticmethod
    def _bootstrap(**auth: object):
        from crewlet.config import (
            ApiAuthConfig,
            ApiAuthTokenConfig,
            ApiConfig,
            BootstrapConfig,
        )

        tokens = [ApiAuthTokenConfig(id="founder", token="s3cret")]
        # Reads are open by default; these cases are about the posture
        # that closes them, so it is set explicitly rather than relied on.
        auth.setdefault("allow_anonymous_read", False)
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

    def test_reads_are_open_unless_the_operator_closes_them(self) -> None:
        """The default, asserted on a config that says nothing about it.

        The dashboard is a read surface served unauthenticated by
        design — the page that would prompt for a token cannot itself
        require one — so a default of "guarded" put a modal in front of
        every first load.
        """
        from starlette.testclient import TestClient

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
                    tokens=[ApiAuthTokenConfig(id="founder", token="s3cret")]
                )
            )
        )
        app = create_app(MagicMock(), bootstrap=bootstrap)
        app.state.configured = True
        client = TestClient(app, raise_server_exceptions=False)

        assert client.get("/agents").status_code != 401
        assert client.get("/events").status_code != 401
        # …and the half that is never open by default.
        assert client.get("/config").status_code == 401
        assert client.put("/config", content=b"name: x").status_code == 401

    def test_disabled_opens_everything(self) -> None:
        client = self._client(disabled=True)
        assert client.get("/agents").status_code != 401
        assert client.get("/events").status_code != 401


class TestAnUnreachableApiRefusesToStart:
    """The one token combination that cannot serve anybody.

    Guarding every route behind a credential that does not exist is not
    a strict posture, it is an outage — and one whose symptom is a
    uniform 401 that reads exactly like a wrong token.
    """

    @staticmethod
    def _bootstrap(**auth: object):
        from crewlet.config import ApiAuthConfig, ApiConfig, BootstrapConfig

        return BootstrapConfig(api=ApiConfig(auth=ApiAuthConfig(**auth)))  # type: ignore[arg-type]

    def test_reads_closed_with_no_token_refuses_to_start(self) -> None:
        from crewlet.api.auth import TokenLoadError, load_tokens

        with pytest.raises(TokenLoadError, match="nothing is reachable"):
            load_tokens(self._bootstrap(allow_anonymous_read=False))

    @pytest.mark.parametrize("posture", [{}, {"disabled": True}])
    def test_a_serving_posture_needs_no_token(self, posture: dict) -> None:
        from crewlet.api.auth import load_tokens

        assert load_tokens(self._bootstrap(**posture)) == {}


class TestAnonymousReadIsAnnounced:
    """A posture nobody is told about is a posture nobody reviews.

    The level carries the judgement: on loopback this is a laptop, on
    anything else the transcripts are readable by whoever reaches the
    port. A warning that fires identically for both is one nobody reads
    by the third deployment.
    """

    @staticmethod
    def _build(host: str, caplog):
        import logging

        from crewlet.config import ApiConfig, BootstrapConfig
        from tests.test_api.helpers import create_app

        bootstrap = BootstrapConfig(api=ApiConfig(host=host))
        with caplog.at_level(logging.INFO, logger="crewlet.api.app"):
            create_app(MagicMock(), bootstrap=bootstrap)
        return [r for r in caplog.records if "api_anonymous_read_enabled" in r.message]

    def test_a_reachable_bind_warns(self, caplog) -> None:
        records = self._build("0.0.0.0", caplog)
        assert [r.levelname for r in records] == ["WARNING"]

    def test_loopback_states_it_without_warning(self, caplog) -> None:
        records = self._build("127.0.0.1", caplog)
        assert [r.levelname for r in records] == ["INFO"]

    def test_closing_reads_says_nothing(self, caplog) -> None:
        import logging

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
                    allow_anonymous_read=False,
                )
            )
        )
        with caplog.at_level(logging.INFO, logger="crewlet.api.app"):
            create_app(MagicMock(), bootstrap=bootstrap)
        assert not [
            r for r in caplog.records if "api_anonymous_read_enabled" in r.message
        ]


class TestWebSocketAuth:
    """``BaseHTTPMiddleware`` never sees a WebSocket scope, so the stream —
    the single richest endpoint — has to check for itself.

    All of these run with reads closed. That is the posture where the
    handshake check exists at all: with reads open the socket is a read
    like any other and serves without a credential.
    """

    @staticmethod
    def _app(**auth: object):
        auth.setdefault("allow_anonymous_read", False)
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


class TestTheGuardIsAlwaysMounted:
    """The guard's presence must not depend on a second, unrelated flag.

    ``create_app`` registered the ``/config`` write surface whenever a
    ``company_config_store`` was passed, and mounted ``ApiAuthMiddleware``
    whenever a ``bootstrap`` was passed. Two independent conditions
    decided one security property. Every caller in the tree passes both
    or neither, so nothing was exposed — but a caller passing a store
    without a bootstrap got ``PUT /config``, ``POST
    /config/{rev}/activate`` and every entity route with no guard in
    front of them, no error, and no log line saying so.

    Nothing here was asserted before. A test that only ever builds the
    app the way production builds it cannot notice that the guard is
    optional.
    """

    WRITES = [
        ("PUT", "/config"),
        ("PUT", "/config/identity"),
        ("POST", "/config/roles"),
        ("DELETE", "/config/roles/ceo"),
        ("POST", "/config/1/activate"),
        ("PUT", "/config/budgets"),
        ("POST", "/config/mcp-servers"),
        ("DELETE", "/config/extensions/x"),
    ]

    def _no_bootstrap_app(self):
        app = create_app(
            event_queue=_MockQueue(),
            company_config_store=_store_mock(),
        )
        # Otherwise the data routes answer 503 (no active company) and the
        # read assertions below would pass for the wrong reason.
        app.state.configured = True
        return app

    @pytest.mark.parametrize("method,path", WRITES)
    def test_a_config_write_is_refused_with_no_tier_a(
        self, method: str, path: str
    ) -> None:
        client = TestClient(self._no_bootstrap_app(), raise_server_exceptions=False)
        resp = client.request(method, path, json={})
        assert resp.status_code == 401, (
            f"{method} {path} reached its handler with no token and no Tier A"
        )

    def test_reading_the_config_is_refused_too(self) -> None:
        # /config is never eligible for anonymous read: the document is
        # the whole company.
        client = TestClient(self._no_bootstrap_app())
        assert client.get("/config").status_code == 401
        assert client.get("/config/roles").status_code == 401

    def test_ordinary_reads_still_serve(self) -> None:
        # The token-less app takes the posture `load_tokens` documents for
        # a read-only deployment. Closing reads as well would brick every
        # programmatic embed, which is why the fix is a posture and not a
        # refusal to build.
        client = TestClient(self._no_bootstrap_app())
        assert client.get("/health").status_code == 200
        # Not 200: this app has no event store wired, so /events answers
        # its own 503. What matters is that the GUARD let it through —
        # asserting 200 would tie an auth test to an unrelated readiness
        # check and fail for the wrong reason.
        assert client.get("/events").status_code != 401
        assert client.get("/org").status_code != 401

    def test_the_middleware_is_mounted_either_way(self) -> None:
        # The property itself, stated once: whether or not Tier A is
        # present, something is enforcing `requires_token`.
        for app in (self._no_bootstrap_app(), _make_app_with_auth(("f", "s"))):
            assert app.state.auth_enabled is True
            assert any(m.cls is ApiAuthMiddleware for m in app.user_middleware), (
                "ApiAuthMiddleware is not mounted"
            )

    def test_a_config_write_surface_never_ships_unguarded(self) -> None:
        # The invariant behind all of the above, checked against the
        # routes actually registered rather than against a list kept by
        # hand: if a /config route exists, the guard exists.
        from crewlet.api.auth import requires_token

        app = self._no_bootstrap_app()
        config_routes = [
            r for r in app.routes if getattr(r, "path", "").startswith("/config")
        ]
        assert config_routes, "no /config routes registered — the test proves nothing"
        for route in config_routes:
            for method in sorted(getattr(route, "methods", None) or []):
                assert requires_token(app.state, path=route.path, method=method), (
                    f"{method} {route.path} does not require a token"
                )


class TestTheRuleItself:
    """`requires_token` is "the whole rule, in one function" — and nothing
    called it directly.

    Every other test here drives it through a built app, which means each
    one only ever exercises the branches that app's posture happens to
    reach. The rule can be edited into an open one — `POST` added to
    `READ_METHODS`, the `/config` branch dropped, an exemption widened to
    a prefix — while a suite full of app-level assertions keeps passing
    because no app in it is configured to notice.
    """

    class _Open:
        auth_anonymous_read = True

    class _Closed:
        auth_anonymous_read = False

    WRITE_METHODS = ["POST", "PUT", "PATCH", "DELETE"]

    @pytest.mark.parametrize("method", WRITE_METHODS)
    @pytest.mark.parametrize("path", ["/config", "/config/roles", "/agents", "/org"])
    def test_a_write_always_needs_a_token(self, method: str, path: str) -> None:
        # Both postures. `allow_anonymous_read` opens READS; it has never
        # been allowed to open a write, and this is the assertion that
        # says so out loud.
        assert requires_token(self._Open, path=path, method=method) is True
        assert requires_token(self._Closed, path=path, method=method) is True

    @pytest.mark.parametrize("method", ["GET", "HEAD", "OPTIONS"])
    def test_a_read_follows_the_posture(self, method: str) -> None:
        assert requires_token(self._Open, path="/agents", method=method) is False
        assert requires_token(self._Closed, path="/agents", method=method) is True

    @pytest.mark.parametrize("method", ["GET", "HEAD", "OPTIONS", "POST", "PUT"])
    def test_config_is_guarded_in_every_posture_and_method(self, method: str) -> None:
        # Reading it exposes the whole company document; writing it
        # changes the company. Neither is ever anonymous.
        assert requires_token(self._Open, path="/config", method=method) is True
        assert requires_token(self._Closed, path="/config", method=method) is True

    @pytest.mark.parametrize(
        "path", ["/health", "/ready", "/", "/dashboard", "/favicon.ico"]
    )
    def test_the_exemptions_are_exact_not_prefixes(self, path: str) -> None:
        assert requires_token(self._Closed, path=path, method="GET") is False
        # …and a route that merely STARTS with one of them is not exempt.
        # `/health` and `/ready` were prefixes, so the day somebody added
        # a `/health-admin` it would have been unauthenticated, silently.
        assert requires_token(self._Closed, path=path + "-admin", method="GET") is True
        assert requires_token(self._Closed, path=path + "x", method="POST") is True

    @pytest.mark.parametrize("prefix", ["/webhooks/", "/otlp/", "/static/"])
    def test_the_prefix_exemptions_cover_their_subtree_only(self, prefix: str) -> None:
        assert requires_token(self._Closed, path=prefix + "a/b", method="POST") is False
        # Each ends in a slash, which is what stops it exempting a
        # sibling that merely shares its stem.
        sibling = prefix.rstrip("/") + "-admin"
        assert requires_token(self._Closed, path=sibling, method="POST") is True

    def test_case_and_trailing_slash_do_not_dodge_the_config_guard(self) -> None:
        # Starlette gives the middleware the raw (percent-decoded) path,
        # and its router redirects `/config/` to `/config` — so a client
        # that adds a slash must not be handed an unguarded first hop.
        assert requires_token(self._Open, path="/config/", method="PUT") is True
        # An upper-case spelling does NOT match the guarded prefix, and
        # must not match a route either. This is the assertion that fails
        # if anyone ever makes the router case-insensitive without making
        # this rule case-insensitive too.
        assert not any(r.path == "/CONFIG" for r in build_config_routes())
