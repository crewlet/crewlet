"""Tests for the Mattermost REST client and the URL contract it owns."""

from __future__ import annotations

import httpx
import pytest

from crewlet.mattermost.client import (
    DEFAULT_TYPING_THROTTLE_MS,
    MattermostClient,
    MattermostError,
    normalize_base_url,
    site_urls_match,
    typing_throttle_from,
    websocket_url,
)


def _client(handler, *, token: str = "tok") -> MattermostClient:
    """A client whose transport is a scripted handler."""
    client = MattermostClient("https://chat.example", token)
    client._client = httpx.AsyncClient(
        base_url="https://chat.example/api/v4",
        headers=(
            {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
            if token
            else {"Content-Type": "application/json"}
        ),
        transport=httpx.MockTransport(handler),
    )
    return client


# --- the URL contract -----------------------------------------------------


class TestWebsocketURL:
    """One derivation, shared by the config model, the transport and the
    doctor. Divergence here is invisible until an https instance silently
    gets a plaintext socket."""

    def test_https_becomes_wss(self):
        assert (
            websocket_url("https://chat.example")
            == "wss://chat.example/api/v4/websocket"
        )

    def test_http_becomes_ws(self):
        assert (
            websocket_url("http://localhost:8065")
            == "ws://localhost:8065/api/v4/websocket"
        )

    def test_trailing_slash_does_not_double(self):
        assert (
            websocket_url("https://chat.example/")
            == "wss://chat.example/api/v4/websocket"
        )

    def test_a_subpath_is_preserved(self):
        assert (
            websocket_url("https://example.com/mattermost")
            == "wss://example.com/mattermost/api/v4/websocket"
        )

    def test_the_config_model_agrees(self):
        """``MattermostConfig.websocket_url`` must not re-derive this."""
        from crewlet.config import MattermostConfig

        cfg = MattermostConfig(enabled=True, url="https://chat.example/", team="n")
        assert cfg.websocket_url == websocket_url("https://chat.example")


class TestSiteURLComparison:
    def test_trailing_slashes_are_not_a_difference(self):
        assert site_urls_match("https://chat.example/", "https://chat.example")

    def test_a_different_host_is_a_mismatch(self):
        """The exact case that silently blinds every browser."""
        assert not site_urls_match("http://203.0.113.7:8065", "http://localhost:8065")

    def test_a_different_scheme_is_a_mismatch(self):
        assert not site_urls_match("https://chat.example", "http://chat.example")

    def test_normalisation_is_whitespace_safe(self):
        assert normalize_base_url("  https://chat.example/  ") == "https://chat.example"


def test_typing_throttle_falls_back_when_unreported():
    assert typing_throttle_from({}) == DEFAULT_TYPING_THROTTLE_MS
    assert typing_throttle_from({"TimeBetweenUserTypingUpdatesMilliseconds": "0"}) == (
        DEFAULT_TYPING_THROTTLE_MS
    )
    assert (
        typing_throttle_from({"TimeBetweenUserTypingUpdatesMilliseconds": "2000"})
        == 2000
    )


# --- the client itself ----------------------------------------------------


class TestAuthHeader:
    @pytest.mark.asyncio
    async def test_an_empty_token_sends_no_authorization(self):
        """The health-check reads need no credential, and offering an
        empty bearer turns a working read into a 401."""
        seen: list[httpx.Request] = []

        def handler(request: httpx.Request) -> httpx.Response:
            seen.append(request)
            return httpx.Response(200, json={"status": "OK"})

        client = MattermostClient("https://chat.example", "")
        client._client = httpx.AsyncClient(
            base_url="https://chat.example/api/v4",
            headers={"Content-Type": "application/json"},
            transport=httpx.MockTransport(handler),
        )
        try:
            await client.ping()
        finally:
            await client.close()
        assert "authorization" not in seen[0].headers


class TestErrorSurface:
    @pytest.mark.asyncio
    async def test_the_token_is_redacted_out_of_error_bodies(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(500, text="upstream echoed Bearer tok back")

        client = _client(handler)
        try:
            with pytest.raises(MattermostError) as caught:
                await client.me()
        finally:
            await client.close()
        assert "tok" not in str(caught.value).replace("[REDACTED]", "")


class TestServerClock:
    """The reconnect window compares SERVER-stamped post timestamps
    against "now", so "now" has to come from the server."""

    @pytest.mark.asyncio
    async def test_reads_the_date_header(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(
                200,
                json={"status": "OK"},
                headers={"Date": "Wed, 15 Nov 2023 12:00:00 GMT"},
            )

        client = _client(handler)
        try:
            assert await client.server_time_ms() == 1700049600000
        finally:
            await client.close()

    @pytest.mark.asyncio
    async def test_a_missing_date_header_is_zero_not_a_guess(self):
        """Zero means "unknown" so the caller can fall back to its own
        clock deliberately, rather than inheriting a wrong answer."""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(200, json={"status": "OK"}, headers={})

        client = _client(handler)
        try:
            assert await client.server_time_ms() == 0
        finally:
            await client.close()

    @pytest.mark.asyncio
    async def test_an_unparseable_date_is_zero(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(
                200, json={"status": "OK"}, headers={"Date": "not a date"}
            )

        client = _client(handler)
        try:
            assert await client.server_time_ms() == 0
        finally:
            await client.close()


class TestClientConfig:
    @pytest.mark.asyncio
    async def test_site_url_is_normalised(self):
        def handler(request: httpx.Request) -> httpx.Response:
            assert request.url.params["format"] == "old"
            return httpx.Response(200, json={"SiteURL": "http://localhost:8065/"})

        client = _client(handler)
        try:
            assert await client.site_url() == "http://localhost:8065"
        finally:
            await client.close()

    @pytest.mark.asyncio
    async def test_an_unreadable_config_degrades_to_empty(self):
        """A cosmetic side-channel must never fail a transport start."""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(403, text="nope")

        client = _client(handler)
        try:
            assert await client.client_config() == {}
            assert await client.site_url() == ""
        finally:
            await client.close()
