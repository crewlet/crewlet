"""Retention sweeps for the tables that answer "recently" rather than "ever".

Three tables exist to answer a short-horizon question and are written on
every event that asks it:

- ``webhook_deliveries`` — "have I already handled this delivery?", one
  row per inbound webhook;
- ``rate_limits`` — "how many notifications in the last second?", one row
  per bucket per window;
- ``scheduled_runs`` — "has this fire already been claimed?", one row per
  schedule fire.

All three were designed to be swept. Both migrations say so in as many
words (``020_webhook_deliveries.sql``: *"Rows are swept on a TTL rather
than kept: this is a short-horizon 'did I just see this', not an audit
log"*), and both ship the ``seen_at`` / ``window_start`` index a range
delete needs. The sweep itself was never built: ``purge`` existed on the
stores and on their protocols, and **nothing anywhere called it**, so
every one of those tables grew for the life of the deployment. The event
store is the audit log; these are not.

This worker is that sweep, and it is a **company-wide singleton** rather
than a per-node loop. Not because concurrent deletes would corrupt
anything — they would not, the statements are idempotent range deletes —
but because N nodes each scanning and deleting the same rows every tick
is N times the write amplification and N times the vacuum churn for one
table's worth of benefit. It claims ``worker:maintenance`` per tick, the
same shape the sandbox waiter uses: claimed rather than held, so a node
that dies mid-sweep releases it by lapsing and a peer picks it up on its
next tick with no handoff protocol.

Retention is per table and tied to what each one is FOR, not to a single
global number — see the constants below.
"""

from __future__ import annotations

import asyncio
import contextlib
from typing import Any, Protocol

from crewlet._logging import get_logger
from crewlet.db.deliveries import DEFAULT_DEDUPE_TTL_SECONDS

logger = get_logger("db.maintenance")

# How often the sweep runs.
#
# Fifteen minutes, and the tick interval is deliberately much longer than
# any table's retention. These are range deletes on indexed columns
# answering a question nothing reads back, so the only thing a faster
# tick buys is a slightly smaller table between runs; the only thing it
# costs is a write burst and a lease round-trip. The tables are sized by
# their retention, not by the sweep interval, so a node that misses
# several ticks changes nothing an operator would notice.
MAINTENANCE_INTERVAL_SECONDS = 900.0

# Delivery-dedupe rows: garbage collection, NOT expiry.
#
# ``PostgresDeliveryDedupeStore.claim`` enforces the TTL itself in its
# conflict clause, so a row past the TTL is already re-claimable and
# deleting it changes no behaviour. The multiplier keeps that true even
# when the sweep is late: a row is only removed once it is well past the
# point where any claim could still consult it.
DELIVERY_RETENTION_SECONDS = DEFAULT_DEDUPE_TTL_SECONDS * 4

# Rate-limit windows: one width past the widest window a caller could
# plausibly ask for.
#
# The valve's own window is a second; an hour of retention is three
# orders of magnitude of headroom, and a row older than the window it
# describes cannot affect an answer — ``allow`` only ever reads the
# CURRENT window's row.
RATE_LIMIT_RETENTION_SECONDS = 3600.0

# The scheduled-run ledger: bounded by how far back a fire could still be
# re-evaluated, not by how much history is pleasant to read.
#
# Seven days, against a catchup window whose ceiling is two hours
# (``catchup_max_seconds``): a claim row older than that can no longer
# refuse any tick, because no tick will ever evaluate a fire that old.
# The margin is generous because this is the one swept table a human
# actually looks at — the dashboard's ``/schedules`` view reads it — and
# a week of scheduled runs is both a useful history and a trivial number
# of rows.
SCHEDULED_RUN_RETENTION_SECONDS = 7 * 24 * 3600.0


class PurgeableStore(Protocol):
    """Anything with rows that expire. One method, deliberately."""

    async def purge(self, older_than_seconds: float) -> int: ...


