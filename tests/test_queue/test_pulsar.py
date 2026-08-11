"""Integration tests for the Apache Pulsar EventQueue.

All tests are marked ``@pytest.mark.integration`` and skip automatically
when ``pulsar-client`` is not importable or no broker is reachable at
localhost:6650.
"""

from __future__ import annotations

import asyncio
from uuid import uuid4

import pytest

pytest.importorskip("pulsar")

from crewlet.events.types import Event  # noqa: E402
from crewlet.queue.pulsar import PulsarEventQueue  # noqa: E402

PULSAR_URL = "pulsar://localhost:6650"


def _pulsar_available_sync() -> bool:
    """Check if a Pulsar broker is reachable with a short timeout."""
    import socket

    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.settimeout(1)
    try:
        sock.connect(("localhost", 6650))
        return True
    except OSError:
        return False
    finally:
        sock.close()


_PULSAR_UP = _pulsar_available_sync()

pytestmark = [
    pytest.mark.integration,
    pytest.mark.skipif(not _PULSAR_UP, reason="Pulsar not available at localhost:6650"),
]


@pytest.fixture
async def event_queue():
    """Provide a started PulsarEventQueue with a unique topic prefix."""
    topic_prefix = f"crewlet.test.{uuid4().hex[:8]}"
    q = PulsarEventQueue(PULSAR_URL)
    await q.start()
    yield q, topic_prefix
    await q.stop()


class TestPulsarEventQueue:
    async def test_publish_subscribe_roundtrip(self, event_queue):
        """Published event is received by subscriber."""
        q, prefix = event_queue
        topic = f"{prefix}.roundtrip"
        received: list[Event] = []

        async def handler(event: Event) -> None:
            received.append(event)

        await q.subscribe(topic, "test-group-rt", handler)
        event = Event(type="test_event", source="test", payload={"key": "value"})
        await q.publish(topic, event)

        for _ in range(50):
            if received:
                break
            await asyncio.sleep(0.1)

        assert len(received) == 1
        assert received[0].type == "test_event"
        assert received[0].payload == {"key": "value"}

    async def test_consumer_group_semantics(self, event_queue):
        """Two consumers in the same group — each message delivered to only one."""
        q, prefix = event_queue
        topic = f"{prefix}.group"
        received_a: list[Event] = []
        received_b: list[Event] = []

        async def handler_a(event: Event) -> None:
            received_a.append(event)

        async def handler_b(event: Event) -> None:
            received_b.append(event)

        await q.subscribe(topic, "test-group-cg", handler_a)
        await q.subscribe(topic, "test-group-cg", handler_b)

        num_messages = 10
        for i in range(num_messages):
            await q.publish(topic, Event(type="group_test", payload={"i": i}))

        for _ in range(80):
            if len(received_a) + len(received_b) >= num_messages:
                break
            await asyncio.sleep(0.1)

        total = len(received_a) + len(received_b)
        assert total == num_messages, f"Expected {num_messages}, got {total}"

    async def test_handler_failure_redelivery(self, event_queue):
        """Failed handler causes message redelivery via negative ack."""
        q, prefix = event_queue
        topic = f"{prefix}.redeliver"
        attempts: list[str] = []
        fail_once = True

        async def flaky_handler(event: Event) -> None:
            nonlocal fail_once
            attempts.append(event.type)
            if fail_once:
                fail_once = False
                raise ValueError("Simulated failure")

        await q.subscribe(topic, "test-group-rd", flaky_handler)
        await q.publish(topic, Event(type="redeliver_test"))

        for _ in range(80):
            if len(attempts) >= 2:
                break
            await asyncio.sleep(0.1)

        assert len(attempts) >= 2, f"Expected >=2 attempts, got {len(attempts)}"

    async def test_custom_tenant_namespace_roundtrip(self):
        """The engine uses whatever tenant/namespace it is given; the
        tenant is provisioned out-of-band (here: the test plays the
        operator and creates it via the admin API), never by the engine.
        """
        import httpx

        suffix = uuid4().hex[:8]
        tenant = f"crewlet-test-{suffix}"
        admin = "http://localhost:8080"
        async with httpx.AsyncClient(base_url=admin, timeout=10) as client:
            clusters = (await client.get("/admin/v2/clusters")).json()
            r = await client.put(
                f"/admin/v2/tenants/{tenant}",
                json={"adminRoles": [], "allowedClusters": clusters},
            )
            assert r.status_code in (204, 409), r.text
            r = await client.put(f"/admin/v2/namespaces/{tenant}/events", json={})
            assert r.status_code in (204, 409), r.text

        q = PulsarEventQueue(PULSAR_URL, tenant=tenant, namespace="events")
        await q.start()
        try:
            received: list[Event] = []

            async def handler(event: Event) -> None:
                received.append(event)

            await q.subscribe("crewlet.tenant.check", "test-group-ct", handler)
            await q.publish("crewlet.tenant.check", Event(type="tenant_event"))

            for _ in range(50):
                if received:
                    break
                await asyncio.sleep(0.1)

            assert len(received) == 1
            assert received[0].type == "tenant_event"
        finally:
            await q.stop()

    async def test_subscribe_stream_broadcast(self, event_queue):
        """Every stream subscriber receives every matching event."""
        q, prefix = event_queue
        topic = f"{prefix}.stream"
        # Warm up the topic so the pattern consumer discovers it at
        # subscribe time rather than waiting for the auto-discovery cycle.
        await q.publish(topic, Event(type="warmup"))

        seen_a: list[str] = []
        seen_b: list[str] = []

        async def handler_a(subject: str, _event: Event) -> None:
            seen_a.append(subject)

        async def handler_b(subject: str, _event: Event) -> None:
            seen_b.append(subject)

        unsub_a = await q.subscribe_stream(f"{prefix}.>", handler_a)
        unsub_b = await q.subscribe_stream(f"{prefix}.>", handler_b)
        # Give the pattern consumers a moment to attach.
        await asyncio.sleep(1.0)

        await q.publish(topic, Event(type="broadcast"))

        for _ in range(80):
            if seen_a and seen_b:
                break
            await asyncio.sleep(0.1)

        await unsub_a()
        await unsub_b()

        assert topic in seen_a
        assert topic in seen_b
