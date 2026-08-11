"""Tests for the per-provider credential pool.

Covers:
- ``classify`` recognises OpenAI / Anthropic exception classes and
  falls through to ``FATAL`` for unknown errors.
- ``CredentialPool`` ``acquire`` / ``mark_cooled`` invariants.
- ``OpenAIProvider`` and ``AnthropicProvider`` multi-key rotation:
  rotates on rate-limit / auth errors, raises
  ``AllCredentialsExhausted`` only when every key is cooled.
- ``short_hash`` is stable, 12 chars, does not leak short keys.
- ``aclose`` closes every cached client.
"""

from __future__ import annotations

import time
from unittest.mock import AsyncMock, MagicMock

import pytest

from crewlet.providers.credential import (
    CredentialEntry,
    CredentialPool,
    short_hash,
)
from crewlet.providers.errors import (
    AllCredentialsExhausted,
    ProviderErrorKind,
    classify,
    retry_after_seconds,
)
from crewlet.providers.llm.openai import OpenAIProvider

# --- classify ----------------------------------------------------------


def test_classify_openai_rate_limit():
    from openai import RateLimitError

    exc = RateLimitError(message="rate limited", response=MagicMock(), body=None)
    assert classify(exc) == ProviderErrorKind.RATE_LIMIT


def test_classify_openai_auth():
    from openai import AuthenticationError

    exc = AuthenticationError(
        message="bad key",
        response=MagicMock(status_code=401),
        body=None,
    )
    assert classify(exc) == ProviderErrorKind.AUTH


def test_classify_unknown_is_fatal():
    """Any unrecognised exception class falls through to FATAL --
    the safer default. Never silently retry an error we don't
    understand."""
    assert classify(ValueError("???")) == ProviderErrorKind.FATAL


def test_classify_asyncio_timeout_is_timeout():
    assert classify(TimeoutError()) == ProviderErrorKind.TIMEOUT
    assert classify(TimeoutError()) == ProviderErrorKind.TIMEOUT


def test_classify_408_is_timeout_not_fatal():
    """HTTP 408 (Request Timeout) is transport-transient -- it must be
    retryable across the fallback chain, not classified FATAL and
    failing the phase outright."""
    from openai import APIStatusError

    exc = APIStatusError(
        message="request timeout",
        response=MagicMock(status_code=408),
        body=None,
    )
    assert classify(exc) == ProviderErrorKind.TIMEOUT


def test_retry_after_seconds_reads_header():
    """A provider that says 'retry in 20s' via Retry-After overrides
    the static per-error-class cooldown TTL."""
    exc = MagicMock()
    exc.response = MagicMock()
    exc.response.headers = {"retry-after": "20"}
    assert retry_after_seconds(exc) == 20.0


def test_retry_after_seconds_none_without_headers():
    """No response / no headers -> None, so the caller falls back to
    its configured per-class TTL."""
    assert retry_after_seconds(ValueError("no response")) is None


# --- short_hash --------------------------------------------------------


def test_short_hash_is_stable_and_12_chars():
    """``short_hash`` produces a deterministic 12-character SHA-256
    prefix. Used for non-secret identification in logs and events.
    """
    h1 = short_hash("api-key-secret")
    h2 = short_hash("api-key-secret")
    assert h1 == h2
    assert len(h1) == 12
    assert all(c in "0123456789abcdef" for c in h1)


def test_short_hash_does_not_leak_short_keys():
    """For short keys (e.g. test fixtures like ``"key_a"``) the hash
    still produces 12 hex chars -- not a suffix substring of the
    key itself."""
    key = "key_a"
    h = short_hash(key)
    # The original was tempted to use "the last 4 chars", which on
    # a 5-char key leaks 80% of the secret. Hash should not contain
    # the original key.
    assert key not in h


def test_short_hash_empty_returns_empty():
    assert short_hash("") == ""


# --- CredentialPool ----------------------------------------------------


async def test_pool_spreads_concurrent_leases_across_keys():
    """A second ``acquire`` while the head is still in-flight goes to
    the next key -- concurrent callers (the common case in Crewlet,
    one shared provider, many agents) spread across the pool instead
    of all hammering the declaration-order head."""
    entries = [
        CredentialEntry(value=f"k{i}", hint=short_hash(f"k{i}")) for i in range(3)
    ]
    pool = CredentialPool(entries=entries)

    e1 = await pool.acquire()
    e2 = await pool.acquire()
    assert e1 is entries[0]
    assert e2 is entries[1]  # head still in-flight -> next key
    assert entries[0].in_flight == 1
    assert entries[1].in_flight == 1


