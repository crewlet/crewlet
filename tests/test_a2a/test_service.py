"""Tests for A2AService."""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator

import pytest

from crewlet.a2a.channels import MemoryA2AChannelStore
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
def channels() -> MemoryA2AChannelStore:
    return MemoryA2AChannelStore()


@pytest.fixture
def service(channels: MemoryA2AChannelStore, queue: MemoryEventQueue) -> A2AService:
    return A2AService(queue=queue, channels=channels)


async def test_request_channel_records_durable_state(
    service: A2AService, channels: MemoryA2AChannelStore
) -> None:
    """The state every authorization decision reads. It used to be a
    dict on the service, which is empty on the node that has to answer."""
    channel_id = await service.request_channel("alice", "bob")

    assert channel_id.startswith("a2a-")
    channel = await channels.get(channel_id)
    assert channel is not None
    assert channel.is_open
    assert set(channel.participants) == {"alice", "bob"}


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

    channel_id = await service.request_channel(
        "alice", "bob", brief="hello bob", sender_role="engineer"
    )
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


async def test_close_channel_marks_closed_rather_than_forgetting(
    service: A2AService, channels: MemoryA2AChannelStore
) -> None:
    """A closed channel is still a row. Deleting it would make a late
    reply indistinguishable from a typo'd channel id — and "why did my
    reply bounce" is a question an operator has to be able to answer."""
    channel_id = await service.request_channel("alice", "bob")

    await service.close_channel(channel_id)
    channel = await channels.get(channel_id)
    assert channel is not None
    assert not channel.is_open


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


async def test_request_channel_refuses_a_channel_to_yourself(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    """A channel has two parties, and a self-channel has no responder.

    ``_handle_a2a`` decides who answers by comparing the woken seat
    against the channel's requester, so a self-addressed channel wakes
    the asker, is read as an incoming ANSWER, and is never replied to:
    a turn spent on a question nobody was asked, plus a row the idle
    sweep eventually closes. Refused at the chokepoint, where the guard
    against human seats already lives.
    """
    published = _spy_on(queue)

    with pytest.raises(ValueError, match="is you"):
        await service.request_channel("alice", "alice", brief="think out loud")

    assert published == []


# ---------------------------------------------------------------- #
# The reply path — which did not exist at all
# ---------------------------------------------------------------- #


async def test_the_brief_rides_the_wake_event(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    """Content on the event, not in a process-local queue.

    The wake is delivered to whichever node owns the target's seat; the
    content used to sit in an ``asyncio.Queue`` on the node that opened
    the channel. Those are the same node only by luck, and when they
    were not the agent was told nobody had said anything yet.
    """
    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await queue.subscribe("crewlet.agent.bob.inbox", "test-group", handler)
    await service.request_channel(
        "alice", "bob", brief="how big is the migration?", sender_role="Lead"
    )
    await asyncio.sleep(0.1)

    assert received[0].payload["content"] == "how big is the migration?"
    assert received[0].payload["sender_role"] == "Lead"


async def test_a_reply_wakes_the_other_party_with_its_content(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    """The half that never existed. ``a2a_ask`` promises "they reply on
    the same channel"; the wake prompt named a tool that is not
    registered and that a test asserts is absent, so every answer went
    nowhere."""
    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await queue.subscribe("crewlet.agent.alice.inbox", "test-group", handler)
    channel_id = await service.request_channel("alice", "bob", brief="how big?")
    await service.reply(channel_id, "bob", "about two weeks", sender_role="Eng")
    await asyncio.sleep(0.1)

    assert len(received) == 1
    assert received[0].type == "a2a_message"
    assert received[0].payload["content"] == "about two weeks"
    assert received[0].payload["sender"] == "bob"


async def test_a_reply_on_a_closed_channel_is_refused(service: A2AService) -> None:
    """Loudly, not silently. A dropped reply is what the requester
    experiences as "they never answered"."""
    channel_id = await service.request_channel("alice", "bob", brief="?")
    await service.close_channel(channel_id)
    with pytest.raises(ValueError, match="closed"):
        await service.reply(channel_id, "bob", "too late")


async def test_a_reply_on_an_unknown_channel_is_refused(service: A2AService) -> None:
    with pytest.raises(ValueError, match="does not exist"):
        await service.reply("a2a-nope", "bob", "into the void")


async def test_a_non_participant_cannot_reply(service: A2AService) -> None:
    channel_id = await service.request_channel("alice", "bob", brief="?")
    with pytest.raises(ValueError, match="not a participant"):
        await service.reply(channel_id, "mallory", "hello")


async def test_a_reply_carries_the_asks_depth_unchanged(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    """The ask is the delegation; the answer is that hop completing.

    Charging the return leg too halves the budget in the one direction
    nobody meant to spend it. A scheduled 1:1 costs depth 1 to ask and
    would cost 2 to answer, so with the cap at its default 3 the
    report's first follow-up question lands at 3 and the manager's turn
    dies on a ``guard_breach`` — a legitimate second exchange ending as
    an engine failure event. Counting only asks bounds the conversation
    at ``delegation_depth_limit`` exchanges, which is what the limit
    reads as.

    The CHAIN still grows: it is provenance, not a gate.
    """
    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await queue.subscribe("crewlet.agent.alice.inbox", "test-group", handler)
    channel_id = await service.request_channel("alice", "bob", brief="?")
    await service.reply(
        channel_id, "bob", "answer", delegation_depth=2, delegation_chain=["alice"]
    )
    await asyncio.sleep(0.1)

    assert received[0].delegation_depth == 2
    assert received[0].delegation_chain == ["alice", "bob"]


async def test_closing_twice_publishes_one_closed_event(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    """Both sides may reasonably close. The dashboard must not see the
    conversation end twice."""
    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await queue.subscribe("crewlet.events.a2a_channel_closed", "test-group", handler)
    channel_id = await service.request_channel("alice", "bob", brief="?")
    await service.close_channel(channel_id, closer="bob")
    await service.close_channel(channel_id, closer="alice")
    await asyncio.sleep(0.1)

    assert len(received) == 1


async def test_idle_channels_are_closed_and_announced(
    service: A2AService, queue: MemoryEventQueue
) -> None:
    """A turn that crashed leaves a channel open forever — a row that
    never goes away and a promise to the requester that a reply may
    still arrive."""
    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await queue.subscribe("crewlet.events.a2a_channel_closed", "test-group", handler)
    await service.request_channel("alice", "bob", brief="?")
    assert await service.close_idle_channels(older_than_seconds=-1) == 1
    await asyncio.sleep(0.1)
    assert len(received) == 1

    # And it says WHO and HOW MUCH. These events used to be published
    # with no participants, no message count and a zero duration — on
    # exactly the channels an operator needs to trace, since a channel
    # only reaches the idle sweep because the turn that should have
    # answered it never finished. "Who was waiting on whom when that
    # node died" was unanswerable from the one event that exists to
    # answer it.
    closed = received[0]
    assert closed.participants == ["alice", "bob"]
    assert closed.message_count == 1
    assert closed.duration_ms >= 0.0
    # The writer dumps the whole event into the stored payload, so these
    # are the fields the dashboard's event detail actually reads.
    assert "duration_ms" in closed.model_dump(mode="json")

    # Idempotent: the next sweep finds nothing left to close.
    assert await service.close_idle_channels(older_than_seconds=-1) == 0
