"""Unit tests for ``PulsarEventQueue`` that don't need a live broker.

The integration tests in ``test_pulsar.py`` cover the end-to-end
roundtrip when a Pulsar broker is reachable.  This file focuses on the
pieces that are pure logic: the shutdown-sensitive ``_process_message``
paths (cancellation NAK, ack/nak ordering, the in-flight counter) and
the subject<->topic / pattern->regex translation, exercised with fakes
so the tests run wherever ``pulsar-client`` is importable.
"""

from __future__ import annotations

import asyncio
import logging
import re
import time
from concurrent.futures import ThreadPoolExecutor
from unittest.mock import MagicMock

import pytest

pulsar = pytest.importorskip("pulsar")

from crewlet.events.types import Event  # noqa: E402
from crewlet.queue.pulsar import PulsarEventQueue, _Subscription  # noqa: E402


class _FakeConsumer:
    """Stand-in for a Pulsar ``Consumer`` tracking ack/nak counts."""

    def __init__(self) -> None:
        self.acked = 0
        self.naked = 0

    def acknowledge(self, _msg: object) -> None:
        self.acked += 1

    def negative_acknowledge(self, _msg: object) -> None:
        self.naked += 1


class _FakeMsg:
    """Stand-in for a Pulsar ``Message`` (``data()`` is a method)."""

    def __init__(self, event: Event) -> None:
        self._data = event.model_dump_json().encode()

    def data(self) -> bytes:
        return self._data


class _FakeBadMsg:
    """Msg whose data fails to deserialize -- exercises the corrupt path."""

    def data(self) -> bytes:
        return b"not-json"


def _sub(consumer: object, topic: str = "topic", group: str = "group") -> _Subscription:
    """A subscription record around a fake consumer.

    The seam every dispatch path now takes: ``_process_message`` /
    ``_process_batch`` / ``_collect_batch`` read the consumer, the
    topic, the group AND the quiescing flag off one record, so a
    mismatched trio is no longer constructible and the seat-handoff
    paths are testable without a broker.
    """
    return _Subscription(
        consumer=consumer,
        executor=ThreadPoolExecutor(max_workers=1),
        topic=topic,
        group=group,
    )


@pytest.fixture
def queue() -> PulsarEventQueue:
    # ``__init__`` is pure construction -- no network.  We only call
    # ``_process_message`` / translation helpers directly.
    return PulsarEventQueue("pulsar://unused:6650")


# ---------------------------------------------------------------------------
# _process_message
# ---------------------------------------------------------------------------


async def test_process_message_acks_on_success(queue: PulsarEventQueue) -> None:
    """Happy path: handler returns → ack, no nak, counter balanced."""
    consumer = _FakeConsumer()
    msg = _FakeMsg(Event(type="test", source="t"))
    handled: list[Event] = []

    async def handler(e: Event) -> None:
        handled.append(e)

    await queue._process_message(
        _sub(consumer, "topic", "group"),
        msg,
        handler,
    )

    assert consumer.acked == 1
    assert consumer.naked == 0
    assert handled[0].type == "test"
    assert queue._in_flight == 0
    assert queue._idle_event.is_set()


async def test_process_message_naks_on_handler_exception(
    queue: PulsarEventQueue,
) -> None:
    """Handler raises Exception → nak, counter still balanced."""
    consumer = _FakeConsumer()
    msg = _FakeMsg(Event(type="boom", source="t"))

    async def handler(_e: Event) -> None:
        raise RuntimeError("boom")

    await queue._process_message(
        _sub(consumer, "topic", "group"),
        msg,
        handler,
    )

    assert consumer.acked == 0
    assert consumer.naked == 1
    assert queue._in_flight == 0
    assert queue._idle_event.is_set()


async def test_process_message_naks_and_reraises_on_cancellation(
    queue: PulsarEventQueue,
) -> None:
    """Handler raises CancelledError → nak so Pulsar redelivers promptly
    to the next engine subscription AND cancellation propagates so the
    consumer task exits cleanly.
    """
    consumer = _FakeConsumer()
    msg = _FakeMsg(Event(type="cancelled", source="t"))

    async def handler(_e: Event) -> None:
        raise asyncio.CancelledError

    with pytest.raises(asyncio.CancelledError):
        await queue._process_message(
            _sub(consumer, "topic", "group"),
            msg,
            handler,
        )

    assert consumer.acked == 0
    assert consumer.naked == 1, "cancelled in-flight msg must be NAK'd for restart"
    assert queue._in_flight == 0
    assert queue._idle_event.is_set()


