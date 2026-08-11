"""Tests for phase-specific tool surfaces."""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.tools.capabilities import ToolAnnotations
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry
from crewlet.tools.surface import SUBAGENT_CONTROL_DENYLIST, ToolSurface


async def _noop(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
    return ToolResult(success=True, output="")


def _tool(name: str, description: str = "") -> SimpleTool:
    return SimpleTool(
        name=name,
        description=description or f"{name} description",
        parameters={"type": "object"},
        fn=_noop,
    )


class _AnnotatedMcpTool(SimpleTool):
    """A fake MCP-bridged tool carrying behavioural annotations, the way
    a real ``MCPToolWrapper`` exposes server-advertised hints."""

    def __init__(self, name: str, server: str, annotations: ToolAnnotations) -> None:
        super().__init__(
            name=name,
            description=f"{name} on {server}",
            parameters={"type": "object"},
            fn=_noop,
        )
        self._server = server
        self._annotations = annotations

    @property
    def server_name(self) -> str:
        return self._server

    @property
    def annotations(self) -> ToolAnnotations:
        return self._annotations


@pytest.fixture
def registry() -> ToolRegistry:
    r = ToolRegistry()
    r.register(_tool("escalate", "Escalate a task to the lead."))
    r.register(_tool("query_knowledge", "Query the knowledge base."))
    r.register(_tool("lookup_colleague", "Resolve an agent identifier."))
    r.register(_tool("use_skill", "Load a skill's body."))
    r.register(_tool("spawn_subagent", "Spawn an ephemeral worker."))
    r.register(_tool("a2a_ask", "Ask a colleague via A2A."))
    # A representative write-to-shared-surface tool, carrying the MCP
    # annotations a real server advertises so the sub-agent guard can
    # classify it without knowing its name.
    r.register(
        _tool("slack_conversations_postMessage", "Post to Slack."),
        annotations=ToolAnnotations(read_only=False, open_world=True),
    )
    return r


# -- Plan surface ----------------------------------------------------------


def test_plan_surface_tools_are_meta_only(registry):
    """Plan phase ``tools=[...]`` contains only the meta-tools."""
    meta = [_tool("activate_tool", "Activate a catalogue tool.")]
    s = ToolSurface.for_plan(registry, role_mcp_tools=[], meta_tools=meta)
    assert s.names == ["activate_tool"]


def test_plan_surface_catalogue_lists_builtins_by_name(registry):
    """Plan phase catalogue lists every builtin tool by name. MCP tools
    do NOT appear by name — only their server appears under
    ``## MCP servers``. ``test_plan_surface_catalogue_lists_mcp_servers``
    covers the MCP side.
    """
    s = ToolSurface.for_plan(registry, role_mcp_tools=[])
    cat = s.catalogue_text()
    assert "### Builtin tools" in cat
    assert "- escalate:" in cat
    assert "- query_knowledge:" in cat
    assert "- spawn_subagent:" in cat
    # No MCP servers configured -> no MCP-servers block.
    assert "### MCP servers" not in cat


def test_plan_surface_catalogue_lists_mcp_servers_not_tool_names(registry):
    """MCP tool names are hidden behind ``list_mcp_server_tools``;
    only the server name appears in the prompt catalogue.
    """

    class _McpTool(SimpleTool):
        def __init__(self, name: str, server: str) -> None:
            super().__init__(
                name=name,
                description=f"{name} on {server}",
                parameters={"type": "object"},
                fn=_noop,
            )
            self._server = server

        @property
        def server_name(self) -> str:
            return self._server

    mcp_tools = [
        _McpTool("create_pull_request", "github"),
        _McpTool("get_pull_request", "github"),
        _McpTool("jira_add_comment", "atlassian"),
    ]
    s = ToolSurface.for_plan(registry, role_mcp_tools=mcp_tools)
    cat = s.catalogue_text()
    # Server names appear; individual MCP tool names do not.
    assert "### MCP servers" in cat
    assert "- atlassian" in cat
    assert "- github" in cat
    assert "create_pull_request" not in cat
    assert "jira_add_comment" not in cat
    # The discovery instruction is present.
    assert "list_mcp_server_tools" in cat


def test_execute_surface_catalogue_renders_same_as_plan(registry):
    """Execute phase carries the same slim catalogue Plan sees so the
    executor can discover and activate tools mid-run."""
    exe = ToolSurface.for_execute(
        registry, role_mcp_tools=[], tools_needed=[], always_on=["query_knowledge"]
    )
    cat = exe.catalogue_text()
    assert "### Builtin tools" in cat
    assert "- escalate:" in cat


def test_execute_surface_no_catalogue_when_expose_false(registry):
    """``expose_catalogue=False`` (used by the grace path) yields an
    empty catalogue so the wrap-up prompt stays tight."""
    exe = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=[],
        always_on=[],
        expose_catalogue=False,
    )
    assert exe.catalogue_text() == ""
    assert exe.catalogue_names() == []


