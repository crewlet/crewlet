"""Tests for Anthropic LLM provider (with mocked anthropic SDK)."""

from unittest.mock import AsyncMock, MagicMock

import pytest

from crewlet.providers.llm.anthropic import AnthropicProvider
from crewlet.providers.llm.protocol import Message, ToolCall, ToolDef


def _make_usage(input_tokens=50, output_tokens=50, cache_read=0, cache_write=0):
    usage = MagicMock()
    usage.input_tokens = input_tokens
    usage.output_tokens = output_tokens
    usage.cache_read_input_tokens = cache_read
    usage.cache_creation_input_tokens = cache_write
    return usage


def _make_text_block(text: str):
    block = MagicMock()
    block.type = "text"
    block.text = text
    return block


def _make_tool_use_block(id: str, name: str, tool_input: dict):
    block = MagicMock()
    block.type = "tool_use"
    block.id = id
    block.name = name
    block.input = tool_input
    return block


def _make_response(
    content_blocks: list | None = None,
    stop_reason: str = "end_turn",
    input_tokens: int = 50,
    output_tokens: int = 50,
    cache_read: int = 0,
    cache_write: int = 0,
):
    response = MagicMock()
    response.content = content_blocks or []
    response.stop_reason = stop_reason
    response.usage = _make_usage(input_tokens, output_tokens, cache_read, cache_write)
    return response


def make_provider(mock_response=None) -> AnthropicProvider:
    provider = AnthropicProvider(model="claude-test", api_keys=["test-key"])
    if mock_response is not None:
        provider._client = MagicMock()
        provider._client.messages = MagicMock()
        provider._client.messages.create = AsyncMock(return_value=mock_response)
        provider._client.close = AsyncMock()
    return provider


def test_timeout_default_and_override():
    """The per-call HTTP timeout defaults to 120s, is configurable, and
    reaches the underlying AsyncAnthropic client."""
    assert AnthropicProvider(api_keys=["k"])._timeout == 120.0
    p = AnthropicProvider(api_keys=["k"], timeout=300.0)
    assert p._timeout == 300.0
    assert p._client.timeout == 300.0


@pytest.mark.asyncio
async def test_simple_completion():
    response = _make_response(
        content_blocks=[_make_text_block("Test response")],
        input_tokens=50,
        output_tokens=50,
    )
    provider = make_provider(response)
    messages = [
        Message(role="system", content="You are helpful"),
        Message(role="user", content="Hello"),
    ]
    result = await provider.complete(messages)
    assert result.content == "Test response"
    assert result.tokens_used == 100
    await provider.close()


@pytest.mark.asyncio
async def test_completion_with_tools():
    tool_block = _make_tool_use_block(
        "tu_1",
        "send_a2a_message",
        {"channel": "general", "content": "Hi"},
    )
    response = _make_response(content_blocks=[tool_block])
    provider = make_provider(response)

    messages = [Message(role="user", content="Say hi")]
    tools = [
        ToolDef(
            name="send_a2a_message",
            description="Send msg",
            parameters={"type": "object"},
        )
    ]
    result = await provider.complete(messages, tools=tools)
    assert len(result.tool_calls) == 1
    assert result.tool_calls[0].name == "send_a2a_message"
    assert result.tool_calls[0].id == "tu_1"
    await provider.close()


@pytest.mark.asyncio
async def test_prompt_cache_breakpoints_on_system_and_tools():
    """System is a cache-marked block and the final tool carries the
    cache breakpoint, so the static (tools + system) prefix is cached
    across rounds/turns instead of re-billed every round."""
    response = _make_response(content_blocks=[_make_text_block("ok")])
    provider = make_provider(response)
    messages = [
        Message(role="system", content="Big static system prompt"),
        Message(role="user", content="Hello"),
    ]
    tools = [
        ToolDef(name="a", description="A", parameters={"type": "object"}),
        ToolDef(name="b", description="B", parameters={"type": "object"}),
    ]
    await provider.complete(messages, tools=tools)

    kwargs = provider._client.messages.create.call_args.kwargs
    # System is a cache-marked block list, not a bare string.
    assert isinstance(kwargs["system"], list)
    assert kwargs["system"][0]["text"] == "Big static system prompt"
    assert kwargs["system"][0]["cache_control"] == {"type": "ephemeral"}
    # Only the FINAL tool carries the breakpoint (prefix cache covers
    # the whole array up to the marked block).
    assert "cache_control" not in kwargs["tools"][0]
    assert kwargs["tools"][-1]["cache_control"] == {"type": "ephemeral"}
    await provider.close()