class MaintenanceWorker:
    """Sweeps expired rows from the short-horizon tables, fleet-wide once.

    Every store is optional: a deployment with no database wires none of
    them and the worker is a no-op loop, which is correct — the memory
    twins prune themselves inline, because a process-local dict dies with
    the process.
    """

    def __init__(
        self,
        *,
        deliveries: PurgeableStore | None = None,
        rate_limits: PurgeableStore | None = None,
        scheduled_runs: PurgeableStore | None = None,
        interval_seconds: float = MAINTENANCE_INTERVAL_SECONDS,
        claim_duty: Any = None,
    ) -> None:
        self._targets: list[tuple[str, PurgeableStore, float]] = []
        if deliveries is not None:
            self._targets.append(
                ("webhook_deliveries", deliveries, DELIVERY_RETENTION_SECONDS)
            )
        if rate_limits is not None:
            self._targets.append(
                ("rate_limits", rate_limits, RATE_LIMIT_RETENTION_SECONDS)
            )
        if scheduled_runs is not None:
            self._targets.append(
                ("scheduled_runs", scheduled_runs, SCHEDULED_RUN_RETENTION_SECONDS)
            )
        self._interval = max(1.0, float(interval_seconds))
        self._claim_duty = claim_duty
        self._running = False
        self._task: asyncio.Task[None] | None = None

    @property
    def targets(self) -> tuple[str, ...]:
        """Table names this worker sweeps, for logs and tests."""
        return tuple(name for name, _store, _ttl in self._targets)

    async def start(self) -> None:
        if self._running:
            return
        self._running = True
        self._task = asyncio.get_event_loop().create_task(
            self._run_loop(), name="maintenance"
        )
        logger.info(
            "maintenance_worker_started",
            interval_seconds=self._interval,
            tables=list(self.targets),
        )

    async def stop(self) -> None:
        if not self._running:
            return
        self._running = False
        task, self._task = self._task, None
        if task is not None:
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await task
        logger.info("maintenance_worker_stopped")

    async def _run_loop(self) -> None:
        try:
            while self._running:
                try:
                    await asyncio.sleep(self._interval)
                except asyncio.CancelledError:
                    return
                if not self._running:
                    return
                try:
                    if not await self._may_tick():
                        continue
                    await self.tick_once()
                except asyncio.CancelledError:
                    return
                except Exception:
                    logger.exception("maintenance_tick_failed")
        finally:
            self._running = False

    async def _may_tick(self) -> bool:
        """Whether this node holds the maintenance duty this tick.

        Re-claimed every tick rather than held, so a node that dies
        mid-sweep releases it by lapsing. Without a placement host — the
        single-node case — the answer is always yes: there is no fleet to
        be a singleton within.
        """
        if self._claim_duty is None:
            return True
        try:
            return bool(await self._claim_duty())
        except Exception:
            logger.exception("maintenance_duty_claim_failed")
            return False

    async def tick_once(self) -> dict[str, int]:
        """Sweep every configured table once. Returns rows deleted per table.

        A table whose purge raises is logged and SKIPPED rather than
        aborting the pass: these are independent retention policies, and
        one unreachable table must not stop the others from being kept in
        bounds. Exposed so tests can drive a sweep without the interval.
        """
        deleted: dict[str, int] = {}
        for name, store, retention in self._targets:
            try:
                count = await store.purge(retention)
            except Exception:
                logger.exception("maintenance_purge_failed", table=name)
                continue
            deleted[name] = int(count)
        swept = sum(deleted.values())
        if swept:
            logger.info("maintenance_swept", rows=swept, **deleted)
        else:
            logger.debug("maintenance_swept", rows=0)
        return deleted


__all__ = [
    "DELIVERY_RETENTION_SECONDS",
    "MAINTENANCE_INTERVAL_SECONDS",
    "RATE_LIMIT_RETENTION_SECONDS",
    "SCHEDULED_RUN_RETENTION_SECONDS",
    "MaintenanceWorker",
    "PurgeableStore",
]
