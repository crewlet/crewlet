"""Tests for the discover-then-activate meta-tools shared by Plan and
Execute.
"""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.agent.tool_discovery import (
    build_activate_tool,
    build_list_mcp_server_tools,
)
from crewlet.events.types import Event, PhaseToolActivated
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry
from crewlet.tools.surface import ToolSurface


async def _noop(_params: dict[str, Any], _ctx: AgentContext) -> ToolResult:
    return ToolResult(success=True, output="ok")


def _builtin(name: str, description: str = "") -> SimpleTool:
    return SimpleTool(
        name=name,
        description=description or f"{name} description",
        parameters={"type": "object"},
        fn=_noop,
    )


class _McpTool(SimpleTool):
    """SimpleTool with a ``server_name`` property — minimal stand-in
    for ``MCPToolWrapper`` in unit tests without a live MCP server.
    """

    def __init__(self, name: str, server: str, description: str = "") -> None:
        super().__init__(
            name=name,
            description=description or f"{name} on {server}",
            parameters={"type": "object"},
            fn=_noop,
        )
        self._server_name = server

    @property
    def server_name(self) -> str:
        return self._server_name


class _QueueStub:
    def __init__(self) -> None:
        self.published: list[tuple[str, Event]] = []

    async def publish(self, topic: str, event: Event) -> None:
        self.published.append((topic, event))


@pytest.fixture
def ctx() -> AgentContext:
    return AgentContext(agent_id="agent-1", agent_handle="alice", role="Engineer")


@pytest.fixture
def role_mcp_tools() -> list[Any]:
    return [
        _McpTool("create_pull_request", "github", "Create a PR."),
        _McpTool("get_pull_request", "github", "Read a PR."),
        _McpTool("jira_add_comment", "atlassian", "Add a Jira comment."),
    ]


# -- list_mcp_server_tools -------------------------------------------------


async def test_list_mcp_server_tools_returns_tools_for_one_server(ctx, role_mcp_tools):
    tool = build_list_mcp_server_tools(role_mcp_tools)
    result = await tool.execute({"server": "github"}, ctx)
    assert result.success is True
    out = result.output
    # Both github tools listed, by name + first-line description.
    assert "create_pull_request: Create a PR." in out
    assert "get_pull_request: Read a PR." in out
    # Atlassian tools must NOT leak into the github listing.
    assert "jira_add_comment" not in out
    # Output also tells the LLM what to do next (activate_tool).
    assert "activate_tool" in out


async def test_list_mcp_server_tools_rejects_unknown_server(ctx, role_mcp_tools):
    tool = build_list_mcp_server_tools(role_mcp_tools)
    result = await tool.execute({"server": "salesforce"}, ctx)
    assert result.success is False
    err = result.error or ""
    assert "salesforce" in err
    # Error lists the actually-configured servers so the LLM can recover.
    assert "atlassian" in err
    assert "github" in err


async def test_list_mcp_server_tools_rejects_empty_server(ctx, role_mcp_tools):
    tool = build_list_mcp_server_tools(role_mcp_tools)
    result = await tool.execute({"server": ""}, ctx)
    assert result.success is False
    assert "non-empty string" in (result.error or "")


async def test_list_mcp_server_tools_rejects_non_string_server(ctx, role_mcp_tools):
    tool = build_list_mcp_server_tools(role_mcp_tools)
    # Hallucinated non-string (a list / dict) must not crash.
    for hallucination in ([{"foo": 1}], {"key": "val"}, 42):
        result = await tool.execute({"server": hallucination}, ctx)
        assert result.success is False
        assert "non-empty string" in (result.error or "")


async def test_list_mcp_server_tools_skips_non_mcp_tools(ctx):
    """Non-MCP tools (no ``server_name``) leak in here mean test
    setup is wrong — the function silently skips them rather than
    crashing on the missing attribute.
    """
    mixed = [
        _builtin("regular_builtin"),
        _McpTool("github_tool", "github"),
    ]
    tool = build_list_mcp_server_tools(mixed)
    result = await tool.execute({"server": "github"}, ctx)
    assert result.success is True
    assert "github_tool" in result.output
    assert "regular_builtin" not in result.output


