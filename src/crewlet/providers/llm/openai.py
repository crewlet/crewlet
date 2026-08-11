"""OpenAI LLM provider using the official openai SDK."""

from __future__ import annotations

import json
from collections.abc import AsyncIterator
from typing import Any

from openai import AsyncOpenAI

from crewlet._logging import get_logger
from crewlet.providers.credential import (
    CooldownPolicy,
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
from crewlet.providers.llm.protocol import (
    Completion,
    CompletionChunk,
    Message,
    ToolCall,
    ToolDef,
    coerce_usage_int,
)
from crewlet.secrets.resolver import resolve_env

logger = get_logger("llm.openai")


class OpenAIProvider:
    """OpenAI-compatible LLM provider using the official SDK.

    Works with OpenAI API and any compatible endpoint (vLLM, etc.)
    by setting base_url.

    Credential rotation: when ``api_keys`` carries more than one entry,
    the provider walks the pool on rate-limit / auth errors and rotates
    to the next live key within one ``complete`` call.
    ``AllCredentialsExhausted`` is raised only when every key is in
    cooldown -- the fallback wrapper catches that and falls through to
    a different provider in the role's chain.

    SDK-level retries are disabled (``max_retries=0``) so the pool
    sees the rate-limit signal cleanly on the first error; otherwise
    the SDK would burn its own retries against a dead key before the
    pool got a chance to rotate.
    """

    def __init__(
        self,
        model: str = "gpt-4o",
        api_keys: list[str] | None = None,
        cooldowns: dict[str, int] | None = None,
        base_url: str = "https://api.openai.com/v1",
        reasoning: bool = False,
        reasoning_effort: str = "medium",
        timeout: float = 120.0,
    ) -> None:
        self.model = model
        self.base_url = base_url.rstrip("/")
        self.reasoning = reasoning
        self.reasoning_effort = reasoning_effort
        self._timeout = timeout

        # Resolve the working key list. Caller supplies ``api_keys``
        # (one or many); when empty we fall back to ``OPENAI_API_KEY``
        # so a shell-set credential still works without YAML changes.
        merged_keys: list[str] = [k for k in (api_keys or []) if k]
        if not merged_keys:
            env_key = resolve_env("OPENAI_API_KEY")
            if env_key:
                merged_keys.append(env_key)

        # Build the pool entries with stable hash-IDs for logs.
        entries = [CredentialEntry(value=v, hint=short_hash(v)) for v in merged_keys]
        policy = CooldownPolicy(
            rate_limit_seconds=int((cooldowns or {}).get("rate_limit_seconds", 3600)),
            auth_seconds=int((cooldowns or {}).get("auth_seconds", 300)),
        )
        self._pool = CredentialPool(entries=entries, cooldowns=policy)
        # One ``AsyncOpenAI`` client per key.  Building one per call
        # would waste connection pools; caching one per key bounds
        # the lifetime to ``len(api_keys)`` and ``aclose`` cleans up.
        # ``max_retries=0``: the credential pool needs to see rate-limit
        # signals on the first failure, not after the SDK silently
        # burned two retries against the same dead key.
        self._clients: dict[str, AsyncOpenAI] = {}
        for entry in entries:
            self._clients[entry.value] = AsyncOpenAI(
                api_key=entry.value or None,
                base_url=self.base_url,
                timeout=self._timeout,
                max_retries=0,
            )
        # Back-compat surface for callers that read ``self.api_key`` /
        # ``self._client`` directly (test paths). Always points
        # at the head of the pool.
        head = entries[0].value if entries else ""
        self.api_key = head
        if head and head in self._clients:
            self._client = self._clients[head]
        else:
            # No keys configured at all (no api_keys, no env var).
            # Build a keyless client so a misconfigured provider
            # still raises a clean auth error rather than crashing
            # at construction -- and register it in ``_clients`` under
            # the empty-string key so ``aclose`` closes it too (it
            # would otherwise leak its httpx pool).
            self._client = AsyncOpenAI(
                api_key=None,
                base_url=self.base_url,
                timeout=self._timeout,
                max_retries=0,
            )
            self._clients[head] = self._client

    async def aclose(self) -> None:
        """Close every cached ``AsyncOpenAI`` client. Called by
        ``Engine.stop`` to release httpx connection pools.
        """
        for client in self._clients.values():
            try:
                await client.close()
            except Exception:
                logger.exception("openai_client_close_failed")
        self._clients.clear()

    async def _call_with_pool(self, kwargs: dict[str, Any]) -> Any:
        """Walk the credential pool until one key succeeds, or every
        key is in cooldown.

        Rotation invariant: within-provider rotation. When the SDK returns
        a rate-limit / auth error, the offending key is marked cooled-
        down (with the configured TTL for that error class), and the
        next live key is tried for the *same* request. Other error
        classes (server / timeout / fatal) propagate immediately --
        the fallback wrapper handles cross-provider fallback.
        """
        # No rotation when there's at most one key: route through the
        # back-compat ``self._client`` surface so tests that patch
        # ``provider._client`` keep working unchanged. Multi-key pools
        # walk the per-key client cache below.
        if len(self._pool.entries) <= 1:
            return await self._client.chat.completions.create(**kwargs)

        last_kind = ProviderErrorKind.RATE_LIMIT
        attempts = len(self._pool.entries)
        for _ in range(attempts):
            entry = await self._pool.acquire()
            if entry is None:
                break
            client = self._clients.get(entry.value)
            if client is None:
                # Should not happen — every entry was hashed and
                # given a client in __init__ — but defensive.
                logger.warning("openai_pool_missing_client", hint=entry.hint)
                await self._pool.release(entry, succeeded=False)
                continue
            succeeded = False
            try:
                result = await client.chat.completions.create(**kwargs)
                succeeded = True
                return result
            except Exception as exc:
                kind = classify(exc)
                last_kind = kind
                if kind in (ProviderErrorKind.RATE_LIMIT, ProviderErrorKind.AUTH):
                    is_auth = kind == ProviderErrorKind.AUTH
                    default_ttl = (
                        self._pool.cooldowns.auth_seconds
                        if is_auth
                        else self._pool.cooldowns.rate_limit_seconds
                    )
                    # A provider that said "retry in N" via response
                    # headers overrides the static per-class TTL.
                    header_hint = retry_after_seconds(exc)
                    seconds = (
                        int(header_hint) if header_hint is not None else default_ttl
                    )
                    await self._pool.mark_cooled(
                        entry, seconds=seconds, is_auth=is_auth
                    )
                    logger.warning(
                        "openai_credential_cooled",
                        hint=entry.hint,
                        kind=kind.value,
                        cooldown_seconds=seconds,
                        header_hinted=header_hint is not None,
                    )
                    continue
                # Non-credential error: bubble up so the caller's
                # error classifier can decide (provider fallback or
                # FATAL propagation).
                raise
            finally:
                await self._pool.release(entry, succeeded=succeeded)
        raise AllCredentialsExhausted(provider_key="openai", kind_hint=last_kind)

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice: str | None = None,
    ) -> Completion:
        """Call the chat completions API."""
        kwargs: dict = {
            "model": self.model,
            "messages": self._format_messages(messages),
        }

        if self.reasoning:
            kwargs["reasoning_effort"] = self.reasoning_effort
        else:
            kwargs["temperature"] = temperature

        if max_tokens is not None:
            kwargs["max_tokens"] = max_tokens

        if tools:
            kwargs["tools"] = self._format_tools(tools)
            # tool_choice tells the model how to handle tools:
            # "auto" = model decides, "required" = must call a tool,
            # "none" = don't use tools.  Default to "auto" when
            # tools are present so compatible endpoints behave
            # consistently.
            kwargs["tool_choice"] = tool_choice or "auto"

        logger.debug(
            "llm_request",
            model=self.model,
            message_count=len(kwargs["messages"]),
            tool_count=len(kwargs.get("tools", [])),
            reasoning=self.reasoning,
            temperature=kwargs.get("temperature"),
            pool_size=len(self._pool.entries),
        )

        response = await self._call_with_pool(kwargs)

        choice = response.choices[0] if response.choices else None
        if not choice:
            logger.warning("llm_no_choices")
            return Completion(content="", finish_reason="error")

        msg = choice.message
        # OpenAI-compatible providers don't agree on the field name for
        # the model's reasoning trace.  DeepSeek and some MiniMax
        # wrappers use ``reasoning_content``; Nebius's MiniMax-M2.5
        # hosting uses bare ``reasoning``; OpenAI's own o1/o3 family
        # surfaces reasoning via the Responses API instead of chat-
        # completions and so populates neither.  Read both names so
        # the dashboard's ``<think>`` block renders for every vendor
        # we touch; prefer ``reasoning_content`` when both are present
        # since it is the older / more-widespread convention.
        raw_reasoning = getattr(msg, "reasoning_content", None)
        if not isinstance(raw_reasoning, str) or not raw_reasoning:
            raw_reasoning = getattr(msg, "reasoning", None)
        reasoning_content = raw_reasoning if isinstance(raw_reasoning, str) else ""

        tool_calls = []
        if msg.tool_calls:
            for tc in msg.tool_calls:
                try:
                    parsed_args = json.loads(tc.function.arguments)
                except (json.JSONDecodeError, TypeError):
                    parsed_args = {}
                tool_calls.append(
                    ToolCall(
                        id=tc.id,
                        name=tc.function.name,
                        arguments=parsed_args,
                    )
                )

        usage = response.usage
        input_tokens = (usage.prompt_tokens or 0) if usage else 0
        output_tokens = (usage.completion_tokens or 0) if usage else 0
        tokens_used = (
            (usage.total_tokens or 0) if usage else (input_tokens + output_tokens)
        )
        # OpenAI's automatic prompt caching reports the cached portion
        # under ``prompt_tokens_details.cached_tokens`` -- a SUBSET of
        # ``prompt_tokens`` (already counted in ``input_tokens``), not an
        # addition, so we record it for telemetry only and never add it
        # back. There is no separate cache-write count to surface.
        details = getattr(usage, "prompt_tokens_details", None) if usage else None
        cache_read = coerce_usage_int(getattr(details, "cached_tokens", 0))

        logger.info(
            "llm_complete",
            model=self.model,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            total_tokens=tokens_used,
            cache_read_tokens=cache_read,
            tool_call_count=len(tool_calls),
            finish_reason=choice.finish_reason or "stop",
        )
        logger.debug(
            "llm_response_content",
            content=msg.content or "(empty)",
        )
        if tool_calls:
            logger.debug(
                "llm_tool_calls",
                calls=", ".join(f"{tc.name}({tc.arguments})" for tc in tool_calls),
            )

        return Completion(
            content=msg.content or "",
            reasoning_content=reasoning_content,
            tool_calls=tool_calls,
            finish_reason=choice.finish_reason or "stop",
            tokens_used=tokens_used,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            cache_read_input_tokens=cache_read,
        )

    async def stream(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
    ) -> AsyncIterator[CompletionChunk]:
        """Stream chat completions."""
        kwargs: dict = {
            "model": self.model,
            "messages": self._format_messages(messages),
            "stream": True,
        }

        if self.reasoning:
            kwargs["reasoning_effort"] = self.reasoning_effort

        if tools:
            kwargs["tools"] = self._format_tools(tools)

        logger.debug(
            "llm_stream_request",
            model=self.model,
            message_count=len(kwargs["messages"]),
            tool_count=len(kwargs.get("tools", [])),
        )

        # Route through the credential pool: the streaming request is
        # sent eagerly by ``create(stream=True)``, so a rate-limit /
        # auth error surfaces on this call and the pool rotates to a
        # live key just as it does for ``complete``.  (Mid-stream
        # rotation is not attempted -- once the stream is open it runs
        # on the key that opened it.)
        stream = await self._call_with_pool(kwargs)

        tool_call_accum: dict[int, dict[str, str]] = {}

        async for chunk in stream:
            if not chunk.choices:
                continue

            delta = chunk.choices[0].delta
            finish_reason = chunk.choices[0].finish_reason

            if delta.tool_calls:
                for tc_delta in delta.tool_calls:
                    idx = tc_delta.index
                    if idx not in tool_call_accum:
                        tool_call_accum[idx] = {
                            "id": "",
                            "name": "",
                            "arguments": "",
                        }
                    if tc_delta.id:
                        tool_call_accum[idx]["id"] = tc_delta.id
                    if tc_delta.function:
                        if tc_delta.function.name:
                            tool_call_accum[idx]["name"] = tc_delta.function.name
                        if tc_delta.function.arguments:
                            tool_call_accum[idx]["arguments"] += (
                                tc_delta.function.arguments
                            )

            if not finish_reason:
                yield CompletionChunk(
                    content=delta.content or "",
                )
                continue

            final_tool_calls: list[ToolCall] = []
            for tc_data in tool_call_accum.values():
                try:
                    args = (
                        json.loads(tc_data["arguments"]) if tc_data["arguments"] else {}
                    )
                except json.JSONDecodeError:
                    args = {}
                final_tool_calls.append(
                    ToolCall(
                        id=tc_data["id"],
                        name=tc_data["name"],
                        arguments=args,
                    )
                )

            yield CompletionChunk(
                content=delta.content or "",
                tool_calls=final_tool_calls,
                finish_reason=finish_reason,
            )

    def _format_messages(self, messages: list[Message]) -> list[dict[str, Any]]:
        """Convert Message models to OpenAI API format."""
        formatted: list[dict[str, Any]] = []
        for msg in messages:
            entry: dict = {
                "role": msg.role,
                "content": msg.content,
            }
            if msg.tool_calls:
                entry["tool_calls"] = [
                    {
                        "id": tc.id,
                        "type": "function",
                        "function": {
                            "name": tc.name,
                            "arguments": json.dumps(tc.arguments),
                        },
                    }
                    for tc in msg.tool_calls
                ]
            if msg.tool_call_id:
                entry["tool_call_id"] = msg.tool_call_id
            if msg.name:
                entry["name"] = msg.name
            formatted.append(entry)
        return formatted

    def _format_tools(self, tools: list[ToolDef]) -> list[dict[str, Any]]:
        """Convert ToolDef models to OpenAI API format."""
        return [
            {
                "type": "function",
                "function": {
                    "name": t.name,
                    "description": t.description,
                    "parameters": t.parameters,
                },
            }
            for t in tools
        ]

    async def close(self) -> None:
        """Close the underlying HTTP client."""
        await self._client.close()
