"""Persistence for scheduled-run dispatch — the at-most-once ledger.

The ``scheduled_runs`` table is a *dispatch* ledger, not a turn-outcome
store: a row records that the scheduler fired (or deliberately skipped)
a given run exactly once.  The composite PRIMARY KEY
``(scope_type, scope_id, schedule_name, fire_label, target_handle)`` plus
``INSERT … ON CONFLICT DO NOTHING`` gives at-most-once delivery across
engine restarts and re-ticks.  ``fire_label`` is the schedule's local
wall-clock stamp, so the dedup is DST-correct, and keying on the column
tuple (rather than a delimiter-joined string) means a ``:`` in any name
or handle can't collide two distinct fires.

The downstream turn outcome (done / failed / timed out) lives in the
normal turn telemetry — ``TaskStarted`` / ``TaskCompleted`` /
``TaskFailed`` events, ``TurnGuardBreach`` for the wall-clock timeout,
and the episode store — keyed by the same trace.  Keeping the two
concerns separate avoids coupling the scheduler to turn internals.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any, Protocol

from crewlet._logging import get_logger
from crewlet.db.client import Database

logger = get_logger("schedule.store")


def _iso(value: Any) -> str:
    """Render a datetime / string / None as ISO-8601 (or empty string)."""
    if value is None or value == "":
        return ""
    if isinstance(value, datetime):
        return value.isoformat()
    return str(value)


class ScheduledRunStoreProtocol(Protocol):
    """Surface the :class:`~crewlet.schedule.Scheduler` needs from a store."""

    async def claim(
        self,
        *,
        scope_type: str,
        scope_id: str,
        schedule_name: str,
        fire_label: str,
        target_handle: str,
        scheduled_at: datetime,
        outcome: str = "fired",
        trace_id: str = "",
    ) -> bool:
        """Atomically claim a fire.

        Returns ``True`` when this call inserted the row (the caller owns
        the fire), ``False`` when the identity tuple already existed
        (another tick or a pre-restart run already handled it).  The
        dedup identity is ``(scope_type, scope_id, schedule_name,
        fire_label, target_handle)``; ``fire_label`` is the schedule's
        local wall-clock stamp so the dedup is DST-correct.
        """
        ...


class ScheduledRunStore:
    """Postgres-backed :class:`ScheduledRunStoreProtocol`."""

    def __init__(self, db: Database) -> None:
        self._db = db

    async def claim(
        self,
        *,
        scope_type: str,
        scope_id: str,
        schedule_name: str,
        fire_label: str,
        target_handle: str,
        scheduled_at: datetime,
        outcome: str = "fired",
        trace_id: str = "",
    ) -> bool:
        key = await self._db.fetchval(
            """
            INSERT INTO scheduled_runs (
                scope_type, scope_id, schedule_name, fire_label,
                target_handle, scheduled_at, outcome, trace_id
            )
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
            ON CONFLICT (scope_type, scope_id, schedule_name, fire_label,
                         target_handle) DO NOTHING
            RETURNING scope_type
            """,
            scope_type,
            scope_id,
            schedule_name,
            fire_label,
            target_handle,
            scheduled_at,
            outcome,
            trace_id,
        )
        claimed = key is not None
        if claimed:
            logger.debug(
                "scheduled_run_claimed",
                scope_type=scope_type,
                scope_id=scope_id,
                schedule=schedule_name,
                target=target_handle,
                outcome=outcome,
            )
        return claimed

    async def list_recent(self, limit: int = 50) -> list[dict[str, Any]]:
        """Return the most recent dispatch-ledger rows, newest first.

        Read path for the dashboard's ``/schedules`` view; not part of
        the protocol the Scheduler depends on.
        """
        rows = await self._db.execute(
            """
            SELECT scope_type, scope_id, schedule_name, target_handle,
                   scheduled_at, fired_at, outcome, trace_id
            FROM scheduled_runs
            ORDER BY fired_at DESC
            LIMIT $1
            """,
            int(limit),
        )
        out: list[dict[str, Any]] = []
        for row in rows:
            out.append(
                {
                    "scope_type": row.get("scope_type", ""),
                    "scope_id": row.get("scope_id", ""),
                    "schedule_name": row.get("schedule_name", ""),
                    "target_handle": row.get("target_handle", ""),
                    "scheduled_at": _iso(row.get("scheduled_at")),
                    "fired_at": _iso(row.get("fired_at")),
                    "outcome": row.get("outcome", ""),
                    "trace_id": row.get("trace_id", ""),
                }
            )
        return out


class MemoryScheduledRunStore:
    """In-memory store for tests and single-process / no-DB runs.

    Loses the at-most-once-across-restart guarantee (it is process-local),
    so the engine wires :class:`ScheduledRunStore` for real deployments.
    """

    def __init__(self) -> None:
        self._keys: set[tuple[str, str, str, str, str]] = set()
        self.records: list[dict[str, object]] = []

    async def claim(
        self,
        *,
        scope_type: str,
        scope_id: str,
        schedule_name: str,
        fire_label: str,
        target_handle: str,
        scheduled_at: datetime,
        outcome: str = "fired",
        trace_id: str = "",
    ) -> bool:
        key = (scope_type, scope_id, schedule_name, fire_label, target_handle)
        if key in self._keys:
            return False
        self._keys.add(key)
        self.records.append(
            {
                "scope_type": scope_type,
                "scope_id": scope_id,
                "schedule_name": schedule_name,
                "fire_label": fire_label,
                "target_handle": target_handle,
                "scheduled_at": scheduled_at,
                "outcome": outcome,
                "trace_id": trace_id,
            }
        )
        return True


__all__ = [
    "MemoryScheduledRunStore",
    "ScheduledRunStore",
    "ScheduledRunStoreProtocol",
]
