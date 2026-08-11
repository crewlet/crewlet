"""Anthropic LLM provider using the official anthropic SDK."""

from __future__ import annotations

import json
from collections.abc import AsyncIterator
from typing import Any

import anthropic

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

# Anthropic prompt-cache breakpoint. Placed on the system block and on
# the final tool so the (tools + system) prefix -- the large, static
# part of every Plan / Execute / Review round -- is cached and re-read
# at ~10% of the base input price on subsequent rounds and turns,
# instead of being re-billed in full every round. Anthropic silently
# ignores the breakpoint when the prefix is below the cacheable
# minimum, so it is always safe to set.
_CACHE_CONTROL = {"type": "ephemeral"}

logger = get_logger("llm.anthropic")


class AnthropicProvider:
    """Anthropic LLM provider using the official SDK.

    Uses the Messages API.

    Credential rotation: when ``api_keys`` carries more than one entry,
    the provider walks the pool on rate-limit / auth errors, marking
    offending keys as cooled-down, and rotates to the next live key
    within one ``complete`` call. Raises ``AllCredentialsExhausted``
    only when every key is in cooldown; the fallback wrapper catches
    that and falls through to the next provider in the role's chain.

    SDK-level retries are disabled (``max_retries=0``) so the
    credential pool sees the rate-limit signal cleanly.
    """

    def __init__(
        self,
        model: str = "claude-sonnet-4-20250514",
        api_keys: list[str] | None = None,
        cooldowns: dict[str, int] | None = None,
        base_url: str = "https://api.anthropic.com",
        max_tokens: int = 4096,
        reasoning: bool = False,
        reasoning_budget_tokens: int = 10000,
        timeout: float = 120.0,
    ) -> None:
        self.model = model
        self.base_url = base_url.rstrip("/")
        self.reasoning = reasoning
        self._reasoning_budget_tokens = reasoning_budget_tokens
        self._timeout = timeout
        self.default_max_tokens = max_tokens
        if self.reasoning and self.default_max_tokens <= self._reasoning_budget_tokens:
            self.default_max_tokens = self._reasoning_budget_tokens + max_tokens

        # Resolve the working key list. Caller supplies ``api_keys``
        # (one or many); when empty we fall back to
        # ``ANTHROPIC_API_KEY`` so a shell-set credential still works
        # without YAML changes.
        merged_keys: list[str] = [k for k in (api_keys or []) if k]
        if not merged_keys:
            env_key = resolve_env("ANTHROPIC_API_KEY")
            if env_key:
                merged_keys.append(env_key)

        entries = [CredentialEntry(value=v, hint=short_hash(v)) for v in merged_keys]
        policy = CooldownPolicy(
            rate_limit_seconds=int((cooldowns or {}).get("rate_limit_seconds", 3600)),
            auth_seconds=int((cooldowns or {}).get("auth_seconds", 300)),
        )
        self._pool = CredentialPool(entries=entries, cooldowns=policy)
        # One client per key. ``max_retries=0`` so the pool sees
        # rate-limit signals on the first error, not after the SDK
        # silently burned its own retries against a dead key.
        self._clients: dict[str, anthropic.AsyncAnthropic] = {}
        for entry in entries:
            self._clients[entry.value] = anthropic.AsyncAnthropic(
                api_key=entry.value or None,
                base_url=self.base_url,
                timeout=self._timeout,
                max_retries=0,
            )
        head = entries[0].value if entries else ""
        self.api_key = head
        if head and head in self._clients:
            self._client = self._clients[head]
        else:
            # No keys configured at all (no api_keys, no env var).
            # Build a keyless client so a misconfigured provider still
            # raises a clean auth error -- and register it in
            # ``_clients`` under the empty-string key so ``aclose``
            # closes it too (it would otherwise leak its httpx pool).
            self._client = anthropic.AsyncAnthropic(
                api_key=None,
                base_url=self.base_url,
                timeout=self._timeout,
                max_retries=0,
            )
            self._clients[head] = self._client

    async def aclose(self) -> None:
        """Close every cached ``AsyncAnthropic`` client. Called by
        ``Engine.stop`` to release httpx connection pools.
        """
        for client in self._clients.values():
            try:
                await client.close()
            except Exception:
                logger.exception("anthropic_client_close_failed")
        self._clients.clear()

    async def _call_with_pool(self, kwargs: dict[str, Any]) -> Any:
        """Walk the credential pool until one key succeeds or every
        key is in cooldown. Mirror of ``OpenAIProvider._call_with_pool``;
        see that method for the rotation-invariant explanation.
        """
        # No rotation when there's at most one key: route through the
        # back-compat ``self._client`` surface so tests that patch
        # ``provider._client`` keep working unchanged.
        if len(self._pool.entries) <= 1:
            return await self._client.messages.create(**kwargs)
        last_kind = ProviderErrorKind.RATE_LIMIT
        attempts = len(self._pool.entries)
        for _ in range(attempts):
            entry = await self._pool.acquire()
            if entry is None:
                break
            client = self._clients.get(entry.value)
            if client is None:
                logger.warning("anthropic_pool_missing_client", hint=entry.hint)
                await self._pool.release(entry, succeeded=False)
                continue
            succeeded = False
            try:
                result = await client.messages.create(**kwargs)
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
                    header_hint = retry_after_seconds(exc)
                    seconds = (
                        int(header_hint) if header_hint is not None else default_ttl
                    )
                    await self._pool.mark_cooled(
                        entry, seconds=seconds, is_auth=is_auth
                    )
                    logger.warning(
                        "anthropic_credential_cooled",
                        hint=entry.hint,
                        kind=kind.value,
                        cooldown_seconds=seconds,
                        header_hinted=header_hint is not None,
                    )
                    continue
                raise
            finally:
                await self._pool.release(entry, succeeded=succeeded)
        raise AllCredentialsExhausted(provider_key="anthropic", kind_hint=last_kind)

    async def _acquire_stream_client(self) -> tuple[CredentialEntry | None, Any]:
        """Pick a live pooled client for a streaming call.

        Anthropic streaming uses an ``async with client.messages.stream``
        context manager, which does not fit ``_call_with_pool``'s
        ``await client.messages.create`` shape -- so ``stream`` picks
        one live key up front instead of rotating mid-stream. Returns
        ``(entry, client)``; ``entry`` is ``None`` (and ``client`` the
        back-compat ``self._client``) when the pool has at most one key
        or every key is cooled, so streaming still degrades to the
        single-client path rather than failing outright.
        """
        if len(self._pool.entries) <= 1:
            return None, self._client
        entry = await self._pool.acquire()
        if entry is None:
            return None, self._client
        client = self._clients.get(entry.value)
        if client is None:
            await self._pool.release(entry, succeeded=False)
            return None, self._client
        return entry, client

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice: str | None = None,
    ) -> Completion:
        """Call the Anthropic Messages API."""
        system_msg, user_msgs = self._split_system(messages)

        effective_max_tokens = max_tokens or self.default_max_tokens
        if self.reasoning and effective_max_tokens <= self._reasoning_budget_tokens:
            effective_max_tokens = self._reasoning_budget_tokens + effective_max_tokens

        kwargs: dict[str, Any] = {
            "model": self.model,
            "messages": self._format_messages(user_msgs),
            "max_tokens": effective_max_tokens,
        }

        if self.reasoning:
            kwargs["thinking"] = {
                "type": "enabled",
                "budget_tokens": self._reasoning_budget_tokens,
            }
            kwargs["temperature"] = 1
        else:
            kwargs["temperature"] = temperature

        if system_msg:
            kwargs["system"] = self._system_blocks(system_msg)

        if tools:
            kwargs["tools"] = self._format_tools(tools)
            # Anthropic uses {"type": "auto"} / {"type": "any"}
            # instead of plain strings.  Map the OpenAI-style
            # shorthand so the executor can pass the same values.
            choice = tool_choice or "auto"
            choice_map: dict[str, dict[str, str]] = {
                "auto": {"type": "auto"},
                "required": {"type": "any"},
            }
            if choice in choice_map:
                kwargs["tool_choice"] = choice_map[choice]

        logger.debug(
            "llm_request",
            model=self.model,
            message_count=len(kwargs["messages"]),
            tool_count=len(kwargs.get("tools", [])),
            reasoning=self.reasoning,
            temperature=kwargs.get("temperature"),
        )

        response = await self._call_with_pool(kwargs)

        content = ""
        reasoning_content = ""
        thinking_blocks: list[dict[str, str]] = []
        tool_calls = []

        for block in response.content:
            if block.type == "thinking":
                reasoning_content += block.thinking
                thinking_blocks.append(
                    {
                        "type": "thinking",
                        "thinking": block.thinking,
                        "signature": block.signature,
                    }
                )
            elif block.type == "redacted_thinking":
                thinking_blocks.append(
                    {
                        "type": "redacted_thinking",
                        "data": block.data,
                    }
                )
            elif block.type == "text":
                content += block.text
            elif block.type == "tool_use":
                tool_calls.append(
                    ToolCall(
                        id=block.id,
                        name=block.name,
                        arguments=block.input if isinstance(block.input, dict) else {},
                    )
                )

        usage = response.usage
        # Anthropic reports cache reads/writes SEPARATELY from
        # ``input_tokens`` (which counts only the uncached remainder),
        # so sum all three to get the true prompt-token total the
        # budget manager and token_usage ledger consume. The cache
        # components are surfaced on the Completion for cost telemetry.
        cache_read = coerce_usage_int(getattr(usage, "cache_read_input_tokens", 0))
        cache_write = coerce_usage_int(getattr(usage, "cache_creation_input_tokens", 0))
        base_input = coerce_usage_int(getattr(usage, "input_tokens", 0))
        input_tokens = base_input + cache_read + cache_write
        output_tokens = coerce_usage_int(getattr(usage, "output_tokens", 0))
        tokens_used = input_tokens + output_tokens

        logger.info(
            "llm_complete",
            model=self.model,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            cache_read_tokens=cache_read,
            cache_write_tokens=cache_write,
            tool_call_count=len(tool_calls),
            stop_reason=response.stop_reason or "end_turn",
        )
        logger.debug(
            "llm_response_content",
            content=content or "(empty)",
        )
        if tool_calls:
            logger.debug(
                "llm_tool_calls",
                calls=", ".join(f"{tc.name}({tc.arguments})" for tc in tool_calls),
            )

        return Completion(
            content=content,
            reasoning_content=reasoning_content,
            thinking_blocks=thinking_blocks,
            tool_calls=tool_calls,
            finish_reason=response.stop_reason or "end_turn",
            tokens_used=tokens_used,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            cache_read_input_tokens=cache_read,
            cache_creation_input_tokens=cache_write,
        )

    async def stream(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
    ) -> AsyncIterator[CompletionChunk]:
        """Stream from the Anthropic Messages API."""
        system_msg, user_msgs = self._split_system(messages)

        kwargs: dict[str, Any] = {
            "model": self.model,
            "messages": self._format_messages(user_msgs),
            "max_tokens": self.default_max_tokens,
        }

        if self.reasoning:
            kwargs["thinking"] = {
                "type": "enabled",
                "budget_tokens": self._reasoning_budget_tokens,
            }
            kwargs["temperature"] = 1

        if system_msg:
            kwargs["system"] = self._system_blocks(system_msg)

        if tools:
            kwargs["tools"] = self._format_tools(tools)

        logger.debug(
            "llm_stream_request",
            model=self.model,
            message_count=len(kwargs["messages"]),
            tool_count=len(kwargs.get("tools", [])),
        )

        tool_calls: list[ToolCall] = []
        current_tool_json: dict[int, str] = {}
        current_tool_meta: dict[int, dict[str, str]] = {}

        # Pick a live key from the pool rather than always streaming on
        # the declaration-order head -- a cooled head key should not
        # kill every stream.  One-shot pick: streaming does not rotate
        # mid-stream the way ``complete`` does.
        entry, client = await self._acquire_stream_client()
        stream_succeeded = False
        try:
            async with client.messages.stream(**kwargs) as stream:
                stream_succeeded = True
                async for event in stream:
                    if event.type == "content_block_start":
                        idx = event.index
                        block = event.content_block
                        if block.type == "tool_use":
                            current_tool_meta[idx] = {
                                "id": block.id,
                                "name": block.name,
                            }
                            current_tool_json[idx] = ""
                    elif event.type == "content_block_delta":
                        idx = event.index
                        delta = event.delta
                        if delta.type == "text_delta":
                            yield CompletionChunk(content=delta.text)
                        elif delta.type == "input_json_delta":
                            current_tool_json[idx] = (
                                current_tool_json.get(idx, "") + delta.partial_json
                            )
                    elif event.type == "content_block_stop":
                        idx = event.index
                        if idx in current_tool_meta:
                            raw = current_tool_json.get(idx, "")
                            try:
                                args = json.loads(raw) if raw else {}
                            except json.JSONDecodeError:
                                args = {}
                            meta = current_tool_meta.pop(idx)
                            tool_calls.append(
                                ToolCall(
                                    id=meta["id"],
                                    name=meta["name"],
                                    arguments=args,
                                )
                            )
                            current_tool_json.pop(idx, None)
                    elif event.type == "message_delta":
                        stop_reason = event.delta.stop_reason
                        if stop_reason:
                            yield CompletionChunk(
                                tool_calls=tool_calls,
                                finish_reason=stop_reason,
                            )
        finally:
            if entry is not None:
                await self._pool.release(entry, succeeded=stream_succeeded)

    def _split_system(self, messages: list[Message]) -> tuple[str, list[Message]]:
        """Extract system message (Anthropic uses separate system param)."""
        system = ""
        user_msgs = []
        for msg in messages:
            if msg.role == "system":
                system += msg.content + "\n"
            else:
                user_msgs.append(msg)
        return system.strip(), user_msgs

    def _format_messages(self, messages: list[Message]) -> list[dict[str, Any]]:
        """Convert to Anthropic message format."""
        formatted = []
        for msg in messages:
            if msg.role == "tool":
                formatted.append(
                    {
                        "role": "user",
                        "content": [
                            {
                                "type": "tool_result",
                                "tool_use_id": msg.tool_call_id,
                                "content": msg.content,
                            }
                        ],
                    }
                )
            elif msg.tool_calls or msg.thinking_blocks:
                content: list[dict[str, Any]] = []
                content.extend(msg.thinking_blocks)
                if msg.content:
                    content.append({"type": "text", "text": msg.content})
                for tc in msg.tool_calls:
                    content.append(
                        {
                            "type": "tool_use",
                            "id": tc.id,
                            "name": tc.name,
                            "input": tc.arguments,
                        }
                    )
                formatted.append({"role": "assistant", "content": content})
            else:
                formatted.append({"role": msg.role, "content": msg.content})
        return formatted

    @staticmethod
    def _system_blocks(system_msg: str) -> list[dict[str, Any]]:
        """Wrap the system prompt in a single cache-marked text block.

        The breakpoint caches the (tools + system) prefix so the large
        static per-phase instructions are re-read from cache on every
        subsequent round and turn rather than re-billed in full.
        """
        return [{"type": "text", "text": system_msg, "cache_control": _CACHE_CONTROL}]

    def _format_tools(self, tools: list[ToolDef]) -> list[dict[str, Any]]:
        """Convert to Anthropic tool format.

        The final tool carries a prompt-cache breakpoint so the whole
        tool-definition array -- a large, static per-phase payload
        re-sent on every round of the tool loop -- is cached alongside
        the system prompt.
        """
        formatted = [
            {
                "name": t.name,
                "description": t.description,
                "input_schema": t.parameters,
            }
            for t in tools
        ]
        if formatted:
            formatted[-1]["cache_control"] = _CACHE_CONTROL
        return formatted

    async def close(self) -> None:
        await self._client.close()
