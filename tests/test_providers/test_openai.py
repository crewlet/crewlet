"""Tests for OpenAI LLM provider (with mocked openai SDK)."""

import json
from unittest.mock import AsyncMock, MagicMock

import pytest

from crewlet.providers.llm.openai import OpenAIProvider
from crewlet.providers.llm.protocol import Message, ToolDef


def _make_usage(prompt_tokens=50, completion_tokens=50, total_tokens=100):
    usage = MagicMock()
    usage.prompt_tokens = prompt_tokens
    usage.completion_tokens = completion_tokens
    usage.total_tokens = total_tokens
    return usage


def _make_completion_response(
    content: str = "Hello!",
    tool_calls: list | None = None,
    tokens: int = 100,
    finish_reason: str = "stop",
):
    """Build a mock OpenAI ChatCompletion response."""
    message = MagicMock()
    message.content = content
    message.tool_calls = tool_calls

    choice = MagicMock()
    choice.message = message
    choice.finish_reason = finish_reason

    response = MagicMock()
    response.choices = [choice]
    response.usage = _make_usage(
        prompt_tokens=tokens // 2,
        completion_tokens=tokens // 2,
        total_tokens=tokens,
    )
    return response


def _make_tool_call(id: str, name: str, arguments: dict):
    tc = MagicMock()
    tc.id = id
    tc.function = MagicMock()
    tc.function.name = name
    tc.function.arguments = json.dumps(arguments)
    return tc


def make_provider(mock_response=None) -> OpenAIProvider:
    """Create a provider with mocked SDK client."""
    provider = OpenAIProvider(
        model="gpt-4o-test",
        api_keys=["test-key"],
        base_url="https://api.openai.com/v1",
    )
    if mock_response is not None:
        provider._client = MagicMock()
        provider._client.chat = MagicMock()
        provider._client.chat.completions = MagicMock()
        provider._client.chat.completions.create = AsyncMock(return_value=mock_response)
        provider._client.close = AsyncMock()
    return provider


def test_timeout_default_and_override():
    """The per-call HTTP timeout defaults to 120s, is configurable, and
    reaches the underlying AsyncOpenAI client."""
    assert OpenAIProvider(api_keys=["k"])._timeout == 120.0
    p = OpenAIProvider(api_keys=["k"], timeout=300.0)
    assert p._timeout == 300.0
    assert p._client.timeout == 300.0


@pytest.mark.asyncio
async def test_simple_completion():
    response = _make_completion_response("Test response", tokens=100)
    provider = make_provider(response)
    messages = [Message(role="user", content="Hello")]
    result = await provider.complete(messages)
    assert result.content == "Test response"
    assert result.tokens_used == 100
    assert result.finish_reason == "stop"
    await provider.close()


@pytest.mark.asyncio
async def test_cached_tokens_recorded_not_double_counted():
    """OpenAI's cached_tokens is a SUBSET of prompt_tokens (already in
    input_tokens). The provider records it for telemetry but must NOT
    add it back, or input would be over-counted."""
    response = _make_completion_response("ok", tokens=200)
    # prompt_tokens=100 (tokens//2); 60 of those were a cache hit.
    response.usage.prompt_tokens_details = MagicMock()
    response.usage.prompt_tokens_details.cached_tokens = 60
    provider = make_provider(response)
    result = await provider.complete([Message(role="user", content="Hi")])
    assert result.input_tokens == 100  # unchanged: cached is a subset
    assert result.cache_read_input_tokens == 60
    assert result.cache_creation_input_tokens == 0  # OpenAI has no write count
    await provider.close()


@pytest.mark.asyncio
async def test_completion_with_tools():
    tc = _make_tool_call(
        "tc_1",
        "send_a2a_message",
        {"channel": "general", "content": "Hi"},
    )
    response = _make_completion_response(
        "", tool_calls=[tc], finish_reason="tool_calls"
    )
    provider = make_provider(response)

    messages = [Message(role="user", content="Say hi")]
    tools = [
        ToolDef(
            name="send_a2a_message",
            description="Send a message",
            parameters={"type": "object"},
        )
    ]

    result = await provider.complete(messages, tools=tools)
    assert len(result.tool_calls) == 1
    assert result.tool_calls[0].name == "send_a2a_message"
    assert result.tool_calls[0].arguments["channel"] == "general"
    await provider.close()


