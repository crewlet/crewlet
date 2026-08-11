"""Discover-then-activate meta-tools shared by Plan and Execute.

The Plan and Execute phases never see MCP tool schemas in their
``tools=[...]`` upfront — the catalogue in the system prompt lists
*builtin* tools by name and *MCP servers* by name only. Two meta-tools
let the LLM go from server to tool to activation:

- ``list_mcp_server_tools(server)`` — returns the
  ``name: description`` listing for one MCP server, scoped to the
  role's configured MCP tools. The LLM uses this to discover what an
  MCP server offers before deciding to activate.
- ``activate_tool(name)`` — promotes a builtin or MCP tool from the
  role's catalogue into ``tools=[...]`` so the LLM can call it on
  the next round. Works in both Plan (for in-Plan recon) and Execute
  (for filling gaps Plan didn't predict).

Both meta-tools are constructed against a one-element ``surface_holder``
list so the closure can read the live ``ToolSurface`` after the
surface is constructed (chicken-and-egg: ``ToolSurface.for_plan`` /
``ToolSurface.for_execute`` require their meta-tools at construction
time).

The activation event is published as ``phase.tool_activated`` so
operators can see when a phase had to discover a tool mid-loop —
high rates on Execute signal Plan incompleteness; high rates on
Plan signal under-specified initial prompts.
"""

from __future__ import annotations

from collections.abc import Iterable
from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import PhaseToolActivated
from crewlet.tools.protocol import AgentContext, Tool, ToolResult
from crewlet.tools.registry import SimpleTool

logger = get_logger("agent.tool_discovery")


def _tools_by_server(role_mcp_tools: Iterable[Tool]) -> dict[str, list[Tool]]:
    """Bucket the role's MCP tools by their ``server_name`` attribute.

    Tools without ``server_name`` (non-MCP tools that slipped in by
    mistake) are skipped. The result is sorted by server, then by
    tool name within each server, so the prompt rendering and the
    ``list_mcp_server_tools`` output are byte-stable across turns.
    """
    buckets: dict[str, list[Tool]] = {}
    for tool in role_mcp_tools:
        server = getattr(tool, "server_name", "")
        if not server:
            continue
        buckets.setdefault(server, []).append(tool)
    for server in buckets:
        buckets[server].sort(key=lambda t: t.name)
    return dict(sorted(buckets.items()))


def _first_line(text: str) -> str:
    for line in (text or "").splitlines():
        stripped = line.strip()
        if stripped:
            return stripped
    return ""


