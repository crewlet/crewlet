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
- **A broker and a client are different things.**  One
  :class:`MemoryEventQueue` is both by default, which is right for one
  process and wrong for a fleet: attachments, pause holds, quiesce flags
  and the drain pause belong to a *node*, while subscriptions and the
  mail in them belong to the *broker*.  Conflating them meant one node's
  ``detach`` dropped its peer's consumer and one node's sandbox pause
  stopped its peer serving a seat it owned.  Call
  :meth:`MemoryEventQueue.client` for a second node on the same broker;
  the single-process case is unchanged, because it is a fleet of one.

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
    client: Any = None
    """The :class:`MemoryEventQueue` this consumer belongs to.

    A subscription is broker state and is shared by every client; an
    attachment is not.  Without the back-reference ``detach`` could only
    mean "drop everyone", which in a fleet is one node tearing down its
    peer's consumer — and ``attachments()`` could only answer a
    fleet-wide question when the operationally interesting one is "what
    is THIS node serving?"."""
    #: ``(topic, group)`` — the key this attachment's holds are under.
    topic_key: tuple[str, str] = ("", "")
    batched: bool = False
    batch_key: Callable[[Event], str] | None = None
    options: BatchOptions | None = None
    #: Open linger window for a batch consumer.  Per consumer, not per
    #: subscription: two clients batching the same seat linger
    #: independently, exactly as two processes would.
    flush_task: asyncio.Task[None] | None = None


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
    #: Round-robin cursor across deliverable ``members`` — competing
    #: consumers.
    cursor: int = 0
    #: Redeliveries accrued per event id, mirroring the broker's counter.
    redeliveries: dict[str, int] = field(default_factory=dict)

    @property
    def attached(self) -> bool:
        return bool(self.members)

    def members_of(self, client: Any) -> list[_Consumer]:
        return [m for m in self.members if m.client is client]


