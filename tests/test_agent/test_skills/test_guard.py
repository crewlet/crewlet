"""Tests for the required-skill guard (load-before-use enforcement).

Covers the layers bottom-up:

- ``SkillGuard`` unit behaviour (block / unlock / exemptions / events),
- ``build_skill_guard`` arming rules,
- ``execute_tool`` integration (the dispatch gate),
- ``run_tool_loop`` end-to-end (block → load → retry within one
  session),
- phase-runner wiring (``run_plan_phase`` / ``run_execute_phase`` /
  ``spawn_subagent`` all attach a guard to their surface).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.agent.execute import run_execute_phase
from crewlet.agent.llm_loop import execute_tool, run_tool_loop
from crewlet.agent.plan import ExecutionPlan, run_plan_phase
from crewlet.agent.skills import (
    GUARD_EXEMPT_TOOLS,
    Phase,
    PromptSkill,
    PromptSkillRegistry,
    SkillGuard,
    TriggerContext,
    TriggerExpr,
    build_skill_guard,
)
from crewlet.agent.subagent import spawn_subagent
from crewlet.agent.turn_context import TurnContext
from crewlet.events.types import ToolSkillGuardBlocked
from crewlet.providers.llm.protocol import Completion, ToolCall
from crewlet.tools.builtin import _load_tool_skill
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry
from crewlet.tools.surface import ToolSurface

# ---------------------------------------------------------------------------
# Stubs / builders
# ---------------------------------------------------------------------------


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


@dataclass
class _ProviderStub:
    completions: list[Completion]
    calls: int = 0
    model: str = "stub"

    async def complete(
        self, messages, tools=None, temperature=0.7, max_tokens=None, tool_choice=None
    ):
        idx = self.calls
        self.calls += 1
        if idx >= len(self.completions):
            return Completion(content="done", tool_calls=[])
        return self.completions[idx]


_EXECUTED: list[str] = []


async def _recording_noop(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
    _EXECUTED.append("called")
    return ToolResult(success=True, output="done")


def _tool(name: str) -> SimpleTool:
    return SimpleTool(
        name=name,
        description=f"{name} description",
        parameters={"type": "object"},
        fn=_recording_noop,
    )


class _McpTool(SimpleTool):
    def __init__(self, name: str, server: str) -> None:
        super().__init__(
            name=name,
            description=f"{name} on {server}",
            parameters={"type": "object"},
            fn=_recording_noop,
        )
        self._server = server

    @property
    def server_name(self) -> str:
        return self._server


def _load_skill_tool() -> SimpleTool:
    return SimpleTool(
        name="load_tool_skill",
        description="Load the full body of a tool skill by exact key.",
        parameters={"type": "object"},
        fn=_load_tool_skill,
    )


def _skill(
    key: str,
    trigger: TriggerExpr,
    *,
    phases: set[Phase] | None = None,
    required: bool = True,
) -> PromptSkill:
    return PromptSkill(
        key=key,
        trigger=trigger,
        phases=phases or {Phase.PLAN, Phase.EXECUTE, Phase.SUBAGENT},
        title=key.upper(),
        summary=f"summary of {key}",
        body=f"BODY OF {key}",
        required=required,
    )


def _guard(
    skill_registry: PromptSkillRegistry,
    *,
    phase: Phase = Phase.EXECUTE,
    tools: set[str] | None = None,
    mcp_servers: set[str] | None = None,
    event_queue: Any = None,
) -> SkillGuard:
    return SkillGuard(
        registry=skill_registry,
        phase=phase,
        trigger_ctx=TriggerContext(
            tools=frozenset(tools or {"jira_create_issue", "search"}),
            mcp_servers=frozenset(mcp_servers or set()),
        ),
        event_queue=event_queue,
        agent_id="a1",
        agent_role="Engineer",
        turn_id="t1",
        iteration=1,
    )


# ---------------------------------------------------------------------------
# SkillGuard unit behaviour
# ---------------------------------------------------------------------------


async def test_guard_blocks_covered_tool_until_loaded() -> None:
    reg = PromptSkillRegistry()
    reg.seed([_skill("tool:jira", TriggerExpr(tool="jira_create_issue"))])
    guard = _guard(reg)
    tool = _tool("jira_create_issue")

    blocked = await guard.check_tool(tool)
    assert blocked is not None
    assert not blocked.success
    assert "tool:jira" in (blocked.error or "")
    assert "load_tool_skill" in (blocked.error or "")
    assert "jira_create_issue" in (blocked.error or "")

    guard.observe("load_tool_skill", {"key": "tool:jira"}, True)
    assert await guard.check_tool(tool) is None


async def test_guard_block_message_renders_skill_variables() -> None:
    """The block message lists skill summaries and goes straight to the
    LLM, so ${var} references in a summary must be substituted here too."""
    reg = PromptSkillRegistry()
    reg.seed(
        [
            PromptSkill(
                key="tool:jira",
                trigger=TriggerExpr(tool="jira_create_issue"),
                phases={Phase.EXECUTE},
                title="Jira",
                summary="link as ${jira_base_url}/browse/KEY",
                body="BODY",
            )
        ]
    )
    reg.set_variables({"jira_base_url": "https://acme.atlassian.net"})
    guard = _guard(reg)

    blocked = await guard.check_tool(_tool("jira_create_issue"))
    assert blocked is not None
    assert "link as https://acme.atlassian.net/browse/KEY" in (blocked.error or "")
    assert "${jira_base_url}" not in (blocked.error or "")


async def test_guard_ignores_tools_no_required_skill_covers() -> None:
    reg = PromptSkillRegistry()
    reg.seed([_skill("tool:jira", TriggerExpr(tool="jira_create_issue"))])
    guard = _guard(reg)
    assert await guard.check_tool(_tool("search")) is None


async def test_guard_ignores_advisory_skills() -> None:
    reg = PromptSkillRegistry()
    reg.seed(
        [_skill("tool:jira", TriggerExpr(tool="jira_create_issue"), required=False)]
    )
    guard = _guard(reg)
    assert await guard.check_tool(_tool("jira_create_issue")) is None


async def test_guard_respects_skill_phase_scoping() -> None:
    """A required skill declared for Plan only must not gate Execute."""
    reg = PromptSkillRegistry()
    reg.seed(
        [
            _skill(
                "tool:jira",
                TriggerExpr(tool="jira_create_issue"),
                phases={Phase.PLAN},
            )
        ]
    )
    guard = _guard(reg, phase=Phase.EXECUTE)
    assert await guard.check_tool(_tool("jira_create_issue")) is None


async def test_guard_blocks_mcp_server_covered_tool() -> None:
    """An ``mcp_server`` trigger covers every tool on that server."""
    reg = PromptSkillRegistry()
    reg.seed([_skill("mcp:github", TriggerExpr(mcp_server="github"))])
    guard = _guard(reg, mcp_servers={"github"})

    blocked = await guard.check_tool(_McpTool("create_pull_request", "github"))
    assert blocked is not None
    assert "mcp:github" in (blocked.error or "")

    # A builtin (no server) is not covered by the mcp trigger.
    assert await guard.check_tool(_tool("search")) is None

    guard.observe("load_tool_skill", {"key": "mcp:github"}, True)
    assert await guard.check_tool(_McpTool("create_pull_request", "github")) is None


async def test_guard_does_not_fire_when_full_trigger_unmatched() -> None:
    """An ``all_of`` skill gates only when the whole trigger matches the
    session surface — a role with just one of the two servers is not
    the audience for a cross-surface skill."""
    expr = TriggerExpr(
        all_of=[TriggerExpr(mcp_server="atlassian"), TriggerExpr(mcp_server="slack")]
    )
    reg = PromptSkillRegistry()
    reg.seed([_skill("skill:cross", expr)])

    jira_tool = _McpTool("jira_add_comment", "atlassian")
    one_server = _guard(reg, mcp_servers={"atlassian"})
    assert await one_server.check_tool(jira_tool) is None

    both = _guard(reg, mcp_servers={"atlassian", "slack"})
    assert await both.check_tool(jira_tool) is not None


async def test_guard_exempts_engine_plumbing_tools() -> None:
    """Even a trigger naming an exempt tool never blocks it — a
    misauthored skill must not be able to brick a phase."""
    reg = PromptSkillRegistry()
    reg.seed(
        [
            _skill(f"tool:{name}", TriggerExpr(tool=name))
            for name in sorted(GUARD_EXEMPT_TOOLS)
        ]
    )
    guard = _guard(reg, tools=set(GUARD_EXEMPT_TOOLS))
    for name in GUARD_EXEMPT_TOOLS:
        assert await guard.check_tool(_tool(name)) is None, name


async def test_guard_failed_load_does_not_unlock() -> None:
    reg = PromptSkillRegistry()
    reg.seed([_skill("tool:jira", TriggerExpr(tool="jira_create_issue"))])
    guard = _guard(reg)
    guard.observe("load_tool_skill", {"key": "tool:jira"}, False)
    assert await guard.check_tool(_tool("jira_create_issue")) is not None


async def test_guard_block_lists_every_missing_skill() -> None:
    """Two required skills covering one tool produce one block naming
    both keys (deterministic key order)."""
    reg = PromptSkillRegistry()
    reg.seed(
        [
            _skill("a:first", TriggerExpr(tool="jira_create_issue")),
            _skill("b:second", TriggerExpr(tool="jira_create_issue")),
        ]
    )
    guard = _guard(reg)
    blocked = await guard.check_tool(_tool("jira_create_issue"))
    assert blocked is not None
    err = blocked.error or ""
    assert "a:first" in err and "b:second" in err
    assert err.index("a:first") < err.index("b:second")

    # Loading only one keeps the other gating.
    guard.observe("load_tool_skill", {"key": "a:first"}, True)
    still = await guard.check_tool(_tool("jira_create_issue"))
    assert still is not None
    assert "b:second" in (still.error or "")
    assert "a:first" not in (still.error or "")


async def test_guard_publishes_blocked_event() -> None:
    reg = PromptSkillRegistry()
    reg.seed([_skill("tool:jira", TriggerExpr(tool="jira_create_issue"))])
    queue = _QueueStub()
    guard = _guard(reg, event_queue=queue)
    await guard.check_tool(_tool("jira_create_issue"))

    events = [e for _, e in queue.published if isinstance(e, ToolSkillGuardBlocked)]
    assert len(events) == 1
    event = events[0]
    assert event.tool_name == "jira_create_issue"
    assert event.skill_keys == ["tool:jira"]
    assert event.phase == "execute"
    assert event.turn_id == "t1"
    assert "tool:jira" in event.summary


async def test_guard_without_event_queue_still_blocks() -> None:
    reg = PromptSkillRegistry()
    reg.seed([_skill("tool:jira", TriggerExpr(tool="jira_create_issue"))])
    guard = _guard(reg, event_queue=None)
    assert await guard.check_tool(_tool("jira_create_issue")) is not None


# ---------------------------------------------------------------------------
# build_skill_guard arming rules
# ---------------------------------------------------------------------------


def _surface_with(*tools: SimpleTool) -> ToolSurface:
    registry = ToolRegistry()
    for tool in tools:
        registry.register(tool)
    return ToolSurface.for_execute(
        registry,
        [],
        tools_needed=[t.name for t in tools],
        always_on=[],
    )


def test_build_skill_guard_none_without_registry() -> None:
    surface = _surface_with(_load_skill_tool())
    assert (
        build_skill_guard(
            registry=None,
            phase=Phase.EXECUTE,
            surface=surface,
            mcp_servers=set(),
        )
        is None
    )


def test_build_skill_guard_refuses_without_loader_on_surface() -> None:
    """No ``load_tool_skill`` on the surface → the session could never
    satisfy a block; the guard must not arm."""
    surface = _surface_with(_tool("search"))
    assert (
        build_skill_guard(
            registry=PromptSkillRegistry(),
            phase=Phase.EXECUTE,
            surface=surface,
            mcp_servers=set(),
        )
        is None
    )


def test_build_skill_guard_derives_ctx_from_surface_catalogue() -> None:
    surface = _surface_with(_load_skill_tool(), _tool("search"))
    guard = build_skill_guard(
        registry=PromptSkillRegistry(),
        phase=Phase.EXECUTE,
        surface=surface,
        mcp_servers={"github"},
    )
    assert guard is not None
    assert "search" in guard.trigger_ctx.tools
    assert guard.trigger_ctx.mcp_servers == frozenset({"github"})


def test_skill_guard_for_turn_derives_identity_and_servers_from_turn() -> None:
    """The per-turn wrapper used by all three phase runners derives the
    role's MCP-server set and the agent / turn identity from the
    TurnContext, so the three call sites can't drift apart."""
    from crewlet.agent.skills import skill_guard_for_turn

    agent = _mk_agent()
    agent.definition.role.mcp_env = {"github": {}, "atlassian": {}}
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    turn.iteration = 2
    surface = _surface_with(_load_skill_tool(), _tool("search"))

    guard = skill_guard_for_turn(
        registry=PromptSkillRegistry(),
        phase=Phase.EXECUTE,
        surface=surface,
        turn=turn,
        event_queue=None,
    )
    assert guard is not None
    assert guard.trigger_ctx.mcp_servers == frozenset({"github", "atlassian"})
    assert guard.agent_id == agent.id_str
    assert guard.agent_role == agent.role_name
    assert guard.turn_id == turn.turn_id
    assert guard.iteration == 2

    # No registry → no guard, same as build_skill_guard.
    assert (
        skill_guard_for_turn(
            registry=None,
            phase=Phase.EXECUTE,
            surface=surface,
            turn=turn,
        )
        is None
    )