@pytest.mark.asyncio
async def test_cache_tokens_summed_into_input():
    """Anthropic reports cache reads/writes SEPARATELY from
    input_tokens; the provider sums all three so input_tokens stays a
    correct budget figure, and surfaces the cache breakdown."""
    response = _make_response(
        content_blocks=[_make_text_block("ok")],
        input_tokens=10,  # uncached remainder only
        output_tokens=5,
        cache_read=80,
        cache_write=20,
    )
    provider = make_provider(response)
    result = await provider.complete([Message(role="user", content="Hi")])
    assert result.input_tokens == 110  # 10 + 80 + 20
    assert result.cache_read_input_tokens == 80
    assert result.cache_creation_input_tokens == 20
    assert result.output_tokens == 5
    assert result.tokens_used == 115
    await provider.close()


@pytest.mark.asyncio
async def test_system_message_split():
    provider = make_provider()
    messages = [
        Message(role="system", content="Be helpful"),
        Message(role="user", content="Hello"),
    ]
    system, user_msgs = provider._split_system(messages)
    assert system == "Be helpful"
    assert len(user_msgs) == 1
    assert user_msgs[0].role == "user"
    await provider.close()


@pytest.mark.asyncio
async def test_tool_result_formatting():
    provider = make_provider()
    messages = [
        Message(
            role="tool",
            content="Message sent",
            tool_call_id="tu_1",
            name="send_a2a_message",
        )
    ]
    formatted = provider._format_messages(messages)
    assert formatted[0]["role"] == "user"
    assert formatted[0]["content"][0]["type"] == "tool_result"
    await provider.close()


@pytest.mark.asyncio
async def test_stream_completion():
    """Test streaming produces chunks and final finish_reason."""
    # Build mock stream events
    ev1 = MagicMock()
    ev1.type = "content_block_delta"
    ev1.index = 0
    ev1.delta = MagicMock()
    ev1.delta.type = "text_delta"
    ev1.delta.text = "Hello"

    ev2 = MagicMock()
    ev2.type = "content_block_delta"
    ev2.index = 0
    ev2.delta = MagicMock()
    ev2.delta.type = "text_delta"
    ev2.delta.text = " world"

    ev3 = MagicMock()
    ev3.type = "message_delta"
    ev3.delta = MagicMock()
    ev3.delta.stop_reason = "end_turn"

    class MockStreamContext:
        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            pass

        async def __aiter__(self):
            for e in [ev1, ev2, ev3]:
                yield e

    provider = AnthropicProvider(model="claude-test", api_keys=["test-key"])
    provider._client = MagicMock()
    provider._client.messages = MagicMock()
    provider._client.messages.stream = MagicMock(return_value=MockStreamContext())
    provider._client.close = AsyncMock()

    messages = [Message(role="user", content="Hi")]
    chunks = []
    async for chunk in provider.stream(messages):
        chunks.append(chunk)

    assert len(chunks) == 3
    assert chunks[0].content == "Hello"
    assert chunks[0].finish_reason == ""
    assert chunks[1].content == " world"
    assert chunks[2].finish_reason == "end_turn"
    await provider.close()


@pytest.mark.asyncio
async def test_env_key(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "env-key")
    provider = AnthropicProvider()
    assert provider.api_key == "env-key"
    await provider.close()


def _make_thinking_block(thinking_text: str, signature: str = "sig_abc123"):
    block = MagicMock()
    block.type = "thinking"
    block.thinking = thinking_text
    block.signature = signature
    return block


def _make_redacted_thinking_block(data: str = "redacted_data_xyz"):
    block = MagicMock()
    block.type = "redacted_thinking"
    block.data = data
    return block


