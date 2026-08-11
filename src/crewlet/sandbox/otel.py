"""Engine-fronted OTLP receiver for in-sandbox telemetry.

The coding agent inside a sandbox exports its OTel telemetry to a receiver
on the **trusted side** (the engine) — never to the real backend — so the
backend ingest credential never enters the sandbox. The receiver:

- hands each run a **per-run, trace-scoped, short-lived token** embedded in
  the endpoint path (so ``OTEL_EXPORTER_OTLP_HEADERS`` stays empty in the
  sandbox); a leaked token is low-value and expires at run end;
- validates the token on every OTLP POST and **forwards** the payload to
  the real upstream backend, adding the upstream auth *outside* the
  sandbox.

This module owns the token store + the receiver façade; the HTTP route is
``POST /otlp/{token}/v1/{signal}`` in ``api/routes/webhooks.py``.
"""

from __future__ import annotations

import secrets
import time
from collections.abc import Callable
from typing import Any

from crewlet._logging import get_logger

logger = get_logger("sandbox.otel")


class SandboxOtelTokens:
    """Mint + validate per-run, trace-scoped, expiring OTLP tokens.

    In-memory + process-local (the receiver runs in the same engine that
    minted the token). Expiry is lazy: a token past its deadline validates
    as missing. ``now`` is injectable for deterministic tests.
    """

    def __init__(self, *, now: Callable[[], float] = time.monotonic) -> None:
        self._now = now
        # token -> (trace_id, expires_at_monotonic)
        self._tokens: dict[str, tuple[str, float]] = {}

    def mint(self, trace_id: str, *, ttl_seconds: float) -> str:
        token = secrets.token_urlsafe(24)
        self._tokens[token] = (trace_id, self._now() + max(1.0, ttl_seconds))
        return token

    def validate(self, token: str) -> str | None:
        """Return the token's ``trace_id`` if live, else ``None`` (+ reap)."""
        entry = self._tokens.get(token)
        if entry is None:
            return None
        trace_id, expires_at = entry
        if self._now() >= expires_at:
            self._tokens.pop(token, None)
            return None
        return trace_id

    def revoke(self, token: str) -> None:
        self._tokens.pop(token, None)

    def sweep(self) -> int:
        """Drop expired tokens; return how many were reaped."""
        now = self._now()
        dead = [t for t, (_, exp) in self._tokens.items() if now >= exp]
        for t in dead:
            self._tokens.pop(t, None)
        return len(dead)


class SandboxOtelReceiver:
    """Engine-side OTLP receiver: mints endpoints + forwards upstream.

    ``base_url`` is the externally-reachable engine API base the sandbox
    exports to (e.g. ``https://engine.internal``). ``upstream_endpoint`` /
    ``upstream_headers`` are the real OTLP backend (the engine's own
    telemetry destination), applied only here — never handed to the
    sandbox. With no upstream configured the receiver accepts and drops
    (the engine-owned per-turn span still carries the trace).
    """

    def __init__(
        self,
        *,
        base_url: str,
        tokens: SandboxOtelTokens | None = None,
        upstream_endpoint: str = "",
        upstream_headers: dict[str, str] | None = None,
        post: Callable[..., Any] | None = None,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self.tokens = tokens or SandboxOtelTokens()
        self._upstream = upstream_endpoint.rstrip("/")
        self._upstream_headers = dict(upstream_headers or {})
        # Injectable POST for tests; defaults to an httpx call.
        self._post = post

    @property
    def base_url(self) -> str:
        return self._base_url

    def endpoint_for(self, trace_id: str, *, ttl_seconds: float) -> tuple[str, str]:
        """Mint a token and return ``(otlp_endpoint, token)`` for a run.

        The endpoint embeds the token so the sandbox needs no auth header.
        Sweeps expired tokens first, so the store stays bounded to roughly
        the concurrent-run count even when a run dies without its token
        ever being hit again (lazy reaping alone would never collect those).
        """
        self.tokens.sweep()
        token = self.tokens.mint(trace_id, ttl_seconds=ttl_seconds)
        return f"{self._base_url}/otlp/{token}", token

    async def forward(self, signal: str, body: bytes, content_type: str) -> None:
        """Forward a validated OTLP payload to the real backend.

        No-op when no upstream is configured (accept-and-drop). Forwarding
        failures are logged, never raised — telemetry must not 500 the
        sandbox's exporter into retry storms.
        """
        if not self._upstream:
            return
        url = f"{self._upstream}/v1/{signal}"
        headers = {**self._upstream_headers}
        if content_type:
            headers["content-type"] = content_type
        try:
            if self._post is not None:
                await self._post(url, body, headers)
            else:
                import httpx

                async with httpx.AsyncClient(timeout=10.0) as client:
                    await client.post(url, content=body, headers=headers)
        except Exception:
            logger.warning("sandbox_otel_forward_failed", signal=signal)


__all__ = ["SandboxOtelReceiver", "SandboxOtelTokens"]