# ---------------------------------------------------------------------------
# execute_tool integration (the dispatch gate)
# ---------------------------------------------------------------------------


async def test_execute_tool_blocks_then_allows_after_load() -> None:
    skill_reg = PromptSkillRegistry()
    skill_reg.seed([_skill("tool:jira", TriggerExpr(tool="jira_create_issue"))])

    surface = _surface_with(_load_skill_tool(), _tool("jira_create_issue"))
    surface.skill_guard = build_skill_guard(
        registry=skill_reg,
        phase=Phase.EXECUTE,
        surface=surface,
        mcp_servers=set(),
    )
    ctx = AgentContext(prompt_skill_registry=skill_reg)

    _EXECUTED.clear()
    blocked = await execute_tool("jira_create_issue", {}, ctx, surface=surface)
    assert not blocked.success
    assert "tool:jira" in (blocked.error or "")
    assert _EXECUTED == [], "guarded tool must not run while blocked"

    loaded = await execute_tool(
        "load_tool_skill", {"key": "tool:jira"}, ctx, surface=surface
    )
    assert loaded.success
    assert "BODY OF tool:jira" in loaded.output

    allowed = await execute_tool("jira_create_issue", {}, ctx, surface=surface)
    assert allowed.success
    assert _EXECUTED == ["called"]