@dataclass
class _Broker:
    """The state a fleet SHARES: subscriptions, mail, dead letters.

    Split out from :class:`MemoryEventQueue` because that class plays
    two roles the real deployment keeps apart — it is both the broker and
    the client.  For one process the conflation is invisible.  For two it
    inverts the property this backend exists to model: a node's
    ``detach`` dropped its peer's consumer, and "which seats is this node
    attached to?" could only be answered fleet-wide.

    Pass one broker to several :class:`MemoryEventQueue` instances (see
    :meth:`MemoryEventQueue.client`) and they become peers on one broker,
    with independent attachments, pauses and lifecycles.
    """

    subs: dict[tuple[str, str], _Subscription] = field(default_factory=dict)
    history: deque[Event] = field(default_factory=lambda: deque(maxlen=10000))
    dead_letters: dict[str, list[Event]] = field(default_factory=dict)


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

    def __init__(
        self,
        *,
        # In lockstep with the Pulsar backend's ``_MAX_REDELIVER`` — see
        # its comment for why ten. The twin must not disagree about a
        # budget that decides whether a healthy event lives or dies.
        max_redeliveries: int = 10,
        max_history: int = 10000,
        broker: _Broker | None = None,
    ) -> None:
        # Broker state: subscriptions, retained mail, dead letters.
        # Shared with every peer built from the same broker.
        self._broker = broker if broker is not None else _Broker()
        if broker is None:
            self._broker.history = deque(maxlen=max_history)
        self._stream_subscriptions: list[_StreamSubscription] = []
        self._publish_listeners: list[Callable[[str, Event], Awaitable[None]]] = []
        self._running = False
        self._paused = False
        self._max_redeliveries = max_redeliveries
        # Per-attachment holds and quiesce flags, keyed by
        # ``(topic, group)`` — the twin of the Pulsar backend's
        # ``_paused_subs``, and process-local for the same reason: a hold
        # describes THIS node's consumer, and a hold that gated the
        # subscription itself would make one node's sandbox pause stop
        # its peers from serving their own seats.
        self._pauses: dict[tuple[str, str], set[str]] = {}
        self._quiescing: set[tuple[str, str]] = set()
        # In-flight handler tracking, used by ``wait_for_handlers`` so
        # the engine's graceful-shutdown drain can wait for currently-
        # running handlers to complete.  Inline dispatch means the
        # counter is only non-zero while ``publish`` is awaiting a
        # handler -- but the same drain API stays consistent with the
        # Pulsar implementation.
        self._in_flight = 0
        self._idle_event = asyncio.Event()
        self._idle_event.set()

    def client(self) -> MemoryEventQueue:
        """A second client on the SAME broker — another node.

        Returned already stopped, like any freshly constructed queue: it
        is a separate process's connection, so it starts and stops on its
        own schedule.  Its attachments, pause holds and drain state are
        its own; the subscriptions and the mail in them are shared,
        because those live on the broker.
        """
        return MemoryEventQueue(
            max_redeliveries=self._max_redeliveries, broker=self._broker
        )

    @property
    def _subs(self) -> dict[tuple[str, str], _Subscription]:
        return self._broker.subs

    @property
    def _dead_letters(self) -> dict[str, list[Event]]:
        return self._broker.dead_letters

    def add_publish_listener(
        self, listener: Callable[[str, Event], Awaitable[None]]
    ) -> None:
        self._publish_listeners.append(listener)

    @property
    def history(self) -> list[Event]:
        """All events published to this broker (for testing/debugging)."""
        return list(self._broker.history)

    def backlog(self, topic: str, group: str) -> list[Event]:
        """Events a subscription retains and has not yet delivered.

        Test-facing: the assertion "an unowned seat's mail survived" has
        to be able to look at the mail.
        """
        sub = self._subs.get((topic, group))
        return list(sub.events) if sub is not None else []

    def attachments(self) -> list[tuple[str, str]]:
        """Every ``(topic, group)`` pair THIS client has attached to.

        Public because "which seats is this node serving?" is the
        question seat ownership makes operationally central, and a test
        should not have to read the backend's registry to ask it.

        Scoped to this client, not the broker: a peer's attachment is its
        own business, and answering fleet-wide would make "attached to
        exactly the seats I own" untestable — the assertion that catches
        the double-consumer split-brain.
        """
        return [k for k, sub in self._subs.items() if sub.members_of(self)]

    def pause_holds(self, topic: str, group: str) -> set[str]:
        """Reasons currently holding THIS client's attachment paused.

        Public because a hold is operator-visible behaviour — which
        subsystem is gating a seat is the first question when one goes
        quiet — and because tests must not reach into the backend's
        internals to ask.
        """
        return set(self._pauses.get((topic, group), ()))

    def dead_letters(self, topic: str, group: str) -> list[Event]:
        """Events this subscription gave up on. See :func:`dlq_topic`."""
        return list(self._dead_letters.get(dlq_topic(topic, group), []))

    # -- protocol methods --

    async def publish(self, topic: str, event: Event) -> None:
        if not self._running:
            raise RuntimeError("MemoryEventQueue is not started")
        self._broker.history.append(event)
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
        sub.members.append(
            _Consumer(handler=handler, client=self, topic_key=(topic, group))
        )
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
                handler=handler,
                client=self,
                topic_key=(topic, group),
                batched=True,
                batch_key=batch_key,
                options=options,
            )
        )
        logger.debug("batch_subscription_added", topic=topic, group=group)
        await self._drain(sub)

    async def quiesce(self, topic: str, group: str) -> bool:
        """Stop taking new work; keep the attachment and the backlog."""
        sub = self._subs.get((topic, group))
        if sub is None or not sub.members_of(self):
            return False
        self._quiescing.add((topic, group))
        logger.info("subscription_quiesced", topic=topic, group=group)
        return True

    async def unquiesce(self, topic: str, group: str) -> bool:
        """Resume a quiesced attachment and drain what it held back."""
        if (topic, group) not in self._quiescing:
            return False
        self._quiescing.discard((topic, group))
        logger.info("subscription_unquiesced", topic=topic, group=group)
        sub = self._subs.get((topic, group))
        if sub is not None:
            await self._drain(sub)
        return True

    async def detach(self, topic: str, group: str) -> bool:
        """Drop THIS client's consumers; the subscription and mail stay.

        The non-destructive half of the old ``unsubscribe``.  Undelivered
        events remain in the backlog for whoever attaches next, in order
        — the in-memory analogue of a broker cursor surviving a handoff.
        Pause holds are released with the attachment they described.

        A peer's consumer on the same subscription is untouched, which is
        the whole point of the distinction: detaching is a node saying "I
        have stopped serving this seat", never "nobody is serving it".
        """
        self._pauses.pop((topic, group), None)
        self._quiescing.discard((topic, group))
        sub = self._subs.get((topic, group))
        if sub is None:
            return False
        mine = sub.members_of(self)
        if not mine:
            return False
        for member in mine:
            task, member.flush_task = member.flush_task, None
            if task is not None:
                task.cancel()
        sub.members = [m for m in sub.members if m.client is not self]
        if not sub.members:
            sub.cursor = 0
        logger.info(
            "subscription_detached", topic=topic, group=group, consumers=len(mine)
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
        """Take one reason's hold on this client's attachment."""
        self._ensure(topic, group)
        self._pauses.setdefault((topic, group), set()).add(reason)
        logger.info("memory_topic_paused", topic=topic, group=group, reason=reason)

    async def resume_topic(
        self, topic: str, group: str, *, reason: str = "default"
    ) -> None:
        """Release one reason's hold; drain the backlog when none remain."""
        held = self._pauses.get((topic, group))
        if not held:
            return
        held.discard(reason)
        if held:
            logger.info(
                "memory_topic_still_paused",
                topic=topic,
                group=group,
                released=reason,
                held_by=sorted(held),
            )
            return
        self._pauses.pop((topic, group), None)
        sub = self._subs.get((topic, group))
        if sub is None:
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
        """Close THIS client. The broker, and every peer, live on.

        Attachments, pause holds and quiesce flags are process state; the
        subscriptions and their retained mail are not.  Clearing the
        holds matters: a hold that outlived a stop left a reused queue
        silently deaf.
        """
        if not self._running:
            return
        self._running = False
        self._paused = False
        for key, sub in list(self._subs.items()):
            mine = sub.members_of(self)
            if not mine:
                continue
            for member in mine:
                task, member.flush_task = member.flush_task, None
                if task is not None:
                    task.cancel()
            sub.members = [m for m in sub.members if m.client is not self]
            if not sub.members:
                sub.cursor = 0
            del key
        self._pauses.clear()
        self._quiescing.clear()
        self._stream_subscriptions.clear()
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

    def _deliverable(self, member: _Consumer) -> bool:
        """Can this attachment take a delivery right now?

        Asked of the CONSUMER, not the subscription, because every gate
        here belongs to a process: its queue must be started and not
        drain-paused, and its own hold and quiesce flags must be clear.
        A subscription-level answer would let one node's sandbox pause,
        or one node's shutdown, stop a peer from serving the seat it
        owns.
        """
        client = member.client
        if client is None or not client._running or client._paused:
            return False
        key = member.topic_key
        return not client._pauses.get(key) and key not in client._quiescing

    def _deliverable_members(self, sub: _Subscription) -> list[_Consumer]:
        return [m for m in sub.members if self._deliverable(m)]

    async def _drain(self, sub: _Subscription) -> None:
        """Deliver from a subscription's backlog while someone can take it.

        Re-checks deliverability every iteration, so a handler that
        re-pauses its own subscription — the sandbox busy gate's whole
        purpose — stops the drain at the next event rather than being
        run over.  Events published during a drain join the tail, so the
        backlog stays FIFO instead of being overtaken by new arrivals.

        The round robin runs over the members that CAN take a delivery,
        so one paused node does not stall a seat another node is serving
        — and a subscription with no deliverable member simply retains
        its mail, which is the unowned-seat case.
        """
        while sub.events:
            members = self._deliverable_members(sub)
            if not members:
                return
            member = members[sub.cursor % len(members)]
            if member.batched:
                linger = (member.options or BatchOptions()).effective_linger_seconds
                if linger > 0:
                    # Hold the window open and stop draining. The window
                    # is fixed from the first waiting event (matching the
                    # Pulsar backend): later publishes join this backlog
                    # without resetting it.
                    if member.flush_task is None or member.flush_task.done():
                        member.flush_task = asyncio.create_task(
                            self._flush_after_linger(sub, member, linger)
                        )
                    return
            sub.cursor += 1
            if member.batched:
                await self._deliver_batch(sub, member)
            else:
                await self._deliver_one(sub, member)

    async def _flush_after_linger(
        self, sub: _Subscription, opener: _Consumer, linger: float
    ) -> None:
        """Deliver a lingered batch once its window expires.

        A stop or a pause during the window does not lose anything any
        more: the events are in the backlog, which outlives both.
        """
        try:
            await asyncio.sleep(linger)
        except asyncio.CancelledError:
            opener.flush_task = None
            raise
        opener.flush_task = None
        if not self._deliverable(opener):
            logger.debug(
                "memory_linger_window_closed_undelivered",
                topic=sub.topic,
                group=sub.group,
                backlog=len(sub.events),
            )
            return
        # Deliver what the window collected, bypassing the linger check
        # that would otherwise re-open it immediately.
        while sub.events:
            members = self._deliverable_members(sub)
            if not members:
                return
            member = members[sub.cursor % len(members)]
            sub.cursor += 1
            if member.batched:
                await self._deliver_batch(sub, member)
            else:
                await self._deliver_one(sub, member)

    async def _deliver_one(self, sub: _Subscription, member: _Consumer) -> None:
        event = sub.events.popleft()
        await self._invoke(
            sub,
            member,
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
        # Everything still queued is BEHIND this chunk. Remembered so a
        # mid-batch quiesce can tell what a deferral pushed back to the
        # front from what was already there, and slot the undispatched
        # partitions between them.
        tail_before = len(sub.events)
        for index, (key, part) in enumerate(parts):
            if member.client is not None and member.topic_key in (
                member.client._quiescing
            ):
                # Quiesced — by an earlier partition in this very batch,
                # or between batches. Stop dispatching and put the rest
                # back for whoever attaches next. The Pulsar backend
                # checks exactly here (``_process_batch``), and the twin
                # not doing so was worse than a missing guard: after one
                # partition deferred, the loop went on invoking the
                # handler for partitions 2..N on a seat this node had
                # just been told it does not own, and each deferral did
                # ``extendleft`` — so the backlog came back in REVERSE
                # partition order, which is precisely the reordering
                # ``DeferDelivery`` exists to prevent. ``tests/test_fleet``
                # runs against this twin as its "the same suite passes on
                # the twin" criterion, so it would have certified an
                # ordering guarantee the real broker does not give.
                remaining: list[Event] = []
                for _key, rest in parts[index:]:
                    remaining.extend(rest)
                # After the deferring partition, not before it. ``_invoke``
                # has already pushed that partition back to the front, so
                # an ``extendleft`` here would put these ahead of it and
                # reverse the very order this guard exists to keep.
                queued = list(sub.events)
                restored = len(queued) - tail_before
                sub.events.clear()
                sub.events.extend(queued[:restored] + remaining + queued[restored:])
                return
            events = list(part)
            await self._invoke(
                sub,
                member,
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
        member: _Consumer,
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
            # Quiesce the ATTACHMENT that deferred, not the subscription:
            # a seat whose lease moved is not owned by this node, and
            # stopping the subscription would also stop the peer that now
            # owns it from picking these very events up.
            if member.client is not None:
                member.client._quiescing.add(member.topic_key)
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
