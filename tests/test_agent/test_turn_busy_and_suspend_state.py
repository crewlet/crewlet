"""Turn-engine agent-state lifecycle: busy turns WAIT (never drop events),
and a detached-suspend turn parks its OWN agent in ``AWAITING_SANDBOX``.

Raising ``RuntimeError`` for a busy agent would make the queue NAK the
triggering event toward the dead-letter topic (Pulsar: 3 fast
redeliveries; memory: 3 immediate retries then drop) while a live turn
lasts minutes — events arriving mid-turn would be effectively lost.  And
the busy transition for a sandbox suspend is owned by the suspending
turn: applying it asynchronously from the coordinator could hijack an
unrelated turn's WORKING state.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from typing import Any

import pytest

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.execute import ExecuteResult
from crewlet.agent.instance import AgentInstance, AgentState
from crewlet.agent.turn import TurnEngine
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, ToolCall
from crewlet.tools.registry import ToolRegistry


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


class _SkipPlanner:
    """LLM stub that immediately submits a skip plan (shortest legal turn)."""

    model = "stub"

    def __init__(self) -> None:
        self.calls = 0

    async def complete(
        self, messages, tools=None, temperature=0.7, max_tokens=None, tool_choice=None
    ):
        self.calls += 1
        return Completion(
            content="",
            tool_calls=[
                ToolCall(
                    id="p",
                    name="submit_plan",
                    arguments={"decision": "skip", "reasoning": "nothing to do"},
                )
            ],
        )


def _mk_agent() -> AgentInstance:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    agent = AgentInstance(definition=defn, handle="eng", email="e@acme.com")
    agent.activate()
    return agent


async def test_run_turn_waits_for_busy_agent_instead_of_raising() -> None:
    agent = _mk_agent()
    org = agent.definition.org
    provider = _SkipPlanner()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=ToolRegistry(),
        event_queue=_QueueStub(),
    )

    # Simulate an in-flight turn holding the agent.
    assert agent.start_working("earlier-turn")

    task = asyncio.create_task(
        engine.run_turn(agent, task_id="t2", task_description="hi", org=org)
    )
    # Give the parked turn a few ticks: it must WAIT, not raise, and must
    # not have started any LLM work while the agent is busy.
    for _ in range(5):
        await asyncio.sleep(0)
    assert not task.done()
    assert provider.calls == 0

    # The earlier turn finishes → the parked turn takes the slot and runs.
    agent.finish_working()
    await asyncio.wait_for(task, timeout=5)
    assert provider.calls >= 1
    assert agent.state == AgentState.IDLE


async def test_run_turn_still_fails_fast_for_terminated_agent() -> None:
    agent = _mk_agent()
    org = agent.definition.org
    agent.terminate()
    engine = TurnEngine(
        llm_providers={"default": _SkipPlanner()},
        tool_registry=ToolRegistry(),
        event_queue=_QueueStub(),
    )
    with pytest.raises(RuntimeError, match="cannot start working"):
        await engine.run_turn(agent, task_id="t", task_description="x", org=org)


async def test_waiting_turn_aborts_on_shutdown() -> None:
    from crewlet.agent.turn import ShutdownDraining

    agent = _mk_agent()
    org = agent.definition.org
    engine = TurnEngine(
        llm_providers={"default": _SkipPlanner()},
        tool_registry=ToolRegistry(),
        event_queue=_QueueStub(),
    )
    assert agent.start_working("earlier-turn")
    task = asyncio.create_task(
        engine.run_turn(agent, task_id="t2", task_description="hi", org=org)
    )
    await asyncio.sleep(0)
    assert not task.done()

    engine.begin_shutdown()
    with pytest.raises(ShutdownDraining):
        await asyncio.wait_for(task, timeout=5)


async def test_detached_turn_parks_its_own_agent(monkeypatch) -> None:
    # A turn whose Execute suspended for a detached sandbox run transitions
    # its OWN agent WORKING → AWAITING_SANDBOX in its finally — the state
    # never passes through IDLE, so no queued event can slip a turn in
    # between the suspend and the coordinator's (async) busy handling.
    agent = _mk_agent()
    org = agent.definition.org
    engine = TurnEngine(
        llm_providers={"default": _SkipPlanner()},
        tool_registry=ToolRegistry(),
        event_queue=_QueueStub(),
    )

    async def _detached_drive(turn, _typing=None):
        turn.last_execute_result = ExecuteResult(
            status="detached", sandbox_id="sbx-1", text=""
        )
        return "(suspended)", "done"

    monkeypatch.setattr(engine, "_drive_phases", _detached_drive)
    await engine.run_turn(agent, task_id="t", task_description="x", org=org)
    assert agent.state == AgentState.AWAITING_SANDBOX
    assert agent.current_task_id == "t"