async def test_execute_tool_without_guard_is_unchanged() -> None:
    surface = _surface_with(_tool("jira_create_issue"))
    result = await execute_tool(
        "jira_create_issue", {}, AgentContext(), surface=surface
    )
    assert result.success


# ---------------------------------------------------------------------------
# run_tool_loop end-to-end: block → load → retry in one session
# ---------------------------------------------------------------------------


async def test_tool_loop_session_recovers_from_block() -> None:
    skill_reg = PromptSkillRegistry()
    skill_reg.seed([_skill("tool:jira", TriggerExpr(tool="jira_create_issue"))])

    surface = _surface_with(_load_skill_tool(), _tool("jira_create_issue"))
    surface.skill_guard = build_skill_guard(
        registry=skill_reg,
        phase=Phase.EXECUTE,
        surface=surface,
        mcp_servers=set(),
    )

    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="1", name="jira_create_issue", arguments={})],
            ),
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="2",
                        name="load_tool_skill",
                        arguments={"key": "tool:jira"},
                    )
                ],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="3", name="jira_create_issue", arguments={})],
            ),
            Completion(content="done", tool_calls=[]),
        ]
    )

    from crewlet.agent.definition import AgentDefinition
    from crewlet.agent.instance import AgentInstance
    from crewlet.org.models import Organization, Role

    role = Role(name="Engineer", handle="eng")
    org = Organization(name="Acme", roles=[role])
    agent = AgentInstance(
        definition=AgentDefinition(role=role, org=org),
        handle="eng",
        email="e@acme.com",
    )

    from crewlet.providers.llm.protocol import Message

    _EXECUTED.clear()
    loop = await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="go")],
        surface=surface,
        context=AgentContext(prompt_skill_registry=skill_reg),
        agent=agent,
        max_rounds=6,
        event_queue=_QueueStub(),
    )

    by_name = {(e["name"], e["success"]) for e in loop.tool_executions}
    assert ("jira_create_issue", False) in by_name  # the block
    assert ("load_tool_skill", True) in by_name  # the unlock
    assert ("jira_create_issue", True) in by_name  # the retry
    assert _EXECUTED == ["called"], "guarded tool ran exactly once, after the load"
    # The blocked round's tool message carries the instructive error.
    blocked_records = [
        e
        for e in loop.tool_executions
        if e["name"] == "jira_create_issue" and not e["success"]
    ]
    assert "load_tool_skill(key='tool:jira')" in blocked_records[0]["result"]


