"""The scheduled-task wall-clock cap is enforced by the turn engine."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from typing import Any

import pytest

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.turn import TurnEngine
from crewlet.engine import _scheduled_deadline
from crewlet.events.types import TaskAssigned, TurnGuardBreach
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.tools.registry import ToolRegistry


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))

    async def subscribe(self, topic, group, handler):  # pragma: no cover - unused
        pass


def _mk_agent() -> AgentInstance:
    role = Role(name="Engineer", handle="eng")
    org = Organization(
        name="Acme",
        units=[OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])],
    )
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    agent = AgentInstance(definition=defn, handle="eng", email="e@acme.com")
    agent.activate()
    return agent


# --- the helper ------------------------------------------------------------


def test_scheduled_deadline_helper():
    assert (
        _scheduled_deadline(
            TaskAssigned(payload={"scheduled": True, "timeout_seconds": 90})
        )
        == 90.0
    )
    # Not a scheduled task → no cap.
    assert _scheduled_deadline(TaskAssigned(payload={})) is None
    # Scheduled but no/zero/garbage timeout → no cap.
    assert _scheduled_deadline(TaskAssigned(payload={"scheduled": True})) is None
    assert (
        _scheduled_deadline(
            TaskAssigned(payload={"scheduled": True, "timeout_seconds": 0})
        )
        is None
    )
    assert (
        _scheduled_deadline(
            TaskAssigned(payload={"scheduled": True, "timeout_seconds": "bad"})
        )
        is None
    )


# --- the turn-engine cap ---------------------------------------------------


async def test_deadline_terminates_turn_as_failed():
    agent = _mk_agent()
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": object()},
        tool_registry=ToolRegistry(),
        event_queue=queue,
    )

    async def _slow(_turn, _typing=None):
        await asyncio.sleep(5)
        return "should not be reached", "done"

    engine._drive_phases = _slow  # type: ignore[assignment]

    result = await engine.run_turn(
        agent,
        task_id="t1",
        task_description="long running scheduled task",
        org=agent.definition.org,
        deadline_seconds=0.05,
    )

    assert "timed out" in result
    types = [e.type for _, e in queue.published]
    assert "task_started" in types
    assert "task_failed" in types
    assert "task_completed" not in types  # never completed
    breaches = [e for _, e in queue.published if isinstance(e, TurnGuardBreach)]
    assert any(b.kind == "scheduled_timeout" for b in breaches)
    # Agent was released cleanly and can start a fresh turn.
    assert agent.start_working("t2") is True


async def test_inner_timeout_not_misattributed_as_scheduled_cap():
    """A TimeoutError raised by inner tool/IO code on a scheduled turn must
    NOT be blamed on the wall-clock cap; it follows the normal failure path."""
    agent = _mk_agent()
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": object()},
        tool_registry=ToolRegistry(),
        event_queue=queue,
    )

    async def _inner_timeout(_turn, _typing=None):
        raise TimeoutError("inner tool timed out")  # far under the 10s cap

    engine._drive_phases = _inner_timeout  # type: ignore[assignment]

    with pytest.raises(TimeoutError):
        await engine.run_turn(
            agent,
            task_id="t1",
            task_description="x",
            org=agent.definition.org,
            deadline_seconds=10,
        )

    breaches = [e for _, e in queue.published if isinstance(e, TurnGuardBreach)]
    assert not any(b.kind == "scheduled_timeout" for b in breaches)
    assert any(b.kind == "unhandled_exception" for b in breaches)
    fails = [e for _, e in queue.published if e.type == "task_failed"]
    assert fails and all("exceeded" not in (e.error or "") for e in fails)


async def test_timeout_publish_failure_does_not_escalate():
    """If publishing the timeout failure raises, it is swallowed — not
    escalated into the outer handler (which would double-publish + re-raise,
    spuriously NAK-ing the scheduled task)."""
    agent = _mk_agent()

    class _FailOnTaskFailed:
        def __init__(self) -> None:
            self.published: list[tuple[str, Any]] = []

        async def publish(self, topic: str, event: Any) -> None:
            if getattr(event, "type", "") == "task_failed":
                raise RuntimeError("publish boom")
            self.published.append((topic, event))

        async def subscribe(self, *a, **k):  # pragma: no cover - unused
            pass

    engine = TurnEngine(
        llm_providers={"default": object()},
        tool_registry=ToolRegistry(),
        event_queue=_FailOnTaskFailed(),
    )

    async def _slow(_turn, _typing=None):
        await asyncio.sleep(5)
        return "nope", "done"

    engine._drive_phases = _slow  # type: ignore[assignment]

    result = await engine.run_turn(
        agent,
        task_id="t1",
        task_description="x",
        org=agent.definition.org,
        deadline_seconds=0.05,
    )
    assert "timed out" in result
    assert agent.start_working("t2") is True  # released cleanly despite the failure


async def test_no_deadline_runs_to_completion():
    agent = _mk_agent()
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": object()},
        tool_registry=ToolRegistry(),
        event_queue=queue,
    )

    async def _fast(_turn, _typing=None):
        return "done text", "done"

    engine._drive_phases = _fast  # type: ignore[assignment]

    result = await engine.run_turn(
        agent,
        task_id="t1",
        task_description="quick task",
        org=agent.definition.org,
    )
    assert result == "done text"
    types = [e.type for _, e in queue.published]
    assert "task_completed" in types
    assert "task_failed" not in types
