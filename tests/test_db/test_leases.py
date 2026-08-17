"""Contract suite for the lease primitive.

Every test runs against BOTH backends — the PostgreSQL ``LeaseStore``
(driven through a SQL-executing fake) and the ``MemoryLeaseStore`` — so a
semantic divergence between them is a failing test rather than a
production-only surprise.  The plan's ground rule: a twin that cannot
express a semantic is a bug in the twin.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from typing import Any

import pytest

from crewlet.db.leases import (
    Lease,
    LeaseError,
    LeaseStore,
    MemoryLeaseStore,
    seat_resource,
    worker_resource,
)


class _FakeSQL:
    """Executes the LeaseStore's SQL semantically, in Python.

    Not a full SQL engine — it recognises the four statements the store
    issues and applies their documented effects, including the epoch CASE
    and the ``WHERE`` guards.  That keeps the PG path under test without
    a live database, and any change to the SQL that this cannot model is
    a signal the SQL got too clever for its own good.
    """

    def __init__(self) -> None:
        self.rows: dict[str, dict[str, Any]] = {}
        self.fail = False

    def _now(self) -> datetime:
        return datetime.now(UTC)

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        if self.fail:
            raise RuntimeError("database is down")
        q = " ".join(query.split())
        if q.startswith("INSERT INTO leases"):
            resource, owner, ttl, preferred, protocol = args
            row = self.rows.get(resource)
            now = self._now()
            if row is None:
                epoch = 1
            else:
                live = row["expires_at"] > now
                mine = row["owner"] == owner
                if live and not mine:
                    return None  # ON CONFLICT ... WHERE rejected the update
                epoch = row["epoch"] if (live and mine) else row["epoch"] + 1
            self.rows[resource] = {
                "resource": resource,
                "owner": owner,
                "epoch": epoch,
                "expires_at": now + timedelta(seconds=float(ttl)),
                "preferred": preferred or (row or {}).get("preferred", ""),
                "protocol": protocol,
            }
            return dict(self.rows[resource])
        if q.startswith("UPDATE leases SET expires_at = now() + make_interval"):
            resource, owner, epoch, ttl = args
            row = self.rows.get(resource)
            if (
                row is None
                or row["owner"] != owner
                or row["epoch"] != epoch
                or row["expires_at"] <= self._now()
            ):
                return None
            row["expires_at"] = self._now() + timedelta(seconds=float(ttl))
            return {"resource": resource}
        if q.startswith("UPDATE leases SET expires_at = now(), owner = ''"):
            resource, owner, epoch = args
            row = self.rows.get(resource)
            if row is None or row["owner"] != owner or row["epoch"] != epoch:
                return None
            row["owner"] = ""
            row["expires_at"] = self._now()
            return {"resource": resource}
        if q.startswith("SELECT resource"):
            row = self.rows.get(args[0])
            return dict(row) if row else None
        raise AssertionError(f"unexpected fetchrow: {q}")

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        if self.fail:
            raise RuntimeError("database is down")
        q = " ".join(query.split())
        if q.startswith("SELECT resource"):
            now = self._now()
            return [
                dict(r)
                for r in sorted(self.rows.values(), key=lambda r: str(r["resource"]))
                if r["owner"] == args[0] and r["expires_at"] > now
            ]
        raise AssertionError(f"unexpected execute: {q}")

    async def fetchval(self, query: str, *args: Any) -> Any:
        raise AssertionError("unused")

    # -- test helpers ------------------------------------------------
    def expire(self, resource: str) -> None:
        """Force a lease to lapse without waiting for wall-clock time."""
        self.rows[resource]["expires_at"] = self._now() - timedelta(seconds=1)


def _memory() -> Any:
    return MemoryLeaseStore()


def _postgres() -> Any:
    return LeaseStore(_FakeSQL())


def _expire(store: Any, resource: str) -> None:
    """Lapse a lease without sleeping."""
    if isinstance(store, MemoryLeaseStore):
        store._rows[resource]["expires_at"] = datetime.now(UTC) - timedelta(seconds=1)
    else:
        store._db.expire(resource)


BACKENDS = [pytest.param(_memory, id="memory"), pytest.param(_postgres, id="postgres")]


@pytest.fixture(params=BACKENDS)
def store(request: pytest.FixtureRequest) -> Any:
    return request.param()


async def test_acquire_unclaimed_starts_at_epoch_one(store: Any) -> None:
    lease = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    assert isinstance(lease, Lease)
    assert lease.owner == "node-a"
    assert lease.epoch == 1


async def test_second_owner_cannot_take_a_live_lease(store: Any) -> None:
    await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    assert await store.try_acquire("seat:ceo", owner="node-b", ttl_seconds=30) is None


async def test_unbroken_same_owner_reacquire_keeps_the_epoch(store: Any) -> None:
    """A live holder re-acquiring is a renewal — nothing was ever unowned,
    so in-flight work stays valid and the fencing token must not move."""
    first = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    again = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    assert again is not None
    assert again.epoch == first.epoch


async def test_takeover_after_expiry_bumps_the_epoch(store: Any) -> None:
    first = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    _expire(store, "seat:ceo")
    taken = await store.try_acquire("seat:ceo", owner="node-b", ttl_seconds=30)
    assert taken is not None
    assert taken.owner == "node-b"
    assert taken.epoch == first.epoch + 1


async def test_same_owner_reacquire_after_lapse_also_bumps_the_epoch(
    store: Any,
) -> None:
    """The subtle one. During the lapse this owner's in-flight work was no
    longer covered by a lease, so it must be fenced against its own past
    self — otherwise a zombie coroutine from before the gap still passes
    ``WHERE owner_epoch = $current``."""
    first = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    _expire(store, "seat:ceo")
    again = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    assert again is not None
    assert again.epoch == first.epoch + 1


async def test_renew_extends_a_live_lease(store: Any) -> None:
    lease = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    assert await store.renew(
        "seat:ceo", owner="node-a", epoch=lease.epoch, ttl_seconds=30
    )


async def test_renew_rejects_a_lapsed_lease(store: Any) -> None:
    """A lapsed lease cannot be renewed, only re-acquired — which mints a
    new epoch. Renewing across a gap would silently re-cover work that ran
    unprotected."""
    lease = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    _expire(store, "seat:ceo")
    assert not await store.renew(
        "seat:ceo", owner="node-a", epoch=lease.epoch, ttl_seconds=30
    )


async def test_renew_rejects_a_stale_epoch(store: Any) -> None:
    lease = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    _expire(store, "seat:ceo")
    await store.try_acquire("seat:ceo", owner="node-b", ttl_seconds=30)
    assert not await store.renew(
        "seat:ceo", owner="node-a", epoch=lease.epoch, ttl_seconds=30
    )


async def test_renew_rejects_another_owner(store: Any) -> None:
    lease = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    assert not await store.renew(
        "seat:ceo", owner="node-b", epoch=lease.epoch, ttl_seconds=30
    )


async def test_release_is_owner_and_epoch_predicated(store: Any) -> None:
    """The defect this primitive exists to fix: an unqualified release lets
    a departing owner clear its SUCCESSOR's live lease."""
    first = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    _expire(store, "seat:ceo")
    await store.try_acquire("seat:ceo", owner="node-b", ttl_seconds=30)

    # node-a, late to notice it lost the seat, tries to clean up.
    assert not await store.release("seat:ceo", owner="node-a", epoch=first.epoch)

    still = await store.get("seat:ceo")
    assert still is not None and still.owner == "node-b"