async def test_process_message_corrupt_message_is_acked(
    queue: PulsarEventQueue,
) -> None:
    """Undecodable payload is ack'd so the broker stops redelivering it."""
    consumer = _FakeConsumer()
    handler_called = False

    async def handler(_e: Event) -> None:
        nonlocal handler_called
        handler_called = True

    await queue._process_message(
        _sub(consumer, "topic", "group"),
        _FakeBadMsg(),
        handler,
    )

    assert consumer.acked == 1
    assert consumer.naked == 0
    assert handler_called is False
    # Corrupt-msg path does NOT touch the in-flight counter.
    assert queue._in_flight == 0
    assert queue._idle_event.is_set()


async def test_in_flight_counter_visible_during_handler(
    queue: PulsarEventQueue,
) -> None:
    """The in-flight counter is incremented before the handler awaits
    and decremented in the finally block -- so the engine's drain can
    observe the in-flight handler and wait.
    """
    consumer = _FakeConsumer()
    msg = _FakeMsg(Event(type="watch", source="t"))
    saw_in_flight: list[int] = []
    handler_started = asyncio.Event()
    handler_can_finish = asyncio.Event()

    async def handler(_e: Event) -> None:
        handler_started.set()
        await handler_can_finish.wait()

    task = asyncio.create_task(queue._process_message(_sub(consumer), msg, handler))
    await handler_started.wait()

    saw_in_flight.append(queue._in_flight)
    assert not queue._idle_event.is_set()

    handler_can_finish.set()
    await task

    assert saw_in_flight == [1]
    assert queue._in_flight == 0
    assert queue._idle_event.is_set()
    assert consumer.acked == 1


# ---------------------------------------------------------------------------
# subject <-> topic translation
# ---------------------------------------------------------------------------


def test_full_topic_and_local_subject_roundtrip(queue: PulsarEventQueue) -> None:
    subject = "crewlet.agent.alice.inbox"
    full = queue._full_topic(subject)
    assert full == "persistent://public/default/crewlet.agent.alice.inbox"
    assert queue._local_subject(full) == subject


def test_full_topic_honours_tenant_and_namespace() -> None:
    q = PulsarEventQueue("pulsar://unused:6650", tenant="crewlet", namespace="events")
    assert q._full_topic("crewlet.x") == "persistent://crewlet/events/crewlet.x"
    assert q._local_subject("persistent://crewlet/events/crewlet.x") == "crewlet.x"
    # The broadcast pattern regex carries the same tenant/namespace prefix.
    assert q._pattern_to_regex("crewlet.>").startswith("crewlet/events/")


# ---------------------------------------------------------------------------
# pattern -> regex (mirrors crewlet.queue.matching.topic_matches semantics)
# ---------------------------------------------------------------------------

_NS = "persistent://public/default/"


@pytest.mark.parametrize(
    ("pattern", "topic", "expected"),
    [
        # exact match
        ("crewlet.agent.alice.inbox", f"{_NS}crewlet.agent.alice.inbox", True),
        ("crewlet.agent.alice.inbox", f"{_NS}crewlet.agent.bob.inbox", False),
        # single-segment wildcard
        ("crewlet.agent.*.inbox", f"{_NS}crewlet.agent.alice.inbox", True),
        ("crewlet.agent.*.inbox", f"{_NS}crewlet.agent.bob.inbox", True),
        ("crewlet.agent.*.inbox", f"{_NS}crewlet.agent.alice.outbox", False),
        # '*' is exactly one segment, never multi-segment
        ("crewlet.agent.*.inbox", f"{_NS}crewlet.agent.a.b.inbox", False),
        ("crewlet.events.*", f"{_NS}crewlet.events.task_created", True),
        ("crewlet.events.*", f"{_NS}crewlet.events.task.created", False),
        # trailing '>' matches one-or-more trailing segments
        ("crewlet.>", f"{_NS}crewlet.notifications.inbound", True),
        ("crewlet.>", f"{_NS}crewlet.agent.alice.inbox", True),
        ("crewlet.>", f"{_NS}other.thing", False),
        # '>' requires at least one trailing segment
        ("crewlet.>", f"{_NS}crewlet", False),
        # the dashboard's broadcast pattern matches real event topics ...
        ("crewlet.events.>", f"{_NS}crewlet.events.task_assigned", True),
        # ... but NOT dead-letter topics, which carry a ``dlq-`` prefix so
        # a poison event never re-surfaces in the live stream.
        ("crewlet.events.>", f"{_NS}dlq-crewlet.events.task_assigned-routing", False),
    ],
)
def test_pattern_to_regex_matches(
    queue: PulsarEventQueue, pattern: str, topic: str, expected: bool
) -> None:
    regex = queue._pattern_to_regex(pattern)
    assert (re.search(regex, topic) is not None) is expected


def test_pattern_to_regex_has_no_domain_or_leading_anchor(
    queue: PulsarEventQueue,
) -> None:
    """The regex starts with the literal ``tenant/namespace/`` prefix --
    no ``persistent://`` domain (which the client rejects for pattern
    subscriptions) and no leading ``^`` (which would break the client's
    namespace parsing of the pattern).
    """
    regex = queue._pattern_to_regex("crewlet.>")
    assert regex.startswith("public/default/")
    assert "persistent://" not in regex
    assert not regex.startswith("^")


