"""Tests for the Event store writer."""

from __future__ import annotations

import pytest

from crewlet.events.types import (
    AgentPhaseCompleted,
    AgentSpawned,
    Event,
    TaskCreated,
    TaskStarted,
)
from crewlet.timescaledb.memory import MemoryEventStore
from crewlet.timescaledb.writer import _CATEGORY_MAP, EventStoreWriter


@pytest.fixture
def store() -> MemoryEventStore:
    return MemoryEventStore()


@pytest.fixture
def writer(store: MemoryEventStore) -> EventStoreWriter:
    return EventStoreWriter(store)


async def test_on_publish_writes_to_store(
    writer: EventStoreWriter, store: MemoryEventStore
) -> None:
    await store.start()
    event = TaskCreated(source="pm", task_id="T-1")
    await writer.on_publish("crewlet.events.task_created", event)

    events = await store.list_events()
    assert len(events) == 1
    assert events[0]["type"] == "task_created"


async def test_on_publish_skips_untracked_events(
    writer: EventStoreWriter, store: MemoryEventStore
) -> None:
    await store.start()
    event = Event(type="unknown_event", source="test")
    await writer.on_publish("crewlet.events.unknown_event", event)

    events = await store.list_events()
    assert len(events) == 0


async def test_category_mapping(
    writer: EventStoreWriter, store: MemoryEventStore
) -> None:
    await store.start()
    event = AgentSpawned(source="dev", role="Developer")
    await writer.on_publish("crewlet.events.agent_spawned", event)

    events = await store.list_events()
    assert events[0]["category"] == "lifecycle"


async def test_tags_extracted(
    writer: EventStoreWriter, store: MemoryEventStore
) -> None:
    await store.start()
    event = TaskStarted(
        source="pm",
        agent_id="a-1",
        role="PM",
        task_id="T-42",
    )
    await writer.on_publish("crewlet.events.task_started", event)

    # Get the full event to check tags (stored in payload)
    events = await store.list_events()
    assert len(events) == 1


async def test_summary_written_from_event_property(
    writer: EventStoreWriter, store: MemoryEventStore
) -> None:
    """Writer uses event.summary property, not a static method."""
    await store.start()
    event = AgentSpawned(source="dev-lead", role="Dev Lead")
    await writer.on_publish("crewlet.events.agent_spawned", event)

    events = await store.list_events()
    assert "Dev Lead" in events[0]["summary"]
    assert "joined" in events[0]["summary"]


@pytest.mark.parametrize(
    "event_type",
    [
        "turn_completed",
        "episode_written",
        "persist_decider_completed",
        "counterparty_profile_updated",
        "skill_synthesized",
        "skill_refined",
        "skill_promoted",
        # Plan-prefetch telemetry: the once-per-turn prefetch summary
        # (carries the thin-trigger gate decision) and the post-Plan
        # relevant-knowledge re-fetch.  A missing entry silently drops
        # the event at publish time -> the gate / re-fetch are invisible
        # in the dashboard despite being emitted.
        "plan_prefetch_summary",
        "relevant_knowledge_refetched",
    ],
)
def test_learning_event_types_in_category_map(event_type: str) -> None:
    """Learning lifecycle events must be in the writer's category map.

    The writer drops events whose ``type`` isn't in ``_CATEGORY_MAP`` —
    a missing entry is the difference between "events appear in the
    dashboard" and "events are silently swallowed at publish time".
    """
    assert _CATEGORY_MAP.get(event_type) == "learning"


async def test_actor_written_from_event_property(
    writer: EventStoreWriter, store: MemoryEventStore
) -> None:
    """Writer uses event.actor property."""
    await store.start()
    event = AgentSpawned(source="dev", role="Developer")
    await writer.on_publish("crewlet.events.agent_spawned", event)

    events = await store.list_events()
    assert events[0]["actor"] == "Developer"


#: Event types deliberately NOT persisted. Each needs a reason, because
#: the default for a new type is that it belongs in the map — see the
#: comment above ``_CATEGORY_MAP``.
_NOT_PERSISTED = {
    # A live in-flight progress ping. Persisting it would multiply the
    # event table by rounds-per-turn to store a value that is stale the
    # moment it lands.
    "agent_turn_progress": "live progress ping, superseded within the turn",
    # A snapshot of live in-memory meters whose values mean nothing
    # outside the run that produced them; a dashboard hydrating them
    # would render a dead process's counters as the current ones.
    "budget_reported": "in-memory meter snapshot, run-scoped",
}


def _declared_event_types() -> dict[str, str]:
    """Every ``type`` an ``Event`` subclass declares → its class name."""
    import inspect

    from crewlet.events import types as event_types
    from crewlet.events.types import Event

    declared: dict[str, str] = {}
    for name, obj in vars(event_types).items():
        if not inspect.isclass(obj) or not issubclass(obj, Event) or obj is Event:
            continue
        field = obj.model_fields.get("type")
        default = getattr(field, "default", None)
        if isinstance(default, str) and default:
            declared[default] = name
    return declared


