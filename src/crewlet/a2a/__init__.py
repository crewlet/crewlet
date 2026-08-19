"""A2A (agent-to-agent) messaging over the durable queue.

The ``A2ABus`` protocol and its in-memory implementation are gone. They
were a second delivery path — an ``asyncio.Queue`` per channel — beside
the seat inbox that carried the wake, and a second path that only works
within one process cannot be a fast path for a fleet; it can only
disagree with the durable one. Content rides the wake event now, and
:mod:`crewlet.a2a.channels` holds the state every authorization decision
reads.
"""

from crewlet.a2a.channels import (
    A2AChannel,
    A2AChannelStore,
    MemoryA2AChannelStore,
    PostgresA2AChannelStore,
)
from crewlet.a2a.messages import ChannelMessage
from crewlet.a2a.service import A2AService

__all__ = [
    "A2AChannel",
    "A2AChannelStore",
    "A2AService",
    "ChannelMessage",
    "MemoryA2AChannelStore",
    "PostgresA2AChannelStore",
]
