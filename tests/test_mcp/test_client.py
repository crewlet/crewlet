"""Tests for MCP stdio client — SDK-based implementation.

The SDK seam is the high-level ``mcp.Client`` (entered as an async
context manager) plus the ``stdio_client`` transport factory; both are
patched at the ``crewlet.mcp.client`` module level.
"""

import logging
from contextlib import asynccontextmanager
from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from mcp.types import ToolAnnotations

from crewlet.mcp._content import _MAX_TOOL_PAGES
from crewlet.mcp.client import MCPClient

from .conftest import (
    mock_call_result,
    mock_embedded_resource,
    mock_image_content,
    mock_list_result,
    mock_other_content,
    mock_resource_link,
    mock_sdk_client,
    mock_text_content,
    mock_tool,
)


def _client_patches(sdk_client):
    """Patchers wiring MCPClient.start() to *sdk_client*."""

    @asynccontextmanager
    async def fake_client_cm(transport, **kwargs):
        yield sdk_client

    return (
        patch(
            "crewlet.mcp.client.stdio_client",
            side_effect=lambda p, **kw: MagicMock(),
        ),
        patch("crewlet.mcp.client.Client", side_effect=fake_client_cm),
    )


async def _start_client(
    sdk_client=None,
    **kwargs,
) -> MCPClient:
    """Start a client with mocked SDK components."""
    sdk_client = sdk_client if sdk_client is not None else mock_sdk_client()

    stdio_patch, client_patch = _client_patches(sdk_client)
    with stdio_patch, client_patch:
        client = MCPClient(
            name=kwargs.get("name", "test"),
            command=kwargs.get("command", "echo"),
            args=kwargs.get("args", []),
            env=kwargs.get("env", {}),
        )
        await client.start()
        return client


# --- Initialization ---


@pytest.mark.asyncio
async def test_init_defaults():
    client = MCPClient(name="test", command="echo")
    assert client.name == "test"
    assert client.command == "echo"
    assert client.args == []
    assert client.env == {}
    assert not client.is_running
    assert not client._initialized


@pytest.mark.asyncio
async def test_start_initializes_server():
    client = await _start_client()
    assert client._initialized
    assert client.is_running
    await client.stop()


@pytest.mark.asyncio
async def test_start_reads_server_info():
    """The connected SDK client is held and its server identity consumed."""
    sdk_client = mock_sdk_client("my-server")
    client = await _start_client(sdk_client=sdk_client)
    assert client._client is sdk_client
    await client.stop()


@pytest.mark.asyncio
async def test_start_anonymous_server():
    """A modern server may not identify itself (server_info None) — the
    connection is still valid."""
    client = await _start_client(sdk_client=mock_sdk_client(server_name=None))
    assert client.is_running
    await client.stop()


@pytest.mark.asyncio
async def test_start_failure_cleans_up():
    """If the SDK handshake fails, the exit stack is cleaned up."""

    @asynccontextmanager
    async def failing_client_cm(transport, **kwargs):
        raise RuntimeError("connect failed")
        yield  # pragma: no cover

    with (
        patch(
            "crewlet.mcp.client.stdio_client",
            side_effect=lambda p, **kw: MagicMock(),
        ),
        patch(
            "crewlet.mcp.client.Client",
            side_effect=failing_client_cm,
        ),
    ):
        client = MCPClient(name="test", command="echo")
        with pytest.raises(RuntimeError, match="connect failed"):
            await client.start()

    assert client._exit_stack is None
    assert client._client is None
    assert not client.is_running


@pytest.mark.asyncio
async def test_start_failure_surfaces_stderr_tail(caplog):
    """A failed start logs the server's captured last words at error —
    that's usually the actual cause (bad token, import error)."""

    class FakeRelay:
        errlog = MagicMock()

        async def __aenter__(self):
            return self

        async def __aexit__(self, *exc):
            return None

        def tail(self):
            return ["Traceback (most recent call last):", "KeyError: 'TOKEN'"]

    @asynccontextmanager
    async def failing_client_cm(transport, **kwargs):
        raise RuntimeError("connect failed")
        yield  # pragma: no cover

    with (
        patch("crewlet.mcp.client.ServerStderrRelay", return_value=FakeRelay()),
        patch(
            "crewlet.mcp.client.stdio_client",
            side_effect=lambda p, **kw: MagicMock(),
        ),
        patch("crewlet.mcp.client.Client", side_effect=failing_client_cm),
    ):
        client = MCPClient(name="test", command="echo")
        with (
            caplog.at_level(logging.ERROR, logger="crewlet.mcp.client"),
            pytest.raises(RuntimeError, match="connect failed"),
        ):
            await client.start()

    assert "server_stderr_tail" in caplog.text
    assert "KeyError" in caplog.text


