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

    async def purge(self, older_than_seconds: float) -> int:
        """Delete claims older than ``older_than_seconds``. Returns the count.

        Garbage collection, not expiry — :meth:`claim` already enforces
        the TTL — so a deployment whose sweep never runs is wasteful
        rather than wrong. See :class:`crewlet.db.maintenance.MaintenanceWorker`.
        """
        ...


class PostgresDeliveryDedupeStore:
    """PostgreSQL-backed :class:`DeliveryDedupeStore`."""

    def __init__(self, db: Any, *, ttl_seconds: float = DEFAULT_DEDUPE_TTL_SECONDS):
        self._db = db
        self._ttl = float(ttl_seconds)

    async def claim(self, source: str, key: str) -> bool:
        """Claim a delivery, honouring the TTL.

        The conflict clause re-claims a row whose ``seen_at`` is older
        than the TTL rather than refusing. Without the time predicate the
        claim is PERMANENT — which the twin's is not, and which the
        constant, this module's docstring and the migration comment all
        say it should not be. The visible cost of getting it wrong is an
        operator replaying a webhook ten minutes later and watching it
        vanish: claimed forever by a row nothing will ever clear.

        Still one statement, so there is no read-then-write window: the
        ``WHERE`` rides on the ``DO UPDATE``, and a row that fails it
        updates nothing and returns nothing.
        """
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
                ON CONFLICT (source, delivery_key) DO UPDATE
                SET seen_at = now()
                WHERE webhook_deliveries.seen_at
                      < now() - make_interval(secs => $3)
                RETURNING delivery_key
                """,
                source,
                key,
                self._ttl,
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
