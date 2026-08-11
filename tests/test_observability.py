"""Tests for observability system."""

import pytest
import pytest_asyncio

from crewlet.events.types import (
    AgentSpawned,
    AgentTurnCompleted,
    AgentTurnProgress,
    MessageSent,
    TaskCompleted,
    TaskCreated,
    TaskFailed,
)
from crewlet.observability import ObservabilityManager
from crewlet.queue.memory import MemoryEventQueue


@pytest_asyncio.fixture
async def bus():
    q = MemoryEventQueue()
    await q.start()
    yield q
    await q.stop()


@pytest.mark.asyncio
async def test_tracks_events_processed(bus: MemoryEventQueue):
    obs = ObservabilityManager(bus)
    await obs.start()

    await bus.publish(
        "crewlet.events.task_created", TaskCreated(task_id="t1", title="A", source="e")
    )
    await bus.publish(
        "crewlet.events.task_created", TaskCreated(task_id="t2", title="B", source="e")
    )
    assert obs.engine_metrics.events_processed == 2


@pytest.mark.asyncio
async def test_tracks_task_lifecycle(bus: MemoryEventQueue):
    obs = ObservabilityManager(bus)
    await obs.start()

    await bus.publish(
        "crewlet.events.task_created", TaskCreated(task_id="t1", title="A", source="e")
    )
    await bus.publish(
        "crewlet.events.task_completed",
        TaskCompleted(task_id="t1", agent_id="a1", source="e"),
    )
    await bus.publish(
        "crewlet.events.task_failed",
        TaskFailed(task_id="t2", agent_id="a2", source="e"),
    )

    assert obs.engine_metrics.tasks_created == 1
    assert obs.engine_metrics.tasks_completed == 1
    assert obs.engine_metrics.tasks_failed == 1


@pytest.mark.asyncio
async def test_tracks_agent_metrics(bus: MemoryEventQueue):
    obs = ObservabilityManager(bus)
    await obs.start()

    await bus.publish(
        "crewlet.events.agent_spawned",
        AgentSpawned(agent_id="a1", role="Engineer", source="pool"),
    )
    await bus.publish(
        "crewlet.events.task_completed",
        TaskCompleted(task_id="t1", agent_id="a1", source="e"),
    )
    await bus.publish(
        "crewlet.events.task_failed",
        TaskFailed(task_id="t2", agent_id="a1", source="e"),
    )

    assert "a1" in obs.agent_metrics
    assert obs.agent_metrics["a1"].role == "Engineer"
    assert obs.agent_metrics["a1"].turns_completed == 1
    assert obs.agent_metrics["a1"].turns_failed == 1


@pytest.mark.asyncio
async def test_tracks_messages(bus: MemoryEventQueue):
    obs = ObservabilityManager(bus)
    await obs.start()

    await bus.publish(
        "crewlet.events.message_sent",
        MessageSent(channel="ch", sender="a1", content="Hi"),
    )
    assert obs.engine_metrics.messages_sent == 1


@pytest.mark.asyncio
async def test_token_tracking_with_input_output(bus: MemoryEventQueue):
    obs = ObservabilityManager(bus)
    await obs.start()

    await bus.publish(
        "crewlet.events.agent_spawned",
        AgentSpawned(agent_id="a1", role="Engineer", source="pool"),
    )

    obs.track_tokens("a1", input_tokens=300, output_tokens=200)
    obs.track_tokens("a1", input_tokens=100, output_tokens=200)

    assert obs.engine_metrics.total_tokens.total_tokens == 800
    assert obs.engine_metrics.total_tokens.input_tokens == 400
    assert obs.engine_metrics.total_tokens.output_tokens == 400
    assert obs.engine_metrics.total_tokens.call_count == 2
    assert obs.agent_metrics["a1"].tokens.total_tokens == 800
    assert obs.agent_metrics["a1"].tokens.input_tokens == 400


@pytest.mark.asyncio
async def test_async_hooks(bus: MemoryEventQueue):
    obs = ObservabilityManager(bus)
    await obs.start()

    captured = []

    async def on_complete(**kwargs):
        captured.append(kwargs["event"])

    obs.register_hook("on_task_complete", on_complete)

    await bus.publish(
        "crewlet.events.task_completed",
        TaskCompleted(task_id="t1", agent_id="a1", source="e"),
    )
    assert len(captured) == 1


