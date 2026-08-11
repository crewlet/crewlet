"""Apache Pulsar implementation of EventQueue.

Pulsar is the production event-queue backend.  The engine speaks a
single internal subject language (dot-separated subjects with ``*`` /
``>`` wildcards — see :mod:`crewlet.queue.matching`); this module
translates that language onto Pulsar's topic + subscription model:

- A subject (e.g. ``crewlet.agent.alice.inbox``) maps to a persistent
  Pulsar topic ``persistent://{tenant}/{namespace}/{subject}``.  The
  engine never talks to the admin API; a non-default tenant/namespace
  must exist before start (created out-of-band with ``pulsar-admin``).
- :meth:`subscribe` (competing consumers within a ``group``) maps to a
  **Shared** subscription whose name is the group — every member of the
  group shares one cursor, so each message goes to exactly one member.
- :meth:`subscribe_stream` (broadcast — every subscriber sees every
  event) maps to a per-caller subscription over a **regex topic
  pattern**, started at the latest message.

The official ``pulsar-client`` is a synchronous C++ wrapper with no
native asyncio.  One-shot blocking calls (client/producer/consumer
creation, teardown) are bridged via :func:`asyncio.to_thread`; each
consumer's long-lived blocking ``receive()`` poll runs on its own
dedicated single-thread executor so it never competes for the shared
default thread pool (which MCP servers and other ``to_thread`` callers
need) or starves the other consumers.  Publishes use ``send_async``
marshalled back onto the loop with ``call_soon_threadsafe``;
acknowledgements are local, non-blocking operations called directly.

The client's own (C++) logs are routed through Python logging via
:func:`_client_logger`, which drops two benign, self-resolving timeout
warnings the client would otherwise flood: the per-poll "Receive timeout"
(one per consumer per second on an idle topic) and the zero-pending
"Send timeout due to queueing delay" (a transient queueing delay that had
already cleared).  Real failures are still surfaced.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
import re
import time
from collections.abc import Awaitable, Callable, Iterator
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from typing import Any
from uuid import uuid4

import pulsar

from crewlet._logging import get_logger
from crewlet.events.types import Event
from crewlet.queue.protocol import (
    BatchOptions,
    order_partitions_oldest_first,
    partition_by_key,
)
from crewlet.queue.serialization import deserialize_event, serialize_event

logger = get_logger("queue.pulsar")

# Default Pulsar tenant + namespace that hold all crewlet topics.  The
# built-in ``public``/``default`` pair ships enabled (with
# auto-topic-creation) in a standalone broker, so the dev compose stack
# works with no admin bootstrap.  The engine never talks to the admin
# API: Pulsar auto-creates *topics*, never tenants or namespaces, so a
# custom tenant/namespace must be provisioned out-of-band
# (``pulsar-admin tenants create`` / ``namespaces create``) before the
# engine starts.
_DEFAULT_TENANT = "public"
_DEFAULT_NAMESPACE = "default"
_DEFAULT_URL = "pulsar://localhost:6650"

# Time a consumer waits for the handler to ack a delivered message before
# redelivering it.  Must exceed the worst-case handler runtime; agent
# turns can comfortably exceed Pulsar's short defaults, and a redelivery
# during a still-running turn produces a *second* identical turn after
# the first finishes -- the same-event-twice loop pattern.  30 min: the
# handler can now hold a delivery for a WAIT on the previous turn PLUS its
# own turn (``TurnEngine.run_turn`` parks on a busy agent instead of
# erroring), and a single worst-case turn is ~20 min under the Execute
# extension ceiling (40 rounds); two back-to-back fit under 30.  Runs that
# park for hours (a detached sandbox job) never hold a delivery at all --
# the engine requeues + acks those (AWAITING_SANDBOX parking).
_INBOX_ACK_TIMEOUT_MS = 1_800_000

# Cap redelivery attempts so a handler that genuinely keeps failing (or a
# poison message) cannot loop forever; after this many redeliveries the
# message is routed to the subscription's dead-letter topic.  ``3`` is
# the same compromise the rest of the engine uses for retry caps.
_INBOX_MAX_REDELIVER = 3

# Delay before a negatively-acknowledged message is redelivered.  Pulsar
# defaults this to 60s; we drop it to 1s so a failed handler retries
# promptly and so a turn NAK'd during force-stop comes back to the next
# engine subscription quickly instead of waiting out the ack-timeout
# window.
_NEG_ACK_REDELIVERY_DELAY_MS = 1000

# Publish resilience.  A momentarily slow or unreachable broker must not
# drop an event or wedge the engine.  Producers are created with
# ``block_if_queue_full=False`` so a stalled broker makes ``send_async``
# raise *here* (handled by the retry below) instead of blocking the
# asyncio event loop -- a blocked loop freezes every agent turn and the
# embedded API ("lost the server" on a slow-broker startup burst).
# Transient send failures (send timeout / queueing delay / full producer
# queue) are retried with exponential backoff before giving up.
_PUBLISH_MAX_ATTEMPTS = 5
_PUBLISH_RETRY_BASE_DELAY = 0.5

# Blocking ``receive`` timeout.  Short so the consume loop stays
# responsive to ``pause_delivery`` / ``stop`` between fetches.
_RECEIVE_TIMEOUT_MS = 1000

# Zero-linger drain ``receive`` timeout for batch subscriptions.  After
# the first message of a batch, a single drain pass collects whatever
# the consumer has already fetched into its local receiver queue; the
# first poll that comes back empty ends the pass.  Kept short — it is
# the only added latency a single-message batch pays.
_DRAIN_RECEIVE_TIMEOUT_MS = 50

# How long one batch drain may keep dispatching partition handlers
# before the remaining partitions are DEFERRED (requeued: republished,
# then the originals acked) instead of dispatched.  Every drained
# message's ack-timeout clock starts at receive; a partition handler is
# typically a full multi-minute agent turn, so dispatching a long tail
# of partitions sequentially would hold later messages
# delivered-but-unacked for the SUM of preceding turns, blow through
# ``_INBOX_ACK_TIMEOUT_MS`` mid-drain, and produce redelivered
# duplicate turns (plus spurious DLQ routing after the redeliver cap).
# Deferral re-enters those messages with fresh ack clocks and zero
# accrued redeliveries.  The budget plus ONE worst-case handler run
# must stay comfortably inside the ack timeout: 60s + a ~5-minute worst
# turn ≈ 6min against the 10-minute window.
_BATCH_DISPATCH_BUDGET_MS = 60_000

# ``pulsar.Timeout`` is raised by ``receive`` when no message arrives
# within the timeout.  Resolve it once; fall back to a message-substring
# check if a client build doesn't expose the symbol.
_PULSAR_TIMEOUT: type[BaseException] | None = getattr(pulsar, "Timeout", None)


def _is_timeout(exc: BaseException) -> bool:
    """True when *exc* is Pulsar's "no message before timeout" signal."""
    if _PULSAR_TIMEOUT is not None and isinstance(exc, _PULSAR_TIMEOUT):
        return True
    return "timeout" in str(exc).lower()


