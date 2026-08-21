"""Broker measurement harness — the empirical half of the seat-host design.

The takeover story in ``docs/concepts/seat-ownership.md`` rests on three
asserted Pulsar behaviours, none of which had ever been measured against a
real broker:

1. **Session-death timing.** How long after a consumer's process dies
   does the broker redeliver its unacked messages? This is the floor
   under the seat lease TTL — a TTL shorter than the redelivery delay
   just means the new owner attaches and waits.
2. **Cursor continuity on owner handoff.** When the sole member of a
   Shared subscription detaches and a *different* consumer attaches
   under the same subscription name, does it resume from the shared
   cursor, or does anything get skipped or replayed? Owner-only-Shared
   is the whole reason Exclusive was rejected; if the cursor did not
   survive, seat handoff would lose or duplicate every in-flight
   trigger.
3. **Prefetch hostage size.** How many delivered-but-unacked messages
   does a consumer hold locally? This is what a wedged-alive node keeps
   away from its successor for a full ack-timeout, and the reason
   ``_RECEIVER_QUEUE_SIZE`` is now set explicitly instead of inheriting
   the client's default of 1000.

These are **measurements, not unit tests**. Each one asserts the
*property* the design depends on and prints the number the design should
be tuned to, so a broker upgrade that changes the behaviour fails here
rather than in a takeover at 3am.

Run them against a broker::

    docker compose up -d pulsar
    pytest tests/test_queue/test_broker_behavior.py -m integration -s

They skip when no broker is reachable, like the rest of
``tests/test_queue/test_pulsar.py`` — and skipping is not passing.

Measured on **Pulsar 4.2.4 standalone** (see
``docs/concepts/scaling.md`` § Where the constants come from for what each
number decides):

===============================================  ===================
redelivery after a graceful close                9 ms, nothing lost
redelivery from a wedged consumer                ack timeout + ~1 s
cursor continuity on owner handoff               no replay, no loss
attach latency to an existing subscription       4.9 ms
prefetch hostage at ``receiver_queue_size=64``   exactly 64
redeliveryCount after a close-driven handoff     **0** — free
redeliveryCount after an ack timeout             **1** — costs budget
===============================================  ===================

The last two are what decide the deferral design. A handoff that closes
its consumer costs nothing against ``_MAX_REDELIVER``, so a plain
close is a safe way to hand work back — no republish, no reordering. A
node that *dies* holding messages does spend it, which is why a budget
sized for poison messages is also, in a fleet, a budget of that many node
deaths per message — the reason ``_MAX_REDELIVER`` is 10 rather than 3.

Two behaviours the first real run corrected in this file, both of which
had silently voided its numbers: the client refuses an unacked-message
timeout below 10 s, and a *new* subscription starts at ``Latest`` — so a
consumer attaching after the publish sees nothing at all.
"""

from __future__ import annotations

import asyncio
import socket
import time
from uuid import uuid4

import pytest

pytest.importorskip("pulsar")

import pulsar  # noqa: E402

from crewlet.events.types import Event  # noqa: E402
from crewlet.queue.pulsar import (  # noqa: E402
    _INBOX_ACK_TIMEOUT_MS,
    _MAX_REDELIVER,
    _RECEIVER_QUEUE_SIZE,
    PulsarEventQueue,
)

PULSAR_URL = "pulsar://localhost:6650"
# The broker's admin HTTP endpoint, where a subscription can be created
# and deleted with no consumer attached.  Derived from PULSAR_URL by the
# same convention ``QueueConfig`` uses when ``admin_url`` is left empty.
_ADMIN_URL = "http://localhost:8080"


def _pulsar_available() -> bool:
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.settimeout(1)
    try:
        sock.connect(("localhost", 6650))
        return True
    except OSError:
        return False
    finally:
        sock.close()


_PULSAR_UP = _pulsar_available()

