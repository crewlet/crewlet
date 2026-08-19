"""Apache Pulsar implementation of EventQueue.

Pulsar is the production event-queue backend.  The engine speaks a
single internal subject language (dot-separated subjects with ``*`` /
``>`` wildcards — see :mod:`crewlet.queue.matching`); this module
translates that language onto Pulsar's topic + subscription model:

- A subject (e.g. ``crewlet.agent.alice.inbox``) maps to a persistent
  Pulsar topic ``persistent://{tenant}/{namespace}/{subject}``.  Topics
  are auto-created; a non-default tenant/namespace is not, and must
  exist before start (created out-of-band with ``pulsar-admin``).
  Subscription *lifecycle* — create and delete — does go through the
  admin REST API (:mod:`crewlet.queue.admin`), because both operations
  have to work with no consumer attached: see :meth:`ensure_subscription`.
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
from crewlet._tasks import cancel_and_wait
from crewlet.events.types import Event
from crewlet.queue.admin import PulsarBrokerAdmin, derive_admin_url
from crewlet.queue.protocol import (
    BatchOptions,
    DeferDelivery,
    order_partitions_oldest_first,
    partition_by_key,
)
from crewlet.queue.serialization import deserialize_event, serialize_event
from crewlet.queue.topics import AGENT_INBOX_SUFFIX

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

# Cap redeliveries so a handler that genuinely keeps failing (or a poison
# message) cannot loop forever; past this many the message goes to the
# subscription's dead-letter topic.  Applies to EVERY durable
# subscription this process creates, not only agent inboxes — the old
# name said otherwise and the value was chosen as if it did.
#
# Ten, not three, because in a fleet the counter has two causes and only
# one of them is a bad message. A redelivery means "the handler failed"
# — poison — but it ALSO means "a node died holding this": measured, an
# ack-timeout redelivery increments the counter (see
# tests/test_queue/test_broker_behavior.py). Three was sized for a
# single-node world, where the second cause does not exist; in a fleet it
# silently became a budget of three node deaths per message, after which
# a perfectly healthy event is dead-lettered having never been handled.
#
# The two causes mostly separate by RATE, not by count: a poison message
# is NAK'd on failure and redelivered after
# ``_NEG_ACK_REDELIVERY_DELAY_MS`` (1 s), so it still burns ten in about
# ten seconds — poison detection goes from 3 s to 10 s, which nothing
# depends on. Handoff-driven redeliveries are minutes to half an hour
# apart (the ack timeout is 30 minutes), so ten covers a 3-node rolling
# deploy plus several genuine crashes over a message's life.
#
# The separation is not perfect and the honest version is worth stating:
# a node that dies FAST — repeatedly, in a crashloop — produces NAKs a
# second apart, which is indistinguishable from poison at any cap. The
# raise buys the common case; it does not solve that one.
_MAX_REDELIVER = 10

# Delay before a negatively-acknowledged message is redelivered.  Pulsar
# defaults this to 60s; we drop it to 1s so a failed handler retries
# promptly and so a turn NAK'd during force-stop comes back to the next
# engine subscription quickly instead of waiting out the ack-timeout
# window.
_NEG_ACK_REDELIVERY_DELAY_MS = 1000

# How many messages a durable consumer prefetches into its local queue.
#
# Set EXPLICITLY, which it never was: the client default is 1000, and
# that default is a liability here rather than a throughput win.
#
# Prefetched messages are delivered-but-unacked. They are hostage to
# this consumer for as long as it holds them, and ``_INBOX_ACK_TIMEOUT_MS``
# is thirty minutes — deliberately, because an agent turn can run that
# long. A node that is *wedged but alive* (the asyncio loop blocked while
# the C++ client's IO threads keep answering broker keepalives) therefore
# sits on its whole prefetch for half an hour before the broker
# redelivers any of it. At 1000 that is potentially every message a busy
# seat received in that window; a human waits half an hour for a reply
# that was sitting in a dead process's memory.
#
# Throughput does not argue back. Turns are serialized per seat — one at
# a time, minutes each — so a seat's consumer can only ever work through
# its queue one batch at a time, and prefetching beyond a batch buys
# nothing but hostages. 64 covers the default ``max_batch`` of 20 three
# times over, so the batcher's zero-linger drain pass still fills a full
# batch from local state, while cutting the worst-case hostage count by
# ~15x.
#
# Batch subscriptions scale it to their own cap (see ``_make_consumer``)
# so an operator who raises ``notification_coalesce_max_batch`` does not
# silently starve the drain pass.
_RECEIVER_QUEUE_SIZE = 64

# The same knob for the dashboard's broadcast stream consumers.
#
# Different tradeoff, so a different number: these are non-durable, start
# at the latest message, and their events are display-only — nothing is
# lost that matters if one is dropped. The cost being bounded here is
# memory, one queue per connected dashboard, and a browser that stops
# reading must not make the client buffer 1000 events on its behalf.
_STREAM_RECEIVER_QUEUE_SIZE = 200

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

    ``topic`` / ``group`` identify the durable pair so :meth:`detach`
    can find and tear the consumer down (stream subscriptions leave them
    empty — they return their own unsubscribe callable instead).

    ``quiescing`` stops this subscription taking on new work while
    letting whatever is already running finish.  It is checked at four
    points — loop top, immediately after a receive returns, inside the
    batch collector, and immediately before dispatch — because a
    *topic pause* is checked at only one of those (before the blocking
    receive), so a message already fetched still dispatches and a
    positive linger keeps collecting more.  A quiescing subscription
    leaves collected-but-undispatched messages **unacked**: they return
    to the successor at ``redeliveryCount`` 0, where a NAK would spend
    the dead-letter budget on a healthy message.

    ``task`` is assigned right after construction, because the consume
    closure has to capture this record to see the flag.  ``run`` keeps
    that closure so :meth:`PulsarEventQueue.unquiesce` can start a fresh
    task: the loop EXITS when the flag is set, so clearing the flag alone
    would leave the attachment permanently silent.
    """

    consumer: Any
    executor: ThreadPoolExecutor
    topic: str = ""
    group: str = ""
    task: asyncio.Task[None] | None = None
    quiescing: bool = False
    run: Callable[[], Awaitable[None]] | None = None


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
        admin_url: str = "",
    ) -> None:
        self._url = url
        self._tenant = tenant
        self._namespace = namespace
        self._auth_token = auth_token
        self._tls_trust_certs_path = tls_trust_certs_path
        # Subscription lifecycle runs over the admin API, not the client:
        # creating a subscription by SUBSCRIBING joins a Shared
        # subscription a peer may be actively serving and takes a share
        # of that seat's traffic into this process (measured), and
        # deleting one through ``Consumer.unsubscribe`` needs a local
        # consumer, which would make role decommission depend on which
        # node happens to hold the seat.  See :mod:`crewlet.queue.admin`.
        self._admin_url = admin_url or derive_admin_url(url)
        self._admin: PulsarBrokerAdmin | None = None
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
        # (topic, group) -> the set of reasons holding it paused.  A
        # subscription is delivered only when no reason remains; see the
        # protocol.  Keyed by the PAIR, not the topic: holds are
        # process-local state about one attachment, and a topic-keyed
        # hold both leaked across a detach — so a node that re-acquired a
        # seat attached into a still-paused topic and went silently deaf
        # — and gated every group on shared subjects like
        # ``crewlet.events.*``.
        self._paused_subs: dict[tuple[str, str], set[str]] = {}
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

    @property
    def backend(self) -> str:
        return "pulsar"

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

    def _is_sub_paused(self, sub: _Subscription) -> bool:
        return bool(self._paused_subs.get((sub.topic, sub.group)))

    def attachments(self) -> list[tuple[str, str]]:
        """Every ``(topic, group)`` pair this process has attached to.

        Public because "which seats is this node serving?" is the
        question seat ownership makes operationally central, and a test
        should not have to read the backend's registry to ask it.
        """
        return [(s.topic, s.group) for s in self._subscriptions]

    def pause_holds(self, topic: str, group: str) -> set[str]:
        """Reasons currently holding a subscription paused.

        Public because a hold is operator-visible behaviour — which
        subsystem is gating a seat is the first question when one goes
        quiet — and because tests must not reach into the backend's
        internals to ask.
        """
        return set(self._paused_subs.get((topic, group), ()))

    async def pause_topic(
        self, topic: str, group: str, *, reason: str = "default"
    ) -> None:
        """Take one reason's hold on a subscription (sandbox gate, shed)."""
        self._paused_subs.setdefault((topic, group), set()).add(reason)
        logger.info("pulsar_topic_paused", topic=topic, group=group, reason=reason)

    async def resume_topic(
        self, topic: str, group: str, *, reason: str = "default"
    ) -> None:
        """Release one reason's hold; fetch resumes when none remain."""
        key = (topic, group)
        holds = self._paused_subs.get(key)
        if not holds:
            return
        holds.discard(reason)
        if holds:
            logger.info(
                "pulsar_topic_still_paused",
                topic=topic,
                group=group,
                released=reason,
                held_by=sorted(holds),
            )
            return
        self._paused_subs.pop(key, None)
        logger.info("pulsar_topic_resumed", topic=topic, group=group, reason=reason)

    # -- subscription lifecycle (attachment vs existence) --

    async def quiesce(self, topic: str, group: str) -> bool:
        """Stop this process taking NEW work on ``topic``/``group``.

        Leaves the consumer attached and whatever is already running
        running, so the voluntary release path can let an in-flight turn
        finish before the seat moves.  Returns whether an attachment
        existed to quiesce.
        """
        matched = [
            s for s in self._subscriptions if s.topic == topic and s.group == group
        ]
        for sub in matched:
            sub.quiescing = True
        if matched:
            logger.info("subscription_quiesced", topic=topic, group=group)
        return bool(matched)

    async def unquiesce(self, topic: str, group: str) -> bool:
        """Resume a quiesced attachment. Returns whether one was resumed.

        The consume loop **exits** when the flag is set — that is what
        stops it fetching — so this restarts it from the stored closure
        rather than only clearing the flag.  Without the restart a node
        whose lease store blipped would keep its seats, keep its
        consumers, and read nothing from them ever again.

        Pause holds are untouched: a seat coming back from a stale-renew
        window may still be paused for a running sandbox.
        """
        resumed = 0
        for sub in self._subscriptions:
            if sub.topic != topic or sub.group != group or not sub.quiescing:
                continue
            sub.quiescing = False
            # Ask for whatever this consumer fetched and never dispatched.
            #
            # Quiescing drops fetched-but-undispatched messages on the
            # floor deliberately — they are left UNACKED so the seat's
            # next owner gets them at ``redeliveryCount`` 0, and the
            # thing that hands them back is the consumer CLOSING. On this
            # path nothing closes: the same consumer resumes. Without
            # this call those messages stay hostage in the client's
            # prefetch until the ack timeout, which is thirty minutes —
            # so a two-second store blip would silently cost half an hour
            # of a seat's mail.
            with contextlib.suppress(Exception):
                await asyncio.to_thread(sub.consumer.redeliver_unacknowledged_messages)
            if sub.run is not None and (sub.task is None or sub.task.done()):
                sub.task = asyncio.create_task(sub.run())
            resumed += 1
        if resumed:
            logger.info(
                "subscription_unquiesced", topic=topic, group=group, consumers=resumed
            )
        return bool(resumed)

    async def detach(self, topic: str, group: str) -> bool:
        """Close this process's consumer(s), leaving the subscription.

        The **non-destructive** half of what ``unsubscribe`` used to do
        in one call.  The durable subscription and its cursor survive:
        unacked messages return to whoever attaches next, measured at
        ``redeliveryCount`` 0, in order.  That is what makes a seat
        handoff free — and why this must never call
        ``consumer.unsubscribe()``, which would delete the subscription
        and discard an unowned seat's mail.

        Idempotent; returns whether an attachment existed.  Pause holds
        for the pair are dropped, because they are state about *this*
        attachment: a hold that outlived a detach meant a node that
        re-acquired the seat attached into a still-paused subscription
        and went silently deaf.
        """
        matched = [
            s for s in self._subscriptions if s.topic == topic and s.group == group
        ]
        if not matched:
            self._paused_subs.pop((topic, group), None)
            return False
        started = time.monotonic()
        for sub in matched:
            self._subscriptions.remove(sub)
            sub.quiescing = True
            if sub.task is not None:
                await cancel_and_wait(sub.task)
            with contextlib.suppress(Exception):
                # Bridged, like every other close in this module: the
                # pulsar client is a blocking C++ wrapper, and a bare
                # call here stalls the whole event loop — every agent
                # turn and the embedded API with it — for as long as the
                # broker takes to answer.
                await asyncio.to_thread(sub.consumer.close)
            sub.executor.shutdown(wait=False, cancel_futures=True)
        self._paused_subs.pop((topic, group), None)
        logger.info(
            "subscription_detached",
            topic=topic,
            group=group,
            consumers=len(matched),
            elapsed_ms=round((time.monotonic() - started) * 1000, 1),
        )
        return True

    async def ensure_subscription(self, topic: str, group: str) -> bool:
        """Create the durable subscription if absent, with no consumer.

        See :mod:`crewlet.queue.admin` for why this cannot be done by
        subscribing.  Returns whether this call created it.
        """
        return await self._broker_admin().ensure_subscription(topic, group)

    async def delete_subscription(self, topic: str, group: str) -> bool:
        """Delete the durable subscription, dropping any retained mail.

        The **destructive** half of the old ``unsubscribe``, and the one
        a decommissioned role needs so its inbox does not accumulate
        undeliverable events forever.  Detaches locally first if this
        process happens to hold the consumer, but does not require it —
        role removal must not depend on which node ran the seat.
        """
        await self.detach(topic, group)
        return await self._broker_admin().delete_subscription(topic, group)

    def _broker_admin(self) -> PulsarBrokerAdmin:
        if self._admin is None:
            self._admin = PulsarBrokerAdmin(
                self._admin_url,
                tenant=self._tenant,
                namespace=self._namespace,
                auth_token=self._auth_token,
                tls_trust_certs_path=self._tls_trust_certs_path,
            )
        return self._admin

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
            sub.quiescing = True
            if sub.task is not None:
                sub.task.cancel()
        pending = [s.task for s in self._subscriptions if s.task is not None]
        if pending:
            await asyncio.gather(*pending, return_exceptions=True)
        for sub in self._subscriptions:
            with contextlib.suppress(Exception):
                await asyncio.to_thread(sub.consumer.close)
            sub.executor.shutdown(wait=False)
        self._subscriptions.clear()

        # Cancel stream loops, delete their ephemeral subscriptions so the
        # broker isn't left to reap them, close the consumers, and shut
        # down their receive threads.
        for sub in self._stream_subscriptions:
            if sub.task is not None:
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
        if self._admin is not None:
            with contextlib.suppress(Exception):
                await self._admin.close()
            self._admin = None
        # Pause holds are state about attachments that no longer exist.
        self._paused_subs.clear()
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
        sub = _Subscription(
            consumer=consumer, executor=executor, topic=topic, group=group
        )

        async def _consume() -> None:
            while self._running and not sub.quiescing:
                msg = await self._receive_one(sub, loop)
                if msg is None:
                    continue
                if sub.quiescing:
                    # Fetched in the race where quiesce landed mid-receive.
                    # Leave it unacked for the successor rather than NAK.
                    continue
                await self._process_message(sub, msg, handler)

        sub.run = _consume
        sub.task = asyncio.create_task(_consume())
        self._subscriptions.append(sub)
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
        # broker-side lifecycle.  The prefetch is sized from the batch
        # cap in force at subscribe time.
        consumer, executor = await self._create_durable_consumer(
            topic, group, max_batch=options.effective_max_batch
        )
        loop = asyncio.get_running_loop()
        sub = _Subscription(
            consumer=consumer, executor=executor, topic=topic, group=group
        )

        async def _consume() -> None:
            while self._running and not sub.quiescing:
                msg = await self._receive_one(sub, loop)
                if msg is None:
                    continue
                # The dispatch budget is measured from THIS instant — the
                # first message's broker ack-timeout clock started at its
                # receive, so the linger window spent collecting must
                # count against the budget, not reset it.
                received_at = time.monotonic()
                msgs = [msg]
                msgs.extend(await self._collect_batch(sub, loop, options))
                if sub.quiescing:
                    # Quiesced mid-collection.  Leave the whole batch
                    # unacked — a handoff that closes its consumer
                    # returns these at ``redeliveryCount`` 0, whereas
                    # NAKing them here would spend the dead-letter
                    # budget on messages nothing was wrong with.
                    continue
                # ``pause_delivery`` may have fired while we lingered.
                # NAK the whole collected batch so the next engine
                # subscription gets it promptly.
                if self._paused or not self._running:
                    for m in msgs:
                        with contextlib.suppress(Exception):
                            consumer.negative_acknowledge(m)
                    continue
                await self._process_batch(
                    sub, msgs, handler, batch_key, received_at=received_at
                )

        sub.run = _consume
        sub.task = asyncio.create_task(_consume())
        self._subscriptions.append(sub)
        logger.debug("batch_subscription_added", topic=topic, group=group)

    async def _create_durable_consumer(
        self, topic: str, group: str, *, max_batch: int = 0
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
        # Batch subscribers need at least one full batch available
        # locally for the zero-linger drain pass to coalesce; an operator
        # who raises ``notification_coalesce_max_batch`` past the default
        # prefetch would otherwise silently get partial batches.
        queue_size = max(_RECEIVER_QUEUE_SIZE, 2 * int(max_batch))
        consumer = await asyncio.to_thread(
            self._client.subscribe,
            self._full_topic(topic),
            group,
            consumer_type=pulsar.ConsumerType.Shared,
            unacked_messages_timeout_ms=_INBOX_ACK_TIMEOUT_MS,
            negative_ack_redelivery_delay_ms=_NEG_ACK_REDELIVERY_DELAY_MS,
            receiver_queue_size=queue_size,
            dead_letter_policy=pulsar.ConsumerDeadLetterPolicy(
                max_redeliver_count=_MAX_REDELIVER,
                dead_letter_topic=dlq_topic,
            ),
        )
        executor = ThreadPoolExecutor(
            max_workers=1, thread_name_prefix=f"pulsar-recv-{group}"
        )
        return consumer, executor

    async def _receive_one(
        self,
        sub: _Subscription,
        loop: asyncio.AbstractEventLoop,
    ) -> Any | None:
        """One guarded blocking-poll receive shared by both consume loops.

        Returns the fetched message, or ``None`` when the loop should
        just iterate: paused (graceful-shutdown drain — any handler
        started before pause keeps running and acks normally; unfetched
        messages stay on the broker for the next engine subscription),
        quiescing, idle poll timeout, or a transient receive error.

        A message fetched in the race where ``pause_delivery`` / ``stop``
        flipped mid-``receive`` is NAK'd here so it comes back on the
        next engine subscription promptly instead of waiting out the
        ack-timeout window.  A message fetched in the race where
        ``quiesce`` flipped is **not** NAK'd — a seat handoff is not a
        failure, and the successor gets it at ``redeliveryCount`` 0 when
        this consumer closes.
        """
        if self._paused or self._is_sub_paused(sub):
            # Global drain OR this subscription is paused (sandbox busy
            # gate, config shed): don't fetch, so messages accumulate
            # untouched on the broker and redeliver in order once
            # fetching resumes.
            await asyncio.sleep(0.1)
            return None
        if sub.quiescing:
            await asyncio.sleep(0.05)
            return None
        try:
            msg = await loop.run_in_executor(
                sub.executor, sub.consumer.receive, _RECEIVE_TIMEOUT_MS
            )
        except Exception as exc:  # noqa: BLE001
            if _is_timeout(exc):
                return None
            logger.exception(
                "receive_error", topic=sub.topic, group=sub.group, error=str(exc)
            )
            await asyncio.sleep(1)
            return None
        if sub.quiescing:
            return msg  # caller checks the flag and leaves it unacked
        if self._paused or not self._running:
            with contextlib.suppress(Exception):
                sub.consumer.negative_acknowledge(msg)
            return None
        return msg

    async def _collect_batch(
        self,
        sub: _Subscription,
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
            if self._paused or not self._running or sub.quiescing:
                # Quiescing stops the collector too.  A topic pause does
                # not reach here at all, which is why the sandbox busy
                # gate kept filling a batch after its pause landed.
                break
            if linger <= 0:
                timeout_ms = _DRAIN_RECEIVE_TIMEOUT_MS
            else:
                remaining = deadline - loop.time()
                if remaining <= 0:
                    break
                timeout_ms = min(_RECEIVE_TIMEOUT_MS, int(remaining * 1000) + 1)
            try:
                msg = await loop.run_in_executor(
                    sub.executor, sub.consumer.receive, timeout_ms
                )
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
        sub: _Subscription,
        msgs: list[Any],
        handler: Callable[[list[Event]], Awaitable[None]],
        batch_key: Callable[[Event], str],
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
        consumer, topic, group = sub.consumer, sub.topic, sub.group
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
            if sub.quiescing:
                # Quiesced between partitions: stop dispatching and
                # leave everything from here on unacked for the
                # successor.  Not a deferral-by-republish — that would
                # send these to the topic tail while the successor's
                # prefetched siblings replay from the head, reordering
                # the conversation this whole mechanism exists to keep.
                return
            if index > 0 and time.monotonic() - started > budget_seconds:
                await self._defer_partitions(sub, parts[index:])
                return
            events = [event for _, event in part]
            with self._in_flight_guard():
                try:
                    await handler(events)
                    for msg, _ in part:
                        consumer.acknowledge(msg)
                except DeferDelivery as deferral:
                    sub.quiescing = True
                    logger.info(
                        "delivery_deferred",
                        topic=topic,
                        group=group,
                        batch_key=key,
                        event_count=len(events),
                        reason=str(deferral) or "unspecified",
                    )
                    return
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
        sub: _Subscription,
        parts: list[tuple[str, list[tuple[Any, Event]]]],
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
        consumer, topic, group = sub.consumer, sub.topic, sub.group
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
            receiver_queue_size=_STREAM_RECEIVER_QUEUE_SIZE,
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
            if entry.task is not None:
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
        sub: _Subscription,
        msg: Any,
        handler: Callable[[Event], Awaitable[None]],
    ) -> None:
        """Run *handler* over one received Pulsar msg with ack/nak.

        Extracted from the consume loop so the shutdown-sensitive paths
        (cancellation NAK, ack/nak ordering, in-flight bookkeeping) are
        unit-testable with a fake consumer + msg, no live broker.

        Three outcomes, not two: return acks, raising NAKs, and raising
        :class:`~crewlet.queue.protocol.DeferDelivery` leaves the message
        unacked and quiesces the subscription.  A handler that has lost
        the right to do this work needs the third — it must neither
        claim the message nor spend its redelivery budget.
        """
        consumer, topic, group = sub.consumer, sub.topic, sub.group
        event = self._deserialize_or_ack(consumer, msg, topic, group)
        if event is None:
            return
        with self._in_flight_guard():
            try:
                await handler(event)
                consumer.acknowledge(msg)
            except DeferDelivery as deferral:
                sub.quiescing = True
                logger.info(
                    "delivery_deferred",
                    topic=topic,
                    group=group,
                    event_type=event.type,
                    reason=str(deferral) or "unspecified",
                )
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