class _DropBenignClientTimeouts(logging.Filter):
    """Drop the C++ client's benign, self-resolving timeout warnings.

    Two are pure noise for our usage and would otherwise flood the logs:

    * ``Receive timeout`` -- we poll ``receive(_RECEIVE_TIMEOUT_MS)`` so
      the consume loop stays responsive to pause/stop; the client logs a
      WARN on every poll that finds no message (one line per consumer per
      second on an idle topic).
    * ``Send timeout due to queueing delay ... pending messages: 0`` -- a
      transient connection queueing delay that had already cleared (zero
      pending) by the time the timer logged, i.e. the message *was*
      delivered.  Genuine send failures (pending > 0) are kept, and are
      also retried + surfaced by ``publish`` itself.

    Every other client log passes through.
    """

    def filter(self, record: logging.LogRecord) -> bool:
        msg = record.getMessage()
        if "Receive timeout" in msg:
            return False
        return not (
            "Send timeout due to queueing delay" in msg and "pending messages: 0" in msg
        )


def _client_logger() -> logging.Logger:
    """Python logger handed to ``pulsar.Client`` for its C++ log output.

    Routing the client's logs through Python logging integrates them with
    the app's handlers (structlog).  WARN+ only -- the client is chatty at
    INFO (per-connection lifecycle) -- with the benign self-resolving
    receive/send timeout noise filtered out.
    """
    log = logging.getLogger("crewlet.queue.pulsar.client")
    log.setLevel(logging.WARNING)
    if not any(isinstance(f, _DropBenignClientTimeouts) for f in log.filters):
        log.addFilter(_DropBenignClientTimeouts())
    return log


@dataclass
class _Subscription:
    """A live consumer: its consume task, the Pulsar consumer, and the
    dedicated single-thread executor running its blocking ``receive()``.

    ``topic`` / ``group`` identify the durable pair so :meth:`unsubscribe`
    can find and tear the consumer down (stream subscriptions leave them
    empty — they return their own unsubscribe callable instead)."""

    task: asyncio.Task[None]
    consumer: Any
    executor: ThreadPoolExecutor
    topic: str = ""
    group: str = ""