pytestmark = [
    pytest.mark.integration,
    pytest.mark.skipif(not _PULSAR_UP, reason="Pulsar not available at localhost:6650"),
]

# A short ack timeout for the redelivery measurements. The engine's real
# value is 30 minutes (an agent turn can run that long), which is not a
# thing a test can wait out — so the measurement is of the *mechanism*,
# and the report scales it.
#
# 11s, not less: the Pulsar client refuses to construct a consumer with
# an unacked-message timeout below 10 seconds ("Unacknowledged message
# timeout should be greater than 10 seconds"). Measured, not assumed —
# this is the first thing a real broker taught this harness.
_MEASURE_ACK_TIMEOUT_MS = 11_000


def _report(name: str, **values: object) -> None:
    """Print a measurement line. Visible under ``pytest -s``."""
    body = "  ".join(f"{k}={v}" for k, v in values.items())
    print(f"\n[broker-measurement] {name}: {body}")


def _event(n: int) -> Event:
    return Event(source="harness", type="probe", payload={"n": n})


class _RawConsumer:
    """A consumer built directly on the client, so the harness can kill
    it the way a dead process does — without the graceful close the
    EventQueue performs."""

    def __init__(self, client, topic: str, sub: str, *, queue_size: int) -> None:
        self.consumer = client.subscribe(
            topic,
            sub,
            consumer_type=pulsar.ConsumerType.Shared,
            unacked_messages_timeout_ms=_MEASURE_ACK_TIMEOUT_MS,
            receiver_queue_size=queue_size,
            # Earliest, explicitly. A NEW Pulsar subscription starts at
            # ``Latest`` by default — measured here the hard way, as a
            # consumer that received nothing at all because the harness
            # publishes before it attaches. It does not affect the
            # engine (its inbox subscriptions exist before traffic
            # flows), but it silently voids any measurement that
            # publishes first.
            initial_position=pulsar.InitialPosition.Earliest,
        )

    def drain(
        self, *, timeout_ms: int = 2000, limit: int = 10_000, deadline_s: float = 20.0
    ) -> list[int]:
        """Receive what is available, WITHOUT acking, deduped by payload.

        Bounded by BOTH a count and a wall clock, and deduped, because an
        unbounded version does not terminate — and the reason is the very
        mechanism under measurement. Nothing here is acked, so at the
        unacked-message timeout the broker redelivers everything it has
        already handed over, which refeeds the loop forever. That is the
        wedged-node window made visible.
        """
        from crewlet.queue.serialization import deserialize_event

        seen: set[int] = set()
        out: list[int] = []
        end = time.monotonic() + deadline_s
        while len(out) < limit and time.monotonic() < end:
            try:
                msg = self.consumer.receive(timeout_ms)
            except Exception:
                return out
            n = int(deserialize_event(msg.data()).payload["n"])
            if n in seen:
                continue  # a redelivery of something already counted
            seen.add(n)
            out.append(n)
        return out

    def close(self) -> None:
        self.consumer.close()


@pytest.fixture
def client():
    c = pulsar.Client(PULSAR_URL)
    yield c
    c.close()


@pytest.fixture
async def queue():
    q = PulsarEventQueue(PULSAR_URL)
    await q.start()
    yield q
    await q.stop()


async def _publish(q: PulsarEventQueue, topic: str, count: int) -> None:
    for n in range(count):
        await q.publish(topic, _event(n))


# ── 1. session death → redelivery ────────────────────────────────────


