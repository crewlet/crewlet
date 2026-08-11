"""Tests for the SlackManifestClient over a mocked httpx transport."""

from __future__ import annotations

import json
from urllib.parse import parse_qs

import httpx
import pytest

from crewlet.slack.api import SlackApiError, SlackManifestClient


def _client(handler) -> SlackManifestClient:
    transport = httpx.MockTransport(handler)
    return SlackManifestClient(httpx.AsyncClient(transport=transport))


def _form(request: httpx.Request) -> dict[str, str]:
    parsed = parse_qs(request.content.decode())
    return {k: v[0] for k, v in parsed.items()}


async def test_create_app_parses_credentials() -> None:
    seen: dict[str, httpx.Request] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["request"] = request
        return httpx.Response(
            200,
            json={
                "ok": True,
                "app_id": "A0001",
                "credentials": {
                    "client_id": "111.222",
                    "client_secret": "csec",
                    "verification_token": "vtok",
                    "signing_secret": "ssec",
                },
                "oauth_authorize_url": "https://slack.com/oauth/v2/authorize?x=1",
            },
        )

    manifest = {"display_information": {"name": "X"}}
    async with _client(handler) as client:
        creds = await client.create_app("xoxe.config", manifest)

    request = seen["request"]
    assert request.url.path == "/api/apps.manifest.create"
    assert request.headers["authorization"] == "Bearer xoxe.config"
    # The manifest travels as a JSON-encoded string field.
    assert json.loads(_form(request)["manifest"]) == manifest
    assert creds.app_id == "A0001"
    assert creds.client_id == "111.222"
    assert creds.client_secret == "csec"
    assert creds.signing_secret == "ssec"


async def test_update_app_sends_app_id() -> None:
    seen: dict[str, httpx.Request] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["request"] = request
        return httpx.Response(200, json={"ok": True})

    async with _client(handler) as client:
        await client.update_app("xoxe.config", "A0009", {"a": 1})

    assert seen["request"].url.path == "/api/apps.manifest.update"
    assert _form(seen["request"])["app_id"] == "A0009"


async def test_invalid_manifest_error_carries_messages() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "ok": False,
                "error": "invalid_manifest",
                "errors": [
                    {"message": "invalid scope", "pointer": "/oauth_config/scopes"},
                    {"message": "name too long"},
                ],
            },
        )

    async with _client(handler) as client:
        with pytest.raises(SlackApiError) as excinfo:
            await client.validate_manifest("xoxe.config", {})

    err = excinfo.value
    assert err.error == "invalid_manifest"
    assert err.messages == [
        "invalid scope (/oauth_config/scopes)",
        "name too long",
    ]
    assert "invalid scope" in str(err)


async def test_rotate_config_token() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/api/tooling.tokens.rotate"
        assert _form(request)["refresh_token"] == "xoxe-1-old"
        # No auth header on rotation — the refresh token IS the credential.
        assert "authorization" not in request.headers
        return httpx.Response(
            200,
            json={
                "ok": True,
                "token": "xoxe.new",
                "refresh_token": "xoxe-1-new",
                "exp": 1789000000,
            },
        )

    async with _client(handler) as client:
        pair = await client.rotate_config_token("xoxe-1-old")

    assert pair.token == "xoxe.new"
    assert pair.refresh_token == "xoxe-1-new"
    # exp is persisted so later runs can skip rotation while fresh.
    assert pair.expires_at == 1789000000


async def test_exchange_oauth_code() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/api/oauth.v2.access"
        form = _form(request)
        assert form["code"] == "tmpcode"
        assert form["client_id"] == "111.222"
        assert form["client_secret"] == "csec"
        assert form["redirect_uri"] == "https://x.example/webhooks/slack-oauth"
        return httpx.Response(
            200,
            json={
                "ok": True,
                "access_token": "xoxb-abc",
                "app_id": "A0001",
                "bot_user_id": "U0BOT",
                "team": {"id": "T01", "name": "Acme"},
            },
        )

    async with _client(handler) as client:
        result = await client.exchange_oauth_code(
            code="tmpcode",
            client_id="111.222",
            client_secret="csec",
            redirect_url="https://x.example/webhooks/slack-oauth",
        )

    assert result.bot_token == "xoxb-abc"
    assert result.app_id == "A0001"
    assert result.bot_user_id == "U0BOT"
    assert result.team_id == "T01"


async def test_ratelimit_waited_out_with_delta_seconds() -> None:
    calls = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] <= 2:
            return httpx.Response(429, headers={"retry-after": "0"})
        return httpx.Response(200, json={"ok": True})

    async with _client(handler) as client:
        await client.validate_manifest("xoxe.config", {})

    # Multiple consecutive 429s are waited out within the wall-clock
    # budget — a single-retry client aborted a multi-agent run here.
    assert calls["n"] == 3


async def test_ratelimit_http_date_retry_after_handled() -> None:
    """The RFC 9110 HTTP-date form must not crash the client (it used
    to raise an uncaught ValueError from float())."""
    calls = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] == 1:
            # A date in the past → parsed delay clamps to 0.
            return httpx.Response(
                429, headers={"retry-after": "Wed, 21 Oct 2015 07:28:00 GMT"}
            )
        return httpx.Response(200, json={"ok": True})

    async with _client(handler) as client:
        await client.validate_manifest("xoxe.config", {})

    assert calls["n"] == 2


async def test_ratelimit_budget_exhaustion_raises_slack_api_error() -> None:
    """A Retry-After beyond the remaining budget fails the call as a
    typed SlackApiError instead of sleeping past the budget."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(429, headers={"retry-after": "99999"})

    async with _client(handler) as client:
        with pytest.raises(SlackApiError) as excinfo:
            await client.validate_manifest("xoxe.config", {})

    assert "ratelimited" in excinfo.value.error


async def test_non_json_response_raises() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(502, text="<html>bad gateway</html>")

    async with _client(handler) as client:
        with pytest.raises(SlackApiError) as excinfo:
            await client.validate_manifest("xoxe.config", {})

    assert "non-JSON" in excinfo.value.error