# ---------------------------------------------------------------------------
# Phase-runner wiring
# ---------------------------------------------------------------------------


def _phase_registry() -> ToolRegistry:
    registry = ToolRegistry()
    registry.register(_load_skill_tool())
    registry.register(_tool("jira_create_issue"))
    registry.register(_tool("search"))
    return registry


def _mk_agent():
    from crewlet.agent.definition import AgentDefinition
    from crewlet.agent.instance import AgentInstance
    from crewlet.org.models import Organization, OrgUnit, Role

    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    resolved = org.get_role("Engineer")
    assert resolved is not None
    defn = AgentDefinition(role=resolved, org=org)
    return AgentInstance(definition=defn, handle="eng", email="e@acme.com")


async def test_run_execute_phase_enforces_required_skill() -> None:
    """End-to-end through the Execute runner: the first call to the
    covered tool is blocked, the session loads the skill, the retry
    executes, and the block event is published."""
    skill_reg = PromptSkillRegistry()
    skill_reg.seed([_skill("tool:jira", TriggerExpr(tool="jira_create_issue"))])

    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="1", name="jira_create_issue", arguments={})],
            ),
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="2",
                        name="load_tool_skill",
                        arguments={"key": "tool:jira"},
                    )
                ],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="3", name="jira_create_issue", arguments={})],
            ),
            Completion(content="shipped", tool_calls=[]),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    queue = _QueueStub()

    _EXECUTED.clear()
    result = await run_execute_phase(
        turn=turn,
        plan=ExecutionPlan(tools_needed=["jira_create_issue"]),
        provider=provider,
        provider_key="exec",
        registry=_phase_registry(),
        role_mcp_tools=[],
        always_on=["load_tool_skill"],
        event_queue=queue,
        agent_context=AgentContext(prompt_skill_registry=skill_reg),
        prompt_skill_registry=skill_reg,
    )

    assert _EXECUTED == ["called"]
    statuses = [(e["name"], e["success"]) for e in result.tool_executions]
    assert ("jira_create_issue", False) in statuses
    assert ("load_tool_skill", True) in statuses
    assert ("jira_create_issue", True) in statuses
    blocked_events = [
        e for _, e in queue.published if isinstance(e, ToolSkillGuardBlocked)
    ]
    assert len(blocked_events) == 1
    assert blocked_events[0].phase == "execute"
    # The block is a surface-known rejection, not an unknown tool.
    assert "jira_create_issue" not in result.missing_tools


