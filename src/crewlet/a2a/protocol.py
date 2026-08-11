"""A2ABus protocol — abstract interface for agent-to-agent messaging."""

from __future__ import annotations

from collections.abc import AsyncIterator
from typing import Protocol

from crewlet.a2a.messages import ChannelMessage


class A2ABus(Protocol):
    """Low-latency agent-to-agent messaging bus."""

    async def create_channel(self, channel_id: str, participants: list[str]) -> None:
        """Create a temporary channel between agents."""
        ...

    async def send(self, channel_id: str, sender: str, message: ChannelMessage) -> None:
        """Send a message on a channel. Delivered to all participants."""
        ...

    async def receive(
        self, channel_id: str, listener: str
    ) -> AsyncIterator[ChannelMessage]:
        """Async iterator that yields messages as they arrive."""
        ...

    async def close_channel(self, channel_id: str) -> None:
        """Delete a channel and clean up resources."""
        ...

    async def start(self) -> None: ...
    async def stop(self) -> None: ...
