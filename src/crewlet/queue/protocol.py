"""EventQueue protocol — abstract interface for the persistent event queue."""

from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass
from typing import Any, Protocol

from crewlet._logging import get_logger
from crewlet.events.types import Event

logger = get_logger("queue.protocol")

# Hard ceiling on the batch linger window, enforced at the point of
# consumption (``BatchOptions.effective_linger_seconds``).  The linger
# counts against the broker ack-timeout budget that bounds a drained
# message's unacked lifetime (collection + ONE handler run must fit
# inside it — see the Pulsar backend's ``_BATCH_DISPATCH_BUDGET_MS``
# sizing); config-layer validation (``CompanyConfig``) mirrors this
# cap, but programmatic ``Engine(...)`` construction bypasses pydantic,
# so the invariant must hold here regardless of who set the field.
MAX_LINGER_SECONDS = 60.0

PublishListener = Callable[[str, Event], Awaitable[None]]
StreamHandler = Callable[[str, Event], Awaitable[None]]
StreamUnsubscribe = Callable[[], Awaitable[None]]
BatchHandler = Callable[[list[Event]], Awaitable[None]]
BatchKey = Callable[[Event], str]


@dataclass
class BatchOptions:
    """Mutable knobs for :meth:`EventQueue.subscribe_batch` delivery.

    Deliberately a plain mutable dataclass: the subscriber retains a
    reference and may mutate fields at runtime (live config reload);
    the consume loop reads them at the start of every collection
    cycle, so changes take effect on the next batch with no
    re-subscription.

    - ``linger_seconds`` — extra wait after the *first* received
      message to absorb a burst before dispatching.  ``0`` (default)
      still drains everything already fetched/queued locally, so
      backlog that accumulated while a previous handler ran coalesces
      with zero added latency; the window only matters when events
      trickle in while the consumer is idle.  The window is fixed
      (measured from the first message), not sliding — a steady
      trickle cannot delay dispatch unboundedly.
    - ``max_batch`` — hard cap on events collected per cycle.  A
      pathological backlog is delivered as successive capped batches
      rather than one unbounded one.

    Consume loops read through the ``effective_*`` accessors so the
    clamping rules live here once, not per backend.
    """

    linger_seconds: float = 0.0
    max_batch: int = 20

    @property
    def effective_linger_seconds(self) -> float:
        """``linger_seconds`` clamped to ``[0, MAX_LINGER_SECONDS]``."""
        return min(max(0.0, float(self.linger_seconds)), MAX_LINGER_SECONDS)

    @property
    def effective_max_batch(self) -> int:
        """``max_batch`` clamped to its valid range (>= 1)."""
        return max(1, int(self.max_batch))


def partition_by_key[T](
    items: Sequence[T],
    batch_key: BatchKey,
    *,
    event_of: Callable[[T], Event] | None = None,
) -> list[tuple[str, list[T]]]:
    """Group *items* by ``batch_key``, preserving arrival order.

    Returns partitions in first-arrival order with arrival order
    preserved inside each; DISPATCH order is a separate policy —
    backends reorder via :func:`order_partitions_oldest_first`.  A
    ``batch_key`` failure falls back to a unique per-event key so key
    derivation can never block delivery.

    ``event_of`` extracts the :class:`Event` from an item when items
    aren't bare events (the Pulsar backend partitions
    ``(message, event)`` pairs so ack bookkeeping rides along).
    """
    partitions: dict[str, list[T]] = {}
    for item in items:
        event = event_of(item) if event_of is not None else item
        try:
            key = batch_key(event)  # type: ignore[arg-type]
        except Exception:
            logger.exception("batch_key_failed", event_type=getattr(event, "type", ""))
            key = f"event:{getattr(event, 'id', '')}"
        partitions.setdefault(key, []).append(item)
    return list(partitions.items())