@pytest.mark.asyncio
async def test_reasoning_sends_thinking_param():
    """When reasoning=True, thinking param is included and temperature is 1."""
    response = _make_response(
        content_blocks=[
            _make_thinking_block("Let me reason..."),
            _make_text_block("Final answer"),
        ],
        input_tokens=100,
        output_tokens=200,
    )
    provider = AnthropicProvider(
        model="claude-sonnet-4-20250514",
        api_keys=["test-key"],
        reasoning=True,
        reasoning_budget_tokens=8000,
    )
    provider._client = MagicMock()
    provider._client.messages = MagicMock()
    provider._client.messages.create = AsyncMock(return_value=response)
    provider._client.close = AsyncMock()

    messages = [Message(role="user", content="Think about this")]
    result = await provider.complete(messages)

    call_kwargs = provider._client.messages.create.call_args[1]
    assert call_kwargs["thinking"] == {
        "type": "enabled",
        "budget_tokens": 8000,
    }
    assert call_kwargs["temperature"] == 1
    assert result.reasoning_content == "Let me reason..."
    assert result.content == "Final answer"
    assert len(result.thinking_blocks) == 1
    assert result.thinking_blocks[0] == {
        "type": "thinking",
        "thinking": "Let me reason...",
        "signature": "sig_abc123",
    }
    await provider.close()


@pytest.mark.asyncio
async def test_reasoning_disabled_no_thinking_param():
    """When reasoning=False (default), no thinking param is sent."""
    response = _make_response(
        content_blocks=[_make_text_block("Normal response")],
    )
    provider = make_provider(response)

    messages = [Message(role="user", content="Hello")]
    await provider.complete(messages)

    call_kwargs = provider._client.messages.create.call_args[1]
    assert "thinking" not in call_kwargs
    assert call_kwargs["temperature"] == 0.7
    await provider.close()


@pytest.mark.asyncio
async def test_format_messages_includes_thinking_blocks():
    """Assistant messages with thinking_blocks emit signed thinking blocks."""
    provider = make_provider()
    messages = [
        Message(
            role="assistant",
            content="I'll use a tool",
            reasoning_content="Let me think about this...",
            thinking_blocks=[
                {
                    "type": "thinking",
                    "thinking": "Let me think about this...",
                    "signature": "sig_xyz",
                },
            ],
            tool_calls=[
                ToolCall(id="tu_1", name="search", arguments={"q": "test"}),
            ],
        ),
        Message(
            role="tool",
            content="search result",
            tool_call_id="tu_1",
            name="search",
        ),
    ]
    formatted = provider._format_messages(messages)

    assistant_msg = formatted[0]
    assert assistant_msg["role"] == "assistant"
    blocks = assistant_msg["content"]
    assert blocks[0] == {
        "type": "thinking",
        "thinking": "Let me think about this...",
        "signature": "sig_xyz",
    }
    assert blocks[1] == {"type": "text", "text": "I'll use a tool"}
    assert blocks[2]["type"] == "tool_use"
    await provider.close()


@pytest.mark.asyncio
async def test_redacted_thinking_blocks_preserved():
    """Redacted thinking blocks are captured and replayed correctly."""
    response = _make_response(
        content_blocks=[
            _make_thinking_block("Visible thinking", signature="sig_1"),
            _make_redacted_thinking_block("opaque_data"),
            _make_text_block("Answer"),
        ],
        input_tokens=100,
        output_tokens=200,
    )
    provider = AnthropicProvider(
        model="claude-sonnet-4-20250514",
        api_keys=["test-key"],
        reasoning=True,
        reasoning_budget_tokens=8000,
    )
    provider._client = MagicMock()
    provider._client.messages = MagicMock()
    provider._client.messages.create = AsyncMock(return_value=response)
    provider._client.close = AsyncMock()

    messages = [Message(role="user", content="Think")]
    result = await provider.complete(messages)

    assert len(result.thinking_blocks) == 2
    assert result.thinking_blocks[0] == {
        "type": "thinking",
        "thinking": "Visible thinking",
        "signature": "sig_1",
    }
    assert result.thinking_blocks[1] == {
        "type": "redacted_thinking",
        "data": "opaque_data",
    }
    assert result.reasoning_content == "Visible thinking"
    await provider.close()


@pytest.mark.asyncio
async def test_reasoning_auto_adjusts_max_tokens():
    """max_tokens is raised when reasoning budget exceeds default."""
    provider = AnthropicProvider(
        model="claude-sonnet-4-20250514",
        api_keys=["test-key"],
        reasoning=True,
        reasoning_budget_tokens=10000,
        max_tokens=4096,
    )
    assert provider.default_max_tokens == 10000 + 4096  # budget + original max_tokens
    await provider.close()