def test_review_surface_has_empty_catalogue(registry):
    """Review has no domain tools and no catalogue."""
    rev = ToolSurface.for_review(registry, role_mcp_tools=[])
    assert rev.catalogue_text() == ""
    assert rev.catalogue_names() == []


def test_plan_catalogue_uses_first_description_line(registry):
    r = ToolRegistry()
    r.register(
        _tool(
            "noisy",
            description="First line only.\nSecond line with more detail.\nThird.",
        )
    )
    s = ToolSurface.for_plan(r, role_mcp_tools=[])
    assert "- noisy: First line only." in s.catalogue_text()


def test_plan_surface_activate_promotes_catalogue_tool(registry):
    """``activate(name)`` adds a catalogue tool to ``tools=[...]`` so
    the planner can invoke it directly in subsequent rounds (in-Plan
    recon).  Subsequent calls are idempotent.
    """
    meta = [_tool("activate_tool", "Activator.")]
    s = ToolSurface.for_plan(registry, role_mcp_tools=[], meta_tools=meta)
    assert s.has("lookup_colleague") is False
    assert "lookup_colleague" in s.catalogue_names()

    assert s.activate("lookup_colleague") is True
    assert s.has("lookup_colleague") is True
    assert "lookup_colleague" in s.names
    # Idempotent: activating again still returns True without dupes.
    assert s.activate("lookup_colleague") is True
    assert s.names.count("lookup_colleague") == 1


def test_plan_surface_activate_unknown_returns_false(registry):
    """``activate(name)`` for a name not in the catalogue is a no-op."""
    meta = [_tool("activate_tool", "Activator.")]
    s = ToolSurface.for_plan(registry, role_mcp_tools=[], meta_tools=meta)
    assert s.activate("not_a_tool") is False
    assert s.has("not_a_tool") is False


def test_execute_surface_meta_tools_win_over_plan_named_collision(registry):
    """A plan-named tool that happens to share a meta-tool's name (e.g.
    the planner emits ``tools_needed=["activate_tool"]`` either as a
    confusion or because some role registered a same-named registry
    tool) must NOT shadow the meta-tool. The closure-bearing meta-tool
    has to be the one that lands in ``tools=[...]`` — otherwise
    discovery silently breaks for that whole phase.
    """
    r = ToolRegistry()
    # Simulate a registry collision: a builtin named "activate_tool".
    r.register(_tool("activate_tool", "Registry collision impostor"))
    closure_marker = _tool("activate_tool", "REAL closure meta-tool")
    surface = ToolSurface.for_execute(
        r,
        role_mcp_tools=[],
        tools_needed=["activate_tool"],  # plan tries to name the meta-tool.
        always_on=[],
        meta_tools=[closure_marker],
    )
    # Exactly one ``activate_tool`` on the surface.
    assert surface.names.count("activate_tool") == 1
    # And it's the closure-bearing meta-tool, not the registry impostor.
    assert surface.lookup("activate_tool") is closure_marker


def test_execute_surface_activate_promotes_catalogue_tool(registry):
    """Execute supports the discover-then-activate flow too: a
    catalogue tool not in the plan-named set can be promoted into
    ``tools=[...]`` mid-run via ``activate(name)``.  Execute grows its
    surface only through this explicit-activation contract.
    """
    exe = ToolSurface.for_execute(
        registry, role_mcp_tools=[], tools_needed=["escalate"], always_on=[]
    )
    assert exe.has("lookup_colleague") is False
    assert "lookup_colleague" in exe.catalogue_names()
    assert exe.activate("lookup_colleague") is True
    assert exe.has("lookup_colleague") is True


def test_review_surface_activate_returns_false(registry):
    """Activation stays unavailable in Review (which has no catalogue
    and a fixed decision-enum surface)."""
    rev = ToolSurface.for_review(registry, role_mcp_tools=[])
    assert rev.activate("lookup_colleague") is False