async def test_release_frees_the_resource_for_immediate_takeover(store: Any) -> None:
    lease = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=300)
    assert await store.release("seat:ceo", owner="node-a", epoch=lease.epoch)
    assert not await store.list_owned("node-a")
    taken = await store.try_acquire("seat:ceo", owner="node-b", ttl_seconds=30)
    assert taken is not None and taken.owner == "node-b"


async def test_release_keeps_the_epoch_monotonic(store: Any) -> None:
    """A released lease must NOT reset the fencing counter.

    Deleting the row would send the next acquirer back to epoch 1 — the
    exact token a zombie from the released tenure is still stamping its
    writes with, so it would sail through the new owner's
    ``WHERE owner_epoch = $current`` predicate.
    """
    first = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=300)
    await store.release("seat:ceo", owner="node-a", epoch=first.epoch)
    taken = await store.try_acquire("seat:ceo", owner="node-b", ttl_seconds=30)
    assert taken is not None
    assert taken.epoch > first.epoch


async def test_release_then_reacquire_by_the_same_owner_also_advances(
    store: Any,
) -> None:
    """Same hazard without a second node: a process that releases,
    restarts and re-claims must not inherit its predecessor's token."""
    first = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=300)
    await store.release("seat:ceo", owner="node-a", epoch=first.epoch)
    again = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    assert again is not None and again.epoch > first.epoch


