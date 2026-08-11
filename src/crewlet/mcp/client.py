"""MCP client — subprocess servers over stdio using the MCP SDK.

Uses the official ``mcp`` Python SDK for JSON-RPC protocol handling,
session management, and transport via stdin/stdout of child processes.
The SDK's high-level :class:`mcp.Client` owns the protocol handshake
(modern ``server/discover`` with automatic fallback to the legacy
``initialize`` handshake), so this wrapper only manages the subprocess
transport and converts results to Crewlet's plain-dict contract.
"""

from __future__ import annotations

import os
from contextlib import AsyncExitStack
from typing import Any

from mcp import Client
from mcp.client.stdio import StdioServerParameters, stdio_client

from crewlet._logging import get_logger
from crewlet.mcp._content import extract_content, list_tool_dicts
from crewlet.mcp._identity import crewlet_client_info
from crewlet.mcp._stderr import ServerStderrRelay

logger = get_logger("mcp.client")


class MCPClient:
    """Async client for a single MCP server as a subprocess.

    Usage::

        client = MCPClient(
            name="jira",
            command="uvx",
            args=["mcp-atlassian"],
            env={"JIRA_API_TOKEN": "..."},
        )
        await client.start()
        tools = await client.list_tools()
        result = await client.call_tool("search", {"jql": "..."})
        await client.stop()
    """

    def __init__(
        self,
        name: str,
        command: str,
        args: list[str] | None = None,
        env: dict[str, str] | None = None,
    ) -> None:
        self.name = name
        self.command = command
        self.args = args or []
        self.env = env or {}
        self._client: Client | None = None
        self._exit_stack: AsyncExitStack | None = None
        self._tools: list[dict[str, Any]] = []
        self._initialized = False

    async def start(self) -> None:
        """Launch the MCP server subprocess and connect."""
        if self._exit_stack is not None:
            await self.stop()

        # Whole-env inheritance, deliberately kept: MCP servers routinely
        # read undeclared conventional variables (PATH, HOME, proxy vars,
        # a vendor SDK's own key), so narrowing this to `self.env` breaks
        # servers that work today.
        #
        # Note what this does NOT do: secret-store values are not poured
        # in. `self.env` has already been ${VAR}-resolved by the caller
        # (`resolve_env_vars`), so a server receives exactly the stored
        # credentials its config declares — and no others. Injecting the
        # whole store here would hand every seat's token to every
        # subprocess, which is strictly worse than the status quo.
        full_env = {**os.environ, **self.env}

        # Log env var keys (not values) passed to the MCP server
        # to help debug missing configuration.
        custom_keys = sorted(self.env.keys())
        empty_keys = [k for k in custom_keys if not self.env[k]]
        if empty_keys:
            logger.warning(
                "empty_env_vars",
                server=self.name,
                empty_keys=empty_keys,
            )
        logger.debug(
            "custom_env_keys",
            server=self.name,
            keys=custom_keys,
        )

        logger.info(
            "server_starting",
            server=self.name,
            command=self.command,
            args=self.args,
        )

        server_params = StdioServerParameters(
            command=self.command,
            args=self.args,
            env=full_env,
        )

        self._exit_stack = AsyncExitStack()
        relay: ServerStderrRelay | None = None
        try:
            # The relay enters first so LIFO teardown closes it after the
            # subprocess is gone — the reader then drains to EOF.
            relay = await self._exit_stack.enter_async_context(
                ServerStderrRelay(self.name)
            )
            self._client = await self._exit_stack.enter_async_context(
                Client(
                    stdio_client(server_params, errlog=relay.errlog),
                    client_info=crewlet_client_info(),
                )
            )

            # server_info is None when a modern (2026-era) server chooses
            # not to identify itself — that's a valid connection.
            info = self._client.server_info
            logger.info(
                "server_initialized",
                server=self.name,
                server_name=info.name if info else "unknown",
            )
            self._initialized = True
        except BaseException:  # includes CancelledError — must clean up subprocess
            try:
                await self._exit_stack.aclose()
            except Exception as cleanup_exc:
                logger.debug(
                    "start_cleanup_error",
                    server=self.name,
                    error=str(cleanup_exc),
                )
            # The server's own last words usually name the real cause
            # (bad token, missing binary, import error) — surface them
            # with the failure instead of leaving them at debug.
            if relay is not None and (tail := relay.tail()):
                logger.error("server_stderr_tail", server=self.name, lines=tail)
            self._exit_stack = None
            self._client = None
            raise

    async def stop(self) -> None:
        """Shut down the MCP server subprocess."""
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
        logger.info("server_stopped", server=self.name)

    async def list_tools(self) -> list[dict[str, Any]]:
        """Discover available tools from the MCP server."""
        if not self._initialized or not self._client:
            msg = f"MCP server '{self.name}' not initialized"
            raise RuntimeError(msg)

        # Follows pagination cursors; preserves the MCP behavioural hints
        # (readOnlyHint / destructiveHint / openWorldHint / …) so the
        # engine can derive tool behaviour (e.g. the sub-agent
        # shared-write guard) from the server rather than a hardcoded
        # tool-name list.
        self._tools = await list_tool_dicts(self._client)
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
        """Call a tool on the MCP server.

        Returns a list of content blocks with ``type`` and ``text``.
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

    @property
    def is_running(self) -> bool:
        return self._initialized and self._client is not None
