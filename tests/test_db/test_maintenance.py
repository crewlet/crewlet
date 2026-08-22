"""The retention sweep, and the fact that it runs exactly once per fleet.

Three tables answer "recently" and are written on every event that asks.
All three were designed to be swept — both migrations say so, and both
ship the index a range delete needs — and the sweep was never built:
``purge`` existed on the stores and on their protocols and nothing
anywhere called it. These are the tests that keep it called.
"""

from __future__ import annotations

import asyncio

from crewlet.db.chat_thread_follows import FOLLOW_RETENTION_SECONDS
from crewlet.db.maintenance import (
    DELIVERY_RETENTION_SECONDS,
    MAINTENANCE_INTERVAL_SECONDS,
    RATE_LIMIT_RETENTION_SECONDS,
    SCHEDULED_RUN_RETENTION_SECONDS,
    MaintenanceWorker,
)


class _Purgeable:
    """Records the retention it was asked for, and how often."""

    def __init__(self, *, deleted: int = 0, fail: bool = False) -> None:
        self.calls: list[float] = []
        self._deleted = deleted
        self.fail = fail

    async def purge(self, older_than_seconds: float) -> int:
        if self.fail:
            raise RuntimeError("table is unreachable")
        self.calls.append(older_than_seconds)
        return self._deleted


# ── what one pass does ───────────────────────────────────────────────


async def test_each_table_is_swept_with_its_own_retention() -> None:
    """One retention per table, tied to what that table is FOR.

    A single global number would either keep rate-limit windows for a
    week or throw the scheduled-run ledger away inside its own catchup
    window.
    """
    deliveries, rates, runs = _Purgeable(), _Purgeable(), _Purgeable()
    worker = MaintenanceWorker(
        deliveries=deliveries, rate_limits=rates, scheduled_runs=runs
    )

    await worker.tick_once()

    assert deliveries.calls == [DELIVERY_RETENTION_SECONDS]
    assert rates.calls == [RATE_LIMIT_RETENTION_SECONDS]
    assert runs.calls == [SCHEDULED_RUN_RETENTION_SECONDS]


async def test_the_scheduled_run_ledger_outlives_the_catchup_window() -> None:
    """The one retention with a correctness floor rather than a taste.

    A ledger row is the at-most-once claim for a fire. Deleting one that
    a tick could still evaluate would let that fire run a second time —
    so retention has to exceed ``catchup_max_seconds``, whose ceiling is
    two hours.
    """
    assert SCHEDULED_RUN_RETENTION_SECONDS > 7200.0


async def test_chat_thread_follows_is_swept() -> None:
    """The table that grew for the life of the deployment.

    ``ChatThreadFollowRepository`` shipped ``upsert`` and
    ``is_following`` and nothing else, and this worker never named it —
    so follow rows accumulated forever on a table read on the hot path
    of every inbound chat message.
    """
    follows = _Purgeable(deleted=4)
    worker = MaintenanceWorker(chat_thread_follows=follows)

    deleted = await worker.tick_once()

    assert follows.calls == [FOLLOW_RETENTION_SECONDS]
    assert deleted == {"chat_thread_follows": 4}


async def test_conversation_session_retention_is_configurable() -> None:
    """The one retention an operator sets rather than the module.

    A company running quarter-long tickets has a real reason to remember
    a conversation past the event store's 30-day horizon, so this is
    ``turn_engine.conversation_session.retention_days`` rather than a
    constant like every other target.
    """
    sessions = _Purgeable()
    worker = MaintenanceWorker(
        conversation_sessions=sessions,
        conversation_session_retention_seconds=90 * 24 * 3600.0,
    )

    await worker.tick_once()

    assert sessions.calls == [90 * 24 * 3600.0]


async def test_a_configured_retention_cannot_undercut_the_sweep() -> None:
    """The module's invariant survives a hostile config.

    Every retention here must exceed the tick, or a table sits past its
    horizon for the difference and the retention stops describing it.
    A configured value is the one that could break that, so it is
    floored rather than trusted.
    """
    sessions = _Purgeable()
    worker = MaintenanceWorker(
        conversation_sessions=sessions,
        conversation_session_retention_seconds=1.0,
    )

    await worker.tick_once()

    assert sessions.calls == [MAINTENANCE_INTERVAL_SECONDS]


async def test_a_table_that_cannot_be_swept_does_not_stop_the_others() -> None:
    """Independent retention policies. One unreachable table must not
    leave every other table unbounded."""
    broken, healthy = _Purgeable(fail=True), _Purgeable()
    worker = MaintenanceWorker(deliveries=broken, rate_limits=healthy)

    deleted = await worker.tick_once()

    assert healthy.calls == [RATE_LIMIT_RETENTION_SECONDS]
    assert "webhook_deliveries" not in deleted
    assert deleted["rate_limits"] == 0


async def test_it_reports_what_it_deleted() -> None:
    worker = MaintenanceWorker(
        deliveries=_Purgeable(deleted=12), rate_limits=_Purgeable(deleted=3)
    )
    assert await worker.tick_once() == {"webhook_deliveries": 12, "rate_limits": 3}


async def test_no_stores_is_a_no_op_not_a_crash() -> None:
    """The no-database case. The memory twins prune themselves inline —
    a process-local dict dies with the process — so there is nothing for
    this worker to do and it must say so quietly."""
    worker = MaintenanceWorker()
    assert worker.targets == ()
    assert await worker.tick_once() == {}


