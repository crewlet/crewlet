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
    """A client whose transport is a scripted handler.

    Injected through the real constructor so tests drive the real
    ``_request`` path — retries, status classification, redirect
    detection — rather than a stand-in that would assert nothing about
    it.
    """
    return MattermostClient(
        "https://chat.example", token, transport=httpx.MockTransport(handler)
    )


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

        client = _client(handler, token="")
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


# --- failure funnelling ---------------------------------------------------


class TestTransportErrors:
    """Every caller catches ``MattermostError`` and nothing else, so a raw
    httpx exception does not degrade one call — it aborts the sequence the
    call sits in. One connect blip while the transport resolved bot
    identities at boot skipped opening the websocket fleet for the life of
    the process."""

    @pytest.mark.asyncio
    async def test_a_connect_failure_is_a_mattermost_error(self, monkeypatch):
        async def _sleep(delay: float) -> None:
            return None

        monkeypatch.setattr("crewlet.mattermost.client.asyncio.sleep", _sleep)

        def handler(request: httpx.Request) -> httpx.Response:
            raise httpx.ConnectError("all connection attempts failed")

        client = _client(handler)
        try:
            with pytest.raises(MattermostError) as caught:
                await client.me()
        finally:
            await client.close()
        assert caught.value.status == 0
        assert "transport error" in str(caught.value)

    @pytest.mark.asyncio
    async def test_status_zero_does_not_trip_the_not_found_guards(self, monkeypatch):
        """``exc.status in (403, 404)`` guards must not read "unreachable"
        as "absent" and report a missing channel."""

        async def _sleep(delay: float) -> None:
            return None

        monkeypatch.setattr("crewlet.mattermost.client.asyncio.sleep", _sleep)

        def handler(request: httpx.Request) -> httpx.Response:
            raise httpx.ConnectError("boom")

        client = _client(handler)
        try:
            with pytest.raises(MattermostError):
                await client.get_channel("c1")
        finally:
            await client.close()


class TestRetries:
    @pytest.mark.asyncio
    async def test_429_is_waited_out_and_retried(self, monkeypatch):
        slept: list[float] = []

        async def _sleep(delay: float) -> None:
            slept.append(delay)

        monkeypatch.setattr("crewlet.mattermost.client.asyncio.sleep", _sleep)
        calls = {"n": 0}

        def handler(request: httpx.Request) -> httpx.Response:
            calls["n"] += 1
            if calls["n"] == 1:
                return httpx.Response(429, headers={"Retry-After": "2"}, text="slow")
            return httpx.Response(200, json={"id": "u1"})

        client = _client(handler)
        try:
            assert await client.me() == {"id": "u1"}
        finally:
            await client.close()
        assert slept == [2.0], "the server's own Retry-After must win"

    @pytest.mark.asyncio
    async def test_503_is_retried(self, monkeypatch):
        """The docker-compose case: the container is up and still
        migrating."""

        async def _sleep(delay: float) -> None:
            return None

        monkeypatch.setattr("crewlet.mattermost.client.asyncio.sleep", _sleep)
        calls = {"n": 0}

        def handler(request: httpx.Request) -> httpx.Response:
            calls["n"] += 1
            if calls["n"] < 3:
                return httpx.Response(503, text="starting up")
            return httpx.Response(200, json={"status": "OK"})

        client = _client(handler)
        try:
            assert await client.ping() == {"status": "OK"}
        finally:
            await client.close()
        assert calls["n"] == 3

    @pytest.mark.asyncio
    async def test_a_4xx_is_not_retried(self):
        calls = {"n": 0}

        def handler(request: httpx.Request) -> httpx.Response:
            calls["n"] += 1
            return httpx.Response(401, text="Invalid session")

        client = _client(handler)
        try:
            with pytest.raises(MattermostError):
                await client.me()
        finally:
            await client.close()
        assert calls["n"] == 1

    @pytest.mark.asyncio
    async def test_the_retry_budget_is_bounded(self, monkeypatch):
        """A reconnect backfill walks every channel serially — it must not
        stall the seat behind a server that is simply down."""

        async def _sleep(delay: float) -> None:
            return None

        monkeypatch.setattr("crewlet.mattermost.client.asyncio.sleep", _sleep)
        calls = {"n": 0}

        def handler(request: httpx.Request) -> httpx.Response:
            calls["n"] += 1
            return httpx.Response(503, text="down")

        client = _client(handler)
        try:
            with pytest.raises(MattermostError):
                await client.ping()
        finally:
            await client.close()
        assert calls["n"] < 10


class TestRedirects:
    @pytest.mark.asyncio
    async def test_a_redirect_names_the_location_and_the_fix(self):
        """Following it would send the bearer token to whatever host the
        redirect names; the same misconfiguration also hands the websocket
        a plaintext ws:// URL, and nothing else would name the cause."""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(301, headers={"Location": "https://chat.example/"})

        client = _client(handler)
        try:
            with pytest.raises(MattermostError) as caught:
                await client.me()
        finally:
            await client.close()
        assert "https://chat.example/" in str(caught.value)
        assert "integrations.mattermost.url" in str(caught.value)