async def test_pool_is_fill_first_once_leases_are_released():
    """With nothing in-flight, ``acquire`` is fill-first: declaration
    order breaks the (all-zero) in-flight tie."""
    entries = [CredentialEntry(value=f"k{i}") for i in range(3)]
    pool = CredentialPool(entries=entries)

    e1 = await pool.acquire()
    await pool.release(e1, succeeded=True)
    e2 = await pool.acquire()
    assert e1 is entries[0]
    assert e2 is entries[0]  # released -> head is the least-loaded again
    assert entries[0].use_count == 2
    assert entries[0].in_flight == 1


async def test_pool_skips_cooled_entries():
    """After ``mark_cooled``, ``acquire`` returns the next live entry."""
    entries = [
        CredentialEntry(value="bad", hint=short_hash("bad")),
        CredentialEntry(value="good", hint=short_hash("good")),
    ]
    pool = CredentialPool(entries=entries)
    await pool.mark_cooled(entries[0], seconds=60)
    assert await pool.acquire() is entries[1]


async def test_pool_returns_none_when_all_cooled():
    """When every entry is in cooldown the pool returns ``None`` so
    the caller can decide what to do (typically raise
    ``AllCredentialsExhausted``)."""
    entries = [CredentialEntry(value="k1"), CredentialEntry(value="k2")]
    pool = CredentialPool(entries=entries)
    await pool.mark_cooled(entries[0], seconds=60)
    await pool.mark_cooled(entries[1], seconds=60)
    assert await pool.acquire() is None


async def test_pool_revives_after_ttl_expires(monkeypatch):
    """Once the cooldown TTL has elapsed, the entry is acquirable
    again. Tests fast-forward by monkeypatching the ``now_seconds``
    indirection (which wraps ``time.monotonic``)."""
    from crewlet.providers import credential as credential_module

    entries = [CredentialEntry(value="k1")]
    pool = CredentialPool(entries=entries)
    await pool.mark_cooled(entries[0], seconds=60)
    assert await pool.acquire() is None
    # Fast-forward past the cooldown.
    real_now = time.monotonic()
    monkeypatch.setattr(credential_module, "now_seconds", lambda: real_now + 120.0)
    assert await pool.acquire() is entries[0]


async def test_repeated_auth_failures_back_off_exponentially():
    """A permanently-bad key must not thrash: each consecutive AUTH
    failure (no success in between) doubles the cooldown.  A success
    via ``release`` resets the counter."""
    from crewlet.providers import credential as credential_module

    base = 100.0
    monkeypatch_now = base
    # Pin ``now`` so cooldown_until is deterministic.
    real_setattr = credential_module.now_seconds
    try:
        credential_module.now_seconds = lambda: monkeypatch_now  # type: ignore[assignment]
        entry = CredentialEntry(value="bad")
        pool = CredentialPool(entries=[entry])
        await pool.mark_cooled(entry, seconds=300, is_auth=True)
        assert entry.cooldown_until == base + 300  # 1st failure: 300 * 2**0
        await pool.mark_cooled(entry, seconds=300, is_auth=True)
        assert entry.cooldown_until == base + 600  # 2nd: 300 * 2**1
        await pool.mark_cooled(entry, seconds=300, is_auth=True)
        assert entry.cooldown_until == base + 1200  # 3rd: 300 * 2**2
        # A success resets the backoff.
        await pool.release(entry, succeeded=True)
        assert entry.consecutive_auth_failures == 0
        await pool.mark_cooled(entry, seconds=300, is_auth=True)
        assert entry.cooldown_until == base + 300  # back to 2**0
    finally:
        credential_module.now_seconds = real_setattr  # type: ignore[assignment]


# --- Multi-key rotation in OpenAIProvider ------------------------------


def _ok_response(text: str = "ok", tokens: int = 10):
    response = MagicMock()
    response.choices = [MagicMock()]
    response.choices[0].message = MagicMock(
        content=text,
        tool_calls=None,
        reasoning_content=None,
    )
    response.choices[0].finish_reason = "stop"
    response.usage = MagicMock(
        prompt_tokens=tokens, completion_tokens=tokens, total_tokens=tokens * 2
    )
    return response


