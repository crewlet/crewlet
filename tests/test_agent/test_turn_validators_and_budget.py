"""Tests for turn-engine validator, budget, and prompt-size plumbing:

- Validator propagation from TurnEngine into each phase.
- ``subagent_budget_fraction`` caps sub-agent token consumption.
- ``prompt.size`` events are emitted per phase.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import pytest

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.subagent import (
    _FractionalBudgetManager,
    _subagent_budget_cap,
    spawn_subagent,
)
from crewlet.agent.turn import TurnEngine
from crewlet.agent.turn_context import TurnContext
from crewlet.concurrency import BudgetManager
from crewlet.events.types import PromptSize
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, ToolCall
from crewlet.tools.colleague import register_colleague_tools
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry

# ---------------------------------------------------------------------------
# Stubs
# ---------------------------------------------------------------------------


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


class _PhaseScriptedProvider:
    def __init__(
        self,
        *,
        plan: list[Completion],
        execute: list[Completion],
        review: list[Completion],
        model: str = "phased",
    ) -> None:
        self._plan = list(plan)
        self._execute = list(execute)
        self._review = list(review)
        self.model = model

    async def complete(self, messages, tools=None, **_):
        sys_text = next((m.content for m in messages if m.role == "system"), "")
        if "PLAN phase" in sys_text:
            return self._plan.pop(0) if self._plan else Completion(content="plan done")
        if "EXECUTE phase" in sys_text:
            return (
                self._execute.pop(0)
                if self._execute
                else Completion(content="exec done")
            )
        if "REVIEW phase" in sys_text:
            return (
                self._review.pop(0)
                if self._review
                else Completion(content="review done")
            )
        return Completion(content="?", tool_calls=[])


async def _noop(params, ctx):
    return ToolResult(success=True, output="noop")


def _mk_agent(handle: str = "alice") -> AgentInstance:
    role = Role(name="Engineer", handle=handle)
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    agent = AgentInstance(definition=defn, handle=handle, email=f"{handle}@acme.com")
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
            name="query_knowledge",
            description="q",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    register_colleague_tools(r)
    return r


def _plan_submission(tools_needed=None, decision="plan") -> list[Completion]:
    return [
        Completion(
            content="",
            tool_calls=[
                ToolCall(
                    id="cp",
                    name="submit_plan",
                    arguments={
                        "decision": decision,
                        "reasoning": "x",
                        "steps": [{"intent": "s"}] if decision == "plan" else [],
                        "tools_needed": tools_needed or ["search"],
                    },
                )
            ],
        )
    ]


def _review_submission(decision: str, **kwargs) -> list[Completion]:
    args = {"decision": decision, "notes": ""}
    args.update(kwargs)
    return [
        Completion(
            content="",
            tool_calls=[ToolCall(id="cr", name="submit_review", arguments=args)],
        )
    ]


# ---------------------------------------------------------------------------
# Validator propagation
# ---------------------------------------------------------------------------


async def test_validator_from_turn_engine_runs_against_tool_results():
    """A validator registered on TurnEngine must be called on tool
    outputs in every phase (here: Execute)."""
    captured: list[str] = []

    class _V:
        name = "recorder"

        def validate(self, output: str) -> str:
            captured.append(output)
            return output

    agent = _mk_agent()
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={
            "default": _PhaseScriptedProvider(
                plan=_plan_submission(["search"]),
                execute=[
                    Completion(
                        content="",
                        tool_calls=[
                            ToolCall(id="c", name="search", arguments={"q": "x"})
                        ],
                    ),
                    Completion(content="done", tool_calls=[]),
                ],
                review=_review_submission("done", final_artifact="a"),
            )
        },
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    engine.register_result_validator(_V())
    await engine.run_turn(agent, task_description="t", org=agent.definition.org)
    # "noop" is the output of _noop -> the validator saw it via Execute.
    assert any(out == "noop" for out in captured)


# ---------------------------------------------------------------------------
# Sub-agent budget fraction
# ---------------------------------------------------------------------------


async def test_subagent_budget_cap_uses_fraction_of_parent_remaining():
    bm = BudgetManager(org_budget=0)
    bm.set_agent_budget("a1", 1000)
    # After consuming 200, remaining is 800; 20% fraction -> 160.
    await bm.consume("a1", 200)
    cap = _subagent_budget_cap(budget_manager=bm, parent_agent_id="a1", fraction=0.2)
    assert cap == 160


def test_subagent_budget_cap_returns_zero_when_no_agent_budget():
    bm = BudgetManager(org_budget=0)
    cap = _subagent_budget_cap(budget_manager=bm, parent_agent_id="a1", fraction=0.2)
    assert cap == 0


async def test_fractional_budget_raises_once_cap_reached():
    from crewlet.agent.subagent import SubagentBudgetExceeded

    bm = BudgetManager(org_budget=10_000)
    bm.set_agent_budget("a1", 10_000)
    wrapped = _FractionalBudgetManager(bm, max_subagent_tokens=500)
    assert await wrapped.consume("a1", 300) is True
    # 300 + 300 > 500 cap → raise
    with pytest.raises(SubagentBudgetExceeded):
        await wrapped.consume("a1", 300)
    # Parent budget also wasn't charged the rejected slice.
    assert bm.get_agent_budget("a1").used_tokens == 300


async def test_spawn_subagent_enforces_budget_cap():
    """A sub-agent hitting the budget cap fails its turn cleanly."""
    bm = BudgetManager(org_budget=0)
    bm.set_agent_budget("id-alice", 1000)
    # Pre-consume so the remaining budget is 100; fraction 0.5 -> cap 50.
    await bm.consume("id-alice", 900)

    class _BigProvider:
        model = "big"

        async def complete(self, messages, tools=None, **_):
            # Single completion reporting more tokens than the cap.
            return Completion(
                content="done",
                tool_calls=[],
                input_tokens=60,
                output_tokens=10,
                tokens_used=70,
            )

    registry = _mk_registry()
    agent = _mk_agent()
    agent.id = __import__("uuid").UUID("00000000-0000-0000-0000-000000000001")
    # Match the id_str used by BudgetManager above.
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = AgentContext(
        agent_id=agent.id_str,
        agent_handle=agent.handle,
        role=agent.role_name,
        event_queue=_QueueStub(),
    )

    # Point the budget cap entry at the agent's real id_str.
    bm._agent_budgets[agent.id_str] = bm._agent_budgets.pop("id-alice")

    # Sub-agent will try to consume 70 tokens; cap is 50 -> cap breach.
    result = await spawn_subagent(
        parent_turn=turn,
        task_prompt="x",
        system_prompt="y",
        tool_names=[],
        parent_tool_names=[],
        provider=_BigProvider(),
        provider_key="big",
        registry=registry,
        role_mcp_tools=[],
        agent_context=ctx,
        event_queue=ctx.event_queue,
        budget_manager=bm,
        budget_fraction=0.5,
    )
    # Either the loop stopped on budget, or no tokens made it through the cap.
    assert result.tokens_used <= 50


# ---------------------------------------------------------------------------
# prompt.size emission
# ---------------------------------------------------------------------------


async def test_prompt_size_event_published_for_each_phase():
    agent = _mk_agent()
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={
            "default": _PhaseScriptedProvider(
                plan=_plan_submission(),
                execute=[Completion(content="done")],
                review=_review_submission("done", final_artifact="r"),
            )
        },
        tool_registry=_mk_registry(),
        event_queue=queue,
    )
    await engine.run_turn(agent, task_description="x", org=agent.definition.org)
    prompts = [e for _, e in queue.published if isinstance(e, PromptSize)]
    phases = {p.phase for p in prompts}
    # Three phase prompts (plan/execute/review) should each emit one event.
    assert "plan" in phases
    assert "execute" in phases
    assert "review" in phases
    for p in prompts:
        assert p.approximate_tokens > 0
        assert p.system_chars > 0