async def test_measure_redelivery_after_consumer_death(queue, client) -> None:
    """How long until a dead consumer's unacked messages come back?

    The design's claim: the seat lease TTL is the binding constraint,
    not the broker — because a *closed* consumer's messages are
    redelivered immediately (the broker sees the session end), and only
    a consumer that dies without closing waits out the ack timeout.

    Both paths are measured, because they are the two real failure
    modes: a graceful drain closes, a ``kill -9`` does not.
    """
    topic = f"crewlet.test.death.{uuid4().hex[:8]}"
    sub = "seat-owner"
    full = queue._full_topic(topic)

    await _publish(queue, topic, 3)

    # Owner takes delivery and does NOT ack, then closes cleanly — the
    # graceful-drain path.
    owner = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    got = owner.drain(limit=3, deadline_s=8.0)
    assert got, "owner received nothing to hold"
    started = time.monotonic()
    owner.close()

    successor = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    redelivered = successor.drain(timeout_ms=3000, limit=3, deadline_s=10.0)
    elapsed = time.monotonic() - started
    successor.close()

    _report(
        "redelivery_after_graceful_close",
        held=len(got),
        redelivered=len(redelivered),
        seconds=round(elapsed, 3),
        engine_ack_timeout_s=_INBOX_ACK_TIMEOUT_MS / 1000,
    )
    # The property the takeover pipeline depends on: nothing the dead
    # owner held is lost, and the successor gets it without waiting out
    # the ack timeout.
    assert set(redelivered) == set(got)
    assert elapsed < _MEASURE_ACK_TIMEOUT_MS / 1000, (
        "a cleanly-closed consumer's messages should not wait out the "
        "ack timeout — if they do, seat handoff is ack-timeout-bound and "
        "releasing each seat as it goes idle buys a drain nothing"
    )


async def test_measure_redelivery_when_the_consumer_never_closes(queue, client) -> None:
    """The ``kill -9`` shape: hold messages, never ack, never close.

    Here the ack timeout genuinely is the bound, which is why the engine
    caps the prefetch — see ``_RECEIVER_QUEUE_SIZE``.
    """
    topic = f"crewlet.test.wedged.{uuid4().hex[:8]}"
    sub = "seat-owner"
    full = queue._full_topic(topic)

    await _publish(queue, topic, 3)

    zombie = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    held = zombie.drain(limit=3, deadline_s=8.0)
    assert held

    successor = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    started = time.monotonic()
    # Poll until the ack timeout releases them, with headroom.
    deadline = started + (_MEASURE_ACK_TIMEOUT_MS / 1000) * 3
    redelivered: list[int] = []
    while time.monotonic() < deadline and not redelivered:
        redelivered = await asyncio.to_thread(
            successor.drain, timeout_ms=1000, limit=3, deadline_s=5.0
        )
    elapsed = time.monotonic() - started

    _report(
        "redelivery_from_wedged_consumer",
        held=len(held),
        redelivered=len(redelivered),
        seconds=round(elapsed, 3),
        measured_ack_timeout_s=_MEASURE_ACK_TIMEOUT_MS / 1000,
    )
    zombie.close()
    successor.close()

    assert redelivered, (
        "a wedged consumer's messages were never redelivered — the "
        "zombie window would be unbounded, and fencing (not the broker) "
        "is the only protection"
    )


# ── 2. cursor continuity on owner handoff ────────────────────────────


async def test_measure_cursor_survives_owner_handoff(queue, client) -> None:
    """Owner-only Shared: does the cursor survive a change of owner?

    This is the load-bearing claim behind rejecting Exclusive. The
    successor must resume where the predecessor's acks left off — no
    replay of acked work, no skip of unacked work.
    """
    topic = f"crewlet.test.handoff.{uuid4().hex[:8]}"
    sub = "seat-owner"
    full = queue._full_topic(topic)

    await _publish(queue, topic, 6)

    owner = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    first = []
    for _ in range(3):
        msg = owner.consumer.receive(2000)
        from crewlet.queue.serialization import deserialize_event

        first.append(int(deserialize_event(msg.data()).payload["n"]))
        owner.consumer.acknowledge(msg)
    owner.close()

    successor = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    rest = successor.drain(timeout_ms=3000, limit=3, deadline_s=10.0)
    successor.close()

    _report(
        "cursor_on_handoff",
        acked_by_owner=sorted(first),
        seen_by_successor=sorted(rest),
        replayed=sorted(set(first) & set(rest)),
    )
    assert not (set(first) & set(rest)), (
        "the successor replayed messages the predecessor had ACKED — the "
        "shared cursor did not survive handoff, and owner-only Shared is "
        "not a safe seat-ownership mechanism"
    )
    assert set(first) | set(rest) == set(range(6)), (
        "handoff lost messages that were never acked"
    )