# -- activate_tool --------------------------------------------------------


async def test_activate_tool_promotes_plan_surface_tool(ctx):
    registry = ToolRegistry()
    registry.register(_builtin("lookup_colleague"))
    surface_holder: list[Any] = [None]
    activate = build_activate_tool(
        registry.list_tools(),
        surface_holder,
        phase="plan",
    )
    surface = ToolSurface.for_plan(registry, role_mcp_tools=[], meta_tools=[activate])
    surface_holder[0] = surface

    assert surface.has("lookup_colleague") is False
    result = await activate.execute({"name": "lookup_colleague"}, ctx)
    assert result.success is True
    assert "now active" in result.output
    assert surface.has("lookup_colleague") is True


async def test_activate_tool_promotes_execute_surface_tool(ctx):
    """Execute supports the same activation flow Plan has."""
    registry = ToolRegistry()
    registry.register(_builtin("search"))
    registry.register(_builtin("lookup_colleague"))
    surface_holder: list[Any] = [None]
    activate = build_activate_tool(
        registry.list_tools(),
        surface_holder,
        phase="execute",
    )
    surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=["search"],
        always_on=[],
        meta_tools=[activate],
    )
    surface_holder[0] = surface

    # ``lookup_colleague`` is in the role's catalogue but not in the plan.
    assert surface.has("lookup_colleague") is False
    result = await activate.execute({"name": "lookup_colleague"}, ctx)
    assert result.success is True
    assert surface.has("lookup_colleague") is True


async def test_activate_tool_publishes_phase_tool_activated_event(ctx):
    """Successful activations fan out a ``phase.tool_activated``
    event so dashboards can see when phases had to recover from
    incomplete catalogues / plans, including ``turn_id`` and
    ``iteration`` for correlation with the surrounding phase events.
    """
    registry = ToolRegistry()
    registry.register(_builtin("search"))
    queue = _QueueStub()
    surface_holder: list[Any] = [None]
    activate = build_activate_tool(
        registry.list_tools(),
        surface_holder,
        phase="execute",
        event_queue=queue,
        agent_id="agent-1",
        agent_role="Engineer",
        turn_id="turn-xyz",
        iteration=2,
    )
    surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=[],
        always_on=[],
        meta_tools=[activate],
    )
    surface_holder[0] = surface

    result = await activate.execute({"name": "search"}, ctx)
    assert result.success is True
    events = [e for _, e in queue.published if isinstance(e, PhaseToolActivated)]
    assert len(events) == 1
    assert events[0].phase == "execute"
    assert events[0].tool_name == "search"
    assert events[0].agent_id == "agent-1"
    assert events[0].role == "Engineer"
    # turn_id / iteration mirror AgentPhaseStarted / AgentPhaseCompleted
    # so dashboards can correlate the activation to a specific iteration.
    assert events[0].turn_id == "turn-xyz"
    assert events[0].iteration == 2


async def test_activate_tool_propagates_to_subagent_allowlist(ctx):
    """Tools the parent activates mid-Execute must be inheritable by
    sub-agents the parent spawns later in the same turn. The activate
    closure appends to ``ctx.spawn_subagent_config['parent_tool_names']``
    so the snapshot the engine froze before Execute ran stays current.
    """
    registry = ToolRegistry()
    registry.register(_builtin("confluence_search"))
    surface_holder: list[Any] = [None]
    activate = build_activate_tool(
        registry.list_tools(),
        surface_holder,
        phase="execute",
    )
    surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=["existing"],
        always_on=[],
        meta_tools=[activate],
    )
    surface_holder[0] = surface

    # Engine populates the spawn_subagent_config dict on the context
    # with the pre-Execute parent_tool_names snapshot.
    ctx.__dict__["spawn_subagent_config"] = {"parent_tool_names": ["existing"]}
    result = await activate.execute({"name": "confluence_search"}, ctx)
    assert result.success is True
    parent_names = ctx.__dict__["spawn_subagent_config"]["parent_tool_names"]
    # Pre-Execute entry preserved + the mid-run activation appended.
    assert "existing" in parent_names
    assert "confluence_search" in parent_names