@pytest.mark.asyncio
async def test_format_messages():
    provider = make_provider()
    messages = [
        Message(role="system", content="You are helpful"),
        Message(role="user", content="Hello"),
    ]
    formatted = provider._format_messages(messages)
    assert len(formatted) == 2
    assert formatted[0]["role"] == "system"
    assert formatted[1]["content"] == "Hello"
    await provider.close()


@pytest.mark.asyncio
async def test_format_tools():
    provider = make_provider()
    tools = [
        ToolDef(
            name="test",
            description="A test tool",
            parameters={"type": "object", "properties": {}},
        )
    ]
    formatted = provider._format_tools(tools)
    assert len(formatted) == 1
    assert formatted[0]["type"] == "function"
    assert formatted[0]["function"]["name"] == "test"
    await provider.close()


@pytest.mark.asyncio
async def test_stream_completion():
    """Test streaming produces chunks and final finish_reason."""
    # Build mock chunk objects
    chunk1 = MagicMock()
    chunk1.choices = [MagicMock()]
    chunk1.choices[0].delta = MagicMock()
    chunk1.choices[0].delta.content = "Hello"
    chunk1.choices[0].delta.tool_calls = None
    chunk1.choices[0].finish_reason = None

    chunk2 = MagicMock()
    chunk2.choices = [MagicMock()]
    chunk2.choices[0].delta = MagicMock()
    chunk2.choices[0].delta.content = " world"
    chunk2.choices[0].delta.tool_calls = None
    chunk2.choices[0].finish_reason = None

    chunk3 = MagicMock()
    chunk3.choices = [MagicMock()]
    chunk3.choices[0].delta = MagicMock()
    chunk3.choices[0].delta.content = ""
    chunk3.choices[0].delta.tool_calls = None
    chunk3.choices[0].finish_reason = "stop"

    async def mock_stream():
        for c in [chunk1, chunk2, chunk3]:
            yield c

    provider = OpenAIProvider(
        model="gpt-4o-test",
        api_keys=["test-key"],
        base_url="https://api.openai.com/v1",
    )
    provider._client = MagicMock()
    provider._client.chat = MagicMock()
    provider._client.chat.completions = MagicMock()
    provider._client.chat.completions.create = AsyncMock(return_value=mock_stream())
    provider._client.close = AsyncMock()

    messages = [Message(role="user", content="Hi")]
    chunks = []
    async for chunk in provider.stream(messages):
        chunks.append(chunk)

    assert len(chunks) == 3
    assert chunks[0].content == "Hello"
    assert chunks[0].finish_reason == ""
    assert chunks[1].content == " world"
    assert chunks[2].finish_reason == "stop"
    await provider.close()


@pytest.mark.asyncio
async def test_provider_uses_env_key(monkeypatch):
    monkeypatch.setenv("OPENAI_API_KEY", "env-key-123")
    provider = OpenAIProvider(model="gpt-4o")
    assert provider.api_key == "env-key-123"
    await provider.close()


@pytest.mark.asyncio
async def test_reasoning_passes_effort_and_skips_temperature():
    """When reasoning=True, reasoning_effort is sent and temperature is omitted."""
    response = _make_completion_response("Thought it through", tokens=200)
    provider = OpenAIProvider(
        model="o3",
        api_keys=["test-key"],
        reasoning=True,
        reasoning_effort="high",
    )
    provider._client = MagicMock()
    provider._client.chat = MagicMock()
    provider._client.chat.completions = MagicMock()
    provider._client.chat.completions.create = AsyncMock(return_value=response)
    provider._client.close = AsyncMock()

    messages = [Message(role="user", content="Think hard")]
    result = await provider.complete(messages)

    call_kwargs = provider._client.chat.completions.create.call_args[1]
    assert call_kwargs["reasoning_effort"] == "high"
    assert "temperature" not in call_kwargs
    assert result.content == "Thought it through"
    await provider.close()


