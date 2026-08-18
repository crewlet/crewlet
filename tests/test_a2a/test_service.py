"""Tests for A2AService."""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator

import pytest

from crewlet.a2a.memory import MemoryA2ABus
from crewlet.a2a.service import A2AService
from crewlet.events.types import Event
from crewlet.queue.memory import MemoryEventQueue


@pytest.fixture
async def queue() -> AsyncIterator[MemoryEventQueue]:
    q = MemoryEventQueue()
    await q.start()
    yield q
    await q.stop()


@pytest.fixture
def bus() -> MemoryA2ABus:
    return MemoryA2ABus()


@pytest.fixture
def service(bus: MemoryA2ABus, queue: MemoryEventQueue) -> A2AService:
    return A2AService(bus=bus, queue=queue)


async def test_request_channel_creates_bus_channel(
    service: A2AService, bus: MemoryA2ABus
) -> None:
    channel_id = await service.request_channel("alice", "bob")

    assert channel_id.startswith("a2a-")
    assert channel_id in bus._channels
    assert set(bus._channels[channel_id].keys()) == {"alice", "bob"}


async def test_request_channel_publishes_wake_event(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    received_events: list[Event] = []

    async def handler(event: Event) -> None:
        received_events.append(event)

    await queue.subscribe("crewlet.agent.bob.inbox", "test-group", handler)

    channel_id = await service.request_channel("alice", "bob")

    # Give the dispatch loop time to deliver
    await asyncio.sleep(0.1)

    assert len(received_events) == 1
    evt = received_events[0]
    assert evt.type == "a2a_request"
    assert evt.source == "alice"
    assert evt.payload["channel_id"] == channel_id
    assert evt.payload["requester"] == "alice"


async def test_request_channel_increments_depth_and_appends_chain(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    """The wake event must carry ``delegation_depth + 1`` and the
    requester appended to ``delegation_chain``.

    This increment is the sole
    backstop against runaway / circular A2A delegation (bounded by
    ``delegation_depth_limit``), so guard it directly against a silent
    refactor that drops the ``+ 1`` or the chain append.
    """
    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await queue.subscribe("crewlet.agent.bob.inbox", "test-group", handler)

    await service.request_channel(
        "alice", "bob", delegation_depth=2, delegation_chain=["x", "y"]
    )
    await asyncio.sleep(0.1)

    assert len(received) == 1
    evt = received[0]
    assert evt.delegation_depth == 3
    assert evt.delegation_chain == ["x", "y", "alice"]


async def test_request_channel_publishes_opened_event(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await queue.subscribe("crewlet.events.a2a_channel_opened", "test-group", handler)

    channel_id = await service.request_channel("alice", "bob")
    await asyncio.sleep(0.1)

    assert len(received) == 1
    evt = received[0]
    assert evt.type == "a2a_channel_opened"
    assert evt.source == "alice"
    assert evt.channel_id == channel_id
    assert evt.requester == "alice"
    assert evt.target == "bob"


async def test_send_publishes_message_sent_event(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await queue.subscribe("crewlet.events.a2a_message_sent", "test-group", handler)

    channel_id = await service.request_channel("alice", "bob")
    await service.send(channel_id, "alice", "hello bob", sender_role="engineer")
    await asyncio.sleep(0.1)

    assert len(received) == 1
    evt = received[0]
    assert evt.type == "a2a_message_sent"
    assert evt.channel_id == channel_id
    assert evt.sender == "alice"
    assert evt.sender_role == "engineer"
    assert evt.content == "hello bob"
    assert evt.message_id  # non-empty UUID string


async def test_close_channel_publishes_closed_event(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await queue.subscribe("crewlet.events.a2a_channel_closed", "test-group", handler)

    channel_id = await service.request_channel("alice", "bob")
    await service.close_channel(channel_id, closer="alice")
    await asyncio.sleep(0.1)

    assert len(received) == 1
    evt = received[0]
    assert evt.type == "a2a_channel_closed"
    assert evt.channel_id == channel_id
    assert evt.closed_by == "alice"


async def test_close_channel_cleans_up(service: A2AService, bus: MemoryA2ABus) -> None:
    channel_id = await service.request_channel("alice", "bob")
    assert channel_id in bus._channels

    await service.close_channel(channel_id)
    assert channel_id not in bus._channels
    assert channel_id not in service._channels


async def test_close_nonexistent_channel(
    service: A2AService,
) -> None:
    # Should not raise
    await service.close_channel("nonexistent")


# ---------------------------------------------------------------- #
# Agent-seat guard (humans / unknown handles are not on the bus)
# ---------------------------------------------------------------- #


def _spy_on(queue: MemoryEventQueue) -> list[tuple[str, Event]]:
    published: list[tuple[str, Event]] = []
    original_publish = queue.publish

    async def spy(topic: str, event: Event) -> None:
        published.append((topic, event))
        await original_publish(topic, event)

    queue.publish = spy  # type: ignore[method-assign]
    return published


async def test_request_channel_refuses_non_agent_target(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    """With a registry wired, a target that is not an agent seat is
    refused at the chokepoint — no channel, no wake event into a
    subscriber-less topic."""
    from crewlet.org.models import Role
    from tests.conftest import PartyRegistryStub, StubAgent

    sarah = Role(
        name="Sarah Chen",
        kind="human",
        email="sarah@example.com",
        contact={"slack_user_id": "U0HUMAN"},
    )
    service.set_handle_registry(
        PartyRegistryStub(agents=[StubAgent("bob")], humans=[sarah])
    )
    published = _spy_on(queue)

    # Live agent: fine.
    await service.request_channel("alice", "bob")

    # Unknown handle: refused.
    with pytest.raises(ValueError, match="not an agent seat"):
        await service.request_channel("alice", "nobody")
    # A real human seat: also refused — it resolves, but has no inbox.
    with pytest.raises(ValueError, match="not an agent seat"):
        await service.request_channel("alice", "sarah-chen")

    assert not any(
        ".inbox" in t and ("nobody" in t or "sarah-chen" in t) for t, _ in published
    )


async def test_request_channel_accepts_a_seat_running_elsewhere(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    """The guard asks whether the seat EXISTS, not whether it runs here.

    A colleague owned by another node is a valid target: the wake lands
    on its inbox and that node consumes it.  Asking the local pool
    instead made every cross-node ask fail as a typo — the more nodes,
    the fewer colleagues an agent appeared to have.
    """
    from tests.conftest import PartyRegistryStub, StubAgent

    service.set_handle_registry(
        PartyRegistryStub(agents=[StubAgent("bob")], remote_agents=["carol"])
    )
    published = _spy_on(queue)

    channel_id = await service.request_channel("alice", "carol")
    assert channel_id.startswith("a2a-")
    assert any(t == "crewlet.agent.carol.inbox" for t, _ in published)


async def test_request_channel_unguarded_without_registry(
    service: A2AService,
) -> None:
    """No registry wired (tests / embedded use) — behaves as before."""
    channel_id = await service.request_channel("alice", "whoever")
    assert channel_id.startswith("a2a-")