@pytest.mark.asyncio
async def test_start_identifies_as_crewlet():
    """The handshake carries crewlet's client identity, not the SDK
    default."""
    captured_kwargs = {}
    sdk_client = mock_sdk_client()

    @asynccontextmanager
    async def spy_client_cm(transport, **kwargs):
        captured_kwargs.update(kwargs)
        yield sdk_client

    with (
        patch(
            "crewlet.mcp.client.stdio_client",
            side_effect=lambda p, **kw: MagicMock(),
        ),
        patch("crewlet.mcp.client.Client", side_effect=spy_client_cm),
    ):
        client = MCPClient(name="test", command="echo")
        await client.start()

    assert captured_kwargs["client_info"].name == "crewlet"

    await client.stop()


@pytest.mark.asyncio
async def test_start_passes_correct_params():
    """Verify command, args, merged env, and the stderr relay reach
    stdio_client."""
    captured_params = []
    captured_stdio_kwargs = {}

    def spy_stdio(params, **kwargs):
        captured_params.append(params)
        captured_stdio_kwargs.update(kwargs)
        return MagicMock()

    sdk_client = mock_sdk_client()

    @asynccontextmanager
    async def fake_client_cm(transport, **kwargs):
        yield sdk_client

    with (
        patch(
            "crewlet.mcp.client.stdio_client",
            side_effect=spy_stdio,
        ),
        patch(
            "crewlet.mcp.client.Client",
            side_effect=fake_client_cm,
        ),
        patch.dict("os.environ", {"INHERITED": "yes"}, clear=False),
    ):
        client = MCPClient(
            name="test",
            command="uvx",
            args=["mcp-test", "--flag"],
            env={"CUSTOM_KEY": "val"},
        )
        await client.start()

    assert len(captured_params) == 1
    params = captured_params[0]
    assert params.command == "uvx"
    assert params.args == ["mcp-test", "--flag"]
    # Custom env merged with os.environ
    assert params.env["CUSTOM_KEY"] == "val"
    assert params.env["INHERITED"] == "yes"
    # The subprocess's stderr goes to the relay's pipe, not the console.
    assert captured_stdio_kwargs["errlog"].fileno() >= 0

    await client.stop()


@pytest.mark.asyncio
async def test_start_stops_previous_session():
    """Calling start() twice stops the first session cleanly."""
    client = await _start_client()
    first_stack = client._exit_stack

    # Patch again for the second start()
    stdio_patch, client_patch = _client_patches(mock_sdk_client())
    with stdio_patch, client_patch:
        await client.start()

    # Should have a new exit stack, not the first one
    assert client._exit_stack is not first_stack
    assert client.is_running

    await client.stop()


# --- Tool listing ---


@pytest.mark.asyncio
async def test_list_tools():
    tools = [
        mock_tool("search", "Search items", {"type": "object"}),
        mock_tool("create", "Create item"),
    ]
    sdk_client = mock_sdk_client()
    sdk_client.list_tools = AsyncMock(return_value=mock_list_result(tools))

    client = await _start_client(sdk_client=sdk_client)
    result = await client.list_tools()

    assert len(result) == 2
    assert result[0]["name"] == "search"
    assert result[1]["name"] == "create"
    assert result[0]["inputSchema"] == {"type": "object"}

    await client.stop()


@pytest.mark.asyncio
async def test_list_tools_follows_pagination():
    """A paginated listing is walked to the end — tools past page one
    must not be silently dropped."""
    page1 = mock_list_result([mock_tool("alpha")], next_cursor="c1")
    page2 = mock_list_result([mock_tool("beta")], next_cursor=None)
    sdk_client = mock_sdk_client()
    sdk_client.list_tools = AsyncMock(side_effect=[page1, page2])

    client = await _start_client(sdk_client=sdk_client)
    result = await client.list_tools()

    assert [t["name"] for t in result] == ["alpha", "beta"]
    assert sdk_client.list_tools.await_count == 2
    # The second fetch resumes from the server's cursor.
    assert sdk_client.list_tools.await_args_list[1].kwargs["cursor"] == "c1"

    await client.stop()