async def test_every_event_type_is_persisted_or_deliberately_excluded() -> None:
    """The guard the writer's comment says exists.

    A type missing from ``_CATEGORY_MAP`` is dropped SILENTLY, which is
    how the sandbox panel came to show rows that vanished on reload.
    The test that stood here asserted a hand-maintained list of names
    and ``len >= 23`` — so a newly declared event type passed it
    without ever being written, which is exactly the case it was
    supposed to catch.
    """
    declared = _declared_event_types()
    assert len(declared) > 40, "the reflection found nothing — did the module move?"

    unmapped = sorted(set(declared) - set(_CATEGORY_MAP) - set(_NOT_PERSISTED))
    assert unmapped == [], (
        "these event types are declared but not in _CATEGORY_MAP, so the "
        f"writer drops them silently: {[(t, declared[t]) for t in unmapped]}. "
        "Add them to the map, or to _NOT_PERSISTED with a reason."
    )

    # The exclusions must stay excluded — an entry added to the map for
    # one of them would start persisting it without anyone revisiting
    # the reason it was left out.
    for event_type, reason in _NOT_PERSISTED.items():
        assert event_type not in _CATEGORY_MAP, f"{event_type}: {reason}"
        assert event_type in declared, f"{event_type} no longer exists — drop it here"


async def test_on_publish_persists_agent_phase_completed(
    writer: EventStoreWriter, store: MemoryEventStore
) -> None:
    """``AgentPhaseCompleted`` must reach the event store
    so ``get_agent_llm_history`` can surface Plan/Execute/Review rows
    on the dashboard.  A missing ``_CATEGORY_MAP`` entry would
    silently drop phase events.
    """
    await store.start()
    event = AgentPhaseCompleted(
        source="dev",
        agent_id="a-1",
        role="Dev",
        turn_id="t-1",
        iteration=1,
        phase="plan",
        model="sonnet",
        system_prompt="You are a planner.",
        user_prompt="Do a thing.",
        response='{"decision":"direct"}',
        decision="direct",
    )
    await writer.on_publish("crewlet.events.agent_phase_completed", event)

    events = await store.list_events()
    assert len(events) == 1
    assert events[0]["type"] == "agent_phase_completed"
    assert events[0]["category"] == "system"


async def test_extract_tags_empty_for_missing_fields() -> None:
    event = Event(type="org_started", source="engine")
    tags = EventStoreWriter._extract_tags(event)
    # Base Event has no agent_id, role, etc. — tags should be empty
    assert tags == {}


async def test_extract_tags_includes_available_fields() -> None:
    event = AgentSpawned(
        source="pm",
        agent_id="a-1",
        role="PM",
    )
    tags = EventStoreWriter._extract_tags(event)
    assert tags["agent_id"] == "a-1"
    assert tags["agent_role"] == "PM"


async def test_extract_tags_from_task_event() -> None:
    event = TaskStarted(
        source="pm",
        agent_id="a-1",
        task_id="T-1",
    )
    tags = EventStoreWriter._extract_tags(event)
    assert tags == {"agent_id": "a-1", "task_id": "T-1"}


def test_live_meters_are_never_persisted() -> None:
    """``budget_reported`` must stay out of the category map.

    It is a snapshot of in-memory counters whose values mean nothing
    outside the engine run that produced them, so a persisted copy
    would let a dashboard hydrate a dead process's meters and render
    them as the current ones -- a figure that is not merely stale but
    describes a different run.
    """
    assert "budget_reported" not in _CATEGORY_MAP
    assert "agent_turn_progress" not in _CATEGORY_MAP


async def test_failed_events_are_tagged() -> None:
    """A failed phase is a filterable dimension, not just a payload field.

    ``list_events`` never selects the payload column, so a dashboard
    hydrating its feed from history has no other way to know a phase
    died -- and a feed that renders a failed turn identically to a
    successful one is the whole point of carrying this.
    """
    failed = AgentPhaseCompleted(
        source="pm", role="PM", phase="execute", failed=True, error_kind="rate_limit"
    )
    assert EventStoreWriter._extract_tags(failed)["failed"] == "true"


async def test_successful_events_carry_no_failed_tag() -> None:
    """Only set when true, so the tag doubles as a filter."""
    ok = AgentPhaseCompleted(source="pm", role="PM", phase="execute")
    assert "failed" not in EventStoreWriter._extract_tags(ok)


async def test_trace_fields_written_to_store(
    writer: EventStoreWriter, store: MemoryEventStore
) -> None:
    await store.start()
    event = TaskCreated(
        source="pm",
        task_id="T-1",
        trace_id="a" * 32,
        span_id="b" * 16,
        parent_span_id="c" * 16,
    )
    await writer.on_publish("crewlet.events.task_created", event)

    events = await store.list_events()
    assert len(events) == 1
    assert events[0]["trace_id"] == "a" * 32
    assert events[0]["span_id"] == "b" * 16
    assert events[0]["parent_span_id"] == "c" * 16


def test_notifications_coalesced_in_category_map() -> None:
    """``NotificationsCoalesced`` exists FOR the event store/dashboard
    (operators watch when inbox batching kicks in) — a missing entry
    silently drops it at publish time, the exact failure mode the
    learning-events test above guards against."""
    assert _CATEGORY_MAP.get("notifications_coalesced") == "notification"
