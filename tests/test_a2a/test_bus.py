"""Tests for MemoryA2ABus."""

from __future__ import annotations

import asyncio

import pytest

from crewlet.a2a.memory import MemoryA2ABus
from crewlet.a2a.messages import ChannelMessage


@pytest.fixture
def bus() -> MemoryA2ABus:
    return MemoryA2ABus()


async def test_create_channel_and_send_receive(bus: MemoryA2ABus) -> None:
    await bus.create_channel("ch1", ["alice", "bob"])
    msg = ChannelMessage(channel="ch1", sender="alice", content="hello")
    await bus.send("ch1", "alice", msg)

    received: list[ChannelMessage] = []

    async def collect() -> None:
        async for m in bus.receive("ch1", "bob"):
            received.append(m)
            break  # just grab the first one

    await asyncio.wait_for(collect(), timeout=1.0)
    assert len(received) == 1
    assert received[0].content == "hello"
    assert received[0].sender == "alice"


async def test_sender_does_not_receive_own_message(bus: MemoryA2ABus) -> None:
    await bus.create_channel("ch1", ["alice", "bob"])
    msg = ChannelMessage(channel="ch1", sender="alice", content="hi")
    await bus.send("ch1", "alice", msg)

    # Alice's queue should be empty
    queue = bus._channels["ch1"]["alice"]
    assert queue.empty()


async def test_close_channel_terminates_receive(bus: MemoryA2ABus) -> None:
    await bus.create_channel("ch1", ["alice", "bob"])

    received: list[ChannelMessage] = []

    async def listen() -> None:
        async for m in bus.receive("ch1", "bob"):
            received.append(m)

    task = asyncio.create_task(listen())
    # Give the listener a moment to start waiting
    await asyncio.sleep(0.01)

    await bus.close_channel("ch1")
    await asyncio.wait_for(task, timeout=1.0)

    # Listener should have exited cleanly with no messages
    assert received == []


async def test_send_after_close_silently_drops(bus: MemoryA2ABus) -> None:
    await bus.create_channel("ch1", ["alice", "bob"])
    await bus.close_channel("ch1")

    msg = ChannelMessage(channel="ch1", sender="alice", content="late")
    # Should not raise
    await bus.send("ch1", "alice", msg)


async def test_stop_closes_all_channels(bus: MemoryA2ABus) -> None:
    await bus.create_channel("ch1", ["alice", "bob"])
    await bus.create_channel("ch2", ["carol", "dave"])

    received: list[ChannelMessage] = []
    tasks: list[asyncio.Task] = []  # type: ignore[type-arg]

    for ch, listener in [("ch1", "bob"), ("ch2", "dave")]:

        async def listen(channel: str = ch, name: str = listener) -> None:
            async for m in bus.receive(channel, name):
                received.append(m)

        tasks.append(asyncio.create_task(listen()))

    await asyncio.sleep(0.01)
    await bus.stop()

    for t in tasks:
        await asyncio.wait_for(t, timeout=1.0)

    assert received == []
    assert bus._channels == {}


async def test_multiple_participants(bus: MemoryA2ABus) -> None:
    await bus.create_channel("ch1", ["alice", "bob", "carol"])
    msg = ChannelMessage(channel="ch1", sender="alice", content="hey all")
    await bus.send("ch1", "alice", msg)

    # Both bob and carol should receive the message
    bob_q = bus._channels["ch1"]["bob"]
    carol_q = bus._channels["ch1"]["carol"]
    assert not bob_q.empty()
    assert not carol_q.empty()
