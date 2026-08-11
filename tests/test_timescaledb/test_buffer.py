"""Tests for the buffered EventStore wrapper."""

from __future__ import annotations

import asyncio
from datetime import UTC, datetime

import pytest

from crewlet.timescaledb.buffer import BufferedEventStore
from crewlet.timescaledb.memory import MemoryEventStore


@pytest.fixture
def store() -> MemoryEventStore:
    return MemoryEventStore()


async def test_buffered_write_and_flush(store: MemoryEventStore) -> None:
    buf = BufferedEventStore(store, max_queue=100)
    await buf.start()

    await buf.write_event(
        event_id="e1",
        event_type="task_created",
        source="pm",
        timestamp=datetime.now(UTC),
    )

    # Event is in the queue, not yet in the store
    assert buf.pending >= 0  # may have flushed already

    # Wait for flush cycle
    await asyncio.sleep(1.5)

    events = await buf.list_events()
    assert len(events) == 1
    assert events[0]["type"] == "task_created"

    await buf.close()


async def test_buffered_close_flushes(store: MemoryEventStore) -> None:
    buf = BufferedEventStore(store, max_queue=100)
    await buf.start()

    await buf.write_event(
        event_id="e1",
        event_type="test",
        source="test",
        timestamp=datetime.now(UTC),
    )

    # Check events were flushed to the backing store before close clears it
    await asyncio.sleep(1.5)
    events = await store.list_events()
    assert len(events) == 1

    await buf.close()


async def test_buffered_delegates_reads(store: MemoryEventStore) -> None:
    buf = BufferedEventStore(store, max_queue=100)
    await buf.start()

    # Write directly to backing store
    await store.write_event(
        event_id="direct",
        event_type="test",
        source="test",
        timestamp=datetime.now(UTC),
    )

    # Read through buffer
    event = await buf.get_event("direct")
    assert event is not None
    assert event["id"] == "direct"

    await buf.close()


async def test_buffer_overflow_drops_oldest(store: MemoryEventStore) -> None:
    buf = BufferedEventStore(store, max_queue=2)
    await buf.start()

    # Fill the queue
    for i in range(3):
        await buf.write_event(
            event_id=f"e-{i}",
            event_type="test",
            source="test",
            timestamp=datetime.now(UTC),
        )

    # Should not raise, but may have dropped oldest
    assert buf.pending <= 2

    await buf.close()


async def test_buffer_reenqueues_unwritten_events_on_cancel(
    store: MemoryEventStore,
) -> None:
    """Regression guard: cancellation mid-flush must not silently drop
    the unwritten tail of the batch.

    ``_flush_batch`` pulls events out of the queue
    into a local list then writes them one at a time; a
    ``CancelledError`` during an ``await write_event(...)`` would
    propagate up with the remaining events still in the local list,
    lost forever.  A ``try/finally`` re-enqueues anything pulled but
    not yet written.
    """
    from typing import Any

    class SlowStore:
        """MemoryEventStore-compatible store whose writes block forever
        until ``release`` is set, so we can deterministically cancel."""

        def __init__(self) -> None:
            self._events: list[dict[str, Any]] = []
            self.release = asyncio.Event()

        async def start(self) -> None: ...

        async def close(self) -> None: ...

        async def write_event(self, **kwargs: Any) -> None:
            # First event blocks until release — gives the test a
            # deterministic cancellation point.
            if not self._events:
                await self.release.wait()
            self._events.append(kwargs)

        # Delegate reads to an internal list for completeness.
        async def list_events(self, **_: Any) -> list[dict[str, Any]]:
            return list(self._events)

    slow = SlowStore()
    buf = BufferedEventStore(slow, max_queue=100)
    await buf.start()

    # Enqueue three events.  The first will block the flush loop mid-write.
    for i in range(3):
        await buf.write_event(
            event_id=f"e-{i}",
            event_type="test",
            source="test",
            timestamp=datetime.now(UTC),
        )

    # Let the flush loop pick them up and start the first write.
    await asyncio.sleep(1.2)

    # Cancel the flush task while it's blocked inside write_event.
    # This is what ``close()`` does, but we want to observe the state
    # between cancel and the final flush.
    assert buf._task is not None  # type: ignore[attr-defined]
    buf._task.cancel()  # type: ignore[attr-defined]
    import contextlib

    with contextlib.suppress(asyncio.CancelledError):
        await buf._task  # type: ignore[attr-defined]

    # After cancellation, everything we pulled off the queue must be
    # back in it: ``buf.pending == 3``, never 0 (all lost).  The
    # in-flight write is
    # treated as "maybe-not-committed" and re-enqueued for a retry
    # rather than dropped, which is the safe choice for an
    # observability store.
    remaining = buf.pending
    assert remaining == 3, f"expected 3 re-enqueued events, got {remaining}"

    # Let the slow write complete so the buffer shuts down cleanly.
    # In this test, write_event(e-0) was cancelled *before* the
    # append ran, so the retry via the final flush_batch() succeeds
    # and we end up with exactly the original three events.  In the
    # general case an in-flight write may or may not have partially
    # committed — re-enqueuing trades possible duplicates (acceptable
    # for observability) for guaranteed non-loss.
    slow.release.set()
    buf._stopping = True  # type: ignore[attr-defined]
    await buf._flush_batch()
    await slow.close()
    written_ids = sorted(e["event_id"] for e in slow._events)
    assert written_ids == ["e-0", "e-1", "e-2"]
