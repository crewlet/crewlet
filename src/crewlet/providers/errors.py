"""Provider error classification for credential rotation and
multi-provider fallback.

Maps SDK-raised exceptions (OpenAI / Anthropic) into a small enum the
provider's credential pool and the fallback wrapper can
both branch on. Unknown exceptions are treated as ``FATAL`` -- the
safer default: never silently swallow an error we don't understand.
"""

from __future__ import annotations

from enum import StrEnum

# We import the OpenAI / Anthropic exception classes lazily inside
# ``classify`` so the providers module stays importable even when only
# one SDK is installed.


class ProviderErrorKind(StrEnum):
    """Classification of provider-call failures."""

    RATE_LIMIT = "rate_limit"
    """HTTP 429 or 402 (quota exhausted, billing problem). The
    credential pool puts the key into a ``rate_limit`` cooldown."""

    AUTH = "auth"
    """HTTP 401 or 403. The credential pool puts the key into the
    shorter ``auth`` cooldown -- a temporary token refresh may rescue
    it. Repeated AUTH failures on the same key (no success in
    between) back the cooldown off exponentially, so a permanently-bad
    key stops thrashing the provider; one successful call resets it."""

    TIMEOUT = "timeout"
    """Network or asyncio timeout, or an HTTP 408 / 425. Treated as
    retryable across the provider chain but never marks the
    credential as exhausted -- a timeout is a transport issue, not a
    key issue."""

    SERVER = "server"
    """HTTP 5xx. Same treatment as TIMEOUT -- retryable across the
    provider chain, no credential cooldown."""

    FATAL = "fatal"
    """HTTP 400 / 404, content-policy violations, validation errors
    -- any non-retryable failure. The fallback wrapper does not try
    other providers; the exception propagates."""


def _classify_status(status: int) -> ProviderErrorKind:
    """Map an HTTP status code to a :class:`ProviderErrorKind`.

    Shared by the OpenAI and Anthropic ``APIStatusError`` branches so
    the two stay in lockstep. ``408`` (Request Timeout) and ``425``
    (Too Early) are transport-transient -- classified ``TIMEOUT`` so
    the fallback chain retries them rather than failing the phase.
    """
    if status in (401, 403):
        return ProviderErrorKind.AUTH
    if status in (402, 429):
        return ProviderErrorKind.RATE_LIMIT
    if status in (408, 425):
        return ProviderErrorKind.TIMEOUT
    if 500 <= status < 600:
        return ProviderErrorKind.SERVER
    return ProviderErrorKind.FATAL


def parse_retry_after(raw: str) -> float | None:
    """Parse one ``Retry-After`` header value into a seconds delay.

    Handles both RFC 9110 forms — delta-seconds (``"20"``) and an
    HTTP-date (``"Wed, 21 Oct 2026 07:28:00 GMT"``).  Returns ``None``
    when *raw* is neither.  Shared by every HTTP client that honours
    429 cooldowns (provider SDK errors here, the Slack provisioning
    client) so no caller re-implements the date form.
    """
    try:
        return max(0.0, float(raw))
    except ValueError:
        from email.utils import parsedate_to_datetime

        try:
            target = parsedate_to_datetime(raw)
        except (TypeError, ValueError):
            return None
        from datetime import UTC, datetime

        if target.tzinfo is None:
            target = target.replace(tzinfo=UTC)
        return max(0.0, (target - datetime.now(UTC)).total_seconds())