@pytest.mark.asyncio
async def test_sync_hooks(bus: MemoryEventQueue):
    """Sync hook callbacks should work without raising TypeError."""
    obs = ObservabilityManager(bus)
    await obs.start()

    captured = []

    def on_complete_sync(**kwargs):
        captured.append(kwargs["event"])

    obs.register_hook("on_task_complete", on_complete_sync)

    await bus.publish(
        "crewlet.events.task_completed",
        TaskCompleted(task_id="t1", agent_id="a1", source="e"),
    )
    assert len(captured) == 1


@pytest.mark.asyncio
async def test_llm_history_keyed_by_turn_coordinates(bus: MemoryEventQueue):
    """Per-round progress updates for the same loop (same turn_id /
    phase / iteration) collapse into one record; a different phase of
    the same turn appends; the turn aggregate lands as its own
    ``phase="turn"`` record."""
    obs = ObservabilityManager(bus)
    await obs.start()

    await bus.publish(
        "crewlet.events.agent_spawned",
        AgentSpawned(agent_id="a1", role="Engineer", source="pool"),
    )
    await bus.publish(
        "crewlet.events.agent_turn_progress",
        AgentTurnProgress(
            agent_id="a1",
            turn_id="T1",
            phase="plan",
            iteration=1,
            prompt="plan it",
            response="r1",
            round_num=0,
            total_tokens=10,
        ),
    )
    await bus.publish(
        "crewlet.events.agent_turn_progress",
        AgentTurnProgress(
            agent_id="a1",
            turn_id="T1",
            phase="plan",
            iteration=1,
            prompt="plan it",
            response="r1 r2",
            round_num=1,
            total_tokens=20,
        ),
    )
    await bus.publish(
        "crewlet.events.agent_turn_progress",
        AgentTurnProgress(
            agent_id="a1",
            turn_id="T1",
            phase="execute",
            iteration=1,
            prompt="plan it",
            response="exec",
            round_num=0,
            total_tokens=5,
        ),
    )
    await bus.publish(
        "crewlet.events.agent_turn_completed",
        AgentTurnCompleted(
            agent_id="a1",
            turn_id="T1",
            prompt="plan it",
            response="all done",
            total_tokens=25,
        ),
    )

    history = obs.agent_metrics["a1"].llm_history
    assert [(r.phase, r.response) for r in history] == [
        ("plan", "r1 r2"),
        ("execute", "exec"),
        ("turn", "all done"),
    ]


@pytest.mark.asyncio
async def test_llm_history_prompt_fallback_without_turn_id(bus: MemoryEventQueue):
    """Loop callers outside the turn engine emit progress without a
    ``turn_id`` — the same-prompt fallback heuristic collapses
    their rounds, and the completion replaces the in-flight record."""
    obs = ObservabilityManager(bus)
    await obs.start()

    await bus.publish(
        "crewlet.events.agent_spawned",
        AgentSpawned(agent_id="a1", role="Engineer", source="pool"),
    )
    await bus.publish(
        "crewlet.events.agent_turn_progress",
        AgentTurnProgress(agent_id="a1", prompt="ad hoc", response="r1"),
    )
    await bus.publish(
        "crewlet.events.agent_turn_progress",
        AgentTurnProgress(agent_id="a1", prompt="ad hoc", response="r1 r2"),
    )
    await bus.publish(
        "crewlet.events.agent_turn_completed",
        AgentTurnCompleted(agent_id="a1", prompt="ad hoc", response="final"),
    )

    history = obs.agent_metrics["a1"].llm_history
    assert len(history) == 1
    assert history[0].response == "final"
    assert history[0].phase == "turn"


@pytest.mark.asyncio
async def test_get_summary(bus: MemoryEventQueue):
    obs = ObservabilityManager(bus)
    await obs.start()

    await bus.publish(
        "crewlet.events.agent_spawned",
        AgentSpawned(agent_id="a1", role="Engineer", source="pool"),
    )
    await bus.publish(
        "crewlet.events.task_created", TaskCreated(task_id="t1", title="A", source="e")
    )

    summary = obs.get_summary()
    assert "engine" in summary
    assert "agents" in summary
    assert summary["engine"]["events_processed"] == 2
    assert "a1" in summary["agents"]
