"""Cross-process inbound-delivery dedupe.

"Have I already handled this delivery" used to be answered from a
per-process dict in each transport. That is right for one process and
wrong for two: the same webhook retried to a different node is a fresh
delivery to that node, so the agent wakes twice and can answer twice —
the duplicate-side-effect failure the people the company talks to
actually see.

The claim is one statement (``INSERT … ON CONFLICT DO NOTHING``), so
there is no read-then-write window for a peer to slip through. Each
source keeps deriving its own key: what counts as "the same delivery" is
genuinely source-specific (a provider delivery id where one exists, event
coordinates where it does not), and that logic already exists and is
correct.
"""

from __future__ import annotations

import time
from typing import Any, Protocol

from crewlet._logging import get_logger

logger = get_logger("db.deliveries")

# How long a delivery stays claimed.
#
# Sized to cover queue redelivery and operator replay, not a provider's
# own retry schedule — those back off for far longer (Plane CE starts at
# ~600 s) and only fire when the API layer failed to return 2xx, i.e.
# when the delivery was never claimed in the first place. The value
# matches the TTL the per-process rings used, so behaviour is unchanged
# for a single node.
DEFAULT_DEDUPE_TTL_SECONDS = 300.0


class DeliveryDedupeStore(Protocol):
    """First-claim-wins registry of handled deliveries."""

    async def claim(self, source: str, key: str) -> bool:
        """``True`` if this caller claimed the delivery (handle it),
        ``False`` if somebody already had it (skip)."""
        ...

    async def purge(self, older_than_seconds: float) -> int: ...


class PostgresDeliveryDedupeStore:
    """PostgreSQL-backed :class:`DeliveryDedupeStore`."""

    def __init__(self, db: Any) -> None:
        self._db = db

    async def claim(self, source: str, key: str) -> bool:
        if not key:
            # No stable identity to dedupe on. Handle it rather than
            # dropping it: a missed duplicate is a doubled reply, a
            # wrongly-dropped delivery is a message nobody ever answers.
            return True
        try:
            row = await self._db.fetchrow(
                """
                INSERT INTO webhook_deliveries (source, delivery_key)
                VALUES ($1, $2)
                ON CONFLICT (source, delivery_key) DO NOTHING
                RETURNING delivery_key
                """,
                source,
                key,
            )
        except Exception:
            # Fail OPEN, deliberately. The store being unreachable must
            # not silently stop inbound work; a duplicate is recoverable
            # noise, a dropped delivery is lost work.
            logger.warning("delivery_dedupe_unavailable", source=source)
            return True
        return row is not None

    async def purge(self, older_than_seconds: float) -> int:
        rows = await self._db.execute(
            """
            DELETE FROM webhook_deliveries
            WHERE seen_at < now() - make_interval(secs => $1)
            RETURNING delivery_key
            """,
            float(older_than_seconds),
        )
        return len(rows)


class MemoryDeliveryDedupeStore:
    """In-memory :class:`DeliveryDedupeStore` twin.

    What every transport did inline. Correct for one process, and the
    right default when no database is configured.
    """

    def __init__(self, *, ttl_seconds: float = DEFAULT_DEDUPE_TTL_SECONDS) -> None:
        self._seen: dict[tuple[str, str], float] = {}
        self._ttl = ttl_seconds
        self._last_prune = 0.0

    async def claim(self, source: str, key: str) -> bool:
        if not key:
            return True
        now = time.monotonic()
        entry = (source, key)
        if entry in self._seen and now - self._seen[entry] < self._ttl:
            return False
        self._seen[entry] = now
        if now - self._last_prune >= self._ttl:
            cutoff = now - self._ttl
            self._seen = {k: v for k, v in self._seen.items() if v > cutoff}
            self._last_prune = now
        return True

    async def purge(self, older_than_seconds: float) -> int:
        cutoff = time.monotonic() - older_than_seconds
        stale = [k for k, v in self._seen.items() if v <= cutoff]
        for key in stale:
            del self._seen[key]
        return len(stale)
