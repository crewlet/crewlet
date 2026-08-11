"""In-memory A2A Bus using asyncio.Queue per channel."""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator

from crewlet.a2a.messages import ChannelMessage

_CLOSED = object()  # sentinel pushed on close


class MemoryA2ABus:
    """In-memory A2A Bus using asyncio.Queue per channel."""

    def __init__(self) -> None:
        self._channels: dict[
            str, dict[str, asyncio.Queue[ChannelMessage | object]]
        ] = {}

    async def create_channel(self, channel_id: str, participants: list[str]) -> None:
        self._channels[channel_id] = {p: asyncio.Queue() for p in participants}

    async def send(self, channel_id: str, sender: str, message: ChannelMessage) -> None:
        channel = self._channels.get(channel_id)
        if channel is None:
            return  # channel already closed, silently drop
        for participant, queue in channel.items():
            if participant != sender:
                queue.put_nowait(message)

    async def receive(
        self, channel_id: str, listener: str
    ) -> AsyncIterator[ChannelMessage]:
        queue = self._channels[channel_id][listener]
        while True:
            msg = await queue.get()
            if msg is _CLOSED:
                return
            yield msg

    async def close_channel(self, channel_id: str) -> None:
        channel = self._channels.pop(channel_id, None)
        if channel is None:
            return
        for queue in channel.values():
            queue.put_nowait(_CLOSED)

    async def start(self) -> None:
        pass

    async def stop(self) -> None:
        for channel_id in list(self._channels):
            await self.close_channel(channel_id)