def test_execute_surface_no_activation_when_catalogue_hidden(registry):
    """``expose_catalogue=False`` removes the activation universe so
    the grace / rescue path cannot pick up tools mid-call.
    """
    exe = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=[],
        always_on=[],
        expose_catalogue=False,
    )
    assert exe.activate("lookup_colleague") is False


# -- Execute surface ------------------------------------------------------


def test_execute_surface_contains_only_plan_tools_plus_always_on(registry):
    s = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=["escalate", "lookup_colleague"],
        always_on=["query_knowledge"],
    )
    assert set(s.names) == {"escalate", "lookup_colleague", "query_knowledge"}


def test_execute_surface_dedups_always_on_already_named(registry):
    """A name appearing in both lists is present exactly once."""
    s = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=["query_knowledge"],
        always_on=["query_knowledge"],
    )
    assert s.names == ["query_knowledge"]


def test_execute_surface_preserves_order(registry):
    """Plan-named tools come first; always-on appended afterwards."""
    s = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=["escalate"],
        always_on=["query_knowledge", "use_skill"],
    )
    assert s.names == ["escalate", "query_knowledge", "use_skill"]


def test_execute_surface_drops_unknown_names(registry):
    """Names the registry doesn't know are silently omitted."""
    s = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=["does_not_exist", "escalate"],
        always_on=["query_knowledge"],
    )
    assert "does_not_exist" not in s.names
    assert "escalate" in s.names


def test_execute_surface_catalogue_names_match_registry(registry):
    """``catalogue_names()`` returns every catalogue entry so
    skill-trigger matching and activation lookup see the full set —
    even though the rendered prompt only shows builtin names + MCP
    server names."""
    s = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=["escalate"],
        always_on=["query_knowledge"],
    )
    cat = set(s.catalogue_names())
    assert {"escalate", "query_knowledge", "lookup_colleague", "use_skill"} <= cat


# -- MCP tool precedence --------------------------------------------------


def test_mcp_tool_overrides_global_by_name(registry):
    """Per-role MCP tool with same name overrides the global registry entry.

    Mirrors the existing AgentExecutor._get_tool_defs precedence rule.
    """
    mcp_escalate = _tool("escalate", "MCP-flavored escalate")
    s = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[mcp_escalate],
        tools_needed=["escalate"],
        always_on=[],
    )
    tool = s.lookup("escalate")
    assert tool is mcp_escalate
    assert tool.description == "MCP-flavored escalate"


def test_plan_catalogue_uses_mcp_override(registry):
    mcp_escalate = _tool("escalate", "MCP-flavored escalate")
    s = ToolSurface.for_plan(registry, role_mcp_tools=[mcp_escalate])
    cat = s.catalogue_text()
    assert "MCP-flavored escalate" in cat


# -- Sub-agent surface ----------------------------------------------------


def test_subagent_surface_rejects_spawn_subagent(registry):
    """Sub-agents cannot spawn sub-agents regardless of parent allowlist."""
    _, rejected = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=[],
        parent_tool_names=["spawn_subagent", "escalate"],
        requested_tool_names=["spawn_subagent"],
    )
    assert "spawn_subagent" in rejected


def test_subagent_surface_rejects_colleague_tools(registry):
    """Sub-agents cannot reach out to colleagues -- ``a2a_ask`` is denied
    by the first-party control set, and a write-to-shared-surface tool
    (here the Slack post tool, classified from its annotations rather
    than its name) is denied by the capability guard."""
    _, rejected = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=[],
        parent_tool_names=[
            "a2a_ask",
            "slack_conversations_postMessage",
            "escalate",
        ],
        requested_tool_names=["a2a_ask", "slack_conversations_postMessage"],
    )
    assert "a2a_ask" in rejected
    assert "slack_conversations_postMessage" in rejected


def test_subagent_surface_rejects_names_not_in_parent_list(registry):
    """Sub-agent tool list must be a subset of the parent's."""
    surf, rejected = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=[],
        parent_tool_names=["escalate"],
        requested_tool_names=["escalate", "query_knowledge"],
    )
    assert surf.names == ["escalate"]
    assert rejected == ["query_knowledge"]


