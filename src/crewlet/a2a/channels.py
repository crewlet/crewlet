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

from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
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
    opened_at: datetime = field(default_factory=lambda: datetime.now(UTC))
    """When the channel opened, in WALL CLOCK UTC.

    Not ``time.monotonic``, which is what this was: a monotonic reading
    is meaningful only inside the process that took it, and a channel is
    opened on one node and closed on another as a matter of course. Read
    back from Postgres it became "opened just now" every time, so a
    duration computed from it was always zero and an idle sweep on the
    twin measured a different quantity than the same sweep in SQL. The
    column is ``TIMESTAMPTZ`` and this now matches it."""
    closed_at: datetime | None = None
    """When the channel closed, or ``None`` while it is open.

    Stamped by whichever store closed it — the database's own ``now()``
    on the Postgres path — so a duration is the difference between two
    readings of ONE clock rather than two nodes' opinions of the time."""

    @property
    def duration_ms(self) -> float:
        """How long the channel was open, in milliseconds.

        Zero while it is still open: a duration is a property of a
        finished thing, and reporting the age of a live channel under
        the same name would make the two indistinguishable downstream.
        """
        if self.closed_at is None:
            return 0.0
        return max(0.0, (self.closed_at - self.opened_at).total_seconds() * 1000.0)

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

    async def close(self, channel_id: str) -> A2AChannel | None:
        """Close it, returning the closed row — ``None`` if this call was
        not the one that closed it (already closed, or never existed).

        The ROW rather than a bool, because the caller publishes the
        conversation's closing event and the row is what names its
        parties, its message count and its ``closed_at``. Returning a
        bool meant the caller had to have read the channel beforehand,
        and on the idle-sweep path it never had — so every idle close
        was announced with no participants, no count and a zero
        duration, on exactly the channels an operator wants to trace
        (the ones a crash left open).
        """
        ...

    async def count_message(self, channel_id: str) -> int: ...

    async def close_idle(self, older_than_seconds: float) -> list[A2AChannel]:
        """Close every channel idle past ``older_than_seconds``.

        Returns the closed rows, for the same reason :meth:`close` does.
        """
        ...

    async def purge(self, older_than_seconds: float) -> int: ...


def _row_to_channel(row: Any) -> A2AChannel:
    """One place that turns a database row into an :class:`A2AChannel`.

    Three statements return this shape and every one of them used to
    build the object by hand — which is how ``get`` came to drop
    ``opened_at`` and hand back a channel that claimed to have opened at
    whatever moment it was read.
    """
    closed_at = row["closed_at"]
    return A2AChannel(
        channel_id=str(row["channel_id"]),
        requester=str(row["requester"]),
        target=str(row["target"]),
        state=str(row["state"]),
        message_count=int(row["message_count"] or 0),
        opened_at=row["opened_at"],
        closed_at=closed_at if closed_at is not None else None,
    )


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
            SELECT channel_id, requester, target, state, message_count,
                   opened_at, closed_at
            FROM a2a_channels WHERE channel_id = $1
            """,
            channel_id,
        )
        if row is None:
            return None
        return _row_to_channel(row)

    async def close(self, channel_id: str) -> A2AChannel | None:
        rows = await self._db.execute(
            """
            UPDATE a2a_channels SET state = 'closed', closed_at = now()
            WHERE channel_id = $1 AND state = 'open'
            RETURNING channel_id, requester, target, state, message_count,
                      opened_at, closed_at
            """,
            channel_id,
        )
        return _row_to_channel(rows[0]) if rows else None

    async def count_message(self, channel_id: str) -> int:
        value = await self._db.fetchval(
            """
            UPDATE a2a_channels SET message_count = message_count + 1
            WHERE channel_id = $1 RETURNING message_count
            """,
            channel_id,
        )
        return int(value or 0)

    async def close_idle(self, older_than_seconds: float) -> list[A2AChannel]:
        rows = await self._db.execute(
            """
            UPDATE a2a_channels SET state = 'closed', closed_at = now()
            WHERE state = 'open'
              AND opened_at < now() - make_interval(secs => $1)
            RETURNING channel_id, requester, target, state, message_count,
                      opened_at, closed_at
            """,
            float(older_than_seconds),
        )
        return [_row_to_channel(row) for row in rows]

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
        return self._copy(found) if found is not None else None

    @staticmethod
    def _copy(found: A2AChannel) -> A2AChannel:
        """A detached copy, so callers cannot mutate stored state by
        holding an object the Postgres store could never hand them."""
        return A2AChannel(
            channel_id=found.channel_id,
            requester=found.requester,
            target=found.target,
            state=found.state,
            message_count=found.message_count,
            opened_at=found.opened_at,
            closed_at=found.closed_at,
        )

    async def close(self, channel_id: str) -> A2AChannel | None:
        found = self._rows.get(channel_id)
        if found is None or not found.is_open:
            return None
        found.state = "closed"
        found.closed_at = datetime.now(UTC)
        return self._copy(found)

    async def count_message(self, channel_id: str) -> int:
        found = self._rows.get(channel_id)
        if found is None:
            return 0
        found.message_count += 1
        return found.message_count

    async def close_idle(self, older_than_seconds: float) -> list[A2AChannel]:
        # Wall clock, matching the SQL twin's `opened_at < now() - …`.
        # This used to be monotonic while the real store compared
        # timestamps, so the two backends were measuring different
        # quantities under one contract suite.
        now = datetime.now(UTC)
        cutoff = now - timedelta(seconds=max(older_than_seconds, 0.0))
        closed: list[A2AChannel] = []
        for channel in self._rows.values():
            if channel.is_open and channel.opened_at < cutoff:
                channel.state = "closed"
                channel.closed_at = now
                closed.append(self._copy(channel))
        return closed

    async def purge(self, older_than_seconds: float) -> int:
        cutoff = datetime.now(UTC) - timedelta(seconds=max(older_than_seconds, 0.0))
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
