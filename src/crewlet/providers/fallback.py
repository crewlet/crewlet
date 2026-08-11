"""``FallbackLLMProvider`` -- multi-provider fallback chain.

Wraps a chain of ``LLMProvider`` instances in a single object that
itself satisfies :class:`~crewlet.providers.llm.protocol.LLMProvider`.
When the primary provider raises a *retryable* error (rate-limit,
server, timeout, auth, or :class:`AllCredentialsExhausted` from the
credential pool),
the wrapper falls through to the next entry in the chain.

The wrapper is opaque to phase runners: ``provider.complete(...)``
still returns one :class:`Completion`. Only the wrapper's own
``model`` / ``attempted_keys`` fields tell the engine which provider
in the chain actually answered, which is what
``AgentTurnCompleted.model_keys`` / per-phase observability needs.

Design choices:

- **Per-phase fallback, not per-round.** On fallback the wrapper
  starts the call from scratch with the next provider. The phase's
  tool-loop state for the failed attempt is dropped. This keeps the
  OTel trace clean -- one ``gen_ai.system`` per phase attempt.
- **Streaming delegates to the head provider, no cross-provider
  fallback.** Mid-stream fallback would require buffering / un-emitting
  partial chunks across providers, which has no clean semantics. But
  every phase provider *is* a ``FallbackLLMProvider``, so ``stream()``
  must still work -- it forwards to the head provider's own ``stream``
  (which keeps its credential-pool rotation). It just doesn't fall
  through to the next provider in the chain.
- **Empty chain is a programmer error.** ``__init__`` raises.
"""

from __future__ import annotations

import inspect
from collections.abc import AsyncIterator, Callable
from typing import Any

from crewlet._logging import get_logger
from crewlet.providers.errors import (
    AllCredentialsExhausted,
    ProviderErrorKind,
    classify,
)
from crewlet.providers.llm.protocol import (
    Completion,
    CompletionChunk,
    LLMProvider,
    Message,
    ToolDef,
)

logger = get_logger("providers.fallback")


# Error classes that should fall through to the next provider in the
# chain. ``FATAL`` (400, content policy, unknown) propagates.
_RETRYABLE_KINDS = frozenset(
    {
        ProviderErrorKind.RATE_LIMIT,
        ProviderErrorKind.AUTH,
        ProviderErrorKind.SERVER,
        ProviderErrorKind.TIMEOUT,
    }
)


class LLMChainExhausted(Exception):
    """Raised by :class:`FallbackLLMProvider` when every provider in
    the chain failed with a retryable error.

    Carries the chain that was attempted and the last underlying
    exception so the turn engine can publish ``LLMUnavailable``
    with full context. The agent is effectively AFK until the
    underlying providers recover.
    """

    def __init__(
        self,
        chain: list[str],
        last_exc: Exception,
        last_error_kind: str = "",
    ) -> None:
        super().__init__(
            f"All {len(chain)} providers exhausted; last error: {last_exc}"
        )
        self.chain = chain
        self.last_exc = last_exc
        self.last_error_kind = last_error_kind


