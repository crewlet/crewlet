"""Async client for the Slack endpoints app provisioning needs.

Covers exactly the surface ``crewlet slack provision`` uses:

- ``tooling.tokens.rotate`` — exchange a config *refresh* token for a
  fresh 12-hour app-configuration access token (+ the next refresh
  token and its expiry timestamp).
- ``apps.manifest.validate`` / ``create`` / ``update`` — manifest CRUD.
  ``create`` is the only call that ever returns the app's credentials
  (client id/secret + signing secret), so the caller must persist them.
- ``oauth.v2.access`` — exchange the temporary code from the operator's
  authorize click for the ``xoxb-`` bot token.

All methods raise :class:`SlackApiError` on an ``ok: false`` response,
carrying Slack's error code and any per-field manifest messages.
"""

from __future__ import annotations

import asyncio
import json
from dataclasses import dataclass
from typing import Any

import httpx

from crewlet._logging import get_logger
from crewlet.providers.errors import parse_retry_after

logger = get_logger("slack.api")

_SLACK_API = "https://slack.com/api"

# Manifest methods are Slack Tier 1 (~1+ req/min), and a sequential
# multi-agent run issues several back-to-back — so a single retry is not
# enough to ride out a burst.  Instead each call waits out 429s
# (honouring Retry-After, defaulting to Tier 1's ~60 s cadence) up to
# this total wall-clock budget: two full Tier-1 waits fit with headroom,
# and a genuinely stuck endpoint still fails the call in bounded time.
_RATELIMIT_BUDGET_SECONDS = 180.0
_RATELIMIT_DEFAULT_DELAY_SECONDS = 60.0

# Per-request HTTP timeout.  apps.manifest.create/update do server-side
# app provisioning and are markedly slower than plain Web API methods
# (the repo's chat-style clients use httpx defaults or ~10 s); 30 s
# covers manifest latencies with headroom while still failing fast
# enough for an interactive CLI.
_HTTP_TIMEOUT_SECONDS = 30.0


class SlackApiError(RuntimeError):
    """A Slack Web API call answered ``ok: false`` (or a transport error).

    Attributes:
        method: The Slack API method that failed (e.g.
            ``apps.manifest.create``).
        error: Slack's error code (e.g. ``invalid_manifest``), or a
            transport description when the HTTP call itself failed.
        messages: Detailed per-field messages when Slack provides them
            (manifest validation errors).
    """

    def __init__(self, method: str, error: str, messages: list[str] | None = None):
        self.method = method
        self.error = error
        self.messages = messages or []
        detail = f"{method}: {error}"
        if self.messages:
            detail += "\n  - " + "\n  - ".join(self.messages)
        super().__init__(detail)


@dataclass
class ConfigTokenPair:
    """A rotated app-configuration token + its next refresh token."""

    token: str
    refresh_token: str
    expires_at: int = 0
    """Unix timestamp when ``token`` expires (Slack's ``exp``); 0 when
    the rotate response didn't carry one.  Persisted so later runs can
    skip rotation while the access token is still fresh."""


@dataclass
class AppCredentials:
    """What ``apps.manifest.create`` returns that provisioning persists.

    Exactly the four values the ledger keeps — Slack's response also
    carries a deprecated ``verification_token`` and a canned authorize
    URL, both deliberately dropped (the provisioner builds its own
    authorize URL so it can attach the agent handle as ``state``).
    """

    app_id: str
    client_id: str
    client_secret: str
    signing_secret: str


@dataclass
class InstallResult:
    """The workspace install minted by ``oauth.v2.access``."""

    bot_token: str
    app_id: str = ""
    bot_user_id: str = ""
    team_id: str = ""


def _manifest_error_messages(data: dict[str, Any]) -> list[str]:
    """Flatten Slack's ``errors: [{message, pointer}]`` array."""
    messages: list[str] = []
    for item in data.get("errors") or []:
        if not isinstance(item, dict):
            continue
        message = str(item.get("message", ""))
        pointer = str(item.get("pointer", ""))
        messages.append(f"{message} ({pointer})" if pointer else message)
    return messages