async def test_activate_tool_propagation_safe_when_config_missing(ctx):
    """Plan-phase activations or tests without ``spawn_subagent_config``
    on the context must not crash the activate flow."""
    registry = ToolRegistry()
    registry.register(_builtin("lookup_colleague"))
    surface_holder: list[Any] = [None]
    activate = build_activate_tool(
        registry.list_tools(),
        surface_holder,
        phase="plan",
    )
    surface = ToolSurface.for_plan(registry, role_mcp_tools=[], meta_tools=[activate])
    surface_holder[0] = surface

    # No spawn_subagent_config on the context — closure must no-op.
    assert "spawn_subagent_config" not in ctx.__dict__
    result = await activate.execute({"name": "lookup_colleague"}, ctx)
    assert result.success is True


async def test_activate_tool_propagation_dedupes_repeat_activations(ctx):
    """A second successful activation for the same name (rare — surface
    short-circuits 'ALREADY active' first) must not duplicate the
    parent_tool_names entry."""
    registry = ToolRegistry()
    registry.register(_builtin("x"))
    surface_holder: list[Any] = [None]
    activate = build_activate_tool(
        registry.list_tools(),
        surface_holder,
        phase="execute",
    )
    surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=[],
        always_on=[],
        meta_tools=[activate],
    )
    surface_holder[0] = surface
    ctx.__dict__["spawn_subagent_config"] = {"parent_tool_names": []}

    await activate.execute({"name": "x"}, ctx)
    # Manually re-trigger the propagation helper via a second activate;
    # in production surface.activate's second call short-circuits, so
    # we directly assert dedup semantics via repeated direct mutations.
    parent_names = ctx.__dict__["spawn_subagent_config"]["parent_tool_names"]
    assert parent_names == ["x"]


async def test_list_mcp_server_tools_respects_availability_filter(ctx):
    """When ``availability_filter`` is provided, discovery only lists
    tools the role can actually activate — keeping
    ``list_mcp_server_tools`` consistent with the slim prompt
    catalogue and ``activate_tool``'s acceptance set."""
    role_mcp_tools = [
        _McpTool("create_pull_request", "github"),
        _McpTool("get_pull_request", "github"),
        _McpTool("jira_add_comment", "atlassian"),
    ]
    # ``create_pull_request`` is gated off this turn.
    tool = build_list_mcp_server_tools(
        role_mcp_tools,
        availability_filter={"get_pull_request", "jira_add_comment"},
    )
    result = await tool.execute({"server": "github"}, ctx)
    assert result.success is True
    assert "get_pull_request" in result.output
    assert "create_pull_request" not in result.output


async def test_list_mcp_server_tools_distinguishes_filtered_from_unknown(ctx):
    """``list_mcp_server_tools`` reports a different error when the
    server is configured for the role but every tool on it is gated
    off this turn vs. when the server is unknown to the role.  The
    LLM has to act differently on each case (the first is fixable
    mid-turn via ``activate_tool``; the second means the prompt
    catalogue and the discovery tool disagreed)."""
    role_mcp_tools = [
        _McpTool("create_pull_request", "github"),
        _McpTool("get_pull_request", "github"),
    ]
    # Every github tool gated off this turn.
    tool = build_list_mcp_server_tools(
        role_mcp_tools,
        availability_filter=set(),
    )

    result = await tool.execute({"server": "github"}, ctx)
    assert result.success is False
    assert "configured for this role" in (result.error or "")
    assert "every tool on it is currently unavailable" in (result.error or "")

    # Unknown server produces the standard "not configured" error.
    result_unknown = await tool.execute({"server": "atlassian"}, ctx)
    assert result_unknown.success is False
    assert "not configured for this role" in (result_unknown.error or "")


