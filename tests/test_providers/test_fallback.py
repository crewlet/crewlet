"""Tests for ``FallbackLLMProvider``."""

from __future__ import annotations

import pytest

from crewlet.providers.errors import AllCredentialsExhausted, ProviderErrorKind
from crewlet.providers.fallback import FallbackLLMProvider, LLMChainExhausted
from crewlet.providers.llm.protocol import Completion, CompletionChunk, Message


class _ScriptedProvider:
    """Minimal LLMProvider stub with a scripted side-effect."""

    def __init__(self, *, side_effect=None, content: str = "ok") -> None:
        self.calls = 0
        self.stream_calls = 0
        self.side_effect = side_effect
        self.content = content
        self.model = "stub"

    async def complete(self, messages, tools=None, **_):
        self.calls += 1
        if self.side_effect is not None:
            if callable(self.side_effect):
                raise self.side_effect()
            raise self.side_effect
        return Completion(content=self.content)

    async def stream(self, *_args, **_kwargs):
        self.stream_calls += 1
        yield CompletionChunk(content=f"stream:{self.content}")


# --- core chain semantics ---------------------------------------------


@pytest.mark.asyncio
async def test_fallback_returns_first_successful_completion():
    """Primary succeeds -> wrapper returns the primary's completion;
    secondary is never called."""
    primary = _ScriptedProvider(content="from primary")
    secondary = _ScriptedProvider(content="from secondary")
    wrapper = FallbackLLMProvider([("p", primary), ("s", secondary)])

    result = await wrapper.complete([Message(role="user", content="hi")])
    assert result.content == "from primary"
    assert primary.calls == 1
    assert secondary.calls == 0
    assert wrapper.attempted_keys == ["p"]


@pytest.mark.asyncio
async def test_fallback_falls_through_on_all_credentials_exhausted():
    """``AllCredentialsExhausted`` is a retryable signal --
    the wrapper falls through to the next provider in the chain."""
    primary = _ScriptedProvider(
        side_effect=lambda: AllCredentialsExhausted(
            provider_key="p", kind_hint=ProviderErrorKind.RATE_LIMIT
        )
    )
    secondary = _ScriptedProvider(content="recovered")
    wrapper = FallbackLLMProvider([("p", primary), ("s", secondary)])

    result = await wrapper.complete([Message(role="user", content="hi")])
    assert result.content == "recovered"
    assert wrapper.attempted_keys == ["p", "s"]


@pytest.mark.asyncio
async def test_fallback_falls_through_on_classified_retryable_error():
    """OpenAI's ``RateLimitError`` (and friends) classify as
    retryable -- wrapper falls through to the next provider."""
    from unittest.mock import MagicMock

    from openai import RateLimitError

    primary = _ScriptedProvider(
        side_effect=lambda: RateLimitError(
            message="too many", response=MagicMock(), body=None
        )
    )
    secondary = _ScriptedProvider(content="recovered")
    wrapper = FallbackLLMProvider([("p", primary), ("s", secondary)])

    result = await wrapper.complete([Message(role="user", content="hi")])
    assert result.content == "recovered"
    assert wrapper.attempted_keys == ["p", "s"]


@pytest.mark.asyncio
async def test_fallback_does_not_retry_on_fatal_error():
    """A FATAL error (unrecognised exception) propagates immediately
    without trying the next provider. The fallback chain is for
    *recoverable* failures, not for swallowing fatal bugs."""
    primary = _ScriptedProvider(side_effect=lambda: ValueError("bad request"))
    secondary = _ScriptedProvider(content="should-not-be-called")
    wrapper = FallbackLLMProvider([("p", primary), ("s", secondary)])

    with pytest.raises(ValueError):
        await wrapper.complete([Message(role="user", content="hi")])
    assert secondary.calls == 0
    assert wrapper.attempted_keys == ["p"]


@pytest.mark.asyncio
async def test_fallback_raises_chain_exhausted_when_chain_exhausted():
    """When every provider in the chain raises retryable errors, the
    wrapper raises ``LLMChainExhausted`` carrying the chain and the
    last underlying exception (the turn engine catches this to
    publish ``LLMUnavailable``)."""
    primary = _ScriptedProvider(
        side_effect=lambda: AllCredentialsExhausted(
            provider_key="p", kind_hint=ProviderErrorKind.RATE_LIMIT
        )
    )
    secondary = _ScriptedProvider(
        side_effect=lambda: AllCredentialsExhausted(
            provider_key="s", kind_hint=ProviderErrorKind.AUTH
        )
    )
    wrapper = FallbackLLMProvider([("p", primary), ("s", secondary)])

    with pytest.raises(LLMChainExhausted) as exc_info:
        await wrapper.complete([Message(role="user", content="hi")])
    assert exc_info.value.chain == ["p", "s"]
    # Underlying cause is the last entry's AllCredentialsExhausted.
    assert isinstance(exc_info.value.last_exc, AllCredentialsExhausted)
    assert exc_info.value.last_exc.provider_key == "s"
    assert exc_info.value.last_error_kind == "auth"