async def test_release_preserves_the_preferred_placement_hint(store: Any) -> None:
    """The hint has to survive a graceful release — that is the only
    moment it is useful, since a rolling deploy is exactly when we want
    the seat to land back on its warm node."""
    lease = await store.try_acquire(
        "seat:ceo", owner="node-a:1", ttl_seconds=300, preferred="node-a"
    )
    await store.release("seat:ceo", owner="node-a:1", epoch=lease.epoch)
    row = await store.get("seat:ceo")
    assert row is not None and row.preferred == "node-a"


async def test_expires_at_is_an_aware_datetime_in_both_backends(store: Any) -> None:
    """A heartbeat computes its next tick from this field. A float in one
    backend and a datetime in the other passes every test and raises
    TypeError on the first production tick."""
    lease = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    assert isinstance(lease.expires_at, datetime)
    assert lease.expires_at.tzinfo is not None


async def test_two_holders_under_one_node_id_do_not_both_win(store: Any) -> None:
    """`owner` is a process incarnation, not a machine. Sharing an owner
    string — which the default node id `node-0` would do across two
    engines — would hand both the same fencing epoch."""
    first = await store.try_acquire("seat:ceo", owner="node-0:aaaa", ttl_seconds=30)
    second = await store.try_acquire("seat:ceo", owner="node-0:bbbb", ttl_seconds=30)
    assert first is not None
    assert second is None


async def test_list_owned_excludes_lapsed_leases(store: Any) -> None:
    await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    await store.try_acquire("seat:cto", owner="node-a", ttl_seconds=30)
    await store.try_acquire("seat:pm", owner="node-b", ttl_seconds=30)
    _expire(store, "seat:cto")

    owned = await store.list_owned("node-a")
    assert [lease.resource for lease in owned] == ["seat:ceo"]


async def test_preferred_hint_round_trips(store: Any) -> None:
    lease = await store.try_acquire(
        "seat:ceo", owner="node-a", ttl_seconds=30, preferred="node-a"
    )
    assert lease.preferred == "node-a"
    assert (await store.get("seat:ceo")).preferred == "node-a"


async def test_blank_arguments_are_rejected(store: Any) -> None:
    with pytest.raises(ValueError):
        await store.try_acquire("", owner="node-a", ttl_seconds=30)
    with pytest.raises(ValueError):
        await store.try_acquire("seat:ceo", owner="", ttl_seconds=30)
    with pytest.raises(ValueError):
        await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=0)


async def test_acquire_fails_closed_on_a_database_error() -> None:
    """A claim that cannot be verified must not be assumed won — the
    caller would proceed as owner on unknown state."""
    sql = _FakeSQL()
    store = LeaseStore(sql)
    sql.fail = True
    assert await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30) is None


async def test_renew_raises_rather_than_reporting_loss_on_a_database_error() -> None:
    """The opposite of acquire, and deliberately so.

    ``False`` from renew instructs the caller to shed everything the
    lease covered. A two-second connection blip changed nothing about
    ownership — the row is untouched and still held — so answering
    ``False`` would tear down a healthy node's seats for a whole TTL
    during which no peer could claim them either.
    """
    sql = _FakeSQL()
    store = LeaseStore(sql)
    await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    sql.fail = True
    with pytest.raises(LeaseError):
        await store.renew("seat:ceo", owner="node-a", epoch=1, ttl_seconds=30)
    with pytest.raises(LeaseError):
        await store.release("seat:ceo", owner="node-a", epoch=1)
    with pytest.raises(LeaseError):
        await store.get("seat:ceo")
    with pytest.raises(LeaseError):
        await store.list_owned("node-a")


def test_resource_naming_helpers() -> None:
    assert seat_resource("sarah-chen") == "seat:sarah-chen"
    assert worker_resource("scheduler") == "worker:scheduler"
