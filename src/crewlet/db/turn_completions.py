"""The completion ledger: has this trigger already been worked?

A seat's inbox delivers at-least-once, and the window that matters is
narrow but real — a turn finishes, its outbound effects ship, and the
node dies before the delivery is acked. The trigger is then redelivered
to the seat's next owner, which re-runs the whole turn: a second Slack
reply, a second Jira comment, a second push.

This records what finished. It is deliberately **not** a claim:

- **No ``in_progress`` state.** The seat lease is already the mutual
  exclusion — one consuming node per seat, serial within it — so a
  claim's only honest disposition for a stale in-progress row is
  "supersede and re-run", which is what you do with no row at all. The
  design this replaced had one, and every defect five reviewers found in
  it existed to service that state.
- **No expiry.** A row means the work is done, and done does not lapse.
  Rows are deleted on a retention horizon by
  :class:`~crewlet.db.maintenance.MaintenanceWorker`, which is garbage
  collection, not semantics.
- **Keyed on CONSTITUENT event ids.** A multi-event partition coalesces
  into one digest before the turn runs, and that digest gets a fresh
  ``uuid4`` on every coalesce — so a key taken from it would differ on
  every redelivery and match nothing. Recording constituents also lets a
  partially-overlapping redelivery (A+B, then A+B+C) skip A and B and
  run C.

**Both directions fail open, and that is the whole failure policy.**
Reading tells you whether work was already done; when the store cannot
answer, the honest fallback is the answer the engine gave before this
table existed — assume not done, run it. Writing happens *after* the
side effects shipped, and refusing to record them cannot un-ship them.
Neither failure is allowed to stop or park real work: the ledger is an
improvement on at-least-once, not a gate in front of it.
"""

from __future__ import annotations

import time
from typing import Any, Protocol

from crewlet._logging import get_logger

logger = get_logger("db.turn_completions")

# How long a completion is remembered.
#
# The floor is how long a trigger can keep coming back: the broker's
# unacked-message timeout (30 min) times the redelivery budget
# (``_MAX_REDELIVER`` = 10) is about five hours, and a seat nobody claims
# holds its backlog for as long as it stays unowned — which has no bound
# at all. Seven days covers a weekend outage plus the Monday morning that
# discovers it, and the table is one row per trigger, so a week of a busy
# company's traffic is small.
#
# Too short and the mechanism silently stops working exactly when it is
# needed (a long unowned window is precisely when a redelivery is old);
# too long is only disk.
TURN_COMPLETION_RETENTION_SECONDS = 7 * 24 * 3600.0


class TurnCompletionStore(Protocol):
    """Records which triggers a seat has finished working."""

    async def completed(self, seat: str, trigger_ids: list[str]) -> set[str]:
        """Which of ``trigger_ids`` are already recorded for ``seat``.

        Returns an EMPTY set when the store cannot answer — see the
        module docstring: not knowing means falling back to the answer
        the engine gave before this table existed.
        """
        ...

    async def record(
        self,
        seat: str,
        trigger_ids: list[str],
        *,
        turn_id: str = "",
        node: str = "",
        owner_epoch: int = 0,
    ) -> int:
        """Record ``trigger_ids`` as done. Returns rows newly written.

        First writer wins: a zombie recording a turn its successor
        already recorded is a no-op, and either way the trigger ends up
        marked done exactly once.
        """
        ...

    async def purge(self, older_than_seconds: float) -> int: ...


class PostgresTurnCompletionStore:
    """PostgreSQL-backed :class:`TurnCompletionStore`."""

    def __init__(self, db: Any) -> None:
        self._db = db

    async def completed(self, seat: str, trigger_ids: list[str]) -> set[str]:
        if not seat or not trigger_ids:
            return set()
        try:
            rows = await self._db.execute(
                """
                SELECT trigger_id FROM turn_completions
                WHERE seat = $1 AND trigger_id = ANY($2::text[])
                """,
                seat,
                list(trigger_ids),
            )
        except Exception:
            # Fail OPEN. "I cannot tell whether this was done" has one
            # safe answer and it is the pre-ledger one: run it. Parking
            # instead would stop real work during a database blip, and
            # the seat's own admission gate already refuses new turns
            # within one heartbeat of a store it cannot reach.
            logger.warning("turn_completions_unavailable", seat=seat)
            return set()
        return {str(row["trigger_id"]) for row in rows}

    async def record(
        self,
        seat: str,
        trigger_ids: list[str],
        *,
        turn_id: str = "",
        node: str = "",
        owner_epoch: int = 0,
    ) -> int:
        if not seat or not trigger_ids:
            return 0
        try:
            rows = await self._db.execute(
                """
                INSERT INTO turn_completions
                    (seat, trigger_id, turn_id, node, owner_epoch)
                SELECT $1, t, $3, $4, $5 FROM unnest($2::text[]) AS t
                ON CONFLICT (seat, trigger_id) DO NOTHING
                RETURNING trigger_id
                """,
                seat,
                list(trigger_ids),
                turn_id,
                node,
                int(owner_epoch),
            )
        except Exception:
            # Fail OPEN, and loudly: the turn's side effects already
            # shipped, so the only thing lost is the record of them —
            # which costs at most one duplicate turn if the delivery is
            # redelivered, exactly the behaviour without this table.
            logger.exception("turn_completion_record_failed", seat=seat)
            return 0
        return len(rows)

    async def purge(self, older_than_seconds: float) -> int:
        rows = await self._db.execute(
            """
            DELETE FROM turn_completions
            WHERE completed_at < now() - make_interval(secs => $1)
            RETURNING trigger_id
            """,
            float(older_than_seconds),
        )
        return len(rows)


class MemoryTurnCompletionStore:
    """In-memory :class:`TurnCompletionStore` twin.

    Process-local, so it cannot deduplicate across a takeover — which is
    the whole point of the table. Correct for a single node, where the
    only redelivery a completion can race is one this same process will
    handle, and honest about being nothing more than that: a fleet
    without a database gets the loud ``seat_placement_is_process_local``
    warning at boot for the same reason.
    """

    def __init__(self) -> None:
        self._rows: dict[tuple[str, str], float] = {}

    async def completed(self, seat: str, trigger_ids: list[str]) -> set[str]:
        if not seat or not trigger_ids:
            return set()
        return {tid for tid in trigger_ids if (seat, tid) in self._rows}

    async def record(
        self,
        seat: str,
        trigger_ids: list[str],
        *,
        turn_id: str = "",
        node: str = "",
        owner_epoch: int = 0,
    ) -> int:
        if not seat or not trigger_ids:
            return 0
        now = time.monotonic()
        written = 0
        for tid in trigger_ids:
            if (seat, tid) in self._rows:
                continue  # first writer wins, as ON CONFLICT DO NOTHING
            self._rows[(seat, tid)] = now
            written += 1
        return written

    async def purge(self, older_than_seconds: float) -> int:
        cutoff = time.monotonic() - max(older_than_seconds, 0.0)
        stale = [key for key, at in self._rows.items() if at < cutoff]
        for key in stale:
            del self._rows[key]
        return len(stale)


__all__ = [
    "TURN_COMPLETION_RETENTION_SECONDS",
    "MemoryTurnCompletionStore",
    "PostgresTurnCompletionStore",
    "TurnCompletionStore",
]
