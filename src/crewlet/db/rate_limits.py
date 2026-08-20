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
            # ``limit`` is deliberately NOT a parameter here: the
            # comparison is done in Python below, so passing it left
            # ``$2`` bound but unreferenced — and PostgreSQL refuses to
            # parse a statement whose parameter it cannot type
            # ("could not determine data type of parameter $2"). Every
            # call then raised, hit the fail-open beneath, and returned
            # True: the valve was open on every database-backed
            # deployment, with one warning line as the only symptom.
            row = await self._db.fetchrow(
                """
                INSERT INTO rate_limits (bucket, window_start, count)
                VALUES (
                    $1,
                    to_timestamp(floor(extract(epoch FROM now()) / $2) * $2),
                    1
                )
                ON CONFLICT (bucket, window_start) DO UPDATE
                SET count = rate_limits.count + 1
                RETURNING count
                """,
                bucket,
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
        # Keyed by (bucket, window START in monotonic seconds), NOT by a
        # window index. An index is only comparable to other indices of
        # the same width, and both the eviction below and :meth:`purge`
        # need to compare a key against a WALL of elapsed time — with an
        # index those comparisons are dimensionally wrong the moment a
        # caller uses a window wider than one second, and a purge would
        # judge every live window stale.
        self._windows: dict[tuple[str, float], int] = {}

    @staticmethod
    def _window_start(window_seconds: float) -> tuple[float, float]:
        width = max(window_seconds, 0.001)
        return (time.monotonic() // width) * width, width

    async def allow(self, bucket: str, *, limit: int, window_seconds: float) -> bool:
        if limit <= 0:
            return True
        start, width = self._window_start(window_seconds)
        key = (bucket, start)
        count = self._windows.get(key, 0) + 1
        self._windows[key] = count
        if len(self._windows) > 4096:
            # Keep the current window and the one before it; everything
            # older can no longer affect an answer.
            cutoff = start - width
            self._windows = {k: v for k, v in self._windows.items() if k[1] >= cutoff}
        return count <= limit

    async def purge(self, older_than_seconds: float) -> int:
        """Drop windows that started more than ``older_than_seconds`` ago.

        Honours the cutoff rather than clearing, which is what the
        Postgres store does and therefore what the twin must do: a purge
        that wiped every window would reset the LIVE one too, and the
        valve would pass a full limit's worth of notifications again the
        instant the sweep ran — turning housekeeping into a periodic
        hole in the rate limit.
        """
        cutoff = time.monotonic() - max(older_than_seconds, 0.0)
        stale = [key for key in self._windows if key[1] < cutoff]
        for key in stale:
            del self._windows[key]
        return len(stale)