# ---------------------------------------------------------------------------
# subscription lifecycle (dedicated receive executor, cleanup on stop)
# ---------------------------------------------------------------------------


async def test_subscribe_uses_dedicated_executor_cleaned_up_on_stop() -> None:
    """Each subscription gets its own receive executor (so the consume
    poll never shares the default thread pool), and ``stop`` closes the
    consumer and shuts that executor down.  Exercised with a fake client
    whose ``receive`` always times out, so no broker is needed.
    """

    def _slow_timeout(*_a: object, **_k: object) -> None:
        # Throttle the poll loop and mimic receive()'s blocking timeout.
        time.sleep(0.02)
        raise pulsar.Timeout

    fake_consumer = MagicMock()
    fake_consumer.receive.side_effect = _slow_timeout
    fake_client = MagicMock()
    fake_client.subscribe.return_value = fake_consumer

    q = PulsarEventQueue("pulsar://unused:6650")
    q._client = fake_client
    q._running = True

    async def handler(_e: Event) -> None: ...

    await q.subscribe("topic", "group", handler)
    assert len(q._subscriptions) == 1
    sub = q._subscriptions[0]
    assert isinstance(sub.executor, ThreadPoolExecutor)
    await asyncio.sleep(0.1)  # let the consume loop spin at least once

    await q.stop()

    assert q._subscriptions == []
    fake_consumer.close.assert_called()  # consumer closed explicitly
    with pytest.raises(RuntimeError):
        sub.executor.submit(lambda: None)  # executor was shut down


# ---------------------------------------------------------------------------
# client logger (drops the benign per-poll "Receive timeout" warnings)
# ---------------------------------------------------------------------------


def test_client_logger_filters_benign_timeouts_only() -> None:
    from crewlet.queue.pulsar import _client_logger

    log = _client_logger()
    assert log.level == logging.WARNING
    # Idempotent: building it again must not stack duplicate filters.
    _client_logger()
    drops = [
        f for f in log.filters if f.__class__.__name__ == "_DropBenignClientTimeouts"
    ]
    assert len(drops) == 1

    def _record(msg: str) -> logging.LogRecord:
        return logging.LogRecord(
            log.name, logging.WARNING, __file__, 0, msg, None, None
        )

    flt = drops[0]
    # Benign per-poll receive timeout is dropped ...
    assert flt.filter(_record("Receive timeout after 1000 ms, queue size: 0")) is False
    # ... as is the self-resolved (zero-pending) send queueing-delay noise ...
    assert (
        flt.filter(
            _record(
                "[persistent://public/default/crewlet.notifications.inbound, "
                "standalone-0-10] Send timeout due to queueing delay, "
                "connection: [...], pending messages: 0, queue size: 0"
            )
        )
        is False
    )
    # ... but a real backlog send timeout (pending > 0) is kept ...
    assert (
        flt.filter(_record("Send timeout due to queueing delay, pending messages: 7"))
        is True
    )
    # ... and every other client log passes through.
    assert flt.filter(_record("Reconnecting to broker after disconnect")) is True


# ---------------------------------------------------------------------------
# publish resilience (retry transient send failures; never wedge the loop)
# ---------------------------------------------------------------------------


