"""Buffered write-behind wrapper for EventStore.

Accepts writes into an in-memory asyncio queue and flushes them to the
backing store in a background task.  If the backing store is temporarily
unreachable, events stay in the queue and are retried on the next flush
cycle.  This keeps webhook handlers fast and non-blocking — database
downtime never affects the notification pipeline.
"""

from __future__ import annotations

import asyncio
import contextlib
from typing import Any

from crewlet._logging import get_logger

logger = get_logger("timescaledb.buffer")

_MAX_QUEUE = 10_000
_FLUSH_INTERVAL = 1.0  # seconds between flush attempts
_BATCH_SIZE = 100  # max events per flush cycle


class BufferedEventStore:
    """Write-behind buffer that delegates reads to the backing store.

    Writes are enqueued immediately (never blocks the caller).
    A background task drains the queue in batches and writes to the
    backing store.  Failed batches are re-enqueued for retry.
    """

    def __init__(self, store: Any, *, max_queue: int = _MAX_QUEUE) -> None:
        self._store = store
        self._queue: asyncio.Queue[dict[str, Any]] = asyncio.Queue(maxsize=max_queue)
        self._task: asyncio.Task[None] | None = None
        self._stopping = False

    async def start(self) -> None:
        await self._store.start()
        self._stopping = False
        self._task = asyncio.create_task(self._flush_loop())
        logger.info(
            "buffered_event_store_started",
            max_queue=self._queue.maxsize,
        )

    async def close(self) -> None:
        """Drain remaining events and shut down."""
        self._stopping = True
        if self._task is not None:
            self._task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._task
        # Best-effort final flush
        await self._flush_batch()
        await self._store.close()
        logger.info(
            "buffered_event_store_closed",
            remaining=self._queue.qsize(),
        )

    async def write_event(self, **kwargs: Any) -> None:
        """Enqueue an event for background flushing.

        Never blocks the caller.  If the queue is full, the oldest
        event is dropped with a warning.
        """
        try:
            self._queue.put_nowait(kwargs)
        except asyncio.QueueFull:
            try:
                dropped = self._queue.get_nowait()
                logger.warning(
                    "buffer_overflow_dropped",
                    dropped_type=dropped.get("event_type", "?"),
                )
                self._queue.put_nowait(kwargs)
            except (asyncio.QueueEmpty, asyncio.QueueFull):
                logger.warning("buffer_write_failed")

    async def list_events(self, **kwargs: Any) -> list[dict[str, Any]]:
        return await self._store.list_events(**kwargs)

    async def get_event(self, event_id: str) -> dict[str, Any] | None:
        return await self._store.get_event(event_id)

    async def list_trace(self, trace_id: str) -> list[dict[str, Any]]:
        return await self._store.list_trace(trace_id)

    async def get_agent_states(
        self, agent_roles: list[str]
    ) -> dict[str, dict[str, Any]]:
        return await self._store.get_agent_states(agent_roles)

    async def get_agent_llm_history(
        self, agent_id: str, *, limit: int = 50
    ) -> list[dict[str, Any]]:
        return await self._store.get_agent_llm_history(agent_id, limit=limit)

    async def list_token_usage_events(
        self, *, since_days: int = 30
    ) -> list[dict[str, Any]]:
        return await self._store.list_token_usage_events(since_days=since_days)

    async def list_phase_token_events(
        self, *, since_days: int = 30, agent_role: str | None = None
    ) -> list[dict[str, Any]]:
        return await self._store.list_phase_token_events(
            since_days=since_days, agent_role=agent_role
        )

    async def _flush_loop(self) -> None:
        """Background loop: drain queue → write to store."""
        while not self._stopping:
            try:
                await asyncio.sleep(_FLUSH_INTERVAL)
                await self._flush_batch()
            except asyncio.CancelledError:
                break
            except Exception as exc:
                logger.exception("flush_loop_error", error=str(exc))

    async def _flush_batch(self) -> None:
        """Write up to BATCH_SIZE events from the queue to the store."""
        batch: list[dict[str, Any]] = []
        for _ in range(_BATCH_SIZE):
            try:
                batch.append(self._queue.get_nowait())
            except asyncio.QueueEmpty:
                break

        if not batch:
            return

        written = 0
        failed: list[dict[str, Any]] = []
        try:
            for event in batch:
                try:
                    await self._store.write_event(**event)
                    written += 1
                except Exception as exc:
                    logger.warning(
                        "flush_write_failed",
                        event_type=event.get("event_type", "?"),
                        error=str(exc),
                    )
                    failed.append(event)
        finally:
            # If cancelled mid-flush, re-enqueue any events we pulled
            # off the queue but didn't get a chance to write.  Without
            # this the per-batch tail would be silently dropped every
            # time ``close()`` races with the flush loop.
            unwritten = batch[written + len(failed) :]
            for event in unwritten:
                try:
                    self._queue.put_nowait(event)
                except asyncio.QueueFull:
                    logger.warning(
                        "cancel_requeue_dropped",
                        event_type=event.get("event_type", "?"),
                    )

        # Re-enqueue failed events for retry
        for event in failed:
            try:
                self._queue.put_nowait(event)
            except asyncio.QueueFull:
                logger.warning(
                    "requeue_dropped",
                    event_type=event.get("event_type", "?"),
                )

        if failed:
            logger.warning(
                "flush_partial",
                written=len(batch) - len(failed),
                failed=len(failed),
                queue_depth=self._queue.qsize(),
            )
        elif batch:
            logger.debug(
                "flush_complete",
                count=len(batch),
                queue_depth=self._queue.qsize(),
            )

    @property
    def pending(self) -> int:
        """Number of events waiting to be flushed."""
        return self._queue.qsize()