def retry_after_seconds(exc: Exception) -> float | None:
    """Return the provider-suggested cooldown for ``exc``, if any.

    Reads ``Retry-After`` (delta-seconds or an HTTP-date) and the
    ``x-ratelimit-reset*`` family off the SDK exception's response
    headers. A provider that says "retry in 20s" should not bench the
    key for the full fixed TTL (default an hour); the credential pool
    uses this to override the static cooldown when present.

    Returns ``None`` when the exception carries no usable hint --
    the caller then falls back to its configured per-error-class TTL.
    """
    response = getattr(exc, "response", None)
    headers = getattr(response, "headers", None)
    if not headers:
        return None

    def _get(name: str) -> str | None:
        try:
            value = headers.get(name)
        except Exception:
            return None
        return str(value) if value not in (None, "") else None

    # Retry-After: either an integer delta-seconds or an HTTP-date.
    raw = _get("retry-after")
    if raw is not None:
        parsed = parse_retry_after(raw)
        if parsed is not None:
            return parsed

    # Provider-specific reset headers carry a delta-seconds value.
    for name in ("x-ratelimit-reset-requests", "x-ratelimit-reset-tokens"):
        raw = _get(name)
        if raw is None:
            continue
        # Values look like "20s" / "1.5s" / "20".
        cleaned = raw.rstrip("s").strip()
        try:
            return max(0.0, float(cleaned))
        except ValueError:
            continue
    return None


def classify(exc: Exception) -> ProviderErrorKind:
    """Classify a provider SDK exception into a :class:`ProviderErrorKind`.

    Recognises OpenAI and Anthropic exception shapes. Falls back to
    ``FATAL`` for anything unrecognised -- the safer default. Callers
    that need a different behaviour for unknown errors should branch
    on this explicitly.
    """
    # OpenAI SDK.
    try:
        from openai import (
            APIConnectionError as _OAConnError,
        )
        from openai import (
            APIStatusError as _OAStatusError,
        )
        from openai import (
            APITimeoutError as _OATimeoutError,
        )
        from openai import (
            AuthenticationError as _OAAuthError,
        )
        from openai import (
            RateLimitError as _OARateLimit,
        )

        if isinstance(exc, _OARateLimit):
            return ProviderErrorKind.RATE_LIMIT
        if isinstance(exc, _OAAuthError):
            return ProviderErrorKind.AUTH
        if isinstance(exc, _OATimeoutError | _OAConnError):
            return ProviderErrorKind.TIMEOUT
        if isinstance(exc, _OAStatusError):
            return _classify_status(getattr(exc, "status_code", None) or 0)
    except ImportError:
        pass

    # Anthropic SDK.
    try:
        from anthropic import (
            APIConnectionError as _AnthConnError,
        )
        from anthropic import (
            APIStatusError as _AnthStatusError,
        )
        from anthropic import (
            APITimeoutError as _AnthTimeoutError,
        )
        from anthropic import (
            AuthenticationError as _AnthAuthError,
        )
        from anthropic import (
            RateLimitError as _AnthRateLimit,
        )

        if isinstance(exc, _AnthRateLimit):
            return ProviderErrorKind.RATE_LIMIT
        if isinstance(exc, _AnthAuthError):
            return ProviderErrorKind.AUTH
        if isinstance(exc, _AnthTimeoutError | _AnthConnError):
            return ProviderErrorKind.TIMEOUT
        if isinstance(exc, _AnthStatusError):
            return _classify_status(getattr(exc, "status_code", None) or 0)
    except ImportError:
        pass

    # asyncio.TimeoutError -- network call that didn't return.
    import asyncio

    if isinstance(exc, asyncio.TimeoutError | TimeoutError):
        return ProviderErrorKind.TIMEOUT

    return ProviderErrorKind.FATAL


class AllCredentialsExhausted(Exception):
    """Raised by a provider's credential pool when every key in
    the pool is currently in cooldown. The fallback wrapper
    catches this and falls through to the next provider in the role's
    chain; without a fallback chain it propagates and the phase fails.

    The exception carries ``kind_hint`` (the classification of the
    last error that exhausted a credential) so callers can decide
    whether to retry against a different provider or surface a
    user-facing error.
    """

    def __init__(self, provider_key: str, kind_hint: ProviderErrorKind):
        super().__init__(
            f"all credentials exhausted for provider {provider_key} "
            f"(last error: {kind_hint.value})"
        )
        self.provider_key = provider_key
        self.kind_hint = kind_hint


__all__ = [
    "AllCredentialsExhausted",
    "ProviderErrorKind",
    "classify",
    "retry_after_seconds",
]
