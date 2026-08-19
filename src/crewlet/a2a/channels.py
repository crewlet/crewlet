"""Where an A2A channel's participants and state actually live.

The service used to keep this in a dict. Every authorization decision it
makes reads it — may this sender post here, may this closer close it,
who is the other party — and on one node a dict answers correctly.

On two it does not, and it fails in the direction that looks like the
agent's fault rather than the engine's: the target of an ask wakes on the
node that owns ITS seat, which is rarely the node that opened the
channel, so the reply raises *"channel is not open or does not exist"*
for a channel that exists and is open.

Two backends under one contract, like every other shared counter here.
The memory twin is process-local and therefore has exactly the limitation
above — which is correct for a single node, and is why a fleet without a
database is refused loudly elsewhere rather than quietly degraded.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Any, Protocol

from crewlet._logging import get_logger

logger = get_logger("a2a.channels")

# How long a channel row is kept after it opens.
#
# Long enough that "why did my reply bounce?" has an answer: a closed row
# says *closed*, while a deleted one is indistinguishable from a typo'd
# channel id. Seven days matches the completion ledger, and for the same
# reason — it is the horizon over which a redelivery can still arrive.
A2A_CHANNEL_RETENTION_SECONDS = 7 * 24 * 3600.0

# How long an OPEN channel may stay open with nothing happening.
#
# A channel is closed by the answering turn. A turn that crashes, or a
# node that dies between the wake and the answer, leaves one open
# forever — and an open channel is not free: it is a row, and it is a
# promise to the requester that a reply may still arrive.
#
# One hour, against a turn whose own worst case is ~20 minutes under the
# Execute extension ceiling. Three times the longest thing that could
# legitimately still be running, so the sweep never closes a channel
# somebody is about to answer on.
A2A_CHANNEL_IDLE_TIMEOUT_SECONDS = 3600.0


@dataclass
class A2AChannel:
    """One channel's durable state."""

    channel_id: str
    requester: str
    target: str
    state: str = "open"
    message_count: int = 0
    opened_at: float = field(default_factory=time.monotonic)

    @property
    def is_open(self) -> bool:
        return self.state == "open"

    @property
    def participants(self) -> list[str]:
        return [self.requester, self.target]

    def other_party(self, handle: str) -> str:
        """The party that is not ``handle``, or ``""`` if it is neither."""
        if handle == self.requester:
            return self.target
        if handle == self.target:
            return self.requester
        return ""


class A2AChannelStore(Protocol):
    """Durable channel bookkeeping."""

    async def open(self, channel_id: str, *, requester: str, target: str) -> None: ...

    async def get(self, channel_id: str) -> A2AChannel | None:
        """The channel, or ``None`` if no such channel was ever opened.

        ``None`` and ``state == "closed"`` are different answers and
        callers must not conflate them: one is a typo, the other is a
        conversation that ended.
        """
        ...

    async def close(self, channel_id: str) -> bool:
        """Mark closed. Returns whether this call was the one that did it."""
        ...

    async def count_message(self, channel_id: str) -> int: ...

    async def close_idle(self, older_than_seconds: float) -> list[str]: ...

    async def purge(self, older_than_seconds: float) -> int: ...


class PostgresA2AChannelStore:
    """PostgreSQL-backed :class:`A2AChannelStore`."""

    def __init__(self, db: Any) -> None:
        self._db = db

    async def open(self, channel_id: str, *, requester: str, target: str) -> None:
        await self._db.execute(
            """
            INSERT INTO a2a_channels (channel_id, requester, target, state)
            VALUES ($1, $2, $3, 'open')
            ON CONFLICT (channel_id) DO NOTHING
            RETURNING channel_id
            """,
            channel_id,
            requester,
            target,
        )

    async def get(self, channel_id: str) -> A2AChannel | None:
        row = await self._db.fetchrow(
            """
            SELECT channel_id, requester, target, state, message_count
            FROM a2a_channels WHERE channel_id = $1
            """,
            channel_id,
        )
        if row is None:
            return None
        return A2AChannel(
            channel_id=str(row["channel_id"]),
            requester=str(row["requester"]),
            target=str(row["target"]),
            state=str(row["state"]),
            message_count=int(row["message_count"] or 0),
        )

    async def close(self, channel_id: str) -> bool:
        rows = await self._db.execute(
            """
            UPDATE a2a_channels SET state = 'closed', closed_at = now()
            WHERE channel_id = $1 AND state = 'open'
            RETURNING channel_id
            """,
            channel_id,
        )
        return bool(rows)

    async def count_message(self, channel_id: str) -> int:
        value = await self._db.fetchval(
            """
            UPDATE a2a_channels SET message_count = message_count + 1
            WHERE channel_id = $1 RETURNING message_count
            """,
            channel_id,
        )
        return int(value or 0)

    async def close_idle(self, older_than_seconds: float) -> list[str]:
        rows = await self._db.execute(
            """
            UPDATE a2a_channels SET state = 'closed', closed_at = now()
            WHERE state = 'open'
              AND opened_at < now() - make_interval(secs => $1)
            RETURNING channel_id
            """,
            float(older_than_seconds),
        )
        return [str(row["channel_id"]) for row in rows]

    async def purge(self, older_than_seconds: float) -> int:
        rows = await self._db.execute(
            """
            DELETE FROM a2a_channels
            WHERE opened_at < now() - make_interval(secs => $1)
            RETURNING channel_id
            """,
            float(older_than_seconds),
        )
        return len(rows)


class MemoryA2AChannelStore:
    """In-memory :class:`A2AChannelStore` twin.

    Process-local, so cross-node authorization does not work through it —
    which is the single-node answer, and honest about being nothing more.
    """

    def __init__(self) -> None:
        self._rows: dict[str, A2AChannel] = {}

    async def open(self, channel_id: str, *, requester: str, target: str) -> None:
        self._rows.setdefault(
            channel_id,
            A2AChannel(channel_id=channel_id, requester=requester, target=target),
        )

    async def get(self, channel_id: str) -> A2AChannel | None:
        found = self._rows.get(channel_id)
        if found is None:
            return None
        # A copy: callers must not be able to mutate stored state by
        # holding the object the Postgres store could never hand them.
        return A2AChannel(
            channel_id=found.channel_id,
            requester=found.requester,
            target=found.target,
            state=found.state,
            message_count=found.message_count,
            opened_at=found.opened_at,
        )

    async def close(self, channel_id: str) -> bool:
        found = self._rows.get(channel_id)
        if found is None or not found.is_open:
            return False
        found.state = "closed"
        return True

    async def count_message(self, channel_id: str) -> int:
        found = self._rows.get(channel_id)
        if found is None:
            return 0
        found.message_count += 1
        return found.message_count

    async def close_idle(self, older_than_seconds: float) -> list[str]:
        cutoff = time.monotonic() - max(older_than_seconds, 0.0)
        closed: list[str] = []
        for channel in self._rows.values():
            if channel.is_open and channel.opened_at < cutoff:
                channel.state = "closed"
                closed.append(channel.channel_id)
        return closed

    async def purge(self, older_than_seconds: float) -> int:
        cutoff = time.monotonic() - max(older_than_seconds, 0.0)
        stale = [cid for cid, c in self._rows.items() if c.opened_at < cutoff]
        for cid in stale:
            del self._rows[cid]
        return len(stale)


__all__ = [
    "A2A_CHANNEL_IDLE_TIMEOUT_SECONDS",
    "A2A_CHANNEL_RETENTION_SECONDS",
    "A2AChannel",
    "A2AChannelStore",
    "MemoryA2AChannelStore",
    "PostgresA2AChannelStore",
]