@pytest.mark.asyncio
async def test_openai_provider_with_one_key_uses_back_compat_path():
    """Single-key configs use the direct single-client path -- not
    the pool walker -- so test patches of ``provider._client``
    keep working."""
    provider = OpenAIProvider(model="m", api_keys=["only-key"])
    response = _ok_response()
    provider._client.chat.completions.create = AsyncMock(return_value=response)

    from crewlet.providers.llm.protocol import Message

    result = await provider.complete([Message(role="user", content="hi")])
    assert result.content == "ok"


@pytest.mark.asyncio
async def test_openai_provider_rotates_on_rate_limit():
    """A 3-key pool with the first key rate-limited rotates to the
    second key within ONE ``complete()`` call. The third key is not
    consulted -- ``fill_first`` stops at the first live entry."""
    from openai import RateLimitError

    provider = OpenAIProvider(model="m", api_keys=["k1", "k2", "k3"])

    keys = [e.value for e in provider._pool.entries]
    assert keys == ["k1", "k2", "k3"]

    response = _ok_response("from k2")
    # First client raises rate-limit; second succeeds.
    provider._clients["k1"].chat.completions.create = AsyncMock(
        side_effect=RateLimitError(
            message="too many requests",
            response=MagicMock(),
            body=None,
        )
    )
    provider._clients["k2"].chat.completions.create = AsyncMock(return_value=response)
    provider._clients["k3"].chat.completions.create = AsyncMock(
        return_value=_ok_response("nope")
    )

    from crewlet.providers.llm.protocol import Message

    result = await provider.complete([Message(role="user", content="hi")])
    assert result.content == "from k2"
    # k1 is now in cooldown.
    assert provider._pool.entries[0].cooldown_until > 0
    # k2 was used once; k3 not consulted.
    assert provider._clients["k3"].chat.completions.create.await_count == 0


@pytest.mark.asyncio
async def test_openai_provider_raises_all_credentials_exhausted_when_pool_dead():
    """When every key in a multi-key pool is rate-limited, the
    provider raises ``AllCredentialsExhausted`` with the last error
    classification on the hint."""
    from openai import RateLimitError

    provider = OpenAIProvider(model="m", api_keys=["k1", "k2"])
    err = RateLimitError(
        message="quota",
        response=MagicMock(),
        body=None,
    )
    provider._clients["k1"].chat.completions.create = AsyncMock(side_effect=err)
    provider._clients["k2"].chat.completions.create = AsyncMock(side_effect=err)

    from crewlet.providers.llm.protocol import Message

    with pytest.raises(AllCredentialsExhausted) as exc_info:
        await provider.complete([Message(role="user", content="hi")])
    assert exc_info.value.kind_hint == ProviderErrorKind.RATE_LIMIT
    # Both keys cooled.
    assert all(e.cooldown_until > 0 for e in provider._pool.entries)


@pytest.mark.asyncio
async def test_openai_provider_fatal_errors_propagate_without_rotation():
    """A 400/404 / unknown ValueError must NOT trigger rotation --
    the error propagates directly so the fallback wrapper can
    decide. The credential is not marked cooled."""
    provider = OpenAIProvider(model="m", api_keys=["k1", "k2"])
    provider._clients["k1"].chat.completions.create = AsyncMock(
        side_effect=ValueError("malformed request")
    )

    from crewlet.providers.llm.protocol import Message

    with pytest.raises(ValueError):
        await provider.complete([Message(role="user", content="hi")])
    # Cooldown NOT set on either entry.
    assert provider._pool.entries[0].cooldown_until == 0


@pytest.mark.asyncio
async def test_openai_provider_aclose_closes_every_client():
    """``aclose`` releases every cached client's httpx pool."""
    provider = OpenAIProvider(model="m", api_keys=["k1", "k2"])
    close_calls = {"count": 0}

    async def _fake_close():
        close_calls["count"] += 1

    for client in provider._clients.values():
        client.close = _fake_close

    await provider.aclose()
    assert close_calls["count"] == 2
    assert provider._clients == {}


@pytest.mark.asyncio
async def test_openai_provider_disables_sdk_retries():
    """SDK-level retries must be disabled so the credential pool
    sees rate-limit signals on the first error instead of after
    the SDK silently burned its own retries against a dead key."""
    provider = OpenAIProvider(model="m", api_keys=["k1", "k2"])
    for client in provider._clients.values():
        assert client.max_retries == 0