class SlackManifestClient:
    """Thin async wrapper over the Slack Web API for provisioning.

    Pass an :class:`httpx.AsyncClient` to share/mock the transport in
    tests; otherwise the client owns one (use as an async context
    manager, or call :meth:`aclose`).
    """

    def __init__(self, http: httpx.AsyncClient | None = None) -> None:
        self._http = http or httpx.AsyncClient(timeout=_HTTP_TIMEOUT_SECONDS)
        self._owns_http = http is None

    async def __aenter__(self) -> SlackManifestClient:
        return self

    async def __aexit__(self, *exc: Any) -> None:
        await self.aclose()

    async def aclose(self) -> None:
        if self._owns_http:
            await self._http.aclose()

    async def _call(
        self,
        method: str,
        *,
        token: str = "",
        data: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """POST a Web API method (form-encoded, Slack's canonical shape).

        429 responses are waited out (Retry-After honoured, both the
        delta-seconds and HTTP-date forms) up to
        :data:`_RATELIMIT_BUDGET_SECONDS` of total wall-clock per call.
        """
        headers = {"Authorization": f"Bearer {token}"} if token else {}
        loop = asyncio.get_running_loop()
        deadline = loop.time() + _RATELIMIT_BUDGET_SECONDS
        while True:
            try:
                resp = await self._http.post(
                    f"{_SLACK_API}/{method}", data=data or {}, headers=headers
                )
            except httpx.HTTPError as exc:
                raise SlackApiError(method, f"transport error: {exc}") from exc
            if resp.status_code != 429:
                break
            raw = resp.headers.get("retry-after", "")
            delay = parse_retry_after(raw) if raw else None
            if delay is None:
                delay = _RATELIMIT_DEFAULT_DELAY_SECONDS
            if loop.time() + delay > deadline:
                raise SlackApiError(
                    method,
                    "ratelimited (retry budget "
                    f"{_RATELIMIT_BUDGET_SECONDS:.0f}s exhausted) — re-run "
                    "to resume; completed work is persisted",
                )
            logger.warning("slack_ratelimited", method=method, retry_after=delay)
            await asyncio.sleep(delay)
        try:
            payload: dict[str, Any] = resp.json()
        except ValueError as exc:
            raise SlackApiError(
                method, f"non-JSON response (HTTP {resp.status_code})"
            ) from exc
        if not payload.get("ok"):
            raise SlackApiError(
                method,
                str(payload.get("error", f"HTTP {resp.status_code}")),
                _manifest_error_messages(payload),
            )
        return payload

    # ── Config tokens ────────────────────────────────────────────────

    async def rotate_config_token(self, refresh_token: str) -> ConfigTokenPair:
        """Exchange a config refresh token for a fresh access + refresh pair.

        Rotation invalidates the supplied refresh token — persist the
        returned pair immediately.
        """
        data = await self._call(
            "tooling.tokens.rotate", data={"refresh_token": refresh_token}
        )
        try:
            expires_at = int(data.get("exp", 0) or 0)
        except (TypeError, ValueError):
            expires_at = 0
        pair = ConfigTokenPair(
            token=str(data.get("token", "")),
            refresh_token=str(data.get("refresh_token", "")),
            expires_at=expires_at,
        )
        logger.info("slack_config_token_rotated", expires_at=pair.expires_at)
        return pair

    # ── Manifest CRUD ────────────────────────────────────────────────

    async def validate_manifest(
        self, config_token: str, manifest: dict[str, Any], app_id: str = ""
    ) -> None:
        """Raise :class:`SlackApiError` if Slack rejects the manifest."""
        payload: dict[str, Any] = {"manifest": json.dumps(manifest)}
        if app_id:
            payload["app_id"] = app_id
        await self._call("apps.manifest.validate", token=config_token, data=payload)

    async def create_app(
        self, config_token: str, manifest: dict[str, Any]
    ) -> AppCredentials:
        """Create a new app from *manifest*; returns its credentials.

        This is the ONLY time Slack hands out the client secret and
        signing secret — the caller must persist them.
        """
        data = await self._call(
            "apps.manifest.create",
            token=config_token,
            data={"manifest": json.dumps(manifest)},
        )
        creds = data.get("credentials") or {}
        result = AppCredentials(
            app_id=str(data.get("app_id", "")),
            client_id=str(creds.get("client_id", "")),
            client_secret=str(creds.get("client_secret", "")),
            signing_secret=str(creds.get("signing_secret", "")),
        )
        logger.info("slack_app_created", app_id=result.app_id)
        return result

    async def update_app(
        self, config_token: str, app_id: str, manifest: dict[str, Any]
    ) -> None:
        """Push *manifest* onto the existing app *app_id*."""
        await self._call(
            "apps.manifest.update",
            token=config_token,
            data={"app_id": app_id, "manifest": json.dumps(manifest)},
        )
        logger.info("slack_app_updated", app_id=app_id)

    # ── OAuth install ────────────────────────────────────────────────

    async def exchange_oauth_code(
        self,
        *,
        code: str,
        client_id: str,
        client_secret: str,
        redirect_url: str,
    ) -> InstallResult:
        """Exchange the authorize-click code for the ``xoxb-`` bot token."""
        data = await self._call(
            "oauth.v2.access",
            data={
                "code": code,
                "client_id": client_id,
                "client_secret": client_secret,
                "redirect_uri": redirect_url,
            },
        )
        team = data.get("team") or {}
        result = InstallResult(
            bot_token=str(data.get("access_token", "")),
            app_id=str(data.get("app_id", "")),
            bot_user_id=str(data.get("bot_user_id", "")),
            team_id=str(team.get("id", "")),
        )
        logger.info(
            "slack_app_installed",
            app_id=result.app_id,
            team_id=result.team_id,
            bot_user_id=result.bot_user_id,
        )
        return result