class FallbackLLMProvider:
    """``LLMProvider``-shaped wrapper around a chain of providers."""

    def __init__(
        self,
        chain: list[tuple[str, LLMProvider]],
        *,
        on_fallback: Callable[[str, str, Exception], Any] | None = None,
    ) -> None:
        if not chain:
            raise ValueError("FallbackLLMProvider requires a non-empty chain")
        self._chain = list(chain)
        self._on_fallback = on_fallback
        # The model id (or chain key if the inner provider didn't
        # expose one) the LAST successful call used. Phase runners
        # read ``model`` to populate ``AgentTurnCompleted.model_keys``;
        # we update it after each successful complete.
        self._last_used_model = chain[0][0]
        # Every key attempted during the most recent ``complete``
        # call (in order). Useful for the new
        # ``ProviderFallback`` event + per-phase observability.
        self._attempted_keys: list[str] = []

    @property
    def model(self) -> str:
        """Return the model id that backed the most recent successful
        call -- the inner provider's ``model`` attribute when set,
        otherwise the chain key.

        Mirrors the underlying providers' ``model`` field so the
        ``llm_loop``'s span-attribute / event-payload paths see a
        stable string regardless of which entry in the chain
        succeeded.
        """
        return self._last_used_model

    @property
    def attempted_keys(self) -> list[str]:
        """Keys attempted in the most recent ``complete`` call.

        Always non-empty after a successful complete (the last entry
        is the key that succeeded). When ``complete`` raises, the
        list still carries every key that was tried before the raise.
        """
        return list(self._attempted_keys)

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice: str | None = None,
    ) -> Completion:
        """Walk the chain. The first provider whose ``complete`` does
        not raise a retryable error wins. On exhaustion, the last
        retryable exception is re-raised (so callers can log /
        classify it). A FATAL error short-circuits the chain.
        """
        self._attempted_keys = []
        last_exc: Exception | None = None
        for key, provider in self._chain:
            self._attempted_keys.append(key)
            try:
                completion = await provider.complete(
                    messages,
                    tools=tools,
                    temperature=temperature,
                    max_tokens=max_tokens,
                    tool_choice=tool_choice,
                )
                # Surface the provider's own ``model`` (the model id
                # passed in config) when set so the phase event records
                # the actual model that answered, not just the chain
                # key. Falls back to the chain key for providers that
                # leave ``model`` unset.
                inner_model = getattr(provider, "model", "")
                self._last_used_model = (
                    inner_model if isinstance(inner_model, str) and inner_model else key
                )
                return completion
            except AllCredentialsExhausted as exc:
                # Credential-pool signal that every key in this provider's
                # pool is cooled. Fall through to the next provider in
                # the chain. Logged + on_fallback hook fired.
                last_exc = exc
                logger.warning(
                    "provider_fallback_pool_exhausted",
                    provider_key=key,
                    kind_hint=exc.kind_hint.value,
                )
                await self._fire_hook(key, "next", exc)
                continue
            except Exception as exc:
                kind = classify(exc)
                if kind in _RETRYABLE_KINDS:
                    last_exc = exc
                    logger.warning(
                        "provider_fallback_retryable_error",
                        provider_key=key,
                        kind=kind.value,
                        error_class=type(exc).__name__,
                    )
                    await self._fire_hook(key, "next", exc)
                    continue
                # FATAL or unrecognised -> propagate without further
                # fallback. The caller's existing error path handles
                # logging / event publication.
                raise
        # Chain exhausted. Wrap the last exception in
        # ``LLMChainExhausted`` so the turn engine can publish
        # ``LLMUnavailable`` cleanly.
        if last_exc is None:
            # Should not happen given the non-empty chain guard, but
            # defensive: produce a clear error rather than ``None``.
            raise RuntimeError("FallbackLLMProvider exhausted without an exception")
        if isinstance(last_exc, AllCredentialsExhausted):
            last_kind = last_exc.kind_hint.value
        else:
            try:
                last_kind = classify(last_exc).value
            except Exception:
                last_kind = ""
        raise LLMChainExhausted(
            chain=[k for k, _ in self._chain],
            last_exc=last_exc,
            last_error_kind=last_kind,
        ) from last_exc

    async def _fire_hook(self, from_key: str, label: str, exc: Exception) -> None:
        """Invoke ``on_fallback`` if set. Awaits awaitable returns so
        the caller's event-publish task is observed before we proceed
        to the next provider in the chain. Best-effort: an exception
        in the hook is logged and swallowed."""
        if self._on_fallback is None:
            return
        try:
            result = self._on_fallback(from_key, label, exc)
            if inspect.isawaitable(result):
                await result  # type: ignore[func-returns-value]
        except Exception:
            logger.exception("provider_fallback_hook_failed")

    async def stream(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
    ) -> AsyncIterator[CompletionChunk]:
        """Delegate streaming to the head provider.

        The wrapper does *not* fall through to the next provider in the
        chain on a streaming error -- mid-stream fallback has no clean
        unwind semantics. But every phase provider is wrapped in a
        ``FallbackLLMProvider``, so ``stream`` must still be a working
        async generator rather than a crash; it forwards to the head
        provider's ``stream`` (which keeps that provider's own
        credential-pool rotation). ``complete`` is where cross-provider
        fallback happens.
        """
        head_provider = self._chain[0][1]
        async for chunk in head_provider.stream(messages, tools=tools):
            yield chunk


__all__ = ["FallbackLLMProvider", "LLMChainExhausted"]