def test_subagent_surface_dedupes_requested_tools(registry):
    """Duplicate names in the
    LLM-provided ``requested_tool_names`` must be collapsed.  Some
    provider SDKs reject ``tools=[...]`` arrays containing multiple
    entries with the same name; leaving them in also makes traces
    harder to read.  De-dup while preserving first-seen order.
    """
    surf, rejected = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=[],
        parent_tool_names=["escalate", "query_knowledge"],
        requested_tool_names=[
            "escalate",
            "query_knowledge",
            "escalate",  # dup
            "query_knowledge",  # dup
            "escalate",  # dup
        ],
    )
    assert surf.names == ["escalate", "query_knowledge"]
    # Dupes are collapsed silently (not "rejected" -- the first
    # occurrence is allowed).
    assert rejected == []
    # ``to_tool_defs()`` returns one ToolDef per name.
    defs = surf.to_tool_defs()
    assert len(defs) == 2
    assert [d.name for d in defs] == ["escalate", "query_knowledge"]


def test_subagent_control_denylist_is_first_party_only():
    """The static denylist covers only Crewlet's *own* engine-control
    tools — spawn, the discovery meta-tools, and A2A.  It must NOT name
    any third-party write tool (Slack/Jira/Confluence/GitHub); those are
    denied by annotation-derived classification instead, so the engine
    stays decoupled from any specific tool stack."""
    for name in (
        "spawn_subagent",
        "a2a_ask",
        # Sub-agents inherit the parent's frozen tool list and cannot
        # discover or activate additional tools.
        "activate_tool",
        "list_mcp_server_tools",
    ):
        assert name in SUBAGENT_CONTROL_DENYLIST
    # No concrete integration tool names appear in the set.
    for name in (
        "slack_conversations_postMessage",
        "jira_add_comment",
        "jira_update_issue",
        "confluence_add_footer_comment",
        "request_copilot_review",
        "create_pull_request_with_copilot",
    ):
        assert name not in SUBAGENT_CONTROL_DENYLIST


def test_subagent_denies_shared_writes_by_annotation_any_stack(registry):
    """A sub-agent is denied write-to-shared-surface tools derived from
    MCP annotations — for *any* stack, not just Slack/Jira/GitHub.  Here
    a Linear and a Teams write tool (names the engine has never heard
    of) are rejected purely because their annotations say read_only=False
    + open_world, while a read tool from the same servers is allowed."""
    write = ToolAnnotations(read_only=False, open_world=True)
    read = ToolAnnotations(read_only=True)
    mcp_tools = [
        _AnnotatedMcpTool("linear_create_comment", "linear", write),
        _AnnotatedMcpTool("teams_send_message", "teams", write),
        _AnnotatedMcpTool("linear_get_issue", "linear", read),
    ]
    names = [t.name for t in mcp_tools]
    surf, rejected = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=mcp_tools,
        parent_tool_names=names,
        requested_tool_names=names,
    )
    # Writes denied even though the parent named them; the read passes.
    assert surf.names == ["linear_get_issue"]
    assert set(rejected) == {"linear_create_comment", "teams_send_message"}


def test_subagent_allows_unannotated_tools(registry):
    """When a server advertises no annotations we cannot classify the
    tool, so it is NOT auto-denied — the parent's explicit allowlist is
    the curation, and operators can annotate via config.  This keeps the
    guard from silently breaking legitimate reads on under-annotating
    servers."""
    unknown = _AnnotatedMcpTool("custom_lookup", "custom", ToolAnnotations())
    surf, rejected = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=[unknown],
        parent_tool_names=["custom_lookup"],
        requested_tool_names=["custom_lookup"],
    )
    assert surf.names == ["custom_lookup"]
    assert rejected == []


def test_subagent_surface_always_includes_load_tool_skill(registry):
    """``load_tool_skill`` is the skill-body loader the sub-agent
    prompt's skill catalogue points at AND the unlock for the
    required-skill guard — it is included whenever registered, even
    when the parent didn't name it (it can't widen capabilities;
    it only reads prompt fragments)."""
    registry.register(_tool("load_tool_skill", "Load a tool skill body."))
    surf, rejected = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=[],
        parent_tool_names=["escalate"],
        requested_tool_names=["escalate"],
    )
    assert surf.names == ["escalate", "load_tool_skill"]
    assert rejected == []

    # Explicitly requesting it does not duplicate the entry.
    surf2, _ = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=[],
        parent_tool_names=["escalate", "load_tool_skill"],
        requested_tool_names=["escalate", "load_tool_skill"],
    )
    assert surf2.names.count("load_tool_skill") == 1


