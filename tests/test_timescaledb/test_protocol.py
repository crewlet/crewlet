"""Tests for the EventStore protocol conformance."""

from __future__ import annotations

from crewlet.timescaledb.memory import MemoryEventStore
from crewlet.timescaledb.protocol import EventStore


def test_memory_store_implements_protocol() -> None:
    """MemoryEventStore satisfies the EventStore protocol."""
    assert isinstance(MemoryEventStore(), EventStore)
