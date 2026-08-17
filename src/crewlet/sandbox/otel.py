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

import base64
import hashlib
import hmac
import os
import secrets
import time
from collections.abc import Callable
from typing import Any

from crewlet._logging import get_logger

logger = get_logger("sandbox.otel")


class SandboxOtelTokens:
    """Mint + validate per-run, trace-scoped, expiring OTLP tokens.

    **Stateless.** A token is a signed, self-describing string —
    ``v1.<trace_id>.<expiry>.<hmac>`` — not a key into a dictionary. Any
    process holding the same signing key can verify one, which is what
    the deployment actually requires: the sandbox exports to whatever
    address ``CREWLET_SANDBOX_OTEL_RECEIVER_URL`` names, and in a split
    deployment that is the API process, while the token was minted by the
    engine. The previous in-memory store made those the same process by
    assumption — so the documented split deployment answered 503 to every
    trace, metric and log from every detached coding run, visible only as
    exporter retry noise inside the sandbox.

    Expiry is carried in the token and checked on verify, so nothing has
    to be reaped and a restart does not invalidate live runs' endpoints.
    ``now`` is injectable for deterministic tests.
    """

    _VERSION = "v1"

    def __init__(
        self,
        *,
        signing_key: bytes | str = b"",
        now: Callable[[], float] = time.time,
    ) -> None:
        # Wall-clock by default: the expiry travels between processes, so
        # it cannot be a monotonic reading (whose epoch is per-boot and
        # meaningless anywhere else).
        self._now = now
        self._key = (
            signing_key.encode() if isinstance(signing_key, str) else signing_key
        ) or secrets.token_bytes(32)

    def mint(self, trace_id: str, *, ttl_seconds: float) -> str:
        expires_at = int(self._now() + max(1.0, ttl_seconds))
        payload = f"{self._VERSION}.{trace_id}.{expires_at}"
        return f"{payload}.{self._sign(payload)}"

    def validate(self, token: str) -> str | None:
        """Return the token's ``trace_id`` if the signature is valid and
        the token is unexpired, else ``None``."""
        parts = token.split(".")
        if len(parts) != 4 or parts[0] != self._VERSION:
            return None
        _, trace_id, expiry_raw, signature = parts
        payload = f"{self._VERSION}.{trace_id}.{expiry_raw}"
        if not hmac.compare_digest(signature, self._sign(payload)):
            return None
        try:
            expires_at = int(expiry_raw)
        except ValueError:
            return None
        if self._now() >= expires_at:
            return None
        return trace_id

    def _sign(self, payload: str) -> str:
        digest = hmac.new(self._key, payload.encode(), hashlib.sha256).digest()
        return base64.urlsafe_b64encode(digest).decode().rstrip("=")

    # No ``revoke`` / ``sweep``: there is no store to evict from or reap.
    # Neither had a production caller under the old scheme either (the
    # minted token is discarded at the call site), so nothing is lost —
    # a token's lifetime is its TTL, which is already sized to the run.


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
        signing_key: bytes | str = b"",
        upstream_endpoint: str = "",
        upstream_headers: dict[str, str] | None = None,
        post: Callable[..., Any] | None = None,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        # ``signing_key`` must be the SAME in every process that mints or
        # verifies a token — see :class:`SandboxOtelTokens`. Derived from
        # the Tier A keyring by ``build_sandbox_otel_receiver``.
        self.tokens = tokens or SandboxOtelTokens(signing_key=signing_key)
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
        Nothing is stored: the token carries its own expiry and signature,
        so any process holding the signing key can verify it.
        """
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


__all__ = [
    "SandboxOtelReceiver",
    "SandboxOtelTokens",
    "build_sandbox_otel_receiver",
    "otlp_signing_key",
    "parse_otlp_headers",
]


def parse_otlp_headers(raw: str) -> dict[str, str]:
    """Parse the ``OTEL_EXPORTER_OTLP_HEADERS`` ``k=v,k2=v2`` form into a dict.

    Gives the receiver the *upstream* backend's auth — added on the
    trusted side, never handed to the sandbox.
    """
    headers: dict[str, str] = {}
    for pair in raw.split(","):
        key, sep, value = pair.partition("=")
        if sep and key.strip():
            headers[key.strip()] = value.strip()
    return headers


def otlp_signing_key(bootstrap: Any = None) -> bytes:
    """Derive the OTLP token signing key shared by every process.

    Minting and verifying happen in different processes whenever the API
    runs on its own host, so the key cannot be per-process. It is derived
    from the Tier A keyring — the one secret every Crewlet process already
    loads, and never from the database, which the token check must not
    depend on.

    With no keyring configured the key is random per process. Single-
    process deployments are unaffected (the same process mints and
    verifies); a split deployment gets a loud warning, because the
    alternative — inventing a deterministic key from non-secret material —
    would let anyone who can reach the endpoint forge one.
    """
    keys = getattr(getattr(bootstrap, "secrets", None), "keys", None) or []
    material = "".join(sorted(f"{k.id}:{k.material}" for k in keys))
    if material:
        return hashlib.sha256(f"crewlet.otlp.v1|{material}".encode()).digest()
    logger.warning(
        "sandbox_otel_signing_key_ephemeral",
        hint=(
            "no Tier A `secrets.keys` — OTLP tokens are signed with a "
            "per-process key, so a split deployment cannot verify tokens "
            "the other process minted. Configure a keyring "
            "(`crewlet secrets keygen`) to fix."
        ),
    )
    return secrets.token_bytes(32)


def build_sandbox_otel_receiver(bootstrap: Any = None) -> SandboxOtelReceiver | None:
    """Build the OTLP receiver from the environment, or ``None``.

    The ONE construction path, called by the engine and by the standalone
    API alike. The standalone API used to build no receiver at all, so
    ``/otlp/{token}/v1/{signal}`` answered 503 for every export — while
    the docs pointed ``CREWLET_SANDBOX_OTEL_RECEIVER_URL`` at exactly that
    process, since in a split deployment it is the externally-reachable
    one.
    """
    base = os.environ.get("CREWLET_SANDBOX_OTEL_RECEIVER_URL", "")
    if not base:
        return None
    return SandboxOtelReceiver(
        base_url=base,
        signing_key=otlp_signing_key(bootstrap),
        upstream_endpoint=os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
        upstream_headers=parse_otlp_headers(
            os.environ.get("OTEL_EXPORTER_OTLP_HEADERS", "")
        ),
    )
