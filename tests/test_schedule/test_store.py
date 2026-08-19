"""Tests for the scheduled-run store (memory + DB claim semantics)."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from crewlet.schedule.store import MemoryScheduledRunStore, ScheduledRunStore


def _kw(**over: Any) -> dict[str, Any]:
    base = {
        "scope_type": "role",
        "scope_id": "qa",
        "schedule_name": "smoke",
        "fire_label": "20260608T0900",
        "target_handle": "qa",
        "scheduled_at": datetime(2026, 6, 8, 9, 0, tzinfo=UTC),
    }
    base.update(over)
    return base


async def test_memory_store_claims_once():
    store = MemoryScheduledRunStore()
    assert await store.claim(**_kw()) is True
    # Same identity tuple → already claimed.
    assert await store.claim(**_kw()) is False
    # Different runner → distinct identity → claimable.
    assert await store.claim(**_kw(target_handle="other")) is True
    assert len(store.records) == 2


async def test_memory_store_dedups_on_fire_label_not_instant():
    # DST fall-back: two distinct UTC instants share one local fire_label,
    # so the run is claimed once (fires once), not twice.
    store = MemoryScheduledRunStore()
    first = datetime(2026, 10, 25, 0, 30, tzinfo=UTC)
    second = datetime(2026, 10, 25, 1, 30, tzinfo=UTC)
    assert await store.claim(**_kw(scheduled_at=first)) is True
    assert await store.claim(**_kw(scheduled_at=second)) is False
    assert len(store.records) == 1


async def test_memory_store_skip_and_fire_dont_collide():
    # A skip row (target_handle='') and a fire row for the same minute have
    # distinct identities, so both are recorded.
    store = MemoryScheduledRunStore()
    assert await store.claim(**_kw(target_handle="", outcome="skipped_catchup")) is True
    assert await store.claim(**_kw(target_handle="qa", outcome="fired")) is True
    assert len(store.records) == 2


class _FakeDB:
    """Minimal Database stand-in capturing the INSERT and its return."""

    def __init__(self, return_value: Any) -> None:
        self._return_value = return_value
        self.calls: list[tuple[str, tuple[Any, ...]]] = []

    async def fetchval(self, query: str, *args: Any) -> Any:
        self.calls.append((query, args))
        return self._return_value


async def test_db_store_claim_inserted():
    db = _FakeDB(return_value="role")  # RETURNING scope_type
    store = ScheduledRunStore(db)  # type: ignore[arg-type]
    assert await store.claim(**_kw()) is True
    query, args = db.calls[0]
    assert "INSERT INTO scheduled_runs" in query
    assert "ON CONFLICT (scope_type, scope_id, schedule_name, fire_label" in query
    assert "DO NOTHING" in query
    # First bound parameter is scope_type; fire_label + default outcome bound.
    assert args[0] == "role"
    assert "20260608T0900" in args
    assert "fired" in args


async def test_db_store_claim_conflict_returns_false():
    db = _FakeDB(return_value=None)  # ON CONFLICT DO NOTHING → no row returned
    store = ScheduledRunStore(db)  # type: ignore[arg-type]
    assert await store.claim(**_kw()) is False


# --- retention -------------------------------------------------------------


async def test_purge_drops_old_rows_and_frees_their_claim():
    """The ledger is a claim table first and a dashboard feed second.

    A purge that dropped the record but kept the claim key would keep
    refusing a fire the store has no evidence of — worse than not
    sweeping at all, because the refusal is silent.
    """
    store = MemoryScheduledRunStore()
    assert await store.claim(**_kw()) is True
    assert await store.claim(**_kw()) is False

    # Everything is older than "-1 seconds ago".
    assert await store.purge(older_than_seconds=-1) == 1
    assert store.records == []
    assert await store.claim(**_kw()) is True


async def test_purge_keeps_rows_inside_the_window():
    """A row a tick could still evaluate must survive: deleting one
    inside the catchup window would let that fire run twice."""
    store = MemoryScheduledRunStore()
    await store.claim(**_kw())
    assert await store.purge(older_than_seconds=3600) == 0
    assert len(store.records) == 1
    assert await store.claim(**_kw()) is False