class PulsarEventQueue:
    """EventQueue backed by Apache Pulsar.

    Uses Shared subscriptions with explicit ack for at-least-once
    competing-consumer delivery, and regex pattern subscriptions for
    broadcast stream consumers.
    """

    def __init__(
        self,
        url: str = _DEFAULT_URL,
        *,
        tenant: str = _DEFAULT_TENANT,
        namespace: str = _DEFAULT_NAMESPACE,
        auth_token: str = "",
        tls_trust_certs_path: str = "",
    ) -> None:
        self._url = url
        self._tenant = tenant
        self._namespace = namespace
        self._auth_token = auth_token
        self._tls_trust_certs_path = tls_trust_certs_path
        # ``tenant/namespace`` path segment used in every topic name.
        self._ns_path = f"{tenant}/{namespace}"
        self._client: pulsar.Client | None = None
        # Producers are cached per full topic name; creating one is a
        # network round-trip so we do it lazily and reuse.
        self._producers: dict[str, Any] = {}
        self._producer_lock = asyncio.Lock()
        # Live competing-consumer subscriptions (subscribe()).
        self._subscriptions: list[_Subscription] = []
        # Live broadcast subscriptions (subscribe_stream()).  Tracked so
        # ``stop`` can cancel each consume loop, delete the ephemeral
        # subscription server-side, and shut its receive thread down
        # rather than leaving them to age out on the broker.
        self._stream_subscriptions: list[_Subscription] = []
        self._publish_listeners: list[Callable[[str, Event], Awaitable[None]]] = []
        self._running = False
        self._paused = False
        # Per-topic pause (sandbox busy gate): a paused
        # topic's consume loop stops FETCHING, so its messages stay on the
        # broker (no NAK churn) until ``resume_topic`` lets fetching
        # resume.  Reversible, unlike the one-way drain ``pause_delivery``.
        self._paused_topics: set[str] = set()
        # In-flight handler tracking: incremented before each handler
        # invocation, decremented in ``finally`` so the counter is exact
        # under exceptions.  ``wait_for_handlers`` waits on ``_idle_event``
        # so the engine's graceful-shutdown drain has a precise signal.
        self._in_flight = 0
        self._idle_event = asyncio.Event()
        self._idle_event.set()

    # -- topic / pattern translation --

    def _full_topic(self, subject: str) -> str:
        """Map an internal subject to a fully-qualified Pulsar topic."""
        return f"persistent://{self._ns_path}/{subject}"

    def _local_subject(self, full_topic: str) -> str:
        """Recover the internal subject from a ``persistent://t/ns/x`` name."""
        marker = "://"
        idx = full_topic.find(marker)
        rest = full_topic[idx + len(marker) :] if idx >= 0 else full_topic
        # rest == tenant/namespace/subject -- subjects never contain '/'.
        segments = rest.split("/", 2)
        return segments[2] if len(segments) == 3 else full_topic

    def _pattern_to_regex(self, subject_pattern: str) -> str:
        """Translate a subject-wildcard pattern to a Pulsar topic regex.

        Mirrors :func:`crewlet.queue.matching.topic_matches`:

        - ``*`` matches exactly one subject segment (``[^.]+``);
        - ``>`` matches one-or-more trailing segments (``.+``) and is
          terminal;
        - every other token matches literally.

        The pattern is ``tenant/namespace/local-regex`` -- *without* a
        ``persistent://`` domain prefix.  The client parses the leading
        ``tenant/namespace/`` to derive the namespace to watch; the topic
        domain is selected by the consumer's regex-subscription mode
        (which defaults to persistent-only), so a domain in the pattern is
        rejected ("Ignore invalid domain: persistent ..."). It must
        therefore *start* with the literal namespace prefix (no leading
        ``^`` anchor, which would break namespace parsing); the trailing
        ``$`` keeps ``*`` from over-matching multi-segment topics.
        """
        prefix = re.escape(f"{self._ns_path}/")
        out: list[str] = []
        for part in subject_pattern.split("."):
            if part == ">":
                out.append(".+")
                break
            if part == "*":
                out.append("[^.]+")
            else:
                out.append(re.escape(part))
        local = r"\.".join(out)
        return f"{prefix}{local}$"

    # -- protocol surface --

    def add_publish_listener(
        self, listener: Callable[[str, Event], Awaitable[None]]
    ) -> None:
        self._publish_listeners.append(listener)

    @property
    def in_flight_count(self) -> int:
        """Number of handler invocations currently mid-flight."""
        return self._in_flight

    def _client_kwargs(self) -> dict[str, Any]:
        """Keyword arguments for ``pulsar.Client`` (auth/TLS optional).

        With token authentication the broker identifies this engine by
        the token's ``sub`` claim (its *role*); namespace-level grants on
        the broker then confine the engine to its own tenant.
        """
        kwargs: dict[str, Any] = {"logger": _client_logger()}
        if self._auth_token:
            kwargs["authentication"] = pulsar.AuthenticationToken(self._auth_token)
        if self._tls_trust_certs_path:
            kwargs["tls_trust_certs_file_path"] = self._tls_trust_certs_path
        return kwargs

    async def start(self) -> None:
        if self._running:
            return
        self._client = await asyncio.to_thread(
            lambda: pulsar.Client(self._url, **self._client_kwargs())
        )
        self._running = True
        self._paused = False
        logger.info(
            "pulsar_event_queue_started",
            url=self._url,
            tenant=self._tenant,
            namespace=self._namespace,
            auth="token" if self._auth_token else "none",
        )

    async def pause_delivery(self) -> None:
        """Stop fetching new messages; in-flight handlers run to completion.

        Sets a flag the per-subscription consume loops check at the top
        of each iteration.  Running handlers are not preempted -- they
        finish, ack their message, and the in-flight counter drops to 0
        so ``wait_for_handlers`` can return.  Publishes still work, so an
        in-flight turn can emit its ``TaskCompleted`` / ``TaskFailed``
        before the queue is finally closed in :meth:`stop`.
        """
        if self._paused:
            return
        self._paused = True
        logger.info("pulsar_event_queue_paused")

    async def pause_topic(self, topic: str) -> None:
        """Pause fetching for one topic (sandbox busy gate)."""
        self._paused_topics.add(topic)
        logger.info("pulsar_topic_paused", topic=topic)

    async def resume_topic(self, topic: str) -> None:
        """Resume a paused topic; the consume loop fetches its backlog."""
        if topic not in self._paused_topics:
            return
        self._paused_topics.discard(topic)
        logger.info("pulsar_topic_resumed", topic=topic)

    async def wait_for_handlers(self, timeout: float | None = None) -> int:
        """Wait for in-flight handler invocations to complete.

        Returns the number of handlers still running when the wait ended
        -- ``0`` after a clean drain, non-zero if ``timeout`` expired.
        ``timeout=None`` waits indefinitely.  A timeout is not an error:
        the engine's drain loop polls with a short timeout to emit
        periodic progress logs, so logging policy lives with the caller.
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

        # Cancel competing-consumer loops, close their consumers, and shut
        # down each consumer's dedicated receive thread.  Close the
        # consumer (which unblocks any in-flight ``receive``) before
        # shutting the executor down so its thread can exit.
        for sub in self._subscriptions:
            sub.task.cancel()
        if self._subscriptions:
            await asyncio.gather(
                *(s.task for s in self._subscriptions), return_exceptions=True
            )
        for sub in self._subscriptions:
            with contextlib.suppress(Exception):
                await asyncio.to_thread(sub.consumer.close)
            sub.executor.shutdown(wait=False)
        self._subscriptions.clear()

        # Cancel stream loops, delete their ephemeral subscriptions so the
        # broker isn't left to reap them, close the consumers, and shut
        # down their receive threads.
        for sub in self._stream_subscriptions:
            sub.task.cancel()
            with contextlib.suppress(Exception):
                await asyncio.gather(sub.task, return_exceptions=True)
            with contextlib.suppress(Exception):
                await asyncio.to_thread(sub.consumer.unsubscribe)
            with contextlib.suppress(Exception):
                await asyncio.to_thread(sub.consumer.close)
            sub.executor.shutdown(wait=False)
        self._stream_subscriptions.clear()

        # Close producers, then the client connection.
        for producer in self._producers.values():
            with contextlib.suppress(Exception):
                await asyncio.to_thread(producer.close)
        self._producers.clear()
        if self._client is not None:
            with contextlib.suppress(Exception):
                await asyncio.to_thread(self._client.close)
            self._client = None
        logger.info("pulsar_event_queue_stopped")

    async def _get_producer(self, full_topic: str) -> Any:
        """Return a cached producer for *full_topic*, creating one if needed."""
        producer = self._producers.get(full_topic)
        if producer is not None:
            return producer
        async with self._producer_lock:
            producer = self._producers.get(full_topic)
            if producer is None:
                assert self._client is not None
                producer = await asyncio.to_thread(
                    self._client.create_producer,
                    full_topic,
                    # Never block the asyncio event loop when the producer
                    # queue backs up behind a slow broker -- raise instead
                    # and let ``publish`` retry with backoff.
                    block_if_queue_full=False,
                )
                self._producers[full_topic] = producer
        return producer

    async def _send_once(self, producer: Any, data: bytes) -> None:
        """Publish one payload and await the broker ack.

        ``send_async`` fires its callback on a Pulsar background thread;
        we marshal the result back onto the loop via a future so the
        await doesn't tie up an executor thread per publish.  Raises on a
        non-Ok result or if ``send_async`` itself raises (e.g. the
        producer queue is full with ``block_if_queue_full=False``).
        """
        loop = asyncio.get_running_loop()
        future: asyncio.Future[None] = loop.create_future()

        def _on_send(result: Any, _msg_id: Any) -> None:
            if result == pulsar.Result.Ok:
                loop.call_soon_threadsafe(_resolve, future, None)
            else:
                loop.call_soon_threadsafe(
                    _reject, future, RuntimeError(f"pulsar send result: {result}")
                )

        producer.send_async(data, _on_send)
        await future

    async def publish(self, topic: str, event: Event) -> None:
        if not self._running or self._client is None:
            raise RuntimeError("PulsarEventQueue is not started")
        producer = await self._get_producer(self._full_topic(topic))
        data = serialize_event(event)

        # Retry transient send failures (broker queueing delay / send
        # timeout / a momentarily full producer queue) with exponential
        # backoff so a brief broker hiccup neither drops the event nor
        # wedges the engine.
        for attempt in range(_PUBLISH_MAX_ATTEMPTS):
            try:
                await self._send_once(producer, data)
                break
            except Exception as exc:  # noqa: BLE001
                if attempt == _PUBLISH_MAX_ATTEMPTS - 1:
                    raise RuntimeError(
                        f"pulsar publish to {topic!r} failed after "
                        f"{_PUBLISH_MAX_ATTEMPTS} attempts: {exc}"
                    ) from exc
                logger.warning(
                    "publish_retry", topic=topic, attempt=attempt + 1, error=str(exc)
                )
                await asyncio.sleep(_PUBLISH_RETRY_BASE_DELAY * (2**attempt))

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

    async def subscribe(
        self,
        topic: str,
        group: str,
        handler: Callable[[Event], Awaitable[None]],
    ) -> None:
        if not self._running or self._client is None:
            raise RuntimeError("PulsarEventQueue is not started")

        consumer, executor = await self._create_durable_consumer(topic, group)
        loop = asyncio.get_running_loop()

        async def _consume() -> None:
            while self._running:
                msg = await self._receive_one(consumer, executor, loop, topic, group)
                if msg is None:
                    continue
                await self._process_message(consumer, msg, handler, topic, group)

        task = asyncio.create_task(_consume())
        self._subscriptions.append(
            _Subscription(
                task=task,
                consumer=consumer,
                executor=executor,
                topic=topic,
                group=group,
            )
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
        if not self._running or self._client is None:
            raise RuntimeError("PulsarEventQueue is not started")

        # Same durable consumer shape as ``subscribe`` — batch delivery
        # changes *when* messages are handed to the handler, not their
        # broker-side lifecycle.
        consumer, executor = await self._create_durable_consumer(topic, group)
        loop = asyncio.get_running_loop()

        async def _consume() -> None:
            while self._running:
                msg = await self._receive_one(consumer, executor, loop, topic, group)
                if msg is None:
                    continue
                # The dispatch budget is measured from THIS instant — the
                # first message's broker ack-timeout clock started at its
                # receive, so the linger window spent collecting must
                # count against the budget, not reset it.
                received_at = time.monotonic()
                msgs = [msg]
                msgs.extend(
                    await self._collect_batch(consumer, executor, loop, options)
                )
                # ``pause_delivery`` may have fired while we lingered.
                # NAK the whole collected batch so the next engine
                # subscription gets it promptly.
                if self._paused or not self._running:
                    for m in msgs:
                        with contextlib.suppress(Exception):
                            consumer.negative_acknowledge(m)
                    continue
                await self._process_batch(
                    consumer,
                    msgs,
                    handler,
                    batch_key,
                    topic,
                    group,
                    received_at=received_at,
                )

        task = asyncio.create_task(_consume())
        self._subscriptions.append(
            _Subscription(
                task=task,
                consumer=consumer,
                executor=executor,
                topic=topic,
                group=group,
            )
        )
        logger.debug("batch_subscription_added", topic=topic, group=group)

    async def unsubscribe(self, topic: str, group: str) -> None:
        """Tear down the durable consumer(s) for ``topic``/``group``.

        Cancels the consume task, deletes the broker-side subscription
        (``consumer.unsubscribe`` — retained messages for the group are
        dropped, which is what a decommissioned role's inbox needs), and
        releases the receive executor. Idempotent — unknown pairs no-op.
        """
        matched = [
            s for s in self._subscriptions if s.topic == topic and s.group == group
        ]
        if not matched:
            return
        for sub in matched:
            self._subscriptions.remove(sub)
            sub.task.cancel()
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await sub.task
            with contextlib.suppress(Exception):
                await asyncio.to_thread(sub.consumer.unsubscribe)
            with contextlib.suppress(Exception):
                sub.consumer.close()
            sub.executor.shutdown(wait=False, cancel_futures=True)
        logger.info("unsubscribed", topic=topic, group=group, consumers=len(matched))

    async def _create_durable_consumer(
        self, topic: str, group: str
    ) -> tuple[Any, ThreadPoolExecutor]:
        """Create the durable consumer + receive executor for *topic*/*group*.

        One construction path for ``subscribe`` and ``subscribe_batch``
        so the broker-side policy can never diverge between them:

        - Shared subscription named after the group — every consumer
          that joins the same subscription shares one cursor, giving
          competing-consumer semantics (each message to exactly one
          member).  The ack-timeout caps how long a
          delivered-but-unacked message waits before redelivery; the
          dead-letter policy caps poison-message redelivery.
        - The dead-letter topic carries a ``dlq-`` prefix so it falls
          *outside* the ``crewlet.*`` subject space.  Pulsar's default
          DLQ name (``<topic>-<sub>-DLQ``) would sit under
          ``crewlet.events.*`` and get picked up by the dashboard's
          ``crewlet.events.>`` broadcast stream, re-surfacing a poison
          event as if it were live.
        - A dedicated single-thread executor runs this consumer's
          blocking ``receive()`` poll — see the module docstring for
          why it never shares the default ``to_thread`` pool.
        """
        assert self._client is not None
        dlq_topic = f"persistent://{self._ns_path}/dlq-{topic}-{group}"
        consumer = await asyncio.to_thread(
            self._client.subscribe,
            self._full_topic(topic),
            group,
            consumer_type=pulsar.ConsumerType.Shared,
            unacked_messages_timeout_ms=_INBOX_ACK_TIMEOUT_MS,
            negative_ack_redelivery_delay_ms=_NEG_ACK_REDELIVERY_DELAY_MS,
            dead_letter_policy=pulsar.ConsumerDeadLetterPolicy(
                max_redeliver_count=_INBOX_MAX_REDELIVER,
                dead_letter_topic=dlq_topic,
            ),
        )
        executor = ThreadPoolExecutor(
            max_workers=1, thread_name_prefix=f"pulsar-recv-{group}"
        )
        return consumer, executor

    async def _receive_one(
        self,
        consumer: Any,
        executor: ThreadPoolExecutor,
        loop: asyncio.AbstractEventLoop,
        topic: str,
        group: str,
    ) -> Any | None:
        """One guarded blocking-poll receive shared by both consume loops.

        Returns the fetched message, or ``None`` when the loop should
        just iterate: paused (graceful-shutdown drain — any handler
        started before pause keeps running and acks normally; unfetched
        messages stay on the broker for the next engine subscription),
        idle poll timeout, or a transient receive error.  A message
        fetched in the race where ``pause_delivery`` / ``stop`` flipped
        mid-``receive`` is NAK'd here so it comes back on the next
        engine subscription promptly instead of waiting out the
        ack-timeout window.
        """
        if self._paused or topic in self._paused_topics:
            # Global drain OR this topic is paused (sandbox busy gate):
            # don't fetch, so messages accumulate untouched on the broker
            # and redeliver in order once fetching resumes.
            await asyncio.sleep(0.1)
            return None
        try:
            msg = await loop.run_in_executor(
                executor, consumer.receive, _RECEIVE_TIMEOUT_MS
            )
        except Exception as exc:  # noqa: BLE001
            if _is_timeout(exc):
                return None
            logger.exception("receive_error", topic=topic, group=group, error=str(exc))
            await asyncio.sleep(1)
            return None
        if self._paused or not self._running:
            with contextlib.suppress(Exception):
                consumer.negative_acknowledge(msg)
            return None
        return msg

    async def _collect_batch(
        self,
        consumer: Any,
        executor: ThreadPoolExecutor,
        loop: asyncio.AbstractEventLoop,
        options: BatchOptions,
    ) -> list[Any]:
        """Collect the rest of a batch after its first message arrived.

        Reads ``options`` live (it is mutable; live config reloads swap
        the values between cycles).  With ``linger_seconds == 0`` this
        is one drain pass over the consumer's already-fetched local
        queue: receive with a short timeout until the first empty poll.
        With a positive linger, keep receiving until the window
        (measured from now, i.e. from the first message) expires or
        ``max_batch`` is reached; each poll is capped at
        ``_RECEIVE_TIMEOUT_MS`` so the loop stays responsive to
        ``pause_delivery`` / ``stop``.
        """
        collected: list[Any] = []
        linger = options.effective_linger_seconds
        max_batch = options.effective_max_batch
        deadline = loop.time() + linger
        while len(collected) + 1 < max_batch:
            if self._paused or not self._running:
                break
            if linger <= 0:
                timeout_ms = _DRAIN_RECEIVE_TIMEOUT_MS
            else:
                remaining = deadline - loop.time()
                if remaining <= 0:
                    break
                timeout_ms = min(_RECEIVE_TIMEOUT_MS, int(remaining * 1000) + 1)
            try:
                msg = await loop.run_in_executor(executor, consumer.receive, timeout_ms)
            except Exception as exc:  # noqa: BLE001
                if _is_timeout(exc):
                    if linger <= 0:
                        # Local queue drained — the zero-linger pass ends
                        # at the first empty poll.
                        break
                    continue
                logger.exception("batch_collect_receive_error", error=str(exc))
                break
            collected.append(msg)
        return collected

    def _deserialize_or_ack(
        self, consumer: Any, msg: Any, topic: str, group: str
    ) -> Event | None:
        """Deserialize one message; corrupt payloads are acked and dropped.

        One copy of the poison-payload policy for both delivery paths:
        acking stops infinite redelivery, and the corrupt-msg path never
        touches the in-flight counter.
        """
        try:
            return deserialize_event(msg.data())
        except (json.JSONDecodeError, ValueError) as exc:
            logger.error("deserialize_failed", topic=topic, group=group, error=str(exc))
            consumer.acknowledge(msg)
            return None

    @contextlib.contextmanager
    def _in_flight_guard(self) -> Iterator[None]:
        """Track one handler invocation in the in-flight counter.

        Incremented before the handler awaits, decremented exactly once
        even under exceptions — ``wait_for_handlers`` (the engine's
        graceful-shutdown drain) relies on this invariant holding
        identically for single-message and batch delivery.
        """
        self._in_flight += 1
        self._idle_event.clear()
        try:
            yield
        finally:
            self._in_flight -= 1
            if self._in_flight == 0:
                self._idle_event.set()

    async def _process_batch(
        self,
        consumer: Any,
        msgs: list[Any],
        handler: Callable[[list[Event]], Awaitable[None]],
        batch_key: Callable[[Event], str],
        topic: str,
        group: str,
        received_at: float | None = None,
    ) -> None:
        """Partition *msgs* by key and run *handler* per partition.

        Ack/NAK is per partition: messages are acknowledged only after
        their partition's handler returned, negatively acknowledged on
        its failure — a failing conversation redelivers only itself.
        On cancellation (force-stop), the current partition and every
        not-yet-dispatched one are NAK'd so the broker redelivers them
        to the next engine subscription promptly.

        **Ack-budget deferral.** Every drained message's ack-timeout
        clock started at receive, but a partition handler is typically
        a full multi-minute agent turn — so partitions are dispatched
        only while the drain is within ``_BATCH_DISPATCH_BUDGET_MS``.
        Once the budget is spent, every remaining partition is deferred
        via :meth:`_defer_partitions` (republish, then ack the
        original) so no message ever sits delivered-but-unacked for the
        sum of preceding turns.  Fast multi-partition drains
        (informational events, sub-second handlers) finish in one pass;
        slow ones re-enter the backlog with fresh ack clocks and
        re-partition on the next drain.

        Extracted from the consume loop (like ``_process_message``) so
        the ack-ordering, deferral, and cancellation paths are
        unit-testable with a fake consumer + msgs, no live broker.
        """
        # Deserialize up front; corrupt payloads are acked and dropped
        # individually so one bad message never poisons its batch.
        entries: list[tuple[Any, Event]] = []
        for msg in msgs:
            event = self._deserialize_or_ack(consumer, msg, topic, group)
            if event is not None:
                entries.append((msg, event))
        if not entries:
            return

        parts = order_partitions_oldest_first(
            partition_by_key(entries, batch_key, event_of=lambda pair: pair[1]),
            event_of=lambda pair: pair[1],
        )
        # Budget runs from the FIRST receive (``received_at``) — the
        # broker's ack clocks started there, so time spent lingering in
        # collection counts against it.  ``None`` (direct test callers)
        # starts the budget now.
        started = received_at if received_at is not None else time.monotonic()
        budget_seconds = _BATCH_DISPATCH_BUDGET_MS / 1000.0

        for index, (key, part) in enumerate(parts):
            if index > 0 and time.monotonic() - started > budget_seconds:
                await self._defer_partitions(consumer, parts[index:], topic, group)
                return
            events = [event for _, event in part]
            with self._in_flight_guard():
                try:
                    await handler(events)
                    for msg, _ in part:
                        consumer.acknowledge(msg)
                except asyncio.CancelledError:
                    # Force-stop mid-batch: NAK this partition AND the
                    # partitions we never reached, so none of them sit
                    # out the ack-timeout window on the broker.
                    for _, later_part in parts[index:]:
                        for msg, _ in later_part:
                            with contextlib.suppress(Exception):
                                consumer.negative_acknowledge(msg)
                    raise
                except Exception as exc:
                    logger.warning(
                        "batch_handler_failed",
                        topic=topic,
                        group=group,
                        batch_key=key,
                        event_count=len(events),
                        error=str(exc),
                    )
                    for msg, _ in part:
                        consumer.negative_acknowledge(msg)

    async def _defer_partitions(
        self,
        consumer: Any,
        parts: list[tuple[str, list[tuple[Any, Event]]]],
        topic: str,
        group: str,
    ) -> None:
        """Requeue partitions whose dispatch would breach the ack budget.

        Republish-then-ack, per message: the event re-enters the topic
        with its identity (id / timestamp / trace) intact — so it
        re-partitions into the same conversation on a later drain and
        the event store's ``(event_time, event_id)`` upsert stays
        idempotent — and only then is the original acked, preserving
        at-least-once.  Unlike a NAK, the requeued copy carries zero
        accrued redeliveries, so deferral can never push a healthy
        conversation into the dead-letter topic.

        The FIRST republish failure aborts the whole deferral — every
        not-yet-requeued message is NAK'd and normal redelivery takes
        over.  ``publish`` already retried the send with backoff
        internally, so continuing to later partitions against a broken
        broker would only serialize more doomed retry cycles while the
        held messages' ack clocks keep aging.  Cancellation NAKs the
        same remainder and propagates.  Runs inside the in-flight guard
        so a graceful drain waits for the requeue to finish instead of
        stopping the queue mid-deferral.
        """
        remaining: list[tuple[Any, Event]] = [
            pair for _, part in parts for pair in part
        ]
        total = len(remaining)
        deferred = 0
        with self._in_flight_guard():
            try:
                for msg, event in remaining:
                    await self.publish(topic, event)
                    consumer.acknowledge(msg)
                    deferred += 1
            except asyncio.CancelledError:
                for msg, _ in remaining[deferred:]:
                    with contextlib.suppress(Exception):
                        consumer.negative_acknowledge(msg)
                raise
            except Exception as exc:
                logger.warning(
                    "batch_defer_republish_failed",
                    topic=topic,
                    group=group,
                    error=str(exc),
                )
                for msg, _ in remaining[deferred:]:
                    with contextlib.suppress(Exception):
                        consumer.negative_acknowledge(msg)
        logger.info(
            "batch_partitions_deferred",
            topic=topic,
            group=group,
            partitions=len(parts),
            events=deferred,
            total=total,
        )

    async def subscribe_stream(
        self,
        topic_pattern: str,
        handler: Callable[[str, Event], Awaitable[None]],
    ) -> Callable[[], Awaitable[None]]:
        """Broadcast subscription via a per-caller regex pattern consumer.

        Each call creates its own Shared subscription (unique name) over
        the topics matching *topic_pattern*, started at the latest
        message -- so every subscriber receives every matching event,
        appropriate for a live-stream view where backfill is served
        separately by the REST event store.  Newly-created topics are
        picked up on the client's pattern auto-discovery cycle, so a
        brand-new agent's first events may lag the stream by that
        interval; already-active topics match immediately.
        """
        if not self._running or self._client is None:
            raise RuntimeError("PulsarEventQueue is not started")

        subscription_name = f"stream-{uuid4().hex}"
        pattern = re.compile(self._pattern_to_regex(topic_pattern))
        consumer = await asyncio.to_thread(
            self._client.subscribe,
            pattern,
            subscription_name,
            consumer_type=pulsar.ConsumerType.Shared,
            initial_position=pulsar.InitialPosition.Latest,
        )

        executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix="pulsar-stream")
        loop = asyncio.get_running_loop()

        async def _consume() -> None:
            while self._running:
                try:
                    msg = await loop.run_in_executor(
                        executor, consumer.receive, _RECEIVE_TIMEOUT_MS
                    )
                except Exception as exc:  # noqa: BLE001
                    if _is_timeout(exc):
                        continue
                    logger.exception(
                        "stream_receive_error",
                        topic_pattern=topic_pattern,
                        error=str(exc),
                    )
                    await asyncio.sleep(1)
                    continue
                try:
                    event = deserialize_event(msg.data())
                except (json.JSONDecodeError, ValueError) as exc:
                    logger.error(
                        "stream_deserialize_failed",
                        topic_pattern=topic_pattern,
                        error=str(exc),
                    )
                    with contextlib.suppress(Exception):
                        consumer.acknowledge(msg)
                    continue
                subject = self._local_subject(msg.topic_name())
                try:
                    await handler(subject, event)
                except Exception as exc:
                    logger.exception(
                        "stream_handler_failed",
                        topic_pattern=topic_pattern,
                        error=str(exc),
                    )
                # Ack regardless so the ephemeral subscription's backlog
                # doesn't grow -- stream delivery is best-effort.
                with contextlib.suppress(Exception):
                    consumer.acknowledge(msg)

        task = asyncio.create_task(_consume())
        entry = _Subscription(task=task, consumer=consumer, executor=executor)
        self._stream_subscriptions.append(entry)
        logger.debug("stream_subscription_added", topic_pattern=topic_pattern)

        async def _unsubscribe() -> None:
            with contextlib.suppress(ValueError):
                self._stream_subscriptions.remove(entry)
            entry.task.cancel()
            with contextlib.suppress(Exception):
                await asyncio.gather(entry.task, return_exceptions=True)
            with contextlib.suppress(Exception):
                await asyncio.to_thread(entry.consumer.unsubscribe)
            with contextlib.suppress(Exception):
                await asyncio.to_thread(entry.consumer.close)
            entry.executor.shutdown(wait=False)
            logger.debug("stream_subscription_removed", topic_pattern=topic_pattern)

        return _unsubscribe

    async def _process_message(
        self,
        consumer: Any,
        msg: Any,
        handler: Callable[[Event], Awaitable[None]],
        topic: str,
        group: str,
    ) -> None:
        """Run *handler* over one received Pulsar msg with ack/nak.

        Extracted from the consume loop so the shutdown-sensitive paths
        (cancellation NAK, ack/nak ordering, in-flight bookkeeping) are
        unit-testable with a fake consumer + msg, no live broker.
        """
        event = self._deserialize_or_ack(consumer, msg, topic, group)
        if event is None:
            return
        with self._in_flight_guard():
            try:
                await handler(event)
                consumer.acknowledge(msg)
            except asyncio.CancelledError:
                # Handler cancelled (force-stop / second signal).  NAK so
                # Pulsar redelivers promptly to the next engine
                # subscription rather than holding the message until the
                # ack-timeout window elapses.  ``negative_acknowledge`` is
                # a local, non-blocking record, safe to call mid-cancel.
                with contextlib.suppress(Exception):
                    consumer.negative_acknowledge(msg)
                raise
            except Exception as exc:
                logger.warning(
                    "handler_failed",
                    topic=topic,
                    group=group,
                    event_type=event.type,
                    error=str(exc),
                )
                consumer.negative_acknowledge(msg)


def _resolve(future: asyncio.Future[None], value: None) -> None:
    """Resolve *future* from the loop thread, ignoring late callbacks."""
    if not future.done():
        future.set_result(value)


def _reject(future: asyncio.Future[None], exc: Exception) -> None:
    """Fail *future* from the loop thread, ignoring late callbacks."""
    if not future.done():
        future.set_exception(exc)