async def test_run_plan_phase_enforces_required_skill_for_activated_tool() -> None:
    """The planner activates a covered tool for in-Plan recon, calls it
    without loading the skill, gets blocked, loads, retries, then
    submits the plan."""
    skill_reg = PromptSkillRegistry()
    skill_reg.seed([_skill("tool:search", TriggerExpr(tool="search"))])

    submit = ToolCall(
        id="s",
        name="submit_plan",
        arguments={
            "reasoning": "r",
            "steps": [{"intent": "d"}],
            "tools_needed": ["search"],
            "success_criteria": ["done"],
        },
    )
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(id="a", name="activate_tool", arguments={"name": "search"})
                ],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="1", name="search", arguments={})],
            ),
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="2",
                        name="load_tool_skill",
                        arguments={"key": "tool:search"},
                    )
                ],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="3", name="search", arguments={})],
            ),
            Completion(content="", tool_calls=[submit]),
        ]
    )
    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    queue = _QueueStub()

    _EXECUTED.clear()
    plan = await run_plan_phase(
        turn=turn,
        provider=provider,
        provider_key="plan",
        registry=_phase_registry(),
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=AgentContext(prompt_skill_registry=skill_reg),
        prompt_skill_registry=skill_reg,
    )

    assert plan.tools_needed == ["search"]
    assert _EXECUTED == ["called"], "search ran once, only after the load"
    blocked_events = [
        e for _, e in queue.published if isinstance(e, ToolSkillGuardBlocked)
    ]
    assert len(blocked_events) == 1
    assert blocked_events[0].phase == "plan"
    assert blocked_events[0].tool_name == "search"