# ── once per fleet ───────────────────────────────────────────────────


async def test_a_node_without_the_duty_sweeps_nothing() -> None:
    """N nodes deleting the same rows every tick is N times the write
    amplification and vacuum churn for one table's worth of benefit."""
    worker = MaintenanceWorker(deliveries=_Purgeable(), claim_duty=_never)
    assert await worker._may_tick() is False


async def test_no_placement_host_means_always_mine() -> None:
    """Single node: there is no fleet to be a singleton within."""
    worker = MaintenanceWorker(deliveries=_Purgeable())
    assert await worker._may_tick() is True


async def test_a_duty_claim_that_raises_skips_the_tick() -> None:
    """Fail closed. An unreadable lease store says nothing about who
    holds the duty, and sweeping on that basis is how every node decides
    it is the singleton at once."""

    async def _boom() -> bool:
        raise RuntimeError("lease store unreachable")

    worker = MaintenanceWorker(deliveries=_Purgeable(), claim_duty=_boom)
    assert await worker._may_tick() is False


async def test_the_loop_consults_the_duty_before_sweeping() -> None:
    """The gate wired into the loop, not just available beside it.

    Driven at the real interval floor rather than a millisecond one:
    the floor is deliberate — a maintenance sweep that runs more than
    once a second is a misconfiguration, and a test is not a reason to
    weaken a production guard.
    """
    swept, refused = _Purgeable(), _Purgeable()
    holder = MaintenanceWorker(
        deliveries=swept, interval_seconds=1.0, claim_duty=_always
    )
    peer = MaintenanceWorker(
        deliveries=refused, interval_seconds=1.0, claim_duty=_never
    )
    await holder.start()
    await peer.start()
    try:
        await asyncio.sleep(1.3)
    finally:
        await holder.stop()
        await peer.stop()

    assert swept.calls, "the duty holder never swept"
    assert refused.calls == [], "a node without the duty swept anyway"


async def test_stop_is_idempotent_and_survives_never_starting() -> None:
    worker = MaintenanceWorker(deliveries=_Purgeable())
    await worker.stop()
    await worker.start()
    await worker.stop()
    await worker.stop()


async def _always() -> bool:
    return True


async def _never() -> bool:
    return False


def test_targets_names_the_tables_it_sweeps() -> None:
    """Operator-facing: "which tables is this keeping in bounds?"."""
    worker = MaintenanceWorker(
        deliveries=_Purgeable(), rate_limits=_Purgeable(), scheduled_runs=_Purgeable()
    )
    assert worker.targets == (
        "webhook_deliveries",
        "rate_limits",
        "scheduled_runs",
    )


def test_the_apply_status_table_is_swept_too() -> None:
    """The one keyed by NODE rather than by event, which is why it was
    missed: it does not look like a short-horizon table, and it grows
    the same way regardless — one row per node id that has ever run,
    which under pod names is one row per pod."""
    worker = MaintenanceWorker(apply_status=_Purgeable())
    assert worker.targets == ("config_apply_status",)


async def test_a_dead_nodes_apply_status_is_eventually_collected() -> None:
    """Not expiry — the freshness bound already stops a stale row
    affecting a decision within a minute. This is the growth."""
    from crewlet.db.config_plane import MemoryConfigPlaneStore

    plane = MemoryConfigPlaneStore()
    await plane.record_apply("dead-node", epoch=1, revision_id=None, status="ok")
    assert len(await plane.fleet()) == 1

    assert await plane.purge(older_than_seconds=-1) == 1
    assert await plane.fleet() == []


async def test_a_live_nodes_apply_status_survives_the_sweep() -> None:
    """A node reporting every tick must not be collected out from under
    itself — its row is what every peer's health check reads."""
    from crewlet.db.config_plane import MemoryConfigPlaneStore

    plane = MemoryConfigPlaneStore()
    await plane.record_apply("live-node", epoch=1, revision_id=None, status="ok")
    assert await plane.purge(older_than_seconds=3600) == 0
    assert len(await plane.fleet()) == 1


def test_unconfigured_tables_are_absent_not_silently_skipped() -> None:
    worker = MaintenanceWorker(rate_limits=_Purgeable())
    assert worker.targets == ("rate_limits",)


async def test_the_sweep_expires_the_agent_diary() -> None:
    """`agent_diary` rows carry their own TTL and nothing removed them.

    `delete_expired` had no caller outside tests, so the table grew for
    the life of the deployment while every read still paid to filter
    rows nothing was deleting. It expires per row rather than on a
    table retention, so it takes the idle-A2A shape rather than joining
    the age-purge targets.
    """
    calls: list[int] = []

    async def _expire() -> int:
        calls.append(1)
        return 7

    worker = MaintenanceWorker(expire_diary=_expire)
    swept = await worker.tick_once()

    assert calls == [1]
    assert swept["agent_diary"] == 7


async def test_a_failing_diary_expire_does_not_stop_the_sweep() -> None:
    """One table's failure must not strand the others — the same rule
    the age purges already follow."""

    class _Store:
        async def purge(self, older_than_seconds: float) -> int:
            return 3

    async def _boom() -> int:
        raise RuntimeError("diary unavailable")

    worker = MaintenanceWorker(expire_diary=_boom, rate_limits=_Store())
    swept = await worker.tick_once()

    assert "agent_diary" not in swept
    assert swept["rate_limits"] == 3
