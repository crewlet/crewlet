"""Shared sliding-window rate limiting.

Backs ``notification_rate_limit``. A per-process counter multiplies the
effective limit by replica count, and misses the pathology the valve
exists for most completely: a notification loop (A wakes B, B wakes A)
bounces between nodes, so no single process sees enough of it to trip.

Only consulted when the limit is enabled (it defaults to off), so the
common path costs nothing.
"""

from __future__ import annotations

import time
from typing import Any, Protocol

from crewlet._logging import get_logger

logger = get_logger("db.rate_limits")


class RateLimitStore(Protocol):
    """Fixed-window counter shared across processes."""

    async def allow(
        self, bucket: str, *, limit: int, window_seconds: float
    ) -> bool: ...


class PostgresRateLimitStore:
    """PostgreSQL-backed :class:`RateLimitStore`.

    A fixed window rather than a true sliding one: the counter is keyed
    by the window a request falls into, so one statement both increments
    and reports. A sliding window would need per-event rows and a range
    count — more storage and a heavier query for a safety valve whose job
    is to notice runaway volume, not to meter it precisely.
    """

    def __init__(self, db: Any) -> None:
        self._db = db

    async def allow(self, bucket: str, *, limit: int, window_seconds: float) -> bool:
        if limit <= 0:
            return True
        try:
            row = await self._db.fetchrow(
                """
                INSERT INTO rate_limits (bucket, window_start, count)
                VALUES (
                    $1,
                    to_timestamp(floor(extract(epoch FROM now()) / $3) * $3),
                    1
                )
                ON CONFLICT (bucket, window_start) DO UPDATE
                SET count = rate_limits.count + 1
                RETURNING count
                """,
                bucket,
                limit,
                float(window_seconds),
            )
        except Exception:
            # Fail OPEN: a limiter that cannot be reached must not stop
            # real notifications. It is a valve, not a gate.
            logger.warning("rate_limit_unavailable", bucket=bucket)
            return True
        return row is None or int(row["count"]) <= limit

    async def purge(self, older_than_seconds: float) -> int:
        rows = await self._db.execute(
            """
            DELETE FROM rate_limits
            WHERE window_start < now() - make_interval(secs => $1)
            RETURNING bucket
            """,
            float(older_than_seconds),
        )
        return len(rows)


class MemoryRateLimitStore:
    """In-memory :class:`RateLimitStore` twin — the per-process behaviour."""

    def __init__(self) -> None:
        self._windows: dict[tuple[str, int], int] = {}

    async def allow(self, bucket: str, *, limit: int, window_seconds: float) -> bool:
        if limit <= 0:
            return True
        window = int(time.monotonic() / max(window_seconds, 0.001))
        key = (bucket, window)
        count = self._windows.get(key, 0) + 1
        self._windows[key] = count
        if len(self._windows) > 4096:
            self._windows = {
                k: v for k, v in self._windows.items() if k[1] >= window - 1
            }
        return count <= limit

    async def purge(self, older_than_seconds: float) -> int:
        before = len(self._windows)
        self._windows.clear()
        return before