async def test_spawn_subagent_enforces_required_skill() -> None:
    """Sub-agents run on a fresh context: the guard arms with the
    always-included ``load_tool_skill`` and gates the granted tool."""
    skill_reg = PromptSkillRegistry()
    skill_reg.seed([_skill("tool:search", TriggerExpr(tool="search"))])

    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="1", name="search", arguments={})],
            ),
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="2",
                        name="load_tool_skill",
                        arguments={"key": "tool:search"},
                    )
                ],
            ),
            Completion(
                content="",
                tool_calls=[ToolCall(id="3", name="search", arguments={})],
            ),
            Completion(content="found it", tool_calls=[]),
        ]
    )
    agent = _mk_agent()
    parent_turn = TurnContext(
        agent=agent, org=agent.definition.org, task_description="x"
    )
    queue = _QueueStub()

    _EXECUTED.clear()
    result = await spawn_subagent(
        parent_turn=parent_turn,
        task_prompt="find things",
        system_prompt="you are a finder",
        tool_names=["search"],
        parent_tool_names=["search"],
        provider=provider,
        provider_key="sub",
        registry=_phase_registry(),
        role_mcp_tools=[],
        agent_context=AgentContext(prompt_skill_registry=skill_reg),
        event_queue=queue,
        prompt_skill_registry=skill_reg,
    )

    assert result.text == "found it"
    assert _EXECUTED == ["called"]
    blocked_events = [
        e for _, e in queue.published if isinstance(e, ToolSkillGuardBlocked)
    ]
    assert len(blocked_events) == 1
    assert blocked_events[0].phase == "subagent"
