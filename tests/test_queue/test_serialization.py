"""Tests for the shared event (de)serialization used by queue backends."""

from __future__ import annotations

from crewlet.events.types import Event, TaskAssigned
from crewlet.queue.serialization import deserialize_event, serialize_event


def test_roundtrip_base_event() -> None:
    event = Event(type="custom", source="t", payload={"k": "v"})
    restored = deserialize_event(serialize_event(event))
    assert restored.type == "custom"
    assert restored.source == "t"
    assert restored.payload == {"k": "v"}
    assert restored.id == event.id


def test_roundtrip_reconstructs_subclass_and_typed_fields() -> None:
    """A registered subclass round-trips back to its own class with its
    typed fields intact -- not flattened to the base ``Event``.
    """
    event = TaskAssigned(task_id="T-1", agent_id="A-1", role="engineer")
    restored = deserialize_event(serialize_event(event))
    assert isinstance(restored, TaskAssigned)
    assert restored.task_id == "T-1"
    assert restored.agent_id == "A-1"
    assert restored.role == "engineer"


def test_unknown_type_falls_back_to_base_event() -> None:
    raw = b'{"type": "totally_unknown", "source": "x", "payload": {}}'
    restored = deserialize_event(raw)
    assert type(restored) is Event
    assert restored.type == "totally_unknown"
    assert restored.source == "x"
