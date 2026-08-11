"""In-memory implementation of EventQueue for tests."""

from __future__ import annotations

import asyncio
import contextlib
from collections import deque
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import Event
from crewlet.queue.matching import topic_matches
from crewlet.queue.protocol import (
    BatchOptions,
    order_partitions_oldest_first,
    partition_by_key,
)

logger = get_logger("queue.memory")


@dataclass
class _Subscription:
    topic: str
    group: str
    handler: Callable[[Event], Awaitable[None]]


@dataclass
class _BatchSubscription:
    topic: str
    group: str
    handler: Callable[[list[Event]], Awaitable[None]]
    batch_key: Callable[[Event], str]
    options: BatchOptions


@dataclass
class _StreamSubscription:
    topic_pattern: str
    handler: Callable[[str, Event], Awaitable[None]]


class MemoryEventQueue:
    """In-memory EventQueue for tests.

    Dispatches events inline (synchronously within ``publish``) for
    deterministic test behaviour.  Implements competing-consumer
    semantics per group: each event on a topic is delivered to exactly
    one subscriber per group. On handler failure the event is
    redelivered (up to ``max_redeliveries`` attempts).
    """

    def __init__(self, *, max_redeliveries: int = 3, max_history: int = 10000) -> None:
        self._subscriptions: list[_Subscription] = []
        self._batch_subscriptions: list[_BatchSubscription] = []
        # Lingered-batch state, keyed by (topic, group): events waiting
        # for the linger window plus the task that flushes them.
        self._batch_pending: dict[tuple[str, str], list[Event]] = {}
        self._batch_flush_tasks: dict[tuple[str, str], asyncio.Task[None]] = {}
        self._stream_subscriptions: list[_StreamSubscription] = []
        self._publish_listeners: list[Callable[[str, Event], Awaitable[None]]] = []
        self._running = False
        self._paused = False
        # Per-topic pause (sandbox busy gate): handler
        # delivery for a paused topic is held in ``_paused_topic_buffer``
        # (NOT dropped) and flushed on ``resume_topic``.  Observability
        # listeners + stream still fire immediately so the dashboard sees
        # the queued events.
        self._paused_topics: set[str] = set()
        self._paused_topic_buffer: dict[str, list[Event]] = {}
        self._max_redeliveries = max_redeliveries
        self._history: deque[Event] = deque(maxlen=max_history)
        # In-flight handler tracking, used by ``wait_for_handlers`` so
        # the engine's graceful-shutdown drain can wait for currently-
        # running handlers to complete.  Inline dispatch means the
        # counter is only non-zero while ``publish`` is awaiting a
        # handler -- but the same drain API stays consistent with the
        # Pulsar implementation.
        self._in_flight = 0
        self._idle_event = asyncio.Event()
        self._idle_event.set()

    def add_publish_listener(
        self, listener: Callable[[str, Event], Awaitable[None]]
    ) -> None:
        self._publish_listeners.append(listener)

    @property
    def history(self) -> list[Event]:
        """All events that have been published (for testing/debugging)."""
        return list(self._history)

    # -- protocol methods --

    async def publish(self, topic: str, event: Event) -> None:
        if not self._running:
            raise RuntimeError("MemoryEventQueue is not started")
        self._history.append(event)
        if topic.endswith(".inbound") or topic.endswith(".inbox"):
            logger.info("event_published", topic=topic, event_type=event.type)
        else:
            logger.debug("event_published", topic=topic, event_type=event.type)
        for listener in self._publish_listeners:
            try:
                await listener(topic, event)
            except Exception as exc:
                # Listener errors must not prevent event delivery.
                logger.exception("publish_listener_failed", topic=topic, error=str(exc))
        await self._dispatch_stream(topic, event)
        # Per-topic pause: hold handler delivery (buffer, don't drop) until
        # ``resume_topic`` flushes it.  Stream / listeners above already
        # ran so observability isn't blocked.
        if topic in self._paused_topics:
            self._paused_topic_buffer.setdefault(topic, []).append(event)
            logger.debug(
                "memory_topic_paused_buffered", topic=topic, event_type=event.type
            )
            return
        # Dispatch inline for synchronous behavior (important for tests).
        await self._dispatch_one(topic, event)
        await self._dispatch_batch(topic, event)

    async def subscribe(
        self,
        topic: str,
        group: str,
        handler: Callable[[Event], Awaitable[None]],
    ) -> None:
        self._subscriptions.append(
            _Subscription(topic=topic, group=group, handler=handler),
        )
        logger.debug("subscription_added", topic=topic, group=group)

    async def subscribe_batch(
        self,
        topic: str,
        group: str,
        handler: Callable[[list[Event]], Awaitable[None]],
        *,
        batch_key: Callable[[Event], str],
        options: BatchOptions,
    ) -> None:
        self._batch_subscriptions.append(
            _BatchSubscription(
                topic=topic,
                group=group,
                handler=handler,
                batch_key=batch_key,
                options=options,
            ),
        )
        logger.debug("batch_subscription_added", topic=topic, group=group)

    async def unsubscribe(self, topic: str, group: str) -> None:
        """Drop the durable consumer(s) for ``topic``/``group``.

        Mirrors the Pulsar backend: the pair's handlers stop receiving,
        and any lingered batch buffer for the pair is discarded (the
        broker-side analogue is deleting the subscription and its retained
        messages). Idempotent.
        """
        before = len(self._subscriptions) + len(self._batch_subscriptions)
        self._subscriptions = [
            s
            for s in self._subscriptions
            if not (s.topic == topic and s.group == group)
        ]
        self._batch_subscriptions = [
            s
            for s in self._batch_subscriptions
            if not (s.topic == topic and s.group == group)
        ]
        task = self._batch_flush_tasks.pop((topic, group), None)
        if task is not None:
            task.cancel()
        self._batch_pending.pop((topic, group), None)
        removed = before - (len(self._subscriptions) + len(self._batch_subscriptions))
        if removed:
            logger.info("unsubscribed", topic=topic, group=group, consumers=removed)

    async def subscribe_stream(
        self,
        topic_pattern: str,
        handler: Callable[[str, Event], Awaitable[None]],
    ) -> Callable[[], Awaitable[None]]:
        sub = _StreamSubscription(topic_pattern=topic_pattern, handler=handler)
        self._stream_subscriptions.append(sub)
        logger.debug("stream_subscription_added", topic_pattern=topic_pattern)

        async def _unsubscribe() -> None:
            try:
                self._stream_subscriptions.remove(sub)
            except ValueError:
                return
            logger.debug("stream_subscription_removed", topic_pattern=topic_pattern)

        return _unsubscribe

    @property
    def in_flight_count(self) -> int:
        """Number of handler invocations currently mid-flight."""
        return self._in_flight

    async def start(self) -> None:
        if self._running:
            return
        self._running = True
        self._paused = False
        logger.info("memory_event_queue_started")

    async def pause_delivery(self) -> None:
        """Stop dispatching new events to handlers (publishes still work)."""
        if self._paused:
            return
        self._paused = True
        logger.info("memory_event_queue_paused")

    async def pause_topic(self, topic: str) -> None:
        """Pause handler delivery for one topic (buffer, don't drop)."""
        self._paused_topics.add(topic)
        logger.info("memory_topic_paused", topic=topic)

    async def resume_topic(self, topic: str) -> None:
        """Resume a paused topic and flush its buffered events in order."""
        if topic not in self._paused_topics:
            return
        self._paused_topics.discard(topic)
        buffered = self._paused_topic_buffer.pop(topic, [])
        logger.info("memory_topic_resumed", topic=topic, flushed=len(buffered))
        for event in buffered:
            await self._dispatch_one(topic, event)
            await self._dispatch_batch(topic, event)

    async def wait_for_handlers(self, timeout: float | None = None) -> int:
        """Wait for in-flight handler invocations to complete.

        With inline dispatch the counter is only non-zero while a
        ``publish`` call is mid-handler, so this is typically a no-op
        in the test path.  Kept for protocol parity with the Pulsar
        backend.
        """
        if self._in_flight == 0:
            return 0
        if timeout is None:
            await self._idle_event.wait()
            return self._in_flight
        with contextlib.suppress(TimeoutError):
            await asyncio.wait_for(self._idle_event.wait(), timeout=timeout)
        return self._in_flight

    async def stop(self) -> None:
        if not self._running:
            return
        self._running = False
        self._paused = False
        for task in self._batch_flush_tasks.values():
            task.cancel()
        self._batch_flush_tasks.clear()
        # Cancelling a flush task parked in its linger sleep skips the
        # task's own drop-logging branch — log the stranded events here
        # so a stop inside the window never loses them silently.
        for (topic, group), pending in self._batch_pending.items():
            for event in pending:
                logger.error(
                    "event_dropped",
                    topic=topic,
                    group=group,
                    event_type=event.type,
                    reason="lingered_event_at_shutdown",
                )
        self._batch_pending.clear()
        logger.info("memory_event_queue_stopped")

    # -- internals --

    async def _dispatch_stream(self, topic: str, event: Event) -> None:
        """Fan an event out to broadcast (stream) subscribers."""
        if not self._stream_subscriptions:
            return
        # Snapshot so handlers that unsubscribe during dispatch don't
        # mutate the list we're iterating.
        for sub in list(self._stream_subscriptions):
            if not topic_matches(sub.topic_pattern, topic):
                continue
            try:
                await sub.handler(topic, event)
            except Exception as exc:
                logger.exception(
                    "stream_handler_failed",
                    topic=topic,
                    topic_pattern=sub.topic_pattern,
                    error=str(exc),
                )

    async def _dispatch_one(self, topic: str, event: Event) -> None:
        """Dispatch a single event to matching subscribers."""
        if self._paused:
            # Pause is one-way: events are accepted by ``publish`` (they
            # land in ``_history``) but not delivered to handlers.  On
            # the Pulsar backend the equivalent messages stay queued in
            # the broker until the next engine subscribes.  In-memory has
            # no persistence, so paused events are simply not dispatched.
            logger.debug(
                "memory_event_queue_dispatch_skipped_paused",
                topic=topic,
                event_type=event.type,
            )
            return

        groups: dict[str, list[_Subscription]] = {}
        for sub in self._subscriptions:
            if sub.topic == topic:
                groups.setdefault(sub.group, []).append(sub)

        for group_name, members in groups.items():
            await self._deliver_with_retries(
                invoke=lambda member: member.handler(event),
                members=members,
                log_fields={
                    "topic": topic,
                    "group": group_name,
                    "event_type": event.type,
                },
            )

    async def _deliver_with_retries(
        self,
        *,
        invoke: Callable[[Any], Awaitable[None]],
        members: list[Any],
        log_fields: dict[str, Any],
        failure_event: str = "handler_failed",
    ) -> None:
        """Run one delivery with competing-consumer retry semantics.

        The single copy of the redelivery machinery both the
        single-event and batch paths share: each attempt rotates through
        the group *members* (``invoke`` receives the chosen member), up
        to ``max_redeliveries`` attempts, with the in-flight counter
        held exactly for the handler's runtime — the invariant
        ``wait_for_handlers`` (graceful-shutdown drain) depends on.
        Exhausted attempts drop the delivery with an ``event_dropped``
        log.
        """
        for attempt in range(self._max_redeliveries):
            member = members[attempt % len(members)]
            self._in_flight += 1
            self._idle_event.clear()
            try:
                try:
                    await invoke(member)
                    return
                except Exception as exc:
                    logger.warning(
                        failure_event,
                        attempt=attempt + 1,
                        error=str(exc),
                        **log_fields,
                    )
            finally:
                self._in_flight -= 1
                if self._in_flight == 0:
                    self._idle_event.set()
        logger.error("event_dropped", **log_fields)

    async def _dispatch_batch(self, topic: str, event: Event) -> None:
        """Dispatch one published event to matching batch subscribers.

        ``linger_seconds == 0`` dispatches inline as a single-element
        batch — publishes stay synchronous for deterministic tests, and
        partitioning a one-event batch is trivially itself.  A positive
        linger buffers per (topic, group) and schedules a flush task;
        events published within the window coalesce into one delivery.
        """
        if self._paused or not self._batch_subscriptions:
            return

        groups: dict[str, list[_BatchSubscription]] = {}
        for sub in self._batch_subscriptions:
            if sub.topic == topic:
                groups.setdefault(sub.group, []).append(sub)

        for group_name, members in groups.items():
            options = members[0].options
            if options.effective_linger_seconds <= 0:
                await self._deliver_batch_partitions(members, [event])
                continue
            key = (topic, group_name)
            self._batch_pending.setdefault(key, []).append(event)
            task = self._batch_flush_tasks.get(key)
            if task is None or task.done():
                self._batch_flush_tasks[key] = asyncio.create_task(
                    self._flush_batch_after_linger(key, members)
                )

    async def _flush_batch_after_linger(
        self, key: tuple[str, str], members: list[_BatchSubscription]
    ) -> None:
        """Deliver a lingered batch once its window expires.

        The window is fixed from the first buffered event (matching the
        Pulsar backend): the sleep starts when the task is scheduled and
        later publishes join the same pending list without resetting it.
        ``max_batch`` splits an oversized buffer into successive
        deliveries.

        Lingered events caught by ``pause_delivery`` / ``stop`` are
        DROPPED, with one ``event_dropped`` log per event — the same
        fate every paused publish meets in this backend (in-memory has
        no persistence; on the Pulsar backend the equivalent messages
        are NAK'd back to the broker for the next engine subscription).
        Setting a linger therefore trades the inline-dispatch guarantee
        ("publish returned ⇒ handler ran") for coalescing; tests that
        shut down mid-window must expect the drop.
        """
        await asyncio.sleep(members[0].options.effective_linger_seconds)
        pending = self._batch_pending.pop(key, [])
        self._batch_flush_tasks.pop(key, None)
        if self._paused or not self._running:
            for event in pending:
                logger.error(
                    "event_dropped",
                    topic=key[0],
                    group=key[1],
                    event_type=event.type,
                    reason="lingered_event_at_shutdown",
                )
            return
        max_batch = members[0].options.effective_max_batch
        while pending:
            chunk, pending = pending[:max_batch], pending[max_batch:]
            await self._deliver_batch_partitions(members, chunk)

    async def _deliver_batch_partitions(
        self, members: list[_BatchSubscription], events: list[Event]
    ) -> None:
        """Partition *events* by key and invoke the handler per partition.

        Partitioning is the shared protocol contract
        (:func:`~crewlet.queue.protocol.partition_by_key`); delivery
        reuses ``_deliver_with_retries`` so batch and single-event
        redelivery semantics can never drift apart inside this backend.
        """
        parts = order_partitions_oldest_first(
            partition_by_key(events, members[0].batch_key)
        )
        for key, part in parts:
            await self._deliver_with_retries(
                invoke=lambda member, part=part: member.handler(list(part)),
                members=members,
                log_fields={
                    "topic": members[0].topic,
                    "group": members[0].group,
                    "batch_key": key,
                    "event_type": part[0].type,
                    "event_count": len(part),
                },
                # Same machine-parsable failure event the Pulsar backend
                # emits for batch partitions — log consumers must not see
                # different names per backend.
                failure_event="batch_handler_failed",
            )
