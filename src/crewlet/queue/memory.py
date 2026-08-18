"""In-memory implementation of EventQueue for tests.

This backend is a **semantic twin** of the Pulsar one, not a convenience
stub, because it is the backend every unit test runs on: a divergence
here does not merely fail to catch a bug, it actively certifies one.

The property that matters most, and the one it used to invert:

- A ``(topic, group)`` pair is a **durable subscription**.  It exists
  independently of whether anything is attached to it, it retains events
  published while nothing is, and it replays them when a consumer
  attaches.  Seat ownership rests entirely on that — a seat between
  owners must hold its mail, not lose it.
- Members of a group **compete**: each event goes to exactly one of
  them, round-robin.  Delivering always to the first-registered member
  made the double-attach split-brain — two nodes consuming one seat —
  invisible, so a test asserting "exactly one delivery" passed while the
  real broker split the traffic and ran two interleaved turn streams.

What it still does differently, deliberately: **dispatch is inline**.
``publish`` drains the backlogs it can reach before returning, so a test
can publish and assert.  A real broker's handler runs later, elsewhere,
possibly twice.  Anything a test asserts immediately after ``publish``
is a race in production — this backend cannot tell you that.

Redelivery matches the broker's shape: ``max_redeliveries`` counts
*redeliveries after the first delivery* (so N+1 total attempts), and an
exhausted message goes to ``dlq-{topic}-{group}`` rather than being
destroyed, exactly as the Pulsar dead-letter policy does.
"""

from __future__ import annotations

import asyncio
import contextlib
from collections import deque
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import Event
from crewlet.queue.matching import topic_matches
from crewlet.queue.protocol import (
    BatchOptions,
    DeferDelivery,
    order_partitions_oldest_first,
    partition_by_key,
)
from crewlet.queue.topics import AGENT_INBOX_SUFFIX

logger = get_logger("queue.memory")


@dataclass
class _Consumer:
    """One attached consumer on a subscription.

    Single-event and batch consumers share a record because they share a
    subscription: on the real broker ``subscribe`` and ``subscribe_batch``
    against the same ``(topic, group)`` create two consumers on ONE
    Shared subscription, and both compete for its messages.  Modelling
    them as separate registries made the second one silently unreachable.
    """

    handler: Callable[..., Awaitable[None]]
    batched: bool = False
    batch_key: Callable[[Event], str] | None = None
    options: BatchOptions | None = None


@dataclass
class _Subscription:
    """A durable subscription: retained mail plus whoever is attached.

    ``events`` outlives every attachment.  That is the whole point — it
    is what a seat's inbox holds while no node owns the seat.
    """

    topic: str
    group: str
    events: deque[Event] = field(default_factory=deque)
    members: list[_Consumer] = field(default_factory=list)
    pauses: set[str] = field(default_factory=set)
    quiescing: bool = False
    #: Round-robin cursor across ``members`` — competing consumers.
    cursor: int = 0
    #: Redeliveries accrued per event id, mirroring the broker's counter.
    redeliveries: dict[str, int] = field(default_factory=dict)
    #: Open linger window, if a batch member asked for one.  The backlog
    #: IS the linger buffer — events published during the window simply
    #: accumulate in it, so there is no second buffer to keep in step.
    flush_task: asyncio.Task[None] | None = None

    @property
    def attached(self) -> bool:
        return bool(self.members)

    @property
    def deliverable(self) -> bool:
        return self.attached and not self.pauses and not self.quiescing


@dataclass
class _StreamSubscription:
    topic_pattern: str
    handler: Callable[[str, Event], Awaitable[None]]


def dlq_topic(topic: str, group: str) -> str:
    """The dead-letter subject, matching the Pulsar backend's naming.

    The ``dlq-`` prefix keeps it OUTSIDE the ``crewlet.*`` subject space
    so the dashboard's ``crewlet.events.>`` broadcast stream does not
    re-surface a poison event as if it were live.
    """
    return f"dlq-{topic}-{group}"