def build_list_mcp_server_tools(
    role_mcp_tools: Iterable[Tool],
    *,
    availability_filter: set[str] | None = None,
) -> SimpleTool:
    """Build the ``list_mcp_server_tools`` meta-tool.

    The returned tool closes over a sorted server→tools mapping
    derived from the role's MCP tool list. Calling it with a server
    name returns one ``- name: first-line-of-description`` line per
    tool on that server.

    ``availability_filter``: when provided, MCP tools whose
    name is not in the set are excluded from the discovery buckets.
    This keeps discovery consistent with the slim prompt catalogue
    and ``activate_tool``'s acceptance set — without it, the LLM
    could discover a gated tool then fail to activate it.
    """
    # Bucket the full per-role MCP surface BEFORE applying the
    # availability filter so we can distinguish "server unknown for
    # this role" from "server exists but every tool on it is gated
    # off this turn" -- two failure modes the LLM has to act on
    # differently (the second is fixable mid-turn by activating a
    # specific gated tool; the first is not).
    unfiltered_buckets = _tools_by_server(role_mcp_tools)
    if availability_filter is not None:
        role_mcp_tools = [t for t in role_mcp_tools if t.name in availability_filter]
    buckets = _tools_by_server(role_mcp_tools)
    server_names = sorted(buckets)

    async def _list_tools(params: dict[str, Any], _ctx: AgentContext) -> ToolResult:
        server = params.get("server", "")
        if not isinstance(server, str) or not server:
            return ToolResult(
                success=False,
                error="server must be a non-empty string",
            )
        if server not in unfiltered_buckets:
            available = ", ".join(server_names) if server_names else "(none)"
            return ToolResult(
                success=False,
                error=(
                    f"MCP server {server!r} is not configured for this "
                    f"role. Available servers: {available}."
                ),
            )
        if server not in buckets:
            # Server exists but every tool on it is filtered off this
            # turn by ``availability_filter``.  Tell the LLM that
            # specifically so it can pick a different server or skip
            # the response, rather than guessing the server name was
            # wrong.
            return ToolResult(
                success=False,
                error=(
                    f"MCP server {server!r} is configured for this role "
                    f"but every tool on it is currently unavailable "
                    f"(role policy / per-turn gating).  Servers with "
                    f"available tools this turn: "
                    f"{', '.join(server_names) if server_names else '(none)'}."
                ),
            )
        lines = [
            f"- {tool.name}: {_first_line(tool.description)}"
            for tool in buckets[server]
        ]
        body = "\n".join(lines)
        return ToolResult(
            success=True,
            output=(
                f"Tools on MCP server {server!r} ({len(lines)} total). "
                "Call activate_tool(name=...) to promote one into your "
                "tools=[...] so you can invoke it on the next round.\n\n"
                f"{body}"
            ),
        )

    return SimpleTool(
        name="list_mcp_server_tools",
        description=(
            "List the tools available on one MCP server (e.g. github, "
            "atlassian, slack). Use this when you need a tool from an "
            "MCP server and don't yet know its exact name — your "
            "system prompt's `## MCP servers` block lists only server "
            "names, not their individual tools. After picking a tool, "
            "call `activate_tool(name=...)` to promote it into your "
            "`tools=[...]` so its schema arrives on the next message."
        ),
        parameters={
            "type": "object",
            "properties": {
                "server": {
                    "type": "string",
                    "description": (
                        "MCP server name, exactly as it appears in "
                        "the `## MCP servers` block of your system "
                        "prompt."
                    ),
                },
            },
            "required": ["server"],
        },
        fn=_list_tools,
    )