def test_subagent_surface_skips_loader_when_not_registered(registry):
    """No phantom entries: engines without the builtin registered get
    the plain parent-granted surface."""
    surf, _ = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=[],
        parent_tool_names=["escalate"],
        requested_tool_names=["escalate"],
    )
    assert surf.names == ["escalate"]


def test_subagent_discovery_adds_meta_tools_and_safe_catalogue(registry):
    """When spawned with the discovery meta-tools, a sub-agent gets them
    in its active surface AND a catalogue of the role's safe (read-only,
    non-control, non-shared-write) tools to activate -- so it can find
    tools the parent didn't name."""
    read = ToolAnnotations(read_only=True)
    write = ToolAnnotations(read_only=False, open_world=True)
    mcp_tools = [
        _AnnotatedMcpTool("jira_search", "atlassian", read),
        _AnnotatedMcpTool("jira_add_comment", "atlassian", write),
    ]
    meta = [
        _tool("list_mcp_server_tools", "Discover MCP tools."),
        _tool("activate_tool", "Activate a catalogue tool."),
    ]
    surf, _ = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=mcp_tools,
        parent_tool_names=["escalate"],
        requested_tool_names=["escalate"],
        meta_tools=meta,
    )
    # Meta-tools ride on top of the requested set.
    assert "list_mcp_server_tools" in surf.names
    assert "activate_tool" in surf.names
    # The read tool is activatable; the write tool and control tools are
    # NOT in the catalogue (identity-leak / engine-control guards hold).
    catalogue = set(surf.catalogue_names())
    assert "jira_search" in catalogue
    assert "jira_add_comment" not in catalogue
    assert "slack_conversations_postMessage" not in catalogue
    assert "spawn_subagent" not in catalogue
    assert "a2a_ask" not in catalogue


def test_subagent_can_activate_safe_catalogue_tool(registry):
    """A discovery-capable sub-agent can activate a read-only tool from
    its catalogue; a shared-surface write can never be activated."""
    read = ToolAnnotations(read_only=True)
    mcp_tools = [_AnnotatedMcpTool("jira_search", "atlassian", read)]
    meta = [_tool("activate_tool", "Activate a catalogue tool.")]
    surf, _ = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=mcp_tools,
        parent_tool_names=["escalate"],
        requested_tool_names=["escalate"],
        meta_tools=meta,
    )
    assert surf.activate("jira_search") is True
    assert surf.has("jira_search")
    # The write tool is not in the catalogue, so activation fails.
    assert surf.activate("slack_conversations_postMessage") is False


def test_subagent_without_meta_tools_stays_frozen(registry):
    """No meta-tools -> no catalogue, no activation: a frozen
    single-shot surface (what the unit tests and simple spawns use)."""
    surf, _ = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=[],
        parent_tool_names=["escalate"],
        requested_tool_names=["escalate"],
    )
    assert surf.catalogue_names() == []
    assert surf.activate("query_knowledge") is False


def test_subagent_safe_tools_filters_control_and_writes(registry):
    """``subagent_safe_tools`` exposes the role universe minus control
    tools and shared-surface writes -- the discovery catalogue."""
    from crewlet.tools.surface import subagent_safe_tools

    read = ToolAnnotations(read_only=True)
    write = ToolAnnotations(read_only=False, open_world=True)
    mcp_tools = [
        _AnnotatedMcpTool("jira_search", "atlassian", read),
        _AnnotatedMcpTool("jira_add_comment", "atlassian", write),
    ]
    safe = {t.name for t in subagent_safe_tools(registry, mcp_tools)}
    assert "jira_search" in safe
    assert "escalate" in safe  # a plain builtin read
    assert "jira_add_comment" not in safe
    assert "slack_conversations_postMessage" not in safe
    assert "spawn_subagent" not in safe
    assert "a2a_ask" not in safe


def test_surface_skill_guard_defaults_to_none(registry):
    """Surfaces carry no guard until a phase runner attaches one —
    Review / Judge / rescue surfaces stay unguarded."""
    for surface in (
        ToolSurface.for_plan(registry, role_mcp_tools=[]),
        ToolSurface.for_execute(
            registry, role_mcp_tools=[], tools_needed=[], always_on=[]
        ),
        ToolSurface.for_review(registry, role_mcp_tools=[]),
    ):
        assert surface.skill_guard is None


