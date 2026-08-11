"""Tests for MCP HTTP client — SDK-based Streamable HTTP transport.

The SDK seam is the high-level ``mcp.Client`` plus the
``streamable_http_client`` transport factory, patched at the
``crewlet.mcp.http_client`` module level.  The httpx2 client the wrapper
builds (auth headers + error-logging hook) is real — constructing one
performs no I/O — so those tests exercise the actual configuration.
"""

from contextlib import asynccontextmanager
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from crewlet.mcp.http_client import MCPHttpClient

from .conftest import (
    mock_call_result,
    mock_list_result,
    mock_sdk_client,
    mock_text_content,
    mock_tool,
)


def _client_patches(sdk_client, transport_spy=None):
    """Patchers wiring MCPHttpClient.start() to *sdk_client*."""

    @asynccontextmanager
    async def fake_client_cm(transport, **kwargs):
        yield sdk_client

    return (
        patch(
            "crewlet.mcp.http_client.streamable_http_client",
            side_effect=transport_spy or (lambda url, **kw: MagicMock()),
        ),
        patch("crewlet.mcp.http_client.Client", side_effect=fake_client_cm),
    )


async def _start_http_client(
    sdk_client=None,
    *,
    url: str = "https://example.com/mcp",
    auth_headers: dict[str, str] | None = None,
) -> MCPHttpClient:
    """Start an MCPHttpClient with mocked SDK components."""
    sdk_client = sdk_client if sdk_client is not None else mock_sdk_client()

    transport_patch, client_patch = _client_patches(sdk_client)
    with transport_patch, client_patch:
        client = MCPHttpClient(
            name="test",
            url=url,
            headers=auth_headers,
        )
        await client.start()
        return client


# --- Connection / Initialization ---


@pytest.mark.asyncio
async def test_init_defaults():
    client = MCPHttpClient(name="test", url="https://example.com/mcp/")
    assert client.name == "test"
    # trailing / stripped
    assert client.url == "https://example.com/mcp"
    assert not client.is_running


@pytest.mark.asyncio
async def test_start_initializes_session():
    client = await _start_http_client(
        auth_headers={"Authorization": "Bearer tok"},
    )
    assert client._initialized
    assert client.is_running
    await client.stop()


@pytest.mark.asyncio
async def test_start_wires_transport():
    """The transport factory gets the URL and the wrapper-built httpx2
    client (which carries the auth headers + the error-logging hook)."""
    calls = []

    def transport_spy(url, **kwargs):
        calls.append((url, kwargs))
        return MagicMock()

    sdk_client = mock_sdk_client()
    transport_patch, client_patch = _client_patches(
        sdk_client, transport_spy=transport_spy
    )
    with transport_patch, client_patch:
        client = MCPHttpClient(
            name="test",
            url="https://example.com/mcp",
            headers={"Authorization": "Bearer tok"},
        )
        await client.start()

    assert len(calls) == 1
    url, kwargs = calls[0]
    assert url == "https://example.com/mcp"
    http_client = kwargs["http_client"]
    assert http_client.headers["Authorization"] == "Bearer tok"
    assert http_client.event_hooks["response"], "error-logging hook missing"

    await client.stop()


@pytest.mark.asyncio
async def test_start_failure_cleans_up():
    @asynccontextmanager
    async def failing_client_cm(transport, **kwargs):
        raise RuntimeError("connect failed")
        yield  # pragma: no cover

    with (
        patch(
            "crewlet.mcp.http_client.streamable_http_client",
            side_effect=lambda url, **kw: MagicMock(),
        ),
        patch(
            "crewlet.mcp.http_client.Client",
            side_effect=failing_client_cm,
        ),
    ):
        client = MCPHttpClient(name="test", url="https://example.com/mcp")
        with pytest.raises(RuntimeError, match="connect failed"):
            await client.start()

    assert client._exit_stack is None
    assert client._client is None
    assert not client.is_running


@pytest.mark.asyncio
async def test_start_stops_previous_session():
    """Calling start() twice stops the first session cleanly."""
    client = await _start_http_client()
    first_stack = client._exit_stack

    # Patch again for the second start()
    transport_patch, client_patch = _client_patches(mock_sdk_client())
    with transport_patch, client_patch:
        await client.start()

    assert client._exit_stack is not first_stack
    assert client.is_running

    await client.stop()


# --- Tool listing ---


@pytest.mark.asyncio
async def test_list_tools():
    tools_data = [
        mock_tool("search", "Search"),
        mock_tool("create", "Create"),
    ]
    sdk_client = mock_sdk_client()
    sdk_client.list_tools = AsyncMock(return_value=mock_list_result(tools_data))

    client = await _start_http_client(sdk_client=sdk_client)
    tools = await client.list_tools()

    assert len(tools) == 2
    assert tools[0]["name"] == "search"
    assert tools[1]["name"] == "create"

    await client.stop()


@pytest.mark.asyncio
async def test_list_tools_not_initialized():
    client = MCPHttpClient(name="test", url="https://example.com/mcp")
    with pytest.raises(RuntimeError, match="not initialized"):
        await client.list_tools()


# --- Tool calling ---


@pytest.mark.asyncio
async def test_call_tool_success():
    sdk_client = mock_sdk_client()
    sdk_client.call_tool = AsyncMock(
        return_value=mock_call_result([mock_text_content("result data")])
    )

    client = await _start_http_client(sdk_client=sdk_client)
    result = await client.call_tool("search", {"q": "test"})

    assert len(result) == 1
    assert result[0]["text"] == "result data"
    sdk_client.call_tool.assert_called_once_with("search", {"q": "test"})

    await client.stop()


@pytest.mark.asyncio
async def test_call_tool_error():
    sdk_client = mock_sdk_client()
    sdk_client.call_tool = AsyncMock(
        return_value=mock_call_result(
            [mock_text_content("access denied")],
            is_error=True,
        )
    )

    client = await _start_http_client(sdk_client=sdk_client)
    with pytest.raises(RuntimeError, match="access denied"):
        await client.call_tool("admin", {})

    await client.stop()


@pytest.mark.asyncio
async def test_call_tool_not_initialized():
    client = MCPHttpClient(name="test", url="https://example.com/mcp")
    with pytest.raises(RuntimeError, match="not initialized"):
        await client.call_tool("search", {})


# --- Stop / cleanup ---


@pytest.mark.asyncio
async def test_stop_cleans_up():
    client = await _start_http_client()
    await client.stop()
    assert not client.is_running
    assert client._client is None


@pytest.mark.asyncio
async def test_stop_when_not_started():
    client = MCPHttpClient(name="test", url="https://example.com/mcp")
    await client.stop()
    assert not client.is_running