class TestClosedClient:
    @pytest.mark.asyncio
    async def test_a_closed_client_refuses_rather_than_rebuilds(self):
        """Rebuilding would deliver a message through a transport the
        engine believes it tore down, from a pool nothing will close."""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(200, json={"id": "p1"})

        client = _client(handler)
        await client.close()
        with pytest.raises(MattermostError) as caught:
            await client.create_post("c1", "hi")
        assert "closed" in str(caught.value)


# --- endpoint correctness -------------------------------------------------


class TestPostsSince:
    """``since=`` is UpdateAt-based, and UpdateAt is bumped by things that
    are not content — a reaction touches the post, deleting a reply
    touches the thread root. Replaying those wakes an agent to re-answer
    a message it already answered."""

    @staticmethod
    def _payload(posts: dict[str, dict]) -> dict:
        return {"order": list(reversed(list(posts))), "posts": posts}

    @pytest.mark.asyncio
    async def test_only_new_or_edited_posts_are_replayed(self):
        since = 1000

        def handler(request: httpx.Request) -> httpx.Response:
            assert request.url.params["since"] == str(since)
            return httpx.Response(
                200,
                json=self._payload(
                    {
                        "old": {"id": "old", "create_at": 500, "update_at": 1500},
                        "new": {"id": "new", "create_at": 1200, "update_at": 1200},
                        "edited": {
                            "id": "edited",
                            "create_at": 500,
                            "edit_at": 1300,
                            "update_at": 1300,
                        },
                        "gone": {
                            "id": "gone",
                            "create_at": 1100,
                            "delete_at": 1400,
                            "update_at": 1400,
                        },
                    }
                ),
            )

        client = _client(handler)
        try:
            posts = await client.posts_since("c1", since)
        finally:
            await client.close()
        # Ordering is asserted by the next test; what matters here is
        # WHICH posts survive the filter.
        assert sorted(p["id"] for p in posts) == ["edited", "new"]

    @pytest.mark.asyncio
    async def test_results_are_oldest_first(self):
        """The fleet replays in order, so a newest-first list would show a
        conversation backwards."""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(
                200,
                json={
                    "order": ["b", "a"],
                    "posts": {
                        "a": {"id": "a", "create_at": 1100},
                        "b": {"id": "b", "create_at": 1200},
                    },
                },
            )

        client = _client(handler)
        try:
            posts = await client.posts_since("c1", 1000)
        finally:
            await client.close()
        assert [p["id"] for p in posts] == ["a", "b"]


class TestServerLimits:
    @pytest.mark.asyncio
    async def test_it_calls_the_route_that_exists(self):
        """``/limits/users`` has never existed; the real route is
        ``/limits/server``, and the 404 was swallowed — so the
        provisioner's headroom note never printed on any server."""
        seen: list[str] = []

        def handler(request: httpx.Request) -> httpx.Response:
            seen.append(request.url.path)
            return httpx.Response(
                200, json={"maxUsersLimit": 250, "activeUserCount": 12}
            )

        client = _client(handler)
        try:
            limits = await client.server_limits()
        finally:
            await client.close()
        assert seen == ["/api/v4/limits/server"]
        assert limits["activeUserCount"] == 12

    @pytest.mark.asyncio
    async def test_an_old_server_reports_nothing_rather_than_failing(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(404, text="not found")

        client = _client(handler)
        try:
            assert await client.server_limits() == {}
        finally:
            await client.close()

    @pytest.mark.asyncio
    async def test_a_bad_credential_is_not_swallowed(self):
        """A 401 means the operator token is wrong — a provisioner that
        shrugged at that would go on to make writes it cannot make."""

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(401, text="Invalid session")

        client = _client(handler)
        try:
            with pytest.raises(MattermostError):
                await client.server_limits()
        finally:
            await client.close()


class TestCreatePost:
    @pytest.mark.asyncio
    async def test_it_carries_an_idempotency_key(self):
        """Mattermost deduplicates a create by ``pending_post_id``; without
        one, a retried send double-posts into a human's thread."""
        bodies: list[dict] = []

        def handler(request: httpx.Request) -> httpx.Response:
            import json as _json

            bodies.append(_json.loads(request.content))
            return httpx.Response(201, json={"id": "p1"})

        client = _client(handler)
        try:
            await client.create_post("c1", "hello", root_id="r1")
        finally:
            await client.close()
        assert bodies[0]["pending_post_id"]
        assert bodies[0]["root_id"] == "r1"

    @pytest.mark.asyncio
    async def test_a_retry_reuses_the_same_key(self, monkeypatch):
        """The key only prevents a double post if it survives the retry."""

        async def _sleep(delay: float) -> None:
            return None

        monkeypatch.setattr("crewlet.mattermost.client.asyncio.sleep", _sleep)
        bodies: list[dict] = []

        def handler(request: httpx.Request) -> httpx.Response:
            import json as _json

            bodies.append(_json.loads(request.content))
            if len(bodies) == 1:
                return httpx.Response(503, text="restarting")
            return httpx.Response(201, json={"id": "p1"})

        client = _client(handler)
        try:
            await client.create_post("c1", "hello")
        finally:
            await client.close()
        assert len(bodies) == 2
        assert bodies[0]["pending_post_id"] == bodies[1]["pending_post_id"]
