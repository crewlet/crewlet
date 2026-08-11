"""Tests for the :class:`ReflectEngine` dispatcher."""

from __future__ import annotations

from typing import Any

from crewlet.events.types import TurnCompleted
from crewlet.learning.reflect_engine import (
    CONSUMER_GROUP,
    TURN_COMPLETED_TOPIC,
    ReflectEngine,
)
from crewlet.org.models import Organization, OrgUnit, Role


class _QueueStub:
    def __init__(self) -> None:
        self.subscriptions: list[tuple[str, str]] = []
        self._handlers: list[tuple[str, Any]] = []
        self.published: list[tuple[str, Any]] = []

    async def subscribe(self, topic: str, group: str, handler) -> None:
        self.subscriptions.append((topic, group))
        self._handlers.append((topic, handler))

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))

    async def deliver(self, event: Any) -> None:
        for topic, handler in self._handlers:
            if topic == TURN_COMPLETED_TOPIC:
                await handler(event)


class _RecordingDecider:
    def __init__(self, doc_id: str | None = "doc-1") -> None:
        self.calls: list[dict[str, Any]] = []
        self._doc_id = doc_id

    async def decide_and_persist(self, **kwargs) -> str | None:
        self.calls.append(kwargs)
        return self._doc_id


def _mk_org() -> Organization:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    return Organization(name="Acme", units=[unit])


def _mk_event(
    *,
    handle: str = "eng",
    role: str = "Engineer",
    tool_sequence=None,
    plan_tool_sequence=None,
    outcome="done",
) -> TurnCompleted:
    return TurnCompleted(
        source=role,
        agent_handle=handle,
        role=role,
        turn_id="t1",
        task_id="task-1",
        task_summary="Do X",
        plan_summary="Plan: do X",
        tool_sequence=tool_sequence or ["search"],
        plan_tool_sequence=plan_tool_sequence or [],
        skills_used=[],
        review_outcome=outcome,
        iterations=1,
    )


async def test_start_subscribes_to_turn_completed_topic() -> None:
    queue = _QueueStub()
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
    )
    await engine.start()
    assert (TURN_COMPLETED_TOPIC, CONSUMER_GROUP) in queue.subscriptions


async def test_dispatches_to_persist_decider() -> None:
    queue = _QueueStub()
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
    )
    await engine.start()
    decider = _RecordingDecider()
    engine._persist_decider = decider  # type: ignore[assignment]

    await queue.deliver(_mk_event())

    assert len(decider.calls) == 1
    call = decider.calls[0]
    assert call["agent_handle"] == "eng"
    assert call["unit_id"] == "Eng"
    assert call["org_id"] == "Acme"
    assert call["task_summary"] == "Do X"
    assert call["review_outcome"] == "done"


async def test_skips_when_turn_already_called_reflect_and_persist() -> None:
    """``reflect_and_persist`` is a Plan-phase-only builtin, so a turn
    that self-persisted carries it in ``plan_tool_sequence`` -- never in
    the Execute-scoped ``tool_sequence``.  The dedup guard must read the
    Plan trail or PersistDecider double-writes the same fact."""
    queue = _QueueStub()
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
    )
    await engine.start()
    decider = _RecordingDecider()
    engine._persist_decider = decider  # type: ignore[assignment]

    await queue.deliver(
        _mk_event(
            tool_sequence=["search"],
            plan_tool_sequence=["query_episodes", "reflect_and_persist"],
        )
    )

    assert decider.calls == []


async def test_runs_decider_when_plan_did_not_self_persist() -> None:
    """The mirror of the dedup test: a turn whose Plan phase did *not*
    call ``reflect_and_persist`` still gets the post-turn decider."""
    queue = _QueueStub()
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
    )
    await engine.start()
    decider = _RecordingDecider()
    engine._persist_decider = decider  # type: ignore[assignment]

    await queue.deliver(
        _mk_event(
            tool_sequence=["search"],
            plan_tool_sequence=["query_episodes"],
        )
    )

    assert len(decider.calls) == 1


async def test_skips_when_role_not_found_in_org() -> None:
    queue = _QueueStub()
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
    )
    await engine.start()
    decider = _RecordingDecider()
    engine._persist_decider = decider  # type: ignore[assignment]

    await queue.deliver(_mk_event(role="UnknownRole"))

    assert decider.calls == []


async def test_decider_failure_does_not_raise_out_of_handler() -> None:
    queue = _QueueStub()
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
    )
    await engine.start()

    class _Boom:
        async def decide_and_persist(self, **kwargs):
            raise RuntimeError("decider broke")

    engine._persist_decider = _Boom()  # type: ignore[assignment]

    # Must not raise.
    await queue.deliver(_mk_event())


async def test_disabled_persist_decider_means_no_dispatch() -> None:
    queue = _QueueStub()
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
        persist_decider_enabled=False,
    )
    await engine.start()
    # No decider to set — must be a no-op when the event arrives.
    await queue.deliver(_mk_event())


async def test_emits_reflection_completed_after_dispatch() -> None:
    """ReflectEngine fires a ``ReflectionCompleted`` event at the end of
    every real reflection pass — the dashboard's state-derivation uses
    it as the sentinel that flips the agent back to ``idle`` after any
    aux phase events the workers produced."""
    queue = _QueueStub()
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
    )
    await engine.start()
    decider = _RecordingDecider()
    engine._persist_decider = decider  # type: ignore[assignment]

    await queue.deliver(_mk_event())

    types_published = [event.type for _, event in queue.published]
    assert "reflection_completed" in types_published
    completion = next(e for _, e in queue.published if e.type == "reflection_completed")
    assert completion.agent_handle == "eng"
    assert completion.role == "Engineer"
    assert completion.turn_id == "t1"
    assert completion.workers_run >= 1


async def test_no_reflection_completed_when_early_returned() -> None:
    """Early-return gates (no role, role disabled, duplicate, no
    workers) skip the worker dispatch entirely — no aux phase events
    fire, so no ``ReflectionCompleted`` sentinel is needed."""
    queue = _QueueStub()
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
    )
    await engine.start()

    # Role unknown — _resolve_role returns None and _reflect bails out
    # before reaching the worker dispatch block.
    await queue.deliver(_mk_event(role="UnknownRole"))

    types_published = [event.type for _, event in queue.published]
    assert "reflection_completed" not in types_published


async def test_stop_gates_subsequent_handler_calls() -> None:
    queue = _QueueStub()
    engine = ReflectEngine(
        event_queue=queue,  # type: ignore[arg-type]
        llm_providers={"default": object()},  # type: ignore[dict-item]
        organization=_mk_org(),
    )
    await engine.start()
    decider = _RecordingDecider()
    engine._persist_decider = decider  # type: ignore[assignment]

    await engine.stop()
    await queue.deliver(_mk_event())
    assert decider.calls == []
