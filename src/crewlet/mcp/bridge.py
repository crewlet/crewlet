"""Bridge that wraps MCP server tools as Crewlet Tool protocol instances.

For each tool discovered on an MCP server, an ``MCPToolWrapper`` is
created that conforms to Crewlet's ``Tool`` protocol. When an agent
calls the tool, the wrapper delegates to the MCP client transparently.
"""

from __future__ import annotations

import contextlib
from typing import Any

from crewlet._logging import get_logger
from crewlet.mcp.client import MCPClient
from crewlet.mcp.http_client import MCPHttpClient
from crewlet.mcp.timeouts import (
    DEFAULT_MCP_REQUEST_TIMEOUT_SECONDS,
    DEFAULT_MCP_STARTUP_TIMEOUT_SECONDS,
)
from crewlet.tools.capabilities import ToolAnnotations
from crewlet.tools.protocol import AgentContext, ToolResult

logger = get_logger("mcp.bridge")


def mcp_instance_name(server: str, role_name: str) -> str:
    """Build a per-role MCP server instance name.

    Convention: ``"server::Role_Name"`` (spaces replaced with underscores).
    """
    return f"{server}::{role_name.replace(' ', '_')}"


def _render_block(block: dict[str, Any]) -> str:
    """Render one MCP content-block dict into agent-visible text.

    Blocks come from :func:`crewlet.mcp.client.MCPClient.call_tool`, which
    normalises the SDK's typed content objects via
    :func:`crewlet.mcp._content.extract_content`.  ``text`` carries the
    payload directly; ``resource`` (an embedded resource, e.g. GitHub's
    ``get_file_contents`` returning a file as text) carries the body on
    ``text`` when it is textual.  The earlier ``str(block)`` fallback
    stringified the whole dict, so an embedded text resource reached the
    model as the opaque ``{'type': 'resource'}`` — surfacing the body fixes
    that.  Binary payloads (image / audio / blob resources) can't be
    inlined as text, so we emit a compact descriptor instead.
    """
    btype = block.get("type")
    if btype == "text":
        return block.get("text", "")
    if btype == "resource":
        text = block.get("text")
        if text is not None:
            return text
        uri = block.get("uri", "")
        mime = block.get("mimeType", "unknown")
        label = f"{uri} ({mime})" if uri else f"({mime})"
        return f"[resource: {label}]"
    if btype == "resource_link":
        uri = block.get("uri", "")
        name = block.get("name", "")
        mime = block.get("mimeType", "")
        detail = " ".join(p for p in (name, f"({mime})" if mime else "") if p)
        label = f"{uri} {detail}".strip() if detail else uri
        return f"[resource_link: {label}]"
    if btype == "image":
        mime = block.get("mimeType", "unknown")
        return f"[image: {mime}]"
    if btype == "audio":
        mime = block.get("mimeType", "unknown")
        return f"[audio: {mime}]"
    return str(block)


class MCPToolWrapper:
    """Wraps a single MCP server tool as a Crewlet Tool.

    Conforms to the ``Tool`` protocol: ``name``, ``description``,
    ``parameters``, and ``execute``.
    """

    def __init__(
        self,
        mcp_client: MCPClient | MCPHttpClient,
        tool_def: dict[str, Any],
        name_prefix: str = "",
        annotation_overrides: dict[str, ToolAnnotations] | None = None,
    ) -> None:
        raw_name = tool_def["name"]
        self._name = f"{name_prefix}{raw_name}" if name_prefix else raw_name
        self._description = tool_def.get("description", f"MCP tool: {raw_name}")
        self._parameters: dict[str, Any] = tool_def.get(
            "inputSchema",
            {"type": "object", "properties": {}},
        )
        self._client = mcp_client
        self._raw_name = raw_name
        # Behavioural hints advertised by the server, with any operator
        # config override layered on top.  The engine reads these to
        # classify the tool — e.g. whether a sub-agent may call it —
        # without hardcoding a tool-name list.  Match the override by the
        # raw server name OR the (possibly prefixed) catalogue name, so an
        # operator can key it by either the name the server reports or the
        # name they see in the catalogue/dashboard — keying by the
        # prefixed name must not silently no-op.
        overrides = annotation_overrides or {}
        override = overrides.get(raw_name) or overrides.get(self._name)
        self._annotations = ToolAnnotations.from_mcp(tool_def.get("annotations")).merge(
            override
        )

    @property
    def name(self) -> str:
        return self._name

    @property
    def description(self) -> str:
        return self._description

    @property
    def parameters(self) -> dict[str, Any]:
        return self._parameters

    @property
    def annotations(self) -> ToolAnnotations:
        """MCP behavioural hints for this tool (server-advertised, with
        operator config overrides applied).  Read by
        :func:`crewlet.tools.capabilities.resolve_annotations`."""
        return self._annotations

    @property
    def server_name(self) -> str:
        """Bare MCP server name (no per-role suffix).

        Per-role MCP instances are created with ``MCPClient(name=...)``
        where the name is :func:`mcp_instance_name` output —
        ``"{server}::{Role_Name}"`` (e.g. ``"github::Engineer"``).
        For the Plan/Execute discovery flow the LLM sees and types the
        BARE server name (``github``) as it appears in YAML
        ``mcp_env`` keys, so this property strips the per-role suffix.
        Shared MCP servers (no namespacing) pass through unchanged.

        Used by ``ToolSurface.catalogue_text`` to bucket MCP tools by
        server and by ``list_mcp_server_tools`` to resolve a server
        name to its tool list.
        """
        name = self._client.name
        if "::" in name:
            return name.split("::", 1)[0]
        return name

    async def execute(
        self,
        params: dict[str, Any],
        context: AgentContext,
    ) -> ToolResult:
        """Call the MCP server tool and return a ToolResult."""
        try:
            content_blocks = await self._client.call_tool(self._raw_name, params)
            text_parts = [_render_block(block) for block in content_blocks]
            text_parts = [part for part in text_parts if part]

            output = "\n".join(text_parts) if text_parts else "Tool returned no output."
            return ToolResult(output=output, success=True)
        except Exception as exc:
            server = self._client.name
            return ToolResult(
                success=False,
                error=f"MCP tool error ({server}/{self._raw_name}): {exc}",
            )