async def test_publish_retries_transient_send_failures(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A transient send failure (broker queueing delay / timeout) is
    retried with backoff and the publish ultimately succeeds -- no event
    dropped, and the slow broker never blocks the event loop."""
    import crewlet.queue.pulsar as pulsar_mod

    monkeypatch.setattr(pulsar_mod, "_PUBLISH_RETRY_BASE_DELAY", 0.0)
    q = PulsarEventQueue("pulsar://unused:6650")
    q._running = True
    q._client = object()  # non-None; _get_producer is patched below

    async def _fake_get_producer(_full_topic: str) -> object:
        return object()

    monkeypatch.setattr(q, "_get_producer", _fake_get_producer)

    attempts = {"n": 0}

    async def _flaky_send(_producer: object, _data: bytes) -> None:
        attempts["n"] += 1
        if attempts["n"] < 3:
            raise RuntimeError("pulsar send result: Timeout")

    monkeypatch.setattr(q, "_send_once", _flaky_send)

    await q.publish("crewlet.events.x", Event(type="x"))
    assert attempts["n"] == 3  # two failures, then success


async def test_publish_raises_after_exhausting_retries(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """When every attempt fails, publish raises after the cap -- bounded,
    no silent loss, no infinite loop."""
    import crewlet.queue.pulsar as pulsar_mod

    monkeypatch.setattr(pulsar_mod, "_PUBLISH_RETRY_BASE_DELAY", 0.0)
    q = PulsarEventQueue("pulsar://unused:6650")
    q._running = True
    q._client = object()

    async def _fake_get_producer(_full_topic: str) -> object:
        return object()

    monkeypatch.setattr(q, "_get_producer", _fake_get_producer)

    attempts = {"n": 0}

    async def _always_fail(_producer: object, _data: bytes) -> None:
        attempts["n"] += 1
        raise RuntimeError("pulsar send result: Timeout")

    monkeypatch.setattr(q, "_send_once", _always_fail)

    with pytest.raises(RuntimeError, match="after .* attempts"):
        await q.publish("crewlet.events.x", Event(type="x"))
    assert attempts["n"] == pulsar_mod._PUBLISH_MAX_ATTEMPTS


# ---------------------------------------------------------------------------
# client auth (token + TLS trust certs)
# ---------------------------------------------------------------------------


def test_client_kwargs_default_unauthenticated(queue: PulsarEventQueue) -> None:
    kwargs = queue._client_kwargs()
    assert "authentication" not in kwargs
    assert "tls_trust_certs_file_path" not in kwargs
    assert "logger" in kwargs


def test_client_kwargs_with_token_and_tls() -> None:
    q = PulsarEventQueue(
        "pulsar+ssl://broker:6651",
        tenant="crewlet-a",
        namespace="prod",
        auth_token="eyJhbGciOiJIUzI1NiJ9.fake.fake",
        tls_trust_certs_path="/etc/ssl/ca.pem",
    )
    kwargs = q._client_kwargs()
    assert isinstance(kwargs["authentication"], pulsar.AuthenticationToken)
    assert kwargs["tls_trust_certs_file_path"] == "/etc/ssl/ca.pem"


# ---------------------------------------------------------------------------
# _process_batch (batched, key-partitioned delivery)
# ---------------------------------------------------------------------------


class _TrackingConsumer:
    """Fake consumer that records WHICH messages were acked / naked."""

    def __init__(self) -> None:
        self.acked: list[object] = []
        self.naked: list[object] = []

    def acknowledge(self, msg: object) -> None:
        self.acked.append(msg)

    def negative_acknowledge(self, msg: object) -> None:
        self.naked.append(msg)


def _conv_key(event: Event) -> str:
    return event.payload.get("conv", f"event:{event.id}")


async def test_process_batch_partitions_by_key_and_acks_per_partition(
    queue: PulsarEventQueue,
) -> None:
    consumer = _TrackingConsumer()
    m1 = _FakeMsg(Event(type="a", payload={"conv": "c1"}))
    m2 = _FakeMsg(Event(type="b", payload={"conv": "c2"}))
    m3 = _FakeMsg(Event(type="c", payload={"conv": "c1"}))
    batches: list[list[Event]] = []

    async def handler(events: list[Event]) -> None:
        batches.append(events)

    await queue._process_batch(
        _sub(consumer, "t", "g"),
        [m1, m2, m3],
        handler,
        _conv_key,
    )

    # Oldest-constituent dispatch order (same as first-arrival here —
    # auto-now timestamps follow construction order); same-key events
    # stay together.
    assert [[e.type for e in b] for b in batches] == [["a", "c"], ["b"]]
    assert set(consumer.acked) == {m1, m2, m3}
    assert consumer.naked == []
    assert queue._in_flight == 0
    assert queue._idle_event.is_set()


async def test_process_batch_naks_only_the_failing_partition(
    queue: PulsarEventQueue,
) -> None:
    """One conversation's handler failure redelivers ONLY that
    conversation's messages -- the other partition is acked normally."""
    consumer = _TrackingConsumer()
    m1 = _FakeMsg(Event(type="a", payload={"conv": "boom"}))
    m2 = _FakeMsg(Event(type="b", payload={"conv": "boom"}))
    m3 = _FakeMsg(Event(type="c", payload={"conv": "ok"}))

    async def handler(events: list[Event]) -> None:
        if events[0].payload["conv"] == "boom":
            raise RuntimeError("handler blew up")

    await queue._process_batch(
        _sub(consumer, "t", "g"),
        [m1, m2, m3],
        handler,
        _conv_key,
    )

    assert set(consumer.naked) == {m1, m2}
    assert consumer.acked == [m3]
    assert queue._in_flight == 0
    assert queue._idle_event.is_set()


async def test_process_batch_corrupt_message_acked_rest_delivered(
    queue: PulsarEventQueue,
) -> None:
    """A corrupt payload inside a batch is acked-and-dropped without
    poisoning its siblings."""
    consumer = _TrackingConsumer()
    bad = _FakeBadMsg()
    good = _FakeMsg(Event(type="a", payload={"conv": "c1"}))
    batches: list[list[Event]] = []

    async def handler(events: list[Event]) -> None:
        batches.append(events)

    await queue._process_batch(
        _sub(consumer, "t", "g"),
        [bad, good],
        handler,
        _conv_key,
    )

    assert [[e.type for e in b] for b in batches] == [["a"]]
    assert consumer.acked == [bad, good]
    assert consumer.naked == []


async def test_process_batch_cancellation_naks_current_and_remaining(
    queue: PulsarEventQueue,
) -> None:
    """Force-stop mid-batch: the partition being handled AND every
    not-yet-dispatched partition are NAK'd so none of them sit out the
    ack-timeout window on the broker."""
    consumer = _TrackingConsumer()
    m1 = _FakeMsg(Event(type="a", payload={"conv": "done"}))
    m2 = _FakeMsg(Event(type="b", payload={"conv": "cancelled"}))
    m3 = _FakeMsg(Event(type="c", payload={"conv": "never-reached"}))

    async def handler(events: list[Event]) -> None:
        if events[0].payload["conv"] == "cancelled":
            raise asyncio.CancelledError

    with pytest.raises(asyncio.CancelledError):
        await queue._process_batch(
            _sub(consumer, "t", "g"),
            [m1, m2, m3],
            handler,
            _conv_key,
        )

    assert consumer.acked == [m1]
    assert set(consumer.naked) == {m2, m3}
    assert queue._in_flight == 0
    assert queue._idle_event.is_set()


async def test_process_batch_key_failure_falls_back_to_unique_key(
    queue: PulsarEventQueue,
) -> None:
    consumer = _TrackingConsumer()
    m1 = _FakeMsg(Event(type="a"))
    m2 = _FakeMsg(Event(type="b"))
    batches: list[list[Event]] = []

    def bad_key(_event: Event) -> str:
        raise ValueError("no key")

    async def handler(events: list[Event]) -> None:
        batches.append(events)

    await queue._process_batch(
        _sub(consumer, "t", "g"),
        [m1, m2],
        handler,
        bad_key,
    )

    assert [len(b) for b in batches] == [1, 1]
    assert set(consumer.acked) == {m1, m2}


# ---------------------------------------------------------------------------
# _collect_batch (drain / linger collection)
# ---------------------------------------------------------------------------


class _ScriptedReceiveConsumer:
    """Fake consumer whose ``receive`` pops scripted messages, then
    raises ``pulsar.Timeout`` forever (an empty local queue)."""

    def __init__(self, msgs: list[object]) -> None:
        self._msgs = list(msgs)
        self.receive_timeouts: list[int] = []

    def receive(self, timeout_ms: int) -> object:
        self.receive_timeouts.append(timeout_ms)
        if self._msgs:
            return self._msgs.pop(0)
        raise pulsar.Timeout


async def test_collect_batch_zero_linger_drains_until_first_empty_poll() -> None:
    from crewlet.queue.protocol import BatchOptions

    q = PulsarEventQueue("pulsar://unused:6650")
    q._running = True
    msgs = [object(), object(), object()]
    consumer = _ScriptedReceiveConsumer(msgs)
    executor = ThreadPoolExecutor(max_workers=1)
    try:
        collected = await q._collect_batch(
            _Subscription(consumer=consumer, executor=executor),
            asyncio.get_running_loop(),
            BatchOptions(linger_seconds=0.0, max_batch=20),
        )
    finally:
        executor.shutdown(wait=False)

    assert collected == msgs  # everything locally available
    # One extra (timed-out) poll ended the drain pass.
    assert len(consumer.receive_timeouts) == 4


async def test_collect_batch_respects_max_batch_cap() -> None:
    from crewlet.queue.protocol import BatchOptions

    q = PulsarEventQueue("pulsar://unused:6650")
    q._running = True
    msgs = [object() for _ in range(10)]
    consumer = _ScriptedReceiveConsumer(msgs)
    executor = ThreadPoolExecutor(max_workers=1)
    try:
        collected = await q._collect_batch(
            _Subscription(consumer=consumer, executor=executor),
            asyncio.get_running_loop(),
            # max_batch counts the already-received first message too,
            # so the collector fetches at most max_batch - 1 more.
            BatchOptions(linger_seconds=0.0, max_batch=4),
        )
    finally:
        executor.shutdown(wait=False)

    assert collected == msgs[:3]


async def test_collect_batch_linger_window_keeps_collecting_past_empty_polls() -> None:
    """With a positive linger, an empty poll does NOT end collection --
    the window does.  Messages arriving mid-window are picked up."""
    from crewlet.queue.protocol import BatchOptions

    q = PulsarEventQueue("pulsar://unused:6650")
    q._running = True

    late = object()

    class _LateConsumer:
        """Times out once, then returns a late message, then is empty."""

        def __init__(self) -> None:
            self.calls = 0

        def receive(self, _timeout_ms: int) -> object:
            self.calls += 1
            if self.calls == 1:
                raise pulsar.Timeout
            if self.calls == 2:
                return late
            raise pulsar.Timeout

    executor = ThreadPoolExecutor(max_workers=1)
    try:
        collected = await q._collect_batch(
            _Subscription(consumer=_LateConsumer(), executor=executor),
            asyncio.get_running_loop(),
            BatchOptions(linger_seconds=0.2, max_batch=20),
        )
    finally:
        executor.shutdown(wait=False)

    assert collected == [late]


async def test_collect_batch_stops_on_pause() -> None:
    from crewlet.queue.protocol import BatchOptions

    q = PulsarEventQueue("pulsar://unused:6650")
    q._running = True
    q._paused = True
    consumer = _ScriptedReceiveConsumer([object(), object()])
    executor = ThreadPoolExecutor(max_workers=1)
    try:
        collected = await q._collect_batch(
            _Subscription(consumer=consumer, executor=executor),
            asyncio.get_running_loop(),
            BatchOptions(linger_seconds=5.0, max_batch=20),
        )
    finally:
        executor.shutdown(wait=False)

    assert collected == []  # paused before any fetch


# ---------------------------------------------------------------------------
# _process_batch ack-budget deferral
# ---------------------------------------------------------------------------


async def test_process_batch_defers_partitions_past_dispatch_budget(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Once the dispatch budget is spent, remaining partitions are
    requeued (republish, then ack the original) instead of dispatched —
    no message sits delivered-but-unacked behind the sum of preceding
    turns, and the requeued copies carry zero accrued redeliveries."""
    import crewlet.queue.pulsar as pulsar_mod

    monkeypatch.setattr(pulsar_mod, "_BATCH_DISPATCH_BUDGET_MS", 0)
    q = PulsarEventQueue("pulsar://unused:6650")
    q._running = True
    republished: list[tuple[str, Event]] = []

    async def _fake_publish(topic: str, event: Event) -> None:
        republished.append((topic, event))

    monkeypatch.setattr(q, "publish", _fake_publish)

    consumer = _TrackingConsumer()
    m1 = _FakeMsg(Event(type="a", payload={"conv": "c1"}))
    m2 = _FakeMsg(Event(type="b", payload={"conv": "c2"}))
    m3 = _FakeMsg(Event(type="c", payload={"conv": "c2"}))
    handled: list[list[Event]] = []

    async def handler(events: list[Event]) -> None:
        handled.append(events)

    await q._process_batch(
        _sub(consumer, "t", "g"),
        [m1, m2, m3],
        handler,
        _conv_key,
    )

    # First partition dispatched normally; c2 deferred whole.
    assert [[e.type for e in b] for b in handled] == [["a"]]
    assert [e.type for _, e in republished] == ["b", "c"]
    assert all(topic == "t" for topic, _ in republished)
    # Every message acked exactly once (c1 after its handler, c2 after
    # republish); nothing NAK'd, so no redelivery count accrued.
    assert set(consumer.acked) == {m1, m2, m3}
    assert consumer.naked == []
    assert q._in_flight == 0


async def test_process_batch_defer_republish_failure_naks_remainder(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A republish failure mid-deferral NAKs the not-yet-requeued
    messages (normal redelivery takes over) without dropping any."""
    import crewlet.queue.pulsar as pulsar_mod

    monkeypatch.setattr(pulsar_mod, "_BATCH_DISPATCH_BUDGET_MS", 0)
    q = PulsarEventQueue("pulsar://unused:6650")
    q._running = True
    calls = {"n": 0}

    async def _flaky_publish(topic: str, event: Event) -> None:
        calls["n"] += 1
        if calls["n"] >= 2:
            raise RuntimeError("broker down")

    monkeypatch.setattr(q, "publish", _flaky_publish)

    consumer = _TrackingConsumer()
    m1 = _FakeMsg(Event(type="a", payload={"conv": "c1"}))
    m2 = _FakeMsg(Event(type="b", payload={"conv": "c2"}))
    m3 = _FakeMsg(Event(type="c", payload={"conv": "c2"}))

    async def handler(events: list[Event]) -> None: ...

    await q._process_batch(
        _sub(consumer, "t", "g"),
        [m1, m2, m3],
        handler,
        _conv_key,
    )

    # c1 handled + acked; c2's first message republished + acked, its
    # second hit the publish failure and was NAK'd for redelivery.
    assert consumer.acked == [m1, m2]
    assert consumer.naked == [m3]


async def test_process_batch_within_budget_dispatches_all_partitions(
    queue: PulsarEventQueue,
) -> None:
    """Fast multi-partition drains (sub-budget handlers) finish in one
    pass — deferral only kicks in when the budget is actually spent."""
    consumer = _TrackingConsumer()
    m1 = _FakeMsg(Event(type="a", payload={"conv": "c1"}))
    m2 = _FakeMsg(Event(type="b", payload={"conv": "c2"}))
    handled: list[list[Event]] = []

    async def handler(events: list[Event]) -> None:
        handled.append(events)

    await queue._process_batch(
        _sub(consumer, "t", "g"),
        [m1, m2],
        handler,
        _conv_key,
    )

    assert [[e.type for e in b] for b in handled] == [["a"], ["b"]]
    assert set(consumer.acked) == {m1, m2}


async def test_process_batch_budget_counts_time_since_first_receive(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The dispatch budget runs from the FIRST receive — a long linger
    window spent collecting must count against it, because the broker's
    ack clocks started at receive, not at dispatch."""
    q = PulsarEventQueue("pulsar://unused:6650")
    q._running = True
    republished: list[Event] = []

    async def _fake_publish(topic: str, event: Event) -> None:
        republished.append(event)

    monkeypatch.setattr(q, "publish", _fake_publish)

    consumer = _TrackingConsumer()
    m1 = _FakeMsg(Event(type="a", payload={"conv": "c1"}))
    m2 = _FakeMsg(Event(type="b", payload={"conv": "c2"}))
    handled: list[list[Event]] = []

    async def handler(events: list[Event]) -> None:
        handled.append(events)

    # Pretend collection (linger) already consumed the whole budget.
    stale_receive = time.monotonic() - 120.0
    await q._process_batch(
        _sub(consumer, "t", "g"),
        [m1, m2],
        handler,
        _conv_key,
        received_at=stale_receive,
    )

    # First partition still dispatches (it must make progress); the
    # second defers immediately instead of holding its aged ack clock
    # behind another turn.
    assert [[e.type for e in b] for b in handled] == [["a"]]
    assert [e.type for e in republished] == ["b"]


async def test_process_batch_dispatches_oldest_conversation_first(
    queue: PulsarEventQueue,
) -> None:
    """Deferral fairness: the conversation that has waited longest runs
    first, regardless of receive order — its requeued copies carry their
    original timestamps, so a hot conversation's fresh arrivals cannot
    starve it drain after drain."""
    from datetime import UTC, datetime, timedelta

    t0 = datetime(2026, 6, 10, 12, 0, 0, tzinfo=UTC)
    consumer = _TrackingConsumer()
    hot = _FakeMsg(
        Event(type="hot", payload={"conv": "hot"}, timestamp=t0 + timedelta(seconds=90))
    )
    quiet = _FakeMsg(Event(type="quiet", payload={"conv": "quiet"}, timestamp=t0))
    handled: list[str] = []

    async def handler(events: list[Event]) -> None:
        handled.append(events[0].payload["conv"])

    # Receive order: hot first.
    await queue._process_batch(
        _sub(consumer, "t", "g"),
        [hot, quiet],
        handler,
        _conv_key,
    )
    assert handled == ["quiet", "hot"]


# ---------------------------------------------------------------------------
# detach / quiesce (seat handoff)
# ---------------------------------------------------------------------------
#
# The whole of seat ownership rests on detach being NON-destructive: the
# durable subscription and its cursor must survive, so an unowned seat's
# mail is retained rather than discarded. There is exactly one way to get
# that wrong in this module, and it is one character of difference —
# calling ``consumer.unsubscribe()`` instead of ``consumer.close()``.


class _RecordingConsumer:
    """Distinguishes close (keeps the subscription) from unsubscribe
    (deletes it and its retained messages)."""

    def __init__(self) -> None:
        self.closed = 0
        self.unsubscribed = 0
        self.redelivered = 0

    def close(self) -> None:
        self.closed += 1

    def unsubscribe(self) -> None:
        self.unsubscribed += 1

    def redeliver_unacknowledged_messages(self) -> None:
        self.redelivered += 1


def _attached(queue: PulsarEventQueue, topic: str, group: str) -> _Subscription:
    """Register a subscription with no consume task, as if attached."""
    sub = _Subscription(
        consumer=_RecordingConsumer(),
        executor=ThreadPoolExecutor(max_workers=1),
        topic=topic,
        group=group,
    )
    queue._subscriptions.append(sub)
    return sub


async def test_detach_closes_the_consumer_and_never_unsubscribes(
    queue: PulsarEventQueue,
) -> None:
    sub = _attached(queue, "crewlet.agent.alice.inbox", "agent-alice")

    assert await queue.detach("crewlet.agent.alice.inbox", "agent-alice") is True

    assert sub.consumer.closed == 1
    assert sub.consumer.unsubscribed == 0, (
        "detach deleted the broker-side subscription — an unowned seat's "
        "mail would be discarded instead of held for its next owner"
    )
    assert sub.quiescing is True
    assert queue._subscriptions == []


async def test_detach_is_idempotent_and_reports_whether_it_acted(
    queue: PulsarEventQueue,
) -> None:
    _attached(queue, "t", "g")
    assert await queue.detach("t", "g") is True
    assert await queue.detach("t", "g") is False
    assert await queue.detach("never", "attached") is False


async def test_detach_releases_the_pairs_pause_holds(
    queue: PulsarEventQueue,
) -> None:
    """A hold is a fact about ONE attachment.

    A hold that outlived a detach left a node that re-acquired the seat
    attaching into a still-paused subscription and going silently deaf —
    and ``preferred`` stickiness makes flap-and-return the normal case.
    """
    _attached(queue, "t", "g")
    await queue.pause_topic("t", "g", reason="seat")
    assert queue._paused_subs[("t", "g")] == {"seat"}

    await queue.detach("t", "g")
    assert ("t", "g") not in queue._paused_subs


async def test_pause_holds_are_scoped_to_one_group(
    queue: PulsarEventQueue,
) -> None:
    """Shared subjects like ``crewlet.events.*`` carry several groups."""
    sub_a = _attached(queue, "crewlet.events.task_created", "subscriptions")
    sub_b = _attached(queue, "crewlet.events.task_created", "other")

    await queue.pause_topic("crewlet.events.task_created", "subscriptions")
    assert queue._is_sub_paused(sub_a) is True
    assert queue._is_sub_paused(sub_b) is False


async def test_quiesce_leaves_the_consumer_attached(
    queue: PulsarEventQueue,
) -> None:
    """The voluntary release path: stop taking new work, let the
    in-flight turn finish, THEN detach."""
    sub = _attached(queue, "t", "g")

    assert await queue.quiesce("t", "g") is True
    assert sub.quiescing is True
    assert sub.consumer.closed == 0
    assert queue._subscriptions == [sub]
    assert await queue.quiesce("nope", "g") is False


async def test_unquiesce_restarts_the_loop_and_reclaims_the_prefetch(
    queue: PulsarEventQueue,
) -> None:
    """The inverse quiesce needs, and the two things it must do.

    The consume loop EXITS on the flag, so clearing it alone leaves the
    attachment silent forever. And whatever the consumer had already
    fetched was deliberately dropped unacked for a successor that, on
    this path, never comes — so it has to be asked back, or it stays
    hostage in the prefetch until the thirty-minute ack timeout.
    """
    sub = _attached(queue, "t", "g")
    ran = 0

    async def _loop() -> None:
        nonlocal ran
        ran += 1

    sub.run = _loop

    assert await queue.unquiesce("t", "g") is False, "nothing was quiescing"
    assert await queue.quiesce("t", "g") is True
    assert await queue.unquiesce("t", "g") is True

    assert sub.quiescing is False
    assert sub.consumer.redelivered == 1
    assert sub.consumer.closed == 0, "resuming is not a re-attach"
    await asyncio.sleep(0)
    assert ran == 1


async def test_a_quiescing_batch_stops_between_partitions(
    queue: PulsarEventQueue,
) -> None:
    """Undispatched partitions are left UNACKED, not NAK'd.

    A NAK spends the message's dead-letter budget, and a seat handoff is
    not a failure: measured, a close-driven handoff returns the message
    to the successor at ``redeliveryCount`` 0. Nor are they republished
    — that would send them to the topic tail while their prefetched
    siblings replay from the head, reordering the conversation.
    """
    consumer = _TrackingConsumer()
    sub = _sub(consumer, "t", "g")
    m1 = _FakeMsg(Event(type="a", payload={"conv": "c1"}))
    m2 = _FakeMsg(Event(type="b", payload={"conv": "c2"}))
    handled: list[str] = []

    async def handler(events: list[Event]) -> None:
        handled.append(events[0].payload["conv"])
        sub.quiescing = True  # the lease moved mid-batch

    await queue._process_batch(sub, [m1, m2], handler, _conv_key)

    assert handled == ["c1"]  # the second partition never dispatched
    assert consumer.acked == [m1]
    assert consumer.naked == [], "quiescing NAK'd a healthy message"


async def test_defer_delivery_leaves_the_message_and_quiesces(
    queue: PulsarEventQueue,
) -> None:
    """The third handler outcome, on the real dispatch path."""
    from crewlet.queue.protocol import DeferDelivery

    consumer = _FakeConsumer()
    sub = _sub(consumer, "crewlet.agent.alice.inbox", "agent-alice")

    async def handler(_e: Event) -> None:
        raise DeferDelivery("lease moved to another node")

    await queue._process_message(sub, _FakeMsg(Event(type="t")), handler)

    assert consumer.acked == 0, "deferral claimed work this node will not do"
    assert consumer.naked == 0, "deferral spent the dead-letter budget"
    assert sub.quiescing is True