class MemoryEventQueue:
    """In-memory EventQueue for tests. See the module docstring."""

    def __init__(self, *, max_redeliveries: int = 3, max_history: int = 10000) -> None:
        # (topic, group) -> the durable subscription.
        self._subs: dict[tuple[str, str], _Subscription] = {}
        self._stream_subscriptions: list[_StreamSubscription] = []
        self._publish_listeners: list[Callable[[str, Event], Awaitable[None]]] = []
        self._running = False
        self._paused = False
        self._max_redeliveries = max_redeliveries
        self._history: deque[Event] = deque(maxlen=max_history)
        # Dead-lettered events, keyed by their dlq subject.  The Pulsar
        # backend hands these to a real DLQ topic; keeping them here
        # rather than dropping them is what makes "a poison message is
        # recoverable" assertable on this backend too.
        self._dead_letters: dict[str, list[Event]] = {}
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

    def backlog(self, topic: str, group: str) -> list[Event]:
        """Events a subscription retains and has not yet delivered.

        Test-facing: the assertion "an unowned seat's mail survived" has
        to be able to look at the mail.
        """
        sub = self._subs.get((topic, group))
        return list(sub.events) if sub is not None else []

    def attachments(self) -> list[tuple[str, str]]:
        """Every ``(topic, group)`` pair this process has attached to.

        Public because "which seats is this node serving?" is the
        question seat ownership makes operationally central, and a test
        should not have to read the backend's registry to ask it.
        """
        return [k for k, sub in self._subs.items() if sub.attached]

    def pause_holds(self, topic: str, group: str) -> set[str]:
        """Reasons currently holding a subscription paused.

        Public because a hold is operator-visible behaviour — which
        subsystem is gating a seat is the first question when one goes
        quiet — and because tests must not reach into the backend's
        internals to ask.
        """
        sub = self._subs.get((topic, group))
        return set(sub.pauses) if sub is not None else set()

    def dead_letters(self, topic: str, group: str) -> list[Event]:
        """Events this subscription gave up on. See :func:`dlq_topic`."""
        return list(self._dead_letters.get(dlq_topic(topic, group), []))

    # -- protocol methods --

    async def publish(self, topic: str, event: Event) -> None:
        if not self._running:
            raise RuntimeError("MemoryEventQueue is not started")
        self._history.append(event)
        if topic.endswith(".inbound") or topic.endswith(AGENT_INBOX_SUFFIX):
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

        # Every subscription on this topic gets its own copy — that is
        # what a consumer GROUP is.  Competition happens between the
        # members of one group, never across groups.
        targets = [s for s in self._subs.values() if s.topic == topic]
        if not targets:
            # No durable subscription exists, so there is nothing to
            # retain the event — the real broker drops it too.
            # ``ensure_subscription`` exists precisely so a seat's mail
            # never depends on someone being attached at the time.
            logger.debug("event_unsubscribed", topic=topic, event_type=event.type)
            return
        for sub in targets:
            sub.events.append(event)
        for sub in targets:
            await self._drain(sub)

    async def subscribe(
        self,
        topic: str,
        group: str,
        handler: Callable[[Event], Awaitable[None]],
    ) -> None:
        if not self._running:
            raise RuntimeError("MemoryEventQueue is not started")
        sub = self._ensure(topic, group)
        sub.members.append(_Consumer(handler=handler))
        logger.debug("subscription_added", topic=topic, group=group)
        await self._drain(sub)

    async def subscribe_batch(
        self,
        topic: str,
        group: str,
        handler: Callable[[list[Event]], Awaitable[None]],
        *,
        batch_key: Callable[[Event], str],
        options: BatchOptions,
    ) -> None:
        if not self._running:
            raise RuntimeError("MemoryEventQueue is not started")
        sub = self._ensure(topic, group)
        sub.members.append(
            _Consumer(
                handler=handler, batched=True, batch_key=batch_key, options=options
            )
        )
        logger.debug("batch_subscription_added", topic=topic, group=group)
        await self._drain(sub)

    async def quiesce(self, topic: str, group: str) -> bool:
        """Stop taking new work; keep the attachment and the backlog."""
        sub = self._subs.get((topic, group))
        if sub is None or not sub.attached:
            return False
        sub.quiescing = True
        logger.info("subscription_quiesced", topic=topic, group=group)
        return True

    async def detach(self, topic: str, group: str) -> bool:
        """Drop this process's consumers; the subscription and mail stay.

        The non-destructive half of the old ``unsubscribe``.  Undelivered
        events remain in the backlog for whoever attaches next, in order
        — the in-memory analogue of a broker cursor surviving a handoff.
        Pause holds are released with the attachment they described.
        """
        sub = self._subs.get((topic, group))
        if sub is None:
            return False
        if not sub.attached:
            sub.pauses.clear()
            sub.quiescing = False
            return False
        removed = len(sub.members)
        task, sub.flush_task = sub.flush_task, None
        if task is not None:
            task.cancel()
        sub.members.clear()
        sub.pauses.clear()
        sub.quiescing = False
        sub.cursor = 0
        logger.info(
            "subscription_detached", topic=topic, group=group, consumers=removed
        )
        return True

    async def ensure_subscription(self, topic: str, group: str) -> bool:
        """Create the durable subscription if absent, with no consumer."""
        if (topic, group) in self._subs:
            return False
        self._ensure(topic, group)
        logger.info("subscription_created", topic=topic, group=group)
        return True

    async def delete_subscription(self, topic: str, group: str) -> bool:
        """Delete the subscription and discard its retained mail."""
        await self.detach(topic, group)
        sub = self._subs.pop((topic, group), None)
        if sub is None:
            return False
        for event in sub.events:
            logger.info(
                "event_discarded",
                topic=topic,
                group=group,
                event_type=event.type,
                reason="subscription_deleted",
            )
        logger.info("subscription_deleted", topic=topic, group=group)
        return True

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

    @property
    def backend(self) -> str:
        return "memory"

    async def start(self) -> None:
        if self._running:
            return
        self._running = True
        self._paused = False
        logger.info("memory_event_queue_started")

    async def pause_delivery(self) -> None:
        """Stop dispatching new events to handlers (publishes still work).

        Undelivered events stay in their subscription's backlog, which is
        what the Pulsar backend achieves by leaving them on the broker.
        They are not lost; the next attachment gets them.
        """
        if self._paused:
            return
        self._paused = True
        logger.info("memory_event_queue_paused")

    async def pause_topic(
        self, topic: str, group: str, *, reason: str = "default"
    ) -> None:
        """Take one reason's hold on a subscription (buffer, don't drop)."""
        self._ensure(topic, group).pauses.add(reason)
        logger.info("memory_topic_paused", topic=topic, group=group, reason=reason)

    async def resume_topic(
        self, topic: str, group: str, *, reason: str = "default"
    ) -> None:
        """Release one reason's hold; drain the backlog when none remain."""
        sub = self._subs.get((topic, group))
        if sub is None or not sub.pauses:
            return
        sub.pauses.discard(reason)
        if sub.pauses:
            logger.info(
                "memory_topic_still_paused",
                topic=topic,
                group=group,
                released=reason,
                held_by=sorted(sub.pauses),
            )
            return
        logger.info(
            "memory_topic_resumed",
            topic=topic,
            group=group,
            reason=reason,
            backlog=len(sub.events),
        )
        await self._drain(sub)

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
        for sub in self._subs.values():
            task, sub.flush_task = sub.flush_task, None
            if task is not None:
                task.cancel()
            # Attachments are process state; the subscriptions and their
            # retained mail are not.  Clearing the holds matters: a hold
            # that outlived a stop left a reused queue silently deaf.
            sub.members.clear()
            sub.pauses.clear()
            sub.quiescing = False
            sub.cursor = 0
        logger.info("memory_event_queue_stopped")

    # -- internals --

    def _ensure(self, topic: str, group: str) -> _Subscription:
        sub = self._subs.get((topic, group))
        if sub is None:
            sub = _Subscription(topic=topic, group=group)
            self._subs[(topic, group)] = sub
        return sub

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

    async def _drain(self, sub: _Subscription) -> None:
        """Deliver from a subscription's backlog while it is deliverable.

        Re-checks deliverability every iteration, so a handler that
        re-pauses its own subscription — the sandbox busy gate's whole
        purpose — stops the drain at the next event rather than being
        run over.  Events published during a drain join the tail, so the
        backlog stays FIFO instead of being overtaken by new arrivals.
        """
        while sub.events and self._running and not self._paused and sub.deliverable:
            member = sub.members[sub.cursor % len(sub.members)]
            if member.batched:
                linger = (member.options or BatchOptions()).effective_linger_seconds
                if linger > 0:
                    # Hold the window open and stop draining. The window
                    # is fixed from the first waiting event (matching the
                    # Pulsar backend): later publishes join this backlog
                    # without resetting it.
                    if sub.flush_task is None or sub.flush_task.done():
                        sub.flush_task = asyncio.create_task(
                            self._flush_after_linger(sub, linger)
                        )
                    return
            sub.cursor += 1
            if member.batched:
                await self._deliver_batch(sub, member)
            else:
                await self._deliver_one(sub, member)

    async def _flush_after_linger(self, sub: _Subscription, linger: float) -> None:
        """Deliver a lingered batch once its window expires.

        A stop or a pause during the window does not lose anything any
        more: the events are in the backlog, which outlives both.
        """
        try:
            await asyncio.sleep(linger)
        except asyncio.CancelledError:
            sub.flush_task = None
            raise
        sub.flush_task = None
        if not self._running or self._paused or not sub.deliverable:
            logger.debug(
                "memory_linger_window_closed_undelivered",
                topic=sub.topic,
                group=sub.group,
                backlog=len(sub.events),
            )
            return
        # Deliver what the window collected, bypassing the linger check
        # that would otherwise re-open it immediately.
        while sub.events and self._running and not self._paused and sub.deliverable:
            member = sub.members[sub.cursor % len(sub.members)]
            sub.cursor += 1
            if member.batched:
                await self._deliver_batch(sub, member)
            else:
                await self._deliver_one(sub, member)

    async def _deliver_one(self, sub: _Subscription, member: _Consumer) -> None:
        event = sub.events.popleft()
        await self._invoke(
            sub,
            [event],
            lambda: member.handler(event),
            log_fields={
                "topic": sub.topic,
                "group": sub.group,
                "event_type": event.type,
            },
        )

    async def _deliver_batch(self, sub: _Subscription, member: _Consumer) -> None:
        """Take up to ``max_batch`` from the backlog and deliver by key.

        The drain pass is the twin of the Pulsar backend's zero-linger
        collection: everything already waiting coalesces into ONE
        delivery per conversation, which is the property inbox batching
        exists for — events that queued while an agent was busy must
        arrive as one turn, not N.
        """
        options = member.options or BatchOptions()
        max_batch = options.effective_max_batch
        chunk: list[Event] = []
        while sub.events and len(chunk) < max_batch:
            chunk.append(sub.events.popleft())
        if not chunk:
            return
        assert member.batch_key is not None
        parts = order_partitions_oldest_first(partition_by_key(chunk, member.batch_key))
        for key, part in parts:
            events = list(part)
            await self._invoke(
                sub,
                events,
                lambda events=events: member.handler(events),
                log_fields={
                    "topic": sub.topic,
                    "group": sub.group,
                    "batch_key": key,
                    "event_type": events[0].type,
                    "event_count": len(events),
                },
                # Same machine-parsable failure event the Pulsar backend
                # emits for batch partitions — log consumers must not see
                # different names per backend.
                failure_event="batch_handler_failed",
            )

    async def _invoke(
        self,
        sub: _Subscription,
        events: list[Event],
        call: Callable[[], Awaitable[None]],
        *,
        log_fields: dict[str, Any],
        failure_event: str = "handler_failed",
    ) -> None:
        """Run one delivery, applying the three handler outcomes.

        Returning acknowledges.  Raising negatively-acknowledges: the
        events go back to the FRONT of the backlog (order is what a
        conversation depends on) with their redelivery counters bumped,
        and a message past ``max_redeliveries`` moves to the dead-letter
        subject instead of being destroyed — the same fate the broker's
        dead-letter policy gives it.  Raising
        :class:`~crewlet.queue.protocol.DeferDelivery` puts them back
        without bumping anything and quiesces the subscription, because
        a seat whose lease moved is not a failed handler and must not
        spend the message's dead-letter budget.
        """
        self._in_flight += 1
        self._idle_event.clear()
        try:
            await call()
        except DeferDelivery as deferral:
            sub.quiescing = True
            sub.events.extendleft(reversed(events))
            logger.info(
                "delivery_deferred", reason=str(deferral) or "unspecified", **log_fields
            )
        except asyncio.CancelledError:
            # A cancelled handler has not done the work.  Put it back —
            # the Pulsar backend NAKs here — and let the cancellation
            # propagate to whoever asked for it.
            sub.events.extendleft(reversed(events))
            raise
        except Exception as exc:
            logger.warning(failure_event, error=str(exc), **log_fields)
            self._redeliver_or_dead_letter(sub, events)
        finally:
            self._in_flight -= 1
            if self._in_flight == 0:
                self._idle_event.set()

    def _redeliver_or_dead_letter(
        self, sub: _Subscription, events: list[Event]
    ) -> None:
        keep: list[Event] = []
        for event in events:
            count = sub.redeliveries.get(str(event.id), 0) + 1
            if count > self._max_redeliveries:
                sub.redeliveries.pop(str(event.id), None)
                logger.error(
                    "event_dead_lettered",
                    topic=sub.topic,
                    group=sub.group,
                    event_type=event.type,
                    redeliveries=count - 1,
                )
                self._dead_letters.setdefault(
                    dlq_topic(sub.topic, sub.group), []
                ).append(event)
                continue
            sub.redeliveries[str(event.id)] = count
            keep.append(event)
        sub.events.extendleft(reversed(keep))
