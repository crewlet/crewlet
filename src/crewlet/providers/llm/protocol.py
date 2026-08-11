"""LLM provider protocol and supporting models."""

from __future__ import annotations

from collections.abc import AsyncIterator
from typing import Any, Protocol, runtime_checkable

from pydantic import BaseModel, Field


class Message(BaseModel):
    """A message in a conversation."""

    role: str  # "system", "user", "assistant", "tool"
    content: str = ""
    reasoning_content: str = ""
    thinking_blocks: list[dict[str, str]] = Field(default_factory=list)
    tool_calls: list[ToolCall] = Field(default_factory=list)
    tool_call_id: str = ""
    name: str = ""


class ToolDef(BaseModel):
    """Definition of a tool that an LLM can call."""

    name: str
    description: str
    parameters: dict[str, Any] = Field(default_factory=dict)


class ToolCall(BaseModel):
    """A tool call from the LLM."""

    id: str = ""
    name: str
    arguments: dict[str, Any] = Field(default_factory=dict)


class Completion(BaseModel):
    """A completion response from an LLM.

    ``input_tokens`` is always the *full* prompt token count for the
    call, including any tokens served from or written to a prompt
    cache.  ``cache_read_input_tokens`` / ``cache_creation_input_tokens``
    break that total down for cost observability (cache reads bill at a
    fraction of the base rate; cache writes at a small premium).  Both
    default to 0 for providers / calls without prompt caching, so
    ``input_tokens`` stays a correct budget figure regardless of cache
    state.
    """

    content: str = ""
    reasoning_content: str = ""
    thinking_blocks: list[dict[str, str]] = Field(default_factory=list)
    tool_calls: list[ToolCall] = Field(default_factory=list)
    finish_reason: str = "stop"
    tokens_used: int = 0
    input_tokens: int = 0
    output_tokens: int = 0
    cache_read_input_tokens: int = 0
    cache_creation_input_tokens: int = 0


def coerce_usage_int(value: Any) -> int:
    """Coerce a provider usage-field value to a non-negative int.

    Provider SDK usage objects report token/cache fields as ``int`` or
    ``None`` (``None`` when the feature was not exercised on the call).
    Test doubles built from ``MagicMock`` auto-create attributes that
    are neither.  Returning 0 for anything that isn't a real int keeps
    token arithmetic correct against both the live SDK and mocks.
    """
    return value if isinstance(value, int) and not isinstance(value, bool) else 0


class CompletionChunk(BaseModel):
    """A streaming chunk from an LLM."""

    content: str = ""
    tool_calls: list[ToolCall] = Field(default_factory=list)
    finish_reason: str = ""


@runtime_checkable
class LLMProvider(Protocol):
    """Protocol for LLM providers."""

    # The model id the provider answers as. Telemetry (phase events,
    # OTel spans, the dashboard's per-model token breakdown) reads
    # this directly, so every concrete provider must expose it --
    # ``FallbackLLMProvider`` surfaces the wrapped provider's value
    # via a property so callers don't have to care whether they hold
    # a raw provider or a chain wrapper.
    model: str

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice: str | None = None,
    ) -> Completion: ...

    async def stream(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
    ) -> AsyncIterator[CompletionChunk]: ...