@pytest.mark.asyncio
async def test_reasoning_extracts_reasoning_content():
    """reasoning_content from the response is captured in Completion."""
    response = _make_completion_response("Final answer", tokens=150)
    # Simulate reasoning_content on the message object
    response.choices[0].message.reasoning_content = "Let me think step by step..."

    provider = OpenAIProvider(
        model="o3",
        api_keys=["test-key"],
        reasoning=True,
    )
    provider._client = MagicMock()
    provider._client.chat = MagicMock()
    provider._client.chat.completions = MagicMock()
    provider._client.chat.completions.create = AsyncMock(return_value=response)
    provider._client.close = AsyncMock()

    messages = [Message(role="user", content="Reason about this")]
    result = await provider.complete(messages)

    assert result.reasoning_content == "Let me think step by step..."
    assert result.content == "Final answer"
    await provider.close()


@pytest.mark.asyncio
async def test_reasoning_extracts_bare_reasoning_field_for_nebius_minimax():
    """Nebius's MiniMax-M2.5 hosting returns the model's reasoning trace
    on a top-level ``reasoning`` field rather than ``reasoning_content``
    (the DeepSeek-and-others convention).  Without the dual-field read
    the dashboard's ``<think>`` block renders empty on every MiniMax
    turn even though the model emitted reasoning.  Real Nebius response
    shape, captured 2026-05-24:

        message: {role: "assistant", content: "Hi there!",
                  reasoning: "The user just said \"hi\" ..."}
    """
    response = _make_completion_response("Hi there!", tokens=108)
    # No ``reasoning_content`` field — Nebius uses bare ``reasoning``.
    response.choices[
        0
    ].message.reasoning = 'The user just said "hi" - a simple greeting.'

    provider = OpenAIProvider(
        model="MiniMaxAI/MiniMax-M2.5",
        api_keys=["test-key"],
    )
    provider._client = MagicMock()
    provider._client.chat = MagicMock()
    provider._client.chat.completions = MagicMock()
    provider._client.chat.completions.create = AsyncMock(return_value=response)
    provider._client.close = AsyncMock()

    result = await provider.complete([Message(role="user", content="hi")])

    assert result.reasoning_content == ('The user just said "hi" - a simple greeting.')
    assert result.content == "Hi there!"
    await provider.close()


@pytest.mark.asyncio
async def test_reasoning_content_wins_when_both_fields_present():
    """Defensive: if a future provider populates both fields (e.g. a
    wrapper that mirrors DeepSeek-style ``reasoning_content`` while
    also passing through MiniMax-style ``reasoning``), prefer the
    ``reasoning_content`` value to keep DeepSeek and existing
    behaviour stable."""
    response = _make_completion_response("answer", tokens=20)
    response.choices[0].message.reasoning_content = "preferred"
    response.choices[0].message.reasoning = "fallback (should be ignored)"

    provider = OpenAIProvider(model="any", api_keys=["test-key"])
    provider._client = MagicMock()
    provider._client.chat = MagicMock()
    provider._client.chat.completions = MagicMock()
    provider._client.chat.completions.create = AsyncMock(return_value=response)
    provider._client.close = AsyncMock()

    result = await provider.complete([Message(role="user", content="q")])

    assert result.reasoning_content == "preferred"
    await provider.close()


@pytest.mark.asyncio
async def test_reasoning_disabled_sends_temperature():
    """When reasoning=False (default), temperature is sent normally."""
    response = _make_completion_response("Normal reply", tokens=100)
    provider = make_provider(response)

    messages = [Message(role="user", content="Hello")]
    await provider.complete(messages, temperature=0.5)

    call_kwargs = provider._client.chat.completions.create.call_args[1]
    assert call_kwargs["temperature"] == 0.5
    assert "reasoning_effort" not in call_kwargs
    await provider.close()
