"""Tests for turn-engine guard-breach events and Plan decision handling:

- ``TurnGuardBreach`` events emitted for stall / depth-cap breaches so
  they appear in the dashboard events table.
- Plan's ``decision`` field is acted on: ``skip`` returns immediately,
  ``direct`` widens the Execute surface to the full registry.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.turn import TurnEngine
from crewlet.events.types import Event, TurnGuardBreach
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, ToolCall
from crewlet.tools.colleague import register_colleague_tools
from crewlet.tools.protocol import ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


async def _noop(params, ctx):
    return ToolResult(success=True, output="noop")


def _mk_agent() -> AgentInstance:
    role = Role(name="Engineer", handle="alice")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    agent = AgentInstance(definition=defn, handle="alice", email="a@acme.com")
    agent.activate()
    return agent


def _mk_registry() -> ToolRegistry:
    r = ToolRegistry()
    r.register(
        SimpleTool(
            name="search",
            description="s",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    r.register(
        SimpleTool(
            name="load_tool_skill",
            description="q",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    register_colleague_tools(r)
    return r


class _PhaseScriptedProvider:
    model = "s"

    def __init__(self, *, plan, execute, review):
        self._plan = list(plan)
        self._execute = list(execute)
        self._review = list(review)

    async def complete(self, messages, tools=None, **_):
        sys_text = next((m.content for m in messages if m.role == "system"), "")
        if "PLAN phase" in sys_text:
            return self._plan.pop(0) if self._plan else Completion(content="p")
        if "EXECUTE phase" in sys_text:
            return self._execute.pop(0) if self._execute else Completion(content="e")
        if "REVIEW phase" in sys_text:
            return self._review.pop(0) if self._review else Completion(content="r")
        return Completion(content="?")


def _plan(
    decision: str = "plan",
    *,
    tools_needed=None,
    reasoning: str = "",
) -> list[Completion]:
    return [
        Completion(
            content="",
            tool_calls=[
                ToolCall(
                    id="cp",
                    name="submit_plan",
                    arguments={
                        "decision": decision,
                        "reasoning": reasoning or f"decision={decision}",
                        "steps": [{"intent": "x"}] if decision == "plan" else [],
                        "tools_needed": tools_needed or [],
                        "success_criteria": [],
                    },
                )
            ],
        )
    ]


def _review(decision: str, **kwargs) -> list[Completion]:
    args = {"decision": decision, "notes": ""}
    args.update(kwargs)
    return [
        Completion(
            content="",
            tool_calls=[ToolCall(id="cr", name="submit_review", arguments=args)],
        )
    ]


# ---------------------------------------------------------------------------
# TurnGuardBreach events
# ---------------------------------------------------------------------------


async def test_guard_breach_event_on_depth_cap():
    agent = _mk_agent()
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={
            "default": _PhaseScriptedProvider(plan=[], execute=[], review=[])
        },
        tool_registry=_mk_registry(),
        event_queue=queue,
        delegation_depth_limit=2,
    )
    event = Event(
        type="task_assigned",
        source="x",
        payload={"task_id": "t", "task_description": "x"},
        delegation_depth=2,
    )
    await engine.run_turn(agent, event=event, org=agent.definition.org)
    breaches = [e for _, e in queue.published if isinstance(e, TurnGuardBreach)]
    assert any(b.kind == "depth_cap" for b in breaches)


async def test_guard_breach_event_on_stall():
    """Two self_iterate rounds with the same artifact -> stall breach."""
    agent = _mk_agent()
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={
            "default": _PhaseScriptedProvider(
                plan=_plan() + _plan() + _plan(),
                execute=[
                    Completion(content="same"),
                    Completion(content="same"),
                    Completion(content="same"),
                ],
                review=(
                    _review("self_iterate")
                    + _review("self_iterate")
                    + _review("self_iterate")
                ),
            )
        },
        tool_registry=_mk_registry(),
        event_queue=queue,
        max_iterations=5,
    )
    await engine.run_turn(agent, task_description="x", org=agent.definition.org)
    breaches = [e for _, e in queue.published if isinstance(e, TurnGuardBreach)]
    assert any(b.kind == "stall" for b in breaches)


# ---------------------------------------------------------------------------
# Plan decision: skip / direct
# ---------------------------------------------------------------------------


async def test_plan_decision_skip_returns_immediately():
    agent = _mk_agent()
    queue = _QueueStub()
    provider = _PhaseScriptedProvider(
        plan=_plan("skip", reasoning="no action needed here"),
        execute=[],  # must NOT be consumed
        review=[],
    )
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    result = await engine.run_turn(
        agent, task_description="trivial", org=agent.definition.org
    )
    assert "no action" in result.lower()
    # Execute provider was never asked for a phase completion.
    assert provider._execute == []
    assert provider._review == []


async def test_plan_decision_direct_empty_tools_needed_does_not_widen():
    """With decision=direct and empty tools_needed, Execute gets only
    the always-on tools plus the discovery meta-tools — NOT the full
    registry's schemas.

    An empty tools_needed is almost always a planner mistake (the
    planner forgot to name the action tool). Silently widening to
    the full catalogue would turn that mistake into 30-60k tokens of
    schema bloat. The discovery meta-tools
    (``activate_tool``, ``list_mcp_server_tools``) are tiny (their
    schemas are a few hundred chars each), and they exist precisely
    to recover from this case: the executor can drill into a server
    and activate the right tool without seeing every schema upfront.

    ``decision=direct`` also implicitly skips Review, so Execute's
    output is the final artifact verbatim.
    """
    agent = _mk_agent()
    queue = _QueueStub()
    captured_tool_names: list[str] = []

    class _Recorder:
        model = "rec"

        async def complete(self, messages, tools=None, **_):
            sys_text = next((m.content for m in messages if m.role == "system"), "")
            if "EXECUTE phase" in sys_text:
                for td in tools or []:
                    captured_tool_names.append(td.name)
                return Completion(content="done-directly", tool_calls=[])
            if "PLAN phase" in sys_text:
                return _plan("direct", tools_needed=[])[0]
            return Completion(content="?")

    engine = TurnEngine(
        llm_providers={"default": _Recorder()},
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    result = await engine.run_turn(
        agent, task_description="trivial", org=agent.definition.org
    )
    # With Review skipped, the Execute artifact is the final result.
    assert result == "done-directly"
    # The Execute surface contains the always-on tool plus the
    # discovery meta-tools — but NO domain tool schemas
    # from the full registry. A malformed plan gets a minimal
    # surface; the executor can recover via discovery if needed.
    assert set(captured_tool_names) == {
        "load_tool_skill",
        "activate_tool",
        "list_mcp_server_tools",
    }