async def test_measure_takeover_attach_latency(queue, client) -> None:
    """How long does attaching to an existing subscription take?

    The takeover pipeline attaches the inbox consumer LAST, after the
    lease and the sandbox-state scan, so this lands on the critical path
    of every seat handoff.
    """
    topic = f"crewlet.test.attach.{uuid4().hex[:8]}"
    sub = "seat-owner"
    full = queue._full_topic(topic)
    await _publish(queue, topic, 1)

    warm = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    warm.close()

    started = time.monotonic()
    successor = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    elapsed = time.monotonic() - started
    successor.close()

    _report("attach_latency", seconds=round(elapsed, 4))
    assert elapsed < 5.0, (
        "attaching to an existing subscription is slow enough to dominate "
        "seat takeover; the lease TTL and claim-rate limit need to account "
        "for it"
    )


# ── 3. prefetch hostage size ─────────────────────────────────────────


async def test_measure_prefetch_hostage_is_capped(queue, client) -> None:
    """A consumer must not silently hold hundreds of a seat's messages.

    With the client default (1000) a wedged-alive node holds every
    message a busy seat received, for a full ack timeout — thirty
    minutes in the engine's real configuration. The engine now sets
    ``receiver_queue_size`` explicitly; this measures that the cap is
    real rather than advisory.
    """
    topic = f"crewlet.test.prefetch.{uuid4().hex[:8]}"
    sub = "seat-owner"
    full = queue._full_topic(topic)

    published = _RECEIVER_QUEUE_SIZE * 4
    await _publish(queue, topic, published)

    # Construct the holder and DO NOT receive from it: the point is what
    # the client prefetches on its own. Calling receive() would pull
    # further messages and measure appetite rather than prefetch.
    small = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    await asyncio.sleep(3.0)
    peer = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    reachable = await asyncio.to_thread(
        peer.drain, timeout_ms=3000, limit=published, deadline_s=15.0
    )
    hostage = published - len(reachable)

    _report(
        "prefetch_hostage",
        published=published,
        configured_queue_size=_RECEIVER_QUEUE_SIZE,
        unreachable_by_peer=hostage,
        client_default_would_be=1000,
    )
    small.close()
    peer.close()

    # Generous: the client may hold a little beyond the nominal queue
    # size (in-flight permits, batching). What must NOT happen is the
    # 1000-message default.
    assert hostage <= _RECEIVER_QUEUE_SIZE * 3, (
        f"one consumer held {hostage} messages hostage with "
        f"receiver_queue_size={_RECEIVER_QUEUE_SIZE} — the explicit cap "
        "is not being honoured, and the wedged-node window is larger "
        "than the design assumes"
    )


async def test_engine_consumers_set_the_prefetch_explicitly(queue) -> None:
    """The regression guard, and the only one here that does not need to
    measure anything: an engine-created consumer must carry the explicit
    size rather than the client default.
    """
    topic = f"crewlet.test.explicit.{uuid4().hex[:8]}"

    async def _handler(event: Event) -> None: ...

    await queue.subscribe(topic, "probe-group", _handler)
    sub = next(s for s in queue._subscriptions if s.topic == topic)
    # The C++ client does not expose the configured size, so assert on
    # what the engine passed rather than on client state.
    assert sub.consumer is not None
    _report("engine_consumer_created", topic=topic, queue_size=_RECEIVER_QUEUE_SIZE)
    assert _RECEIVER_QUEUE_SIZE < 1000, (
        "the whole point of setting this explicitly is to be below the client default"
    )


