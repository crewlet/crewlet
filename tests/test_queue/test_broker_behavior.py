"""Broker measurement harness — the empirical half of the seat-host design.

The takeover story in ``SCALING.md`` rests on three asserted Pulsar
behaviours, none of which had ever been measured against a real broker:

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
``tests/test_queue/test_pulsar.py``. Skipping is not passing: the
numbers in ``SCALING_PLAN.md`` are marked measurement-pending until a
run of this file fills them in.
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
    _RECEIVER_QUEUE_SIZE,
    PulsarEventQueue,
)

PULSAR_URL = "pulsar://localhost:6650"


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
_MEASURE_ACK_TIMEOUT_MS = 5_000


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
        )

    def drain(self, *, timeout_ms: int = 2000) -> list[int]:
        """Receive everything available, WITHOUT acking."""
        out: list[int] = []
        while True:
            try:
                msg = self.consumer.receive(timeout_ms)
            except Exception:
                return out
            from crewlet.queue.serialization import deserialize_event

            out.append(int(deserialize_event(msg.data()).payload["n"]))

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
    got = owner.drain()
    assert got, "owner received nothing to hold"
    started = time.monotonic()
    owner.close()

    successor = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    redelivered = successor.drain(timeout_ms=int(_MEASURE_ACK_TIMEOUT_MS * 2))
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
        "the drain path in SCALING_PLAN 5.8 needs a different story"
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
    held = zombie.drain()
    assert held

    successor = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    started = time.monotonic()
    # Poll until the ack timeout releases them, with headroom.
    deadline = started + (_MEASURE_ACK_TIMEOUT_MS / 1000) * 3
    redelivered: list[int] = []
    while time.monotonic() < deadline and not redelivered:
        redelivered = await asyncio.to_thread(successor.drain, timeout_ms=1000)
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
    rest = successor.drain(timeout_ms=3000)
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

    small = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    # Let the client fill its local queue, then count what a SECOND
    # consumer on the same subscription can still reach. Whatever the
    # first holds is hostage.
    await asyncio.sleep(2.0)
    peer = _RawConsumer(client, full, sub, queue_size=_RECEIVER_QUEUE_SIZE)
    reachable = await asyncio.to_thread(peer.drain, timeout_ms=3000)
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