# -- to_tool_defs ---------------------------------------------------------


def test_to_tool_defs_matches_registered_parameters(registry):
    s = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=["escalate"],
        always_on=[],
    )
    defs = s.to_tool_defs()
    assert len(defs) == 1
    td = defs[0]
    assert td.name == "escalate"
    assert td.description == "Escalate a task to the lead."
    assert td.parameters == {"type": "object"}


# -- availability_filter ---------------------------------------------------


def test_plan_catalogue_filters_unavailable_names(registry):
    """When ``availability_filter`` is set, the Plan catalogue lists
    only filter members. Meta-tools are not filtered (the planner
    needs ``submit_plan`` / ``activate_tool`` regardless)."""
    meta = [_tool("submit_plan", "Submit a plan.")]
    surface = ToolSurface.for_plan(
        registry,
        role_mcp_tools=[],
        meta_tools=meta,
        availability_filter={"escalate", "query_knowledge"},
    )
    catalogue = surface.catalogue_names()
    assert "escalate" in catalogue
    assert "query_knowledge" in catalogue
    # ``use_skill`` is registered but not in the filter -> dropped.
    assert "use_skill" not in catalogue
    # Meta-tool stays in the schema array regardless of the filter.
    assert "submit_plan" in surface.names


def test_execute_filter_drops_unavailable_plan_tools(registry):
    """An execute surface built from a plan that names a tool whose
    ``check_fn`` returned ``False`` excludes that tool from the
    exec round, even though the plan named it."""
    surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=["escalate", "lookup_colleague"],
        always_on=["query_knowledge"],
        availability_filter={"escalate", "query_knowledge"},
    )
    # ``lookup_colleague`` was named by the plan but is not in the filter.
    assert "lookup_colleague" not in surface.names
    assert "escalate" in surface.names
    assert "query_knowledge" in surface.names


def test_execute_filter_none_means_no_filtering(registry):
    """Default ``availability_filter=None`` preserves existing
    backwards-compatible behaviour: no filtering, only the existing
    registered-or-not check."""
    surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=["escalate", "lookup_colleague"],
        always_on=[],
    )
    assert "escalate" in surface.names
    assert "lookup_colleague" in surface.names


def test_subagent_filter_rejects_unavailable_names(registry):
    """Sub-agent surface inherits the parent's availability
    resolution. A name in the parent's allowlist but not in the
    filter is rejected (same as if the parent never had it)."""
    surface, rejected = ToolSurface.for_subagent(
        registry,
        role_mcp_tools=[],
        parent_tool_names=["escalate", "query_knowledge"],
        requested_tool_names=["escalate", "query_knowledge"],
        availability_filter={"escalate"},
    )
    assert surface.names == ["escalate"]
    assert "query_knowledge" in rejected


# -- onboarding is a discovery phase (regression) ------------------------


def test_onboarding_surface_supports_discovery_and_activation(registry):
    """A surface relabelled ``phase="onboarding"`` (the dedicated pre-Plan
    pass) must behave like a discovery phase: it renders the slim catalogue
    AND ``activate`` promotes a catalogue tool.

    The "availability gate" trap: if onboarding can SEE its
    knowledge-base MCP tools via ``list_mcp_server_tools``
    (phase-independent) but ``activate`` silently refuses (phase not in
    the allowlist), the
    agent never reads its pages, never calls ``mark_onboarded``, and the
    pass re-fires every turn.
    """
    mcp = _AnnotatedMcpTool(
        "confluence_search", "atlassian", ToolAnnotations(read_only=True)
    )
    surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[mcp],
        tools_needed=[],
        always_on=["query_knowledge"],
    )
    surface.phase = "onboarding"

    # The slim catalogue renders (it was "" -> the onboarding prompt had no
    # "## Available tools" block at all, so the agent flew blind).
    assert "### MCP servers" in surface.catalogue_text()
    assert "confluence_search" in surface.catalogue_names()

    # And the knowledge-base tool can actually be activated.
    assert surface.has("confluence_search") is False
    assert surface.activate("confluence_search") is True
    assert surface.has("confluence_search") is True


def test_non_discovery_phase_still_blocks_activation(registry):
    """Inverse guard: a Review surface (no discovery meta-tools) exposes no
    catalogue and never activates."""
    surface = ToolSurface.for_review(registry, role_mcp_tools=[])
    assert surface.catalogue_names() == []
    assert surface.activate("escalate") is False