@pytest.mark.asyncio
async def test_list_tools_empty_string_cursor_ends_listing():
    """Some serializers emit ``nextCursor: ""`` for 'no more pages' —
    out-of-spec but unambiguous; the walk must end, not re-query with an
    empty cursor forever."""
    sdk_client = mock_sdk_client()
    sdk_client.list_tools = AsyncMock(
        return_value=mock_list_result([mock_tool("only")], next_cursor="")
    )

    client = await _start_client(sdk_client=sdk_client)
    result = await client.list_tools()

    assert [t["name"] for t in result] == ["only"]
    assert sdk_client.list_tools.await_count == 1

    await client.stop()


@pytest.mark.asyncio
async def test_list_tools_repeated_cursor_raises():
    """A server that echoes the same cursor (offset-already-consumed
    bugs) must fail loudly instead of hanging tool discovery."""
    sdk_client = mock_sdk_client()
    sdk_client.list_tools = AsyncMock(
        return_value=mock_list_result([mock_tool("t")], next_cursor="SAME")
    )

    client = await _start_client(sdk_client=sdk_client)
    with pytest.raises(RuntimeError, match="repeated cursor"):
        await client.list_tools()

    await client.stop()


@pytest.mark.asyncio
async def test_list_tools_page_cap_raises():
    """A server minting fresh cursors forever hits the page ceiling."""
    calls = 0

    async def endless_pages(*, cursor=None):
        nonlocal calls
        calls += 1
        return mock_list_result([mock_tool(f"t{calls}")], next_cursor=f"c{calls}")

    sdk_client = mock_sdk_client()
    sdk_client.list_tools = AsyncMock(side_effect=endless_pages)

    client = await _start_client(sdk_client=sdk_client)
    with pytest.raises(RuntimeError, match="exceeded"):
        await client.list_tools()
    assert calls == _MAX_TOOL_PAGES

    await client.stop()


@pytest.mark.asyncio
async def test_list_tools_annotations_round_trip():
    """Real snake_case SDK annotations come out as the spec's camelCase
    wire keys — the contract tools/capabilities.py is written against."""
    sdk_client = mock_sdk_client()
    sdk_client.list_tools = AsyncMock(
        return_value=mock_list_result(
            [
                mock_tool(
                    "write",
                    annotations=ToolAnnotations(
                        read_only_hint=False,
                        destructive_hint=True,
                        open_world_hint=True,
                    ),
                )
            ]
        )
    )

    client = await _start_client(sdk_client=sdk_client)
    (tool,) = await client.list_tools()

    assert tool["annotations"]["readOnlyHint"] is False
    assert tool["annotations"]["destructiveHint"] is True
    assert tool["annotations"]["openWorldHint"] is True

    await client.stop()


@pytest.mark.asyncio
async def test_list_tools_not_initialized():
    client = MCPClient(name="test", command="echo")
    with pytest.raises(RuntimeError, match="not initialized"):
        await client.list_tools()


def test_mock_sdk_client_rejects_unknown_attributes():
    """The client mock is spec'd against the real 2.x Client — a 1.x-era
    attribute name must raise, not auto-materialize (that masking is how
    an API mismatch would slip through this suite)."""
    with pytest.raises(AttributeError):
        _ = mock_sdk_client().serverInfo


# --- Tool calling ---


@pytest.mark.asyncio
async def test_call_tool_success():
    sdk_client = mock_sdk_client()
    sdk_client.call_tool = AsyncMock(
        return_value=mock_call_result([mock_text_content("found 3 results")])
    )

    client = await _start_client(sdk_client=sdk_client)
    result = await client.call_tool("search", {"query": "test"})

    assert len(result) == 1
    assert result[0]["text"] == "found 3 results"
    sdk_client.call_tool.assert_called_once_with("search", {"query": "test"})

    await client.stop()


@pytest.mark.asyncio
async def test_call_tool_server_error():
    sdk_client = mock_sdk_client()
    sdk_client.call_tool = AsyncMock(
        return_value=mock_call_result(
            [mock_text_content("permission denied")],
            is_error=True,
        )
    )

    client = await _start_client(sdk_client=sdk_client)

    with pytest.raises(RuntimeError, match="permission denied"):
        await client.call_tool("admin", {})

    await client.stop()


@pytest.mark.asyncio
async def test_call_tool_not_initialized():
    client = MCPClient(name="test", command="echo")
    with pytest.raises(RuntimeError, match="not initialized"):
        await client.call_tool("search", {})