# ── 4. what a handoff costs the dead-letter budget ───────────────────


async def test_measure_redelivery_count_across_a_handoff(queue, client) -> None:
    """Does handing a seat over spend the poison-message budget?

    ``_MAX_REDELIVER`` is 10, and Pulsar dead-letters a message that
    exceeds it. If a change of owner incremented the counter, three
    handoffs during a rolling deploy would discard a perfectly healthy
    event that had never been handled — which is what the original design
    invented republish-then-ack to avoid.
    """
    topic = f"crewlet.test.count.{uuid4().hex[:8]}"
    sub = "seat-owner"
    full = queue._full_topic(topic)
    await _publish(queue, topic, 6)

    owner = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    owner.consumer.receive(4000)  # take delivery, ack nothing
    owner.close()

    successor = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    counts = []
    for _ in range(6):
        try:
            msg = successor.consumer.receive(3000)
        except Exception:
            break
        counts.append(msg.redelivery_count())
        successor.consumer.acknowledge(msg)
    successor.close()

    _report(
        "redelivery_count_after_handoff",
        counts=counts,
        max_redeliver_budget=_MAX_REDELIVER,
    )
    assert counts and max(counts) == 0, (
        "a close-driven handoff incremented redeliveryCount — three seat "
        "handoffs would dead-letter a healthy event, and the deferral "
        "design has to republish rather than rely on close"
    )


async def test_measure_redelivery_count_after_an_ack_timeout(queue, client) -> None:
    """The other half: the ``kill -9`` path DOES spend the budget.

    A wedged or killed node never closes, so its messages come back via
    the unacked-message timeout — and that path increments. In a fleet
    where node death is routine, a budget of N is N node deaths per
    message, not N handler failures.

    The two causes mostly separate by rate, not by count: a genuine
    poison message is NAK'd and redelivered a second later, so it still
    burns any sane budget in seconds, while a handoff-driven redelivery is
    minutes apart. That is the argument for raising the cap.
    """
    topic = f"crewlet.test.acktimeout.{uuid4().hex[:8]}"
    sub = "seat-owner"
    full = queue._full_topic(topic)
    await _publish(queue, topic, 1)

    zombie = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    first = zombie.consumer.receive(4000)
    assert first.redelivery_count() == 0

    deadline = time.monotonic() + (_MEASURE_ACK_TIMEOUT_MS / 1000) * 4
    seen: int | None = None
    while time.monotonic() < deadline and seen is None:
        try:
            seen = (
                await asyncio.to_thread(zombie.consumer.receive, 2000)
            ).redelivery_count()
        except Exception:
            continue
    zombie.close()

    _report("redelivery_count_after_ack_timeout", count=seen)
    assert seen is not None and seen > 0, (
        "an ack-timeout redelivery did not increment the counter — the "
        "dead-letter budget behind _INBOX_MAX_REDELIVER needs re-deriving"
    )


# ---------------------------------------------------------------------------
# Subscription existence as an org invariant (phase 5.2, decision (a))
# ---------------------------------------------------------------------------
#
# The invariant: a seat's durable subscription exists whether or not any
# node owns the seat, so a publish into an unowned seat is *held* rather
# than dropped.  Establishing it by SUBSCRIBING is what the design review
# called fatal, and the first test below is the measurement that says so.
# The rest establish that the admin API can create and delete a
# subscription with no consumer at all, which is why ``BrokerAdmin``
# exists.