@pytest.mark.asyncio
async def test_fallback_on_fallback_callback_names_the_provider_taking_over():
    """The hook fires once per hop with ``(from_key, to_key, exception)``.

    ``to_key`` is the chain entry that actually takes over.  Both call
    sites used to pass the literal string ``"next"``, so every
    ``ProviderFallback`` event ever published recorded
    ``to_provider_key="next"`` -- the one field that says which provider
    answered, naming no provider at all.
    """
    calls: list[tuple[str, str, str]] = []

    def _hook(from_key: str, to_key: str, exc: Exception) -> None:
        calls.append((from_key, to_key, type(exc).__name__))

    primary = _ScriptedProvider(
        side_effect=lambda: AllCredentialsExhausted(
            provider_key="p", kind_hint=ProviderErrorKind.RATE_LIMIT
        )
    )
    secondary = _ScriptedProvider(content="recovered")
    wrapper = FallbackLLMProvider([("p", primary), ("s", secondary)], on_fallback=_hook)
    await wrapper.complete([Message(role="user", content="hi")])
    assert calls == [("p", "s", "AllCredentialsExhausted")]


@pytest.mark.asyncio
async def test_fallback_hook_on_the_last_entry_names_no_destination():
    """There is nothing to fall back to, and the event says exactly that.

    Naming a destination that was never tried would be worse than the
    empty string: the chain is exhausted here and ``LLMChainExhausted``
    is what the caller sees.
    """
    calls: list[tuple[str, str]] = []

    def _hook(from_key: str, to_key: str, exc: Exception) -> None:
        calls.append((from_key, to_key))

    def _boom() -> Exception:
        return AllCredentialsExhausted(
            provider_key="x", kind_hint=ProviderErrorKind.RATE_LIMIT
        )

    wrapper = FallbackLLMProvider(
        [
            ("p", _ScriptedProvider(side_effect=_boom)),
            ("s", _ScriptedProvider(side_effect=_boom)),
        ],
        on_fallback=_hook,
    )
    with pytest.raises(LLMChainExhausted):
        await wrapper.complete([Message(role="user", content="hi")])
    assert calls == [("p", "s"), ("s", "")]


@pytest.mark.asyncio
async def test_fallback_empty_chain_rejected():
    """Empty chain is a configuration error; the constructor refuses
    it rather than producing a wrapper that always fails."""
    with pytest.raises(ValueError):
        FallbackLLMProvider([])


@pytest.mark.asyncio
async def test_fallback_stream_delegates_to_head_provider():
    """``stream`` forwards to the head provider rather than crashing --
    every phase provider is a ``FallbackLLMProvider``, so a broken
    ``stream`` would be a latent crash.  It does *not* fall through to
    the next provider in the chain on error (mid-stream fallback has no
    clean unwind), but it must be a working async generator."""
    primary = _ScriptedProvider(content="head")
    secondary = _ScriptedProvider(content="tail")
    wrapper = FallbackLLMProvider([("p", primary), ("s", secondary)])

    chunks = [
        chunk async for chunk in wrapper.stream([Message(role="user", content="x")])
    ]
    assert [c.content for c in chunks] == ["stream:head"]
    assert primary.stream_calls == 1
    assert secondary.stream_calls == 0  # head only -- no chain fallthrough


@pytest.mark.asyncio
async def test_fallback_model_reflects_succeeding_provider():
    """``wrapper.model`` updates to the actual provider that
    answered, so the phase-event ``model`` field carries the right
    information. When the inner provider exposes a ``model``,
    that wins over the chain key."""
    primary = _ScriptedProvider(
        side_effect=lambda: AllCredentialsExhausted(
            provider_key="primary", kind_hint=ProviderErrorKind.RATE_LIMIT
        )
    )
    secondary = _ScriptedProvider(content="ok")
    secondary.model = "claude-haiku-actual"
    wrapper = FallbackLLMProvider([("p", primary), ("s", secondary)])
    await wrapper.complete([Message(role="user", content="hi")])
    assert wrapper.model == "claude-haiku-actual"
