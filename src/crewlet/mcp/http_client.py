"""MCP HTTP client — Streamable HTTP transport using the MCP SDK.

Uses the official ``mcp`` Python SDK for protocol handling, session
management, and Streamable HTTP transport to remote servers.  The SDK's
high-level :class:`mcp.Client` owns the protocol handshake (modern
``server/discover`` with automatic fallback to the legacy ``initialize``
handshake); this wrapper supplies the transport's ``httpx2`` client
(auth headers + error-body logging) and converts results to Crewlet's
plain-dict contract.
"""

from __future__ import annotations

import asyncio
from contextlib import AsyncExitStack
from typing import Any

import httpx2
from mcp import Client
from mcp.client.streamable_http import streamable_http_client
from mcp.shared._httpx_utils import create_mcp_http_client

from crewlet._logging import get_logger
from crewlet.mcp._content import extract_content, list_tool_dicts
from crewlet.mcp._identity import crewlet_client_info
from crewlet.mcp.timeouts import (
    DEFAULT_MCP_REQUEST_TIMEOUT_SECONDS,
    DEFAULT_MCP_STARTUP_TIMEOUT_SECONDS,
)

logger = get_logger("mcp.http_client")


class MCPHttpClient:
    """Async client for a remote MCP server over Streamable HTTP.

    Usage::

        client = MCPHttpClient(
            name="atlassian",
            url="https://mcp.atlassian.com/v1/mcp",
            headers={"Authorization": "Bearer <token>"},
        )
        await client.start()
        tools = await client.list_tools()
        result = await client.call_tool("search", {"jql": "..."})
        await client.stop()
    """

    def __init__(
        self,
        name: str,
        url: str,
        headers: dict[str, str] | None = None,
        startup_timeout_seconds: float = DEFAULT_MCP_STARTUP_TIMEOUT_SECONDS,
        request_timeout_seconds: float = DEFAULT_MCP_REQUEST_TIMEOUT_SECONDS,
    ) -> None:
        self.name = name
        self.url = url.rstrip("/")
        self._auth_headers = headers or {}
        self.startup_timeout_seconds = startup_timeout_seconds
        self.request_timeout_seconds = request_timeout_seconds
        self._client: Client | None = None
        self._exit_stack: AsyncExitStack | None = None
        self._tools: list[dict[str, Any]] = []
        self._initialized = False

    async def start(self) -> None:
        """Connect and initialize the MCP session."""
        if self._exit_stack is not None:
            await self.stop()

        logger.info(
            "server_connecting",
            server=self.name,
            url=self.url,
        )

        self._exit_stack = AsyncExitStack()
        try:
            # The transport does not manage the lifecycle of a caller-
            # provided httpx client, so it goes on our exit stack first
            # (LIFO: transport closes before the client it rides on).
            http_client = self._build_http_client()
            await self._exit_stack.enter_async_context(http_client)

            # Bounded for the same reason as the stdio client, and with
            # the same two layers: ``read_timeout_seconds`` per request,
            # ``wait_for`` over the connect. The httpx transport already
            # has its own read timeout, but it resets on every chunk —
            # an endpoint that dribbles keepalives and never answers
            # holds the connection open indefinitely underneath it.
            async def _connect() -> None:
                self._client = await self._exit_stack.enter_async_context(  # type: ignore[union-attr]
                    Client(
                        streamable_http_client(self.url, http_client=http_client),
                        client_info=crewlet_client_info(),
                        read_timeout_seconds=self.request_timeout_seconds,
                    )
                )

            try:
                await asyncio.wait_for(_connect(), self.startup_timeout_seconds)
            except TimeoutError as exc:
                msg = (
                    f"MCP server '{self.name}' did not connect within "
                    f"{self.startup_timeout_seconds:g}s ({self.url}). Raise "
                    "startup_timeout_seconds on the server if it legitimately "
                    "takes longer to respond."
                )
                raise TimeoutError(msg) from exc

            # server_info is None when a modern (2026-era) server chooses
            # not to identify itself — that's a valid connection.
            info = self._client.server_info
            logger.info(
                "server_initialized",
                server=self.name,
                server_name=info.name if info else "unknown",
            )
            self._initialized = True
        except BaseException:  # includes CancelledError — must clean up connection
            try:
                await self._exit_stack.aclose()
            except Exception as cleanup_exc:
                logger.debug(
                    "start_cleanup_error",
                    server=self.name,
                    error=str(cleanup_exc),
                )
            self._exit_stack = None
            self._client = None
            raise

    async def stop(self) -> None:
        """Terminate the session and close connections."""
        if self._exit_stack:
            try:
                await self._exit_stack.aclose()
            except Exception as exc:
                logger.debug(
                    "stop_cleanup_error",
                    server=self.name,
                    error=str(exc),
                )
            self._exit_stack = None
        self._client = None
        self._initialized = False
        logger.info("server_disconnected", server=self.name)

    async def list_tools(self) -> list[dict[str, Any]]:
        """Discover available tools from the remote server."""
        if not self._initialized or not self._client:
            msg = f"MCP server '{self.name}' not initialized"
            raise RuntimeError(msg)

        # Follows pagination cursors; preserves the MCP behavioural hints
        # — see MCPClient.list_tools.
        # Discovery answers to the startup budget — see MCPClient.
        try:
            self._tools = await asyncio.wait_for(
                list_tool_dicts(self._client), self.startup_timeout_seconds
            )
        except TimeoutError as exc:
            msg = (
                f"MCP server '{self.name}' did not answer tool discovery "
                f"within {self.startup_timeout_seconds:g}s"
            )
            raise TimeoutError(msg) from exc
        logger.info(
            "tools_listed",
            server=self.name,
            tool_count=len(self._tools),
            tool_names=[t["name"] for t in self._tools],
        )
        return self._tools

    async def call_tool(
        self, tool_name: str, arguments: dict[str, Any]
    ) -> list[dict[str, Any]]:
        """Call a tool on the remote MCP server.

        Returns a list of content blocks.
        """
        if not self._initialized or not self._client:
            msg = f"MCP server '{self.name}' not initialized"
            raise RuntimeError(msg)

        result = await self._client.call_tool(tool_name, arguments)

        content = extract_content(result.content)

        if result.is_error:
            text = " ".join(
                c.get("text", "") for c in content if c.get("type") == "text"
            )
            msg = f"MCP tool '{tool_name}' error: {text}"
            raise RuntimeError(msg)

        return content

    def _build_http_client(self) -> httpx2.AsyncClient:
        """Build the transport's httpx2 client: auth headers + error logging.

        The SDK surfaces an HTTP failure only after its task group
        unwinds, by which point the response body is gone — so we hook
        into the response lifecycle to log the body *before* it is lost.
        """
        server_name = self.name

        async def _log_response(
            response: httpx2.Response,
        ) -> None:
            if response.status_code >= 400:
                await response.aread()
                req = response.request
                # Read request body for context; may be
                # bytes, stream, or None.
                req_body = ""
                if req.content:
                    try:
                        req_body = req.content.decode(errors="replace")
                    except AttributeError:
                        req_body = str(req.content)
                logger.error(
                    "http_error",
                    server=server_name,
                    status_code=response.status_code,
                    method=req.method,
                    url=str(req.url),
                    request_body=req_body or "(empty)",
                    response_body=response.text or "(empty)",
                )

        client = create_mcp_http_client(headers=self._auth_headers)
        client.event_hooks["response"].append(_log_response)
        return client

    @property
    def is_running(self) -> bool:
        return self._initialized and self._client is not None