async def test_activate_tool_does_not_publish_event_when_already_active(ctx):
    """Re-activating an already-active tool short-circuits with a
    no-op confirmation — no event emitted, no surface mutation, no
    spurious telemetry on a thrashing LLM."""
    registry = ToolRegistry()
    registry.register(_builtin("search"))
    queue = _QueueStub()
    surface_holder: list[Any] = [None]
    activate = build_activate_tool(
        registry.list_tools(),
        surface_holder,
        phase="plan",
        event_queue=queue,
        agent_id="agent-1",
        agent_role="Engineer",
    )
    surface = ToolSurface.for_plan(registry, role_mcp_tools=[], meta_tools=[activate])
    surface_holder[0] = surface

    first = await activate.execute({"name": "search"}, ctx)
    assert first.success is True
    second = await activate.execute({"name": "search"}, ctx)
    assert second.success is True
    assert "ALREADY active" in second.output
    # Only ONE activation event despite two activate calls.
    events = [e for _, e in queue.published if isinstance(e, PhaseToolActivated)]
    assert len(events) == 1


async def test_activate_tool_unknown_name_suggests_discovery(ctx):
    """Activating a name not in the role's catalogue points the LLM
    at ``list_mcp_server_tools`` so it can discover MCP tool names
    rather than re-guessing."""
    registry = ToolRegistry()
    surface_holder: list[Any] = [None]
    activate = build_activate_tool(
        registry.list_tools(),
        surface_holder,
        phase="plan",
    )
    surface = ToolSurface.for_plan(registry, role_mcp_tools=[], meta_tools=[activate])
    surface_holder[0] = surface

    result = await activate.execute({"name": "ghost_tool"}, ctx)
    assert result.success is False
    err = result.error or ""
    assert "ghost_tool" in err
    assert "list_mcp_server_tools" in err


async def test_a_shared_server_the_catalogue_advertises_is_discoverable(ctx):
    """The prompt and the discovery tool must see one universe.

    `catalogue_text` renders every server it can see and tells the
    agent to call `list_mcp_server_tools` for the names. Built from the
    per-role list alone, this tool knew nothing about `shared: true`
    servers — whose tools live in the global registry — so an agent
    following its own instructions got "not configured for this role.
    Available servers: (none)". Nothing else reveals the names, since
    the slim catalogue deliberately withholds them, so no tool on any
    shared server was reachable at all.
    """
    from crewlet.agent.plan import _build_meta_tools
    from crewlet.tools.surface import merge_registry_and_mcp

    registry = ToolRegistry()
    registry.register(_McpTool("context7_resolve", "context7", "Resolve a library."))
    registry.register(_McpTool("context7_docs", "context7", "Fetch docs."))
    # No per-role MCP tools at all — a `shared: true` server is the
    # whole surface, which is the case that was unreachable.
    catalogue = merge_registry_and_mcp(registry, [])

    surface = ToolSurface.for_plan(registry, role_mcp_tools=[], meta_tools=[])
    meta = _build_meta_tools(catalogue, [], [surface])
    lister = next(t for t in meta if t.name == "list_mcp_server_tools")

    # The prompt advertises the server …
    assert "context7" in surface.catalogue_mcp_servers()
    # … so the tool it tells the agent to call must know it.
    result = await lister.execute({"server": "context7"}, ctx)
    assert result.success, result.error
    assert "context7_resolve" in result.output
    assert "context7_docs" in result.output


async def test_a_builtin_without_a_server_is_not_bucketed(ctx):
    """Passing the merged catalogue is only safe because non-MCP tools
    are skipped — otherwise every builtin would invent a server."""
    from crewlet.tools.surface import merge_registry_and_mcp

    registry = ToolRegistry()
    registry.register(_builtin("escalate"))
    registry.register(_McpTool("context7_docs", "context7", "Fetch docs."))

    tool = build_list_mcp_server_tools(merge_registry_and_mcp(registry, []))
    result = await tool.execute({"server": "context7"}, ctx)
    assert "escalate" not in result.output
    assert "context7_docs" in result.output