def order_partitions_oldest_first[T](
    parts: list[tuple[str, list[T]]],
    *,
    event_of: Callable[[T], Event] | None = None,
) -> list[tuple[str, list[T]]]:
    """Dispatch order for partitions: oldest constituent event first.

    Receive order alone starves a quiet conversation behind a hot one
    under deferral: the quiet conversation's requeued copies re-enter
    the topic AFTER whatever arrived during the hot conversation's
    turn, so receive-ordered dispatch picks the hot conversation first
    on every drain.  Event timestamps survive requeue by design, so
    they carry the aging signal — the conversation that has waited
    longest dispatches first, and deferred conversations win priority
    over fresh arrivals on the next drain.  Stable sort: ties keep
    arrival order.  Falls back to arrival order when timestamps are
    incomparable (e.g. a malformed naive-datetime constituent) —
    ordering is a fairness policy and must never block delivery.
    """

    def _oldest(entry: tuple[str, list[T]]) -> Any:
        _, part_items = entry
        return min(
            (event_of(item) if event_of is not None else item).timestamp  # type: ignore[union-attr]
            for item in part_items
        )

    try:
        return sorted(parts, key=_oldest)
    except Exception:
        logger.exception("partition_ordering_failed")
        return parts


class EventQueue(Protocol):
    """Abstract interface for the persistent event queue."""

    async def publish(self, topic: str, event: Event) -> None:
        """Publish an event to a topic. Must persist before returning."""
        ...

    async def subscribe(
        self,
        topic: str,
        group: str,
        handler: Callable[[Event], Awaitable[None]],
    ) -> None:
        """Subscribe to a topic within a consumer group.

        - ``group`` enables competing-consumer semantics (only one member of
          the group receives each message).
        - Messages are acknowledged after the handler returns successfully.
          On handler failure, the message is redelivered.
        """
        ...

    async def subscribe_batch(
        self,
        topic: str,
        group: str,
        handler: BatchHandler,
        *,
        batch_key: BatchKey,
        options: BatchOptions,
    ) -> None:
        """Subscribe with batched, key-partitioned delivery.

        Like :meth:`subscribe` (durable consumer group, at-least-once),
        but instead of one handler call per message the consume loop:

        1. **Drains** — after the first message arrives, collects
           everything immediately available, plus anything arriving
           within ``options.linger_seconds`` of the first message, up
           to ``options.max_batch`` events.
        2. **Partitions** — groups the collected events by
           ``batch_key(event)``, preserving arrival order within each
           partition.  A ``batch_key`` failure falls back to a
           per-event unique key — key derivation must never block
           delivery.
        3. **Dispatches** — invokes ``handler(events)`` once per
           partition, sequentially, **oldest conversation first** (by
           oldest constituent event timestamp, via
           :func:`order_partitions_oldest_first`; arrival order breaks
           ties and is the fallback for incomparable timestamps) — so
           a deferred conversation ages into priority and steady
           inflow on a hot conversation cannot starve it.
        4. **Acks per partition** — a partition's messages are
           acknowledged only after its handler returns; on handler
           failure exactly that partition's messages are negatively
           acknowledged (normal redelivery / dead-letter policy
           applies per message).  A failing partition never blocks or
           replays a different partition from the same drain.

        Backends with broker-side ack timeouts may **defer** later
        partitions from one drain instead of dispatching them all:
        once the drain has spent its dispatch budget running handlers,
        remaining partitions are requeued (republished, then the
        originals acked) so no message sits delivered-but-unacked for
        the sum of preceding handlers' runtimes.  Deferred events are
        redelivered promptly with their identity (id / timestamp /
        trace) intact and re-partition on a later drain — the handler
        still sees one batch per conversation, just not necessarily in
        the same drain it was fetched in.

        ``options`` is read live on every cycle — mutating its fields
        (live config reload) takes effect on the next batch.

        Used by the engine's agent-inbox subscriptions so events for
        the same conversation that queued up while the agent was busy
        are handled as one coalesced batch.  See
        ``docs/concepts/event-system.md`` § Inbox batching.
        """
        ...

    async def unsubscribe(self, topic: str, group: str) -> None:
        """Tear down the durable group consumer(s) for ``topic``/``group``.

        Stops the consume loop(s) this process created for that pair AND
        deletes the broker-side subscription, so retained messages for the
        group are dropped — the semantics a decommissioned role needs (its
        inbox must not accumulate undeliverable events forever). Idempotent:
        unknown pairs are a no-op. Only affects consumers created by
        :meth:`subscribe` / :meth:`subscribe_batch`; broadcast stream
        subscriptions return their own unsubscribe callable.
        """
        ...

    async def subscribe_stream(
        self,
        topic_pattern: str,
        handler: StreamHandler,
    ) -> StreamUnsubscribe:
        """Broadcast subscription for live-stream consumers.

        Unlike :meth:`subscribe` which uses durable consumer groups and
        competing-consumer semantics, ``subscribe_stream`` creates an
        ephemeral subscription unique to this caller — every subscriber
        receives every matching event.  Designed for dashboards / live
        log views where messages are pushed and durability is not
        required.

        ``topic_pattern`` supports subject wildcards: ``*`` matches a
        single subject segment, ``>`` matches one-or-more trailing
        segments.

        Returns an async callable that cancels the subscription and
        releases backend resources.  Implementations should make the
        consumer ephemeral so it cleans up automatically when the
        process exits, even if the caller forgets to invoke the
        returned unsubscribe.
        """
        ...

    def add_publish_listener(self, listener: PublishListener) -> None:
        """Register a listener called on every publish (topic, event).

        Listeners are invoked inline during ``publish()`` — they run in
        the same coroutine and should not block for long.  Exceptions in
        listeners are logged but do not prevent the publish.
        """
        ...

    async def start(self) -> None:
        """Connect to the backend and begin consuming."""
        ...

    @property
    def in_flight_count(self) -> int:
        """Number of handler invocations currently mid-flight.

        Useful as a runtime-only metric (it's not event-derived) for
        operators watching a graceful shutdown drain converge to 0 or
        spotting a stuck-handler regression in normal operation.
        """
        ...

    @property
    def backend(self) -> str:
        """Stable lowercase backend name, for operator display only.

        Declared on the Protocol rather than sniffed from the class
        name downstream: a name-sniff lies the moment a queue is wrapped
        or a third party implements this interface, and the Protocol is
        where this codebase states what a provider can answer.  It is
        NOT a capability flag -- nothing may branch behaviour on it.
        """
        ...

    async def pause_delivery(self) -> None:
        """Stop dispatching new events to handlers.

        Pause is the first step of graceful shutdown: no new messages
        are delivered to subscribers, but ``publish()`` still works so
        in-flight handlers can emit their terminal events
        (``TaskCompleted`` / ``TaskFailed`` / ``OrgStopped``) before
        the queue is fully torn down.  Pause is one-way -- there is no
        ``resume_delivery``; once paused, the engine is shutting down.

        Messages that were already fetched at the moment ``pause_delivery``
        is called are still run to completion (the in-flight count is
        observable via :meth:`wait_for_handlers`).  New messages stay
        queued in the backend (Pulsar) and are delivered to the next
        engine that subscribes -- typically the next process after a
        restart.
        """
        ...

    async def pause_topic(self, topic: str, *, reason: str = "default") -> None:
        """Pause delivery of ONE topic's messages to its handlers.

        Used by the sandbox subsystem to keep an agent "busy" while a
        detached coding job runs: the agent's inbox
        is paused so new task / notification events stay queued (broker
        backlog / in-memory buffer) instead of starting a fresh turn
        mid-job. Unlike :meth:`pause_delivery` this is per-topic and
        reversible via :meth:`resume_topic`. Idempotent.

        **Pauses are reason-scoped and a topic stays paused while ANY
        reason holds it.** Two independent subsystems gate the same
        inbox — the sandbox busy gate and the config-divergence shed —
        and with one flat set the sandbox resuming its own run would
        un-gate a node that is serving a stale company, on a completely
        ordinary code path.
        """
        ...

    async def resume_topic(self, topic: str, *, reason: str = "default") -> None:
        """Release ONE reason's hold on a topic; flush if none remain.

        Buffered (in-memory) / queued (broker) messages are delivered to
        the topic's handlers again once no reason holds it. Idempotent —
        releasing a hold that was never taken is a no-op.
        """
        ...

    async def wait_for_handlers(self, timeout: float | None = None) -> int:
        """Wait for all in-flight handler invocations to complete.

        Returns the number of handlers still running when the wait
        ended.  ``0`` means a clean drain.  A non-zero return means
        ``timeout`` expired with handlers still mid-flight — not an
        error: the engine's graceful drain calls this in a loop with a
        short timeout to emit periodic ``drain_in_progress`` logs, so
        any "took too long" policy belongs to the caller.

        Always pause delivery first (:meth:`pause_delivery`) before
        calling this; otherwise new handlers keep starting and the
        drain never converges.
        """
        ...

    async def stop(self) -> None:
        """Close the connection and disconnect."""
        ...
