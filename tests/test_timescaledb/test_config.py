"""Tests for the CLI event-store builder helpers.

The persistent event store is now backed by the shared PostgreSQL /
TimescaleDB instance, so there's no dedicated ``providers.clickhouse``
section anymore — the builder takes an optional ``Database`` handle
directly and falls back to two in-memory legs when it's ``None``.
"""

from __future__ import annotations

from datetime import UTC, datetime

from crewlet.cli import (
    _build_api_event_store,
    _build_engine_event_store,
)
from crewlet.timescaledb import (
    BufferedEventStore,
    CompositeEventStore,
    MemoryEventStore,
    TimescaleDBEventStore,
)

# --------------------------------------------------------------------------- #
# CLI event store builder helpers — gate the composite wrapping that drives
# per-agent token aggregation on the dashboard.
# --------------------------------------------------------------------------- #


def test_build_api_event_store_fallback_returns_composite() -> None:
    """Without a Database the helper still returns a composite."""
    store = _build_api_event_store(None)
    assert isinstance(store, CompositeEventStore)


def test_build_api_event_store_with_db_returns_composite() -> None:
    """With a Database passed in, the helper wraps
    ``BufferedEventStore(TimescaleDBEventStore(db))`` in a composite.
    """
    # A sentinel object is enough — ``TimescaleDBEventStore`` doesn't
    # touch the DB until a query/write is issued.
    fake_db = object()
    store = _build_api_event_store(fake_db)
    assert isinstance(store, CompositeEventStore)
    persistent = store._persistent  # type: ignore[attr-defined]
    assert isinstance(persistent, BufferedEventStore)
    backing = persistent._store  # type: ignore[attr-defined]
    assert isinstance(backing, TimescaleDBEventStore)


def test_build_engine_event_store_without_persistent_returns_composite() -> None:
    """cmd_run no-persistent path must wrap the memory store in a composite."""
    memory_store = MemoryEventStore()
    store = _build_engine_event_store(memory_store, None)
    assert isinstance(store, CompositeEventStore)
    # The given memory_store is the persistent leg (the one the listener
    # writes to); the satellite is a fresh empty MemoryEventStore.
    assert store._persistent is memory_store  # type: ignore[attr-defined]
    assert isinstance(store._memory, MemoryEventStore)  # type: ignore[attr-defined]
    assert store._memory is not memory_store  # type: ignore[attr-defined]


def test_build_engine_event_store_with_persistent_returns_composite() -> None:
    """cmd_run persistent path: composite wraps TimescaleDB + memory stores."""
    memory_store = MemoryEventStore()
    # Build a TimescaleDBEventStore with a dummy db — never started.
    persistent_store = TimescaleDBEventStore(db=None)  # type: ignore[arg-type]
    store = _build_engine_event_store(memory_store, persistent_store)
    assert isinstance(store, CompositeEventStore)
    assert store._persistent is persistent_store  # type: ignore[attr-defined]
    assert store._memory is memory_store  # type: ignore[attr-defined]


async def test_build_engine_event_store_fallback_aggregates_tokens() -> None:
    """Regression: the no-persistent path must aggregate tokens end-to-end.

    Mirrors the exact wiring ``cmd_run`` uses when there is no persistent
    store: a memory store fed by the publish listener, wrapped in a
    composite.  Verifies that ``get_agent_states`` returns non-zero token
    totals after a spawn + turn event.  If ``cmd_run`` ever stops
    wrapping the memory store (e.g. ``event_store = memory_store``),
    this test — via the shared helper — documents the expected result.
    """
    memory_store = MemoryEventStore()
    await memory_store.start()
    store = _build_engine_event_store(memory_store, None)

    now = datetime.now(UTC)
    # Simulate the publish-listener path: events land in memory_store.
    await memory_store.write_event(
        event_id="spawn",
        event_type="agent_spawned",
        source="pm",
        timestamp=now,
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )
    await memory_store.write_event(
        event_id="turn",
        event_type="agent_turn_completed",
        source="pm",
        timestamp=now,
        payload={"input_tokens": 100, "output_tokens": 50, "total_tokens": 150},
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )

    states = await store.get_agent_states(["PM"])
    pm = states["PM"]
    assert pm["input_tokens"] == 100, pm
    assert pm["output_tokens"] == 50, pm
    assert pm["total_tokens"] == 150, pm