@pytest.mark.asyncio
async def test_call_tool_image_content():
    sdk_client = mock_sdk_client()
    sdk_client.call_tool = AsyncMock(
        return_value=mock_call_result([mock_image_content("image/png", "abc123")])
    )

    client = await _start_client(sdk_client=sdk_client)
    result = await client.call_tool("screenshot", {})

    assert len(result) == 1
    assert result[0]["type"] == "image"
    assert result[0]["mimeType"] == "image/png"
    assert result[0]["data"] == "abc123"

    await client.stop()


@pytest.mark.asyncio
async def test_call_tool_unknown_content_type():
    """Unknown content types are passed through with just type.

    The result is a stub built inline: the real CallToolResult union
    rejects unknown block types, and this test exists precisely for the
    forward-compat path where a future SDK ships one.
    """
    stub_result = MagicMock(spec=["content", "is_error"])
    stub_result.content = [mock_other_content("custom")]
    stub_result.is_error = False
    sdk_client = mock_sdk_client()
    sdk_client.call_tool = AsyncMock(return_value=stub_result)

    client = await _start_client(sdk_client=sdk_client)
    result = await client.call_tool("fetch", {})

    assert len(result) == 1
    assert result[0] == {"type": "custom"}

    await client.stop()


@pytest.mark.asyncio
async def test_call_tool_embedded_text_resource():
    """An embedded text resource (e.g. GitHub ``get_file_contents``)
    surfaces its body — not an opaque ``{'type': 'resource'}``
    placeholder, which would leave the model blind to file contents."""
    sdk_client = mock_sdk_client()
    sdk_client.call_tool = AsyncMock(
        return_value=mock_call_result(
            [
                mock_embedded_resource(
                    text="print('hello')\n",
                    uri="file:///app/main.py",
                    mime_type="text/x-python",
                )
            ]
        )
    )

    client = await _start_client(sdk_client=sdk_client)
    result = await client.call_tool("get_file_contents", {})

    assert result[0] == {
        "type": "resource",
        "uri": "file:///app/main.py",
        "mimeType": "text/x-python",
        "text": "print('hello')\n",
    }

    await client.stop()


@pytest.mark.asyncio
async def test_call_tool_embedded_blob_resource():
    """A binary embedded resource is described (uri/mime) with a ``blob``
    marker — its bytes can't be inlined as text."""
    sdk_client = mock_sdk_client()
    sdk_client.call_tool = AsyncMock(
        return_value=mock_call_result(
            [
                mock_embedded_resource(
                    blob="QUJD",
                    uri="file:///app/logo.png",
                    mime_type="image/png",
                )
            ]
        )
    )

    client = await _start_client(sdk_client=sdk_client)
    result = await client.call_tool("get_file_contents", {})

    assert result[0] == {
        "type": "resource",
        "uri": "file:///app/logo.png",
        "mimeType": "image/png",
        "blob": True,
    }

    await client.stop()


@pytest.mark.asyncio
async def test_call_tool_resource_link():
    """A resource_link surfaces its uri/name/mime."""
    sdk_client = mock_sdk_client()
    sdk_client.call_tool = AsyncMock(
        return_value=mock_call_result(
            [
                mock_resource_link(
                    uri="https://example/file.txt",
                    name="file.txt",
                    mime_type="text/plain",
                )
            ]
        )
    )

    client = await _start_client(sdk_client=sdk_client)
    result = await client.call_tool("search_code", {})

    assert result[0] == {
        "type": "resource_link",
        "uri": "https://example/file.txt",
        "name": "file.txt",
        "mimeType": "text/plain",
    }

    await client.stop()


# --- Cleanup / shutdown ---


@pytest.mark.asyncio
async def test_stop_cleans_up():
    client = await _start_client()
    assert client.is_running
    await client.stop()
    assert not client._initialized
    assert not client.is_running
    assert client._client is None


@pytest.mark.asyncio
async def test_stop_when_not_started():
    client = MCPClient(name="test", command="echo")
    await client.stop()
    assert not client._initialized


# --- Multiple calls ---


@pytest.mark.asyncio
async def test_multiple_tool_calls():
    call_count = 0

    async def fake_call_tool(name, args):
        nonlocal call_count
        call_count += 1
        return mock_call_result([mock_text_content(f"r{call_count}")])

    sdk_client = mock_sdk_client()
    sdk_client.call_tool = AsyncMock(side_effect=fake_call_tool)

    client = await _start_client(sdk_client=sdk_client)

    r1 = await client.call_tool("tool1", {"a": 1})
    r2 = await client.call_tool("tool2", {"b": 2})
    assert r1[0]["text"] == "r1"
    assert r2[0]["text"] == "r2"

    await client.stop()