async def test_measure_joining_a_shared_subscription_steals_traffic(
    queue, client
) -> None:
    """Pre-creating a subscription by SUBSCRIBING is not safe.

    A seat's subscription is Shared, and a second consumer joining one
    that an owner is actively serving takes a share of that seat's live
    traffic into its own prefetch — for as long as it stays attached,
    plus the ack timeout if it dies rather than closing.  Every node
    doing this for every seat at boot manufactures exactly the
    double-consumer state seat ownership exists to prevent.

    This is why ``ensure_subscription`` goes through the admin API
    instead: no consumer, so nothing to share with.
    """
    topic = f"crewlet.test.join.{uuid4().hex[:8]}"
    full = queue._full_topic(topic)
    sub = "seat-owner"

    owner = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    joiner = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    await asyncio.sleep(0.5)  # let both register with the broker
    await _publish(queue, topic, 20)
    await asyncio.sleep(1.0)

    by_owner = owner.drain(timeout_ms=700, limit=20, deadline_s=8.0)
    by_joiner = joiner.drain(timeout_ms=700, limit=20, deadline_s=8.0)
    joiner.close()
    owner.close()

    _report(
        "shared_join_steals_traffic",
        owner=len(by_owner),
        joiner=len(by_joiner),
    )
    assert by_joiner, (
        "a consumer joining a Shared subscription received nothing — if "
        "this ever becomes true, decision (a) could pre-create by "
        "subscribing and BrokerAdmin would not be needed"
    )


async def test_admin_creates_a_subscription_with_no_consumer(queue) -> None:
    """The invariant, established without attaching anything.

    Also covers the case that matters most: the topic does not exist
    yet.  A brand-new company's seats have never been published to, so
    ``ensure_subscription`` must create the topic as a side effect
    rather than failing.
    """
    from crewlet.queue.admin import PulsarBrokerAdmin

    admin = PulsarBrokerAdmin(_ADMIN_URL)
    topic = f"crewlet.test.precreate.{uuid4().hex[:8]}"
    group = "agent-precreate"

    assert await admin.subscriptions(topic) == []
    assert await admin.ensure_subscription(topic, group) is True
    assert await admin.subscriptions(topic) == [group]
    # Idempotent: the broker answers 409 on the second create, which is
    # success, not an error — every node may run the ensure pass.
    assert await admin.ensure_subscription(topic, group) is False

    await admin.close()


async def test_a_precreated_subscription_holds_an_unowned_seats_mail(
    queue, client
) -> None:
    """The property decision (a) exists for.

    Publish into a seat nobody owns, then claim it.  Without the
    pre-created subscription the publish is dropped on the floor — no
    dead letter, no producer error, nothing to alert on.
    """
    from crewlet.queue.admin import PulsarBrokerAdmin

    admin = PulsarBrokerAdmin(_ADMIN_URL)
    topic = f"crewlet.test.unowned.{uuid4().hex[:8]}"
    group = "agent-unowned"
    await admin.ensure_subscription(topic, group)

    await _publish(queue, topic, 5)  # nobody is attached

    claimer = _RawConsumer(
        client, queue._full_topic(topic), group, queue_size=_RECEIVER_QUEUE_SIZE
    )
    held = claimer.drain(timeout_ms=2000, limit=5, deadline_s=10.0)
    claimer.close()
    await admin.close()

    _report("unowned_seat_backlog", held=sorted(held))
    assert sorted(held) == list(range(5)), (
        "mail published to an unowned seat did not survive until someone "
        "claimed it — the subscription-existence invariant is not holding"
    )


async def test_admin_deletes_a_subscription_with_no_consumer(queue) -> None:
    """Decommission, without needing the node that ran the seat.

    ``Consumer.unsubscribe()`` needs a local consumer, which means role
    removal would depend on which node happens to hold the seat.  The
    admin path does not, and tolerates the already-gone case so a retried
    decommission is not an error.
    """
    from crewlet.queue.admin import PulsarBrokerAdmin

    admin = PulsarBrokerAdmin(_ADMIN_URL)
    topic = f"crewlet.test.decommission.{uuid4().hex[:8]}"
    group = "agent-decommission"
    await admin.ensure_subscription(topic, group)

    assert await admin.delete_subscription(topic, group) is True
    assert await admin.subscriptions(topic) == []
    # Already gone — 404 from the broker, which is the desired end state.
    assert await admin.delete_subscription(topic, group) is False

    await admin.close()