def build_activate_tool(
    catalogue_tools: Iterable[Tool],
    surface_holder: list[Any],
    *,
    phase: str,
    event_queue: Any | None = None,
    agent_id: str = "",
    agent_role: str = "",
    turn_id: str = "",
    iteration: int = 0,
) -> SimpleTool:
    """Build the ``activate_tool`` meta-tool for one phase.

    Promotes a tool from the role's full registry+MCP catalogue into
    the surface's active ``tools=[...]``. Works for both Plan
    (in-Plan recon) and Execute (filling Plan gaps mid-execution).

    ``catalogue_tools`` is the role's *unfiltered* registry+MCP set
    so we can distinguish "registered but availability-gated" from
    "truly unknown" — that split avoids the thrashing attractor where
    a gated tool's activation succeeds but its first call fails.

    ``event_queue`` / ``agent_id`` / ``agent_role`` / ``phase`` /
    ``turn_id`` / ``iteration`` are forwarded to a
    ``phase.tool_activated`` event on every successful activation.
    Operators use this to see when phases had to discover a tool
    mid-loop — high rates on Execute mean Plan undershot.

    Side effect on success: the activated tool name is also appended
    to ``ctx.spawn_subagent_config["parent_tool_names"]`` (when set
    by the engine), so any sub-agent the parent spawns later in the
    same turn inherits the activated tool. Without this propagation,
    mid-run activations create a gap between the parent's live
    surface and the sub-agent's parent allowlist.
    """
    catalogue_map = {t.name: t for t in catalogue_tools}

    async def _activate(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
        name = params.get("name", "")
        if not isinstance(name, str) or not name:
            return ToolResult(success=False, error="name must be a non-empty string")
        surface = surface_holder[0]
        if surface.has(name):
            return ToolResult(
                success=True,
                output=(
                    f"Tool '{name}' is ALREADY active in your "
                    "tools=[...] -- call it directly. Do not "
                    "activate again."
                ),
            )
        if surface.activate(name):
            _propagate_to_subagent_allowlist(ctx, name)
            await _publish_activated(
                event_queue=event_queue,
                agent_id=agent_id,
                agent_role=agent_role,
                phase=phase,
                tool_name=name,
                turn_id=turn_id,
                iteration=iteration,
            )
            return ToolResult(
                success=True,
                output=(
                    f"Tool '{name}' is now active in your tools=[...] and "
                    "its schema will appear on the next message -- call "
                    "it directly. No need to re-activate once active."
                ),
            )
        if name in catalogue_map:
            return ToolResult(
                success=False,
                error=(
                    f"Tool '{name}' is registered but not available in "
                    "this context (availability gate). Pick another "
                    "tool from the catalogue."
                ),
            )
        return ToolResult(
            success=False,
            error=(
                f"Tool '{name}' is not registered. Use "
                "list_mcp_server_tools(server=...) to discover MCP "
                "tool names if you're not sure of the exact name."
            ),
        )

    return SimpleTool(
        name="activate_tool",
        description=(
            "Activate a tool from your role's catalogue so you can "
            "call it directly on subsequent rounds. After activation "
            "the tool's schema appears in your `tools=[...]` on the "
            "next message and you invoke it normally; no need to "
            "re-activate once active.\n\n"
            "Use this for both builtin tools (listed by name in "
            "`## Builtin tools`) and MCP tools (whose names you got "
            "from `list_mcp_server_tools(server=...)`). In Plan, "
            "activate read-only recon tools you want to use before "
            "submitting the plan — action / write tools belong in "
            "`submit_plan`'s `tools_needed` so Execute runs them "
            "under Review. In Execute, activate ANY tool — action or "
            "read — if the planner missed it."
        ),
        parameters={
            "type": "object",
            "properties": {"name": {"type": "string"}},
            "required": ["name"],
        },
        fn=_activate,
    )


def _propagate_to_subagent_allowlist(ctx: AgentContext, tool_name: str) -> None:
    """Append ``tool_name`` to the parent's sub-agent allowlist.

    The engine snapshots ``parent_tool_names`` (used by
    ``ToolSurface.for_subagent`` to enforce "sub-agent tools must be
    a subset of parent's") before Execute runs (see
    ``crewlet.agent.turn._drive_phases``). Without propagation, any
    tool the parent activates mid-Execute is invisible to a
    ``spawn_subagent`` call later in the same turn — the sub-agent
    rejects the grant even though the parent legitimately has the
    tool. Mutating the live list closes that gap.

    Plan-phase activations also flow through here. ``parent_tool_names``
    is later overwritten by the Execute path so the propagation is a
    no-op for the next phase, but any sub-agent Plan itself spawns
    (rare but supported) sees the activated tool.
    """
    config = getattr(ctx, "__dict__", {}).get("spawn_subagent_config")
    if not isinstance(config, dict):
        return
    parent_names = config.setdefault("parent_tool_names", [])
    if not isinstance(parent_names, list):
        return
    if tool_name not in parent_names:
        parent_names.append(tool_name)


async def _publish_activated(
    *,
    event_queue: Any | None,
    agent_id: str,
    agent_role: str,
    phase: str,
    tool_name: str,
    turn_id: str = "",
    iteration: int = 0,
) -> None:
    """Publish a ``phase.tool_activated`` event, best-effort."""
    if event_queue is None or not phase:
        return
    event = PhaseToolActivated(
        source=agent_role,
        agent_id=agent_id,
        role=agent_role,
        phase=phase,
        tool_name=tool_name,
        turn_id=turn_id,
        iteration=iteration,
    )
    try:
        await event_queue.publish(f"crewlet.events.{event.type}", event)
    except Exception:
        logger.exception(
            "phase_tool_activated_publish_failed",
            phase=phase,
            tool_name=tool_name,
        )


__all__ = [
    "build_activate_tool",
    "build_list_mcp_server_tools",
]