class MCPToolBridge:
    """Manages MCP servers and bridges their tools into Crewlet.

    Supports both stdio (subprocess) and HTTP (remote) transports.

    Usage::

        bridge = MCPToolBridge()
        await bridge.add_server(
            name="jira", command="uvx",
            args=["mcp-atlassian"],
            env={"JIRA_API_TOKEN": "..."},
        )
        tools = bridge.get_all_tools()
    """

    def __init__(self) -> None:
        self._clients: dict[str, MCPClient | MCPHttpClient] = {}
        self._tools: dict[str, MCPToolWrapper] = {}

    async def add_server(
        self,
        name: str,
        command: str,
        args: list[str] | None = None,
        env: dict[str, str] | None = None,
        tool_prefix: str = "",
        annotation_overrides: dict[str, ToolAnnotations] | None = None,
        startup_timeout_seconds: float = DEFAULT_MCP_STARTUP_TIMEOUT_SECONDS,
        request_timeout_seconds: float = DEFAULT_MCP_REQUEST_TIMEOUT_SECONDS,
    ) -> list[MCPToolWrapper]:
        """Start a stdio MCP server and discover its tools.

        Args:
            name: Server identifier (e.g. "jira").
            command: Command to launch (e.g. "uvx").
            args: Command arguments.
            env: Environment variables for the process.
            tool_prefix: Optional prefix for tool names.
            annotation_overrides: Operator-supplied behavioural hints
                keyed by bare tool name, layered over what the server
                advertises (for servers that under-annotate).

        Returns:
            List of wrapped tools discovered on the server.
        """
        client = MCPClient(
            name=name,
            command=command,
            args=args,
            env=env,
            startup_timeout_seconds=startup_timeout_seconds,
            request_timeout_seconds=request_timeout_seconds,
        )
        await client.start()
        return await self._register(
            client, tool_prefix, annotation_overrides=annotation_overrides
        )

    async def add_http_server(
        self,
        name: str,
        url: str,
        headers: dict[str, str] | None = None,
        tool_prefix: str = "",
        exclude_tools: set[str] | None = None,
        annotation_overrides: dict[str, ToolAnnotations] | None = None,
        startup_timeout_seconds: float = DEFAULT_MCP_STARTUP_TIMEOUT_SECONDS,
        request_timeout_seconds: float = DEFAULT_MCP_REQUEST_TIMEOUT_SECONDS,
    ) -> list[MCPToolWrapper]:
        """Connect to a remote HTTP MCP server and discover tools.

        Args:
            name: Server identifier.
            url: MCP endpoint URL.
            headers: Auth/custom headers.
            tool_prefix: Optional prefix for tool names.
            exclude_tools: Tool names to skip (matched **before**
                ``tool_prefix`` is applied).
            annotation_overrides: Operator-supplied behavioural hints
                keyed by bare tool name (see :meth:`add_server`).

        Returns:
            List of wrapped tools discovered.
        """
        client = MCPHttpClient(
            name=name,
            url=url,
            headers=headers,
            startup_timeout_seconds=startup_timeout_seconds,
            request_timeout_seconds=request_timeout_seconds,
        )
        await client.start()
        return await self._register(
            client, tool_prefix, exclude_tools, annotation_overrides
        )

    async def _register(
        self,
        client: MCPClient | MCPHttpClient,
        tool_prefix: str,
        exclude_tools: set[str] | None = None,
        annotation_overrides: dict[str, ToolAnnotations] | None = None,
    ) -> list[MCPToolWrapper]:
        """Discover a connected client's tools, wrap them, and keep it.

        Registration happens only on success. A client recorded before
        discovery, whose discovery then failed, is a live subprocess
        with no tools: it answers ``has_client`` yes, so a live config
        edit sees a healthy server and the engine's own retry never
        fires, while the process sits there until shutdown. A server
        that could not be discovered is not a server this bridge has.
        """
        try:
            mcp_tools = await client.list_tools()
        except BaseException:
            with contextlib.suppress(Exception):
                await client.stop()
            raise
        wrapped: list[MCPToolWrapper] = []
        _exclude = exclude_tools or set()

        for tool_def in mcp_tools:
            if tool_def["name"] in _exclude:
                continue
            wrapper = MCPToolWrapper(
                mcp_client=client,
                tool_def=tool_def,
                name_prefix=tool_prefix,
                annotation_overrides=annotation_overrides,
            )
            self._tools[wrapper.name] = wrapper
            wrapped.append(wrapper)

        self._clients[client.name] = client
        logger.info(
            "tools_discovered",
            server=client.name,
            tool_count=len(wrapped),
            tool_names=[t.name for t in wrapped],
        )
        return wrapped

    def get_client(self, name: str) -> MCPClient | MCPHttpClient | None:
        """Look up an MCP client by server name."""
        return self._clients.get(name)

    def has_client(self, name: str) -> bool:
        """Check if a server with this name is registered."""
        return name in self._clients

    def get_all_tools(self) -> list[MCPToolWrapper]:
        """Return all bridged tools from all servers."""
        return list(self._tools.values())

    def get_tool(self, name: str) -> MCPToolWrapper | None:
        """Look up a specific bridged tool by name."""
        return self._tools.get(name)

    def get_server_tools(self, server_name: str) -> list[MCPToolWrapper]:
        """Return all tools from a specific server."""
        return [t for t in self._tools.values() if t._client.name == server_name]

    def _remove_server_tools(self, name: str) -> None:
        """Remove all tools belonging to a server."""
        self._tools = {k: v for k, v in self._tools.items() if v._client.name != name}

    async def stop_all(self) -> None:
        """Stop all MCP servers."""
        for name, client in list(self._clients.items()):
            try:
                await client.stop()
            except Exception as exc:
                logger.error("server_stop_failed", server=name, error=str(exc))
            finally:
                self._remove_server_tools(name)
        self._clients.clear()
        logger.info("all_servers_stopped")

    async def stop_server(self, name: str) -> None:
        """Stop a single MCP server by name."""
        client = self._clients.pop(name, None)
        if client:
            try:
                await client.stop()
            finally:
                self._remove_server_tools(name)

    async def restart_server(
        self,
        name: str,
        command: str,
        args: list[str] | None = None,
        env: dict[str, str] | None = None,
        tool_prefix: str = "",
        annotation_overrides: dict[str, ToolAnnotations] | None = None,
    ) -> list[MCPToolWrapper]:
        """Stop a running stdio MCP server and start it with new args/env.

        Used by :meth:`Engine._apply_mcp_servers_diff` when a live
        config edit changes a server's command, args, env, or
        tool_prefix.  Stops the prior process via :meth:`stop_server`,
        then launches a fresh client via :meth:`add_server`.  Returns
        the newly-discovered wrapped tools so the caller can refresh
        any cached per-role tool lists.
        """
        await self.stop_server(name)
        return await self.add_server(
            name=name,
            command=command,
            args=args,
            env=env,
            tool_prefix=tool_prefix,
            annotation_overrides=annotation_overrides,
        )

    async def restart_http_server(
        self,
        name: str,
        url: str,
        headers: dict[str, str] | None = None,
        tool_prefix: str = "",
        exclude_tools: set[str] | None = None,
        annotation_overrides: dict[str, ToolAnnotations] | None = None,
    ) -> list[MCPToolWrapper]:
        """Stop a running HTTP MCP server and reconnect with new url/headers.

        The HTTP analogue of :meth:`restart_server`.  Used by
        ``Engine._apply_mcp_servers_live`` when a live config edit
        changes a shared ``transport: http`` server's url, headers, or
        tool_prefix — :meth:`restart_server` would relaunch it as a
        stdio subprocess (with an empty command), silently dropping the
        remote connection.  Returns the freshly-discovered wrapped tools.
        """
        await self.stop_server(name)
        return await self.add_http_server(
            name=name,
            url=url,
            headers=headers,
            tool_prefix=tool_prefix,
            exclude_tools=exclude_tools,
            annotation_overrides=annotation_overrides,
        )
