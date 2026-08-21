"""Contract suite for the lease primitive.

Every test runs against BOTH backends — the PostgreSQL ``LeaseStore``
(driven through a SQL-executing fake) and the ``MemoryLeaseStore`` — so a
semantic divergence between them is a failing test rather than a
production-only surprise.  The plan's ground rule: a twin that cannot
express a semantic is a bug in the twin.
"""

from __future__ import annotations

import json
import os
from datetime import UTC, datetime, timedelta
from typing import Any

import pytest

from crewlet.db.leases import (
    Lease,
    LeaseError,
    LeaseStore,
    MemoryLeaseStore,
    node_resource,
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
            resource, owner, ttl, preferred, protocol, gated, meta = args
            row = self.rows.get(resource)
            now = self._now()
            # The mixed-version guard: both the INSERT's ``WHERE NOT
            # EXISTS`` and the ON CONFLICT branch's, which are the same
            # predicate written twice — and both skipped when the caller
            # opted out (node presence).
            if gated and any(
                r["expires_at"] > now and int(r.get("protocol") or 1) < int(protocol)
                for r in self.rows.values()
            ):
                return None
            if row is None:
                epoch = 1
            else:
                live = row["expires_at"] > now
                mine = row["owner"] == owner
                if live and not mine:
                    return None  # ON CONFLICT ... WHERE rejected the update
                epoch = row["epoch"] if (live and mine) else row["epoch"] + 1
            # The meta CASE: an empty payload leaves what is there.
            decoded = json.loads(meta) if isinstance(meta, str) else dict(meta or {})
            self.rows[resource] = {
                "resource": resource,
                "owner": owner,
                "epoch": epoch,
                "expires_at": now + timedelta(seconds=float(ttl)),
                "preferred": preferred or (row or {}).get("preferred", ""),
                "protocol": protocol,
                "meta": decoded or dict((row or {}).get("meta") or {}),
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
        now = self._now()
        # Three statements start with "SELECT resource" and they mean
        # completely different things.  Dispatching on the prefix alone
        # sent all three into the list_owned branch, where they compared
        # a row's owner against a resource PREFIX and returned [] — no
        # error, just a wrong answer, under the placement algorithm.
        if q.startswith("SELECT resource, owner, epoch, expires_at, preferred"):
            if "WHERE owner = $1" in q:
                return [
                    dict(r)
                    for r in sorted(
                        self.rows.values(), key=lambda r: str(r["resource"])
                    )
                    if r["owner"] == args[0] and r["expires_at"] > now
                ]
            if "resource LIKE $1" in q:  # list_live(prefix)
                return [
                    dict(r)
                    for r in sorted(
                        self.rows.values(), key=lambda r: str(r["resource"])
                    )
                    if str(r["resource"]).startswith(args[0]) and r["expires_at"] > now
                ]
        if q.startswith("SELECT resource FROM leases"):  # preferred_resources
            return [
                {"resource": r["resource"]}
                for r in sorted(self.rows.values(), key=lambda r: str(r["resource"]))
                if str(r["resource"]).startswith(args[0])
                and str(r.get("preferred") or "") == args[1]
            ]
        raise AssertionError(f"unexpected execute: {q}")

    async def fetchval(self, query: str, *args: Any) -> Any:
        if self.fail:
            raise RuntimeError("database is down")
        q = " ".join(query.split())
        if q.startswith("SELECT MIN(protocol)"):
            now = self._now()
            live = [
                int(r.get("protocol") or 1)
                for r in self.rows.values()
                if r["expires_at"] > now
            ]
            return min(live) if live else None
        raise AssertionError(f"unexpected fetchval: {q}")

    # -- test helpers ------------------------------------------------
    def expire(self, resource: str) -> None:
        """Force a lease to lapse without waiting for wall-clock time."""
        self.rows[resource]["expires_at"] = self._now() - timedelta(seconds=1)


class _RealSQL:
    """The store's SQL against a REAL PostgreSQL, when one is offered.

    ``_FakeSQL`` above keeps the store's semantics under test everywhere,
    but it can only ever confirm that the SQL means what its author
    *thought* it meant — it cannot catch a statement PostgreSQL rejects
    or evaluates differently. The mixed-version guard is exactly that
    shape of risk: an ``INSERT … SELECT … WHERE NOT EXISTS`` with a
    second ``NOT EXISTS`` inside the ``ON CONFLICT DO UPDATE`` branch is
    not something to trust unrun.

    Point ``CREWLET_TEST_DSN`` at a database and the whole contract suite
    runs a third time, against it. Without one this backend is skipped —
    and skipping is not passing.
    """

    # One connection per TEST, opened lazily and closed by the fixture.
    #
    # Not one shared pool for the module, and not one per call: pytest-asyncio
    # gives each test a fresh event loop, and an asyncpg pool is bound to
    # the loop that created it — reusing one across tests hangs the second
    # test rather than failing it. Leaking one per test exhausts
    # ``max_connections`` instead. So: per test, and closed.
    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._db: Any = None

    async def _pool(self) -> Any:
        if self._db is None:
            from crewlet.db.client import Database

            self._db = await Database.connect(self._dsn)
            # Start from an empty table. The mixed-version guard is
            # fleet-wide, so one test's leftover lease blocks the next
            # test's claims. DELETE rather than TRUNCATE — TRUNCATE takes
            # an ACCESS EXCLUSIVE lock that a pooled connection can
            # contend with its own siblings for.
            await self._db.execute("DELETE FROM leases")
        return self._db

    async def aclose(self) -> None:
        if self._db is not None:
            await self._db.close()
            self._db = None

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        return await (await self._pool()).fetchrow(query, *args)

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        return await (await self._pool()).execute(query, *args)

    async def fetchval(self, query: str, *args: Any) -> Any:
        return await (await self._pool()).fetchval(query, *args)

    async def expire_async(self, resource: str) -> None:
        await (await self._pool()).execute(
            "UPDATE leases SET expires_at = now() - interval '1 second' "
            "WHERE resource = $1",
            resource,
        )


_TEST_DSN = os.environ.get("CREWLET_TEST_DSN", "")


def _memory() -> Any:
    return MemoryLeaseStore()


def _postgres() -> Any:
    return LeaseStore(_FakeSQL())


def _real_postgres() -> Any:
    return LeaseStore(_RealSQL(_TEST_DSN))


async def _expire(store: Any, resource: str) -> None:
    """Lapse a lease without sleeping."""
    if isinstance(store, MemoryLeaseStore):
        store._rows[resource]["expires_at"] = datetime.now(UTC) - timedelta(seconds=1)
    elif isinstance(store._db, _RealSQL):
        await store._db.expire_async(resource)
    else:
        store._db.expire(resource)


BACKENDS = [
    pytest.param(_memory, id="memory"),
    pytest.param(_postgres, id="postgres"),
    pytest.param(
        _real_postgres,
        id="postgres-real",
        marks=[
            pytest.mark.integration,
            pytest.mark.skipif(
                not _TEST_DSN, reason="set CREWLET_TEST_DSN to run against real PG"
            ),
        ],
    ),
]


@pytest.fixture(params=BACKENDS)
async def store(request: pytest.FixtureRequest) -> Any:
    built = request.param()
    yield built
    # Only the real-PG backend holds a resource worth releasing, and it
    # MUST be released per test — see _RealSQL on why the connection
    # cannot outlive the test's event loop.
    db = getattr(built, "_db", None)
    if isinstance(db, _RealSQL):
        await db.aclose()


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
    await _expire(store, "seat:ceo")
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
    await _expire(store, "seat:ceo")
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
    await _expire(store, "seat:ceo")
    assert not await store.renew(
        "seat:ceo", owner="node-a", epoch=lease.epoch, ttl_seconds=30
    )


async def test_renew_rejects_a_stale_epoch(store: Any) -> None:
    lease = await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    await _expire(store, "seat:ceo")
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
    await _expire(store, "seat:ceo")
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
    await _expire(store, "seat:cto")

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


async def test_acquire_raises_rather_than_reporting_a_peer_holds_it() -> None:
    """Fails closed, and says WHICH failure it is.

    Both outcomes mean "you are not the owner", so both are safe — but
    only ``None`` means *somebody else is*, and returning it for an
    unreachable store made every caller report the wrong thing. A
    two-second blip had ``_renew_node_presence`` logging "another
    process holds this node id's presence lease", pointing an operator
    at a configuration problem that did not exist, while the node
    silently stopped refreshing its own presence during exactly the
    outage it is built to ride out — so peers widened their share
    against a node that was still healthy and still holding every seat.

    ``renew`` and ``release`` have always drawn this line. Acquire now
    draws it too.
    """
    sql = _FakeSQL()
    store = LeaseStore(sql)
    sql.fail = True
    with pytest.raises(LeaseError):
        await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)


async def test_acquire_still_returns_none_when_a_peer_really_holds_it() -> None:
    """The other half: a genuine refusal is not an error."""
    sql = _FakeSQL()
    store = LeaseStore(sql)
    assert await store.try_acquire("seat:ceo", owner="node-a", ttl_seconds=30)
    assert await store.try_acquire("seat:ceo", owner="node-b", ttl_seconds=30) is None


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


# ── mixed-version fleets ─────────────────────────────────────────────
#
# A rolling upgrade puts a vN and a vN+1 node on this table at once. Two
# nodes that disagree about what holding a lease MEANS are each
# individually correct and jointly wrong, so the newer one waits.


async def test_a_newer_node_refuses_to_claim_beside_an_older_holder(
    store: Any,
) -> None:
    """The gate, stated as the deploy it protects.

    An old node holds one seat. The new node must claim NOTHING — not
    even an unrelated, entirely unclaimed seat — until that hold ends.
    """
    old = await store.try_acquire(
        "seat:ceo", owner="old-node:1", ttl_seconds=30, protocol=1
    )
    assert old is not None

    assert (
        await store.try_acquire(
            "seat:engineer", owner="new-node:1", ttl_seconds=30, protocol=2
        )
        is None
    )


async def test_an_older_node_still_claims_beside_a_newer_holder(store: Any) -> None:
    """Asymmetric on purpose: the old build has no check to run.

    Nothing in the table can stop it, which is exactly why a downgrade
    across a protocol bump needs a full fleet drain — documented at
    ``PROTOCOL_VERSION``.
    """
    assert (
        await store.try_acquire(
            "seat:ceo", owner="new-node:1", ttl_seconds=30, protocol=2
        )
        is not None
    )
    assert (
        await store.try_acquire(
            "seat:engineer", owner="old-node:1", ttl_seconds=30, protocol=1
        )
        is not None
    )


async def test_the_gate_lifts_when_the_old_lease_lapses(store: Any) -> None:
    """A rolling deploy converges: drain the old, the new take over."""
    await store.try_acquire("seat:ceo", owner="old-node:1", ttl_seconds=30, protocol=1)
    assert (
        await store.try_acquire(
            "seat:ceo", owner="new-node:1", ttl_seconds=30, protocol=2
        )
        is None
    )

    await _expire(store, "seat:ceo")
    lease = await store.try_acquire(
        "seat:ceo", owner="new-node:1", ttl_seconds=30, protocol=2
    )
    assert lease is not None
    assert lease.protocol == 2


async def test_the_gate_lifts_when_the_old_lease_is_released(store: Any) -> None:
    old = await store.try_acquire(
        "seat:ceo", owner="old-node:1", ttl_seconds=30, protocol=1
    )
    assert old is not None
    assert await store.release("seat:ceo", owner="old-node:1", epoch=old.epoch)

    lease = await store.try_acquire(
        "seat:engineer", owner="new-node:1", ttl_seconds=30, protocol=2
    )
    assert lease is not None


async def test_same_protocol_peers_are_unaffected(store: Any) -> None:
    """The gate must not fire between peers of the same build — that is
    every normal deployment, and it would be a fleet-wide stall."""
    assert (
        await store.try_acquire(
            "seat:ceo", owner="node-a:1", ttl_seconds=30, protocol=2
        )
        is not None
    )
    assert (
        await store.try_acquire(
            "seat:engineer", owner="node-b:1", ttl_seconds=30, protocol=2
        )
        is not None
    )


async def test_a_newer_node_cannot_renew_its_way_around_the_gate(store: Any) -> None:
    """Re-acquire is the renew path for a live lease, so it goes through
    the same guard — otherwise a node that claimed before the old one
    appeared would keep extending indefinitely."""
    mine = await store.try_acquire(
        "seat:ceo", owner="new-node:1", ttl_seconds=30, protocol=2
    )
    assert mine is not None
    await store.try_acquire(
        "seat:engineer", owner="old-node:1", ttl_seconds=30, protocol=1
    )

    assert (
        await store.try_acquire(
            "seat:ceo", owner="new-node:1", ttl_seconds=30, protocol=2
        )
        is None
    )
    # ``renew`` is deliberately NOT gated: it extends a hold this node
    # already has and already acts on, and refusing it would drop a seat
    # mid-turn rather than prevent anything.
    assert await store.renew(
        "seat:ceo", owner="new-node:1", epoch=mine.epoch, ttl_seconds=30
    )


async def test_fleet_protocol_floor_reports_the_oldest_live_holder(store: Any) -> None:
    """The observability half — ``try_acquire`` can only answer yes/no,
    so a node stalled by the gate would otherwise look identical to one
    whose peers simply hold every seat."""
    assert await store.fleet_protocol_floor() is None

    await store.try_acquire("seat:ceo", owner="new:1", ttl_seconds=30, protocol=3)
    assert await store.fleet_protocol_floor() == 3

    await store.try_acquire("seat:eng", owner="old:1", ttl_seconds=30, protocol=1)
    assert await store.fleet_protocol_floor() == 1

    await _expire(store, "seat:eng")
    assert await store.fleet_protocol_floor() == 3


async def test_fleet_protocol_floor_ignores_lapsed_leases(store: Any) -> None:
    await store.try_acquire("seat:ceo", owner="old:1", ttl_seconds=30, protocol=1)
    await _expire(store, "seat:ceo")
    assert await store.fleet_protocol_floor() is None


# ---------------------------------------------------------------------
# meta — what the holder IS, not just that it holds
# ---------------------------------------------------------------------


async def test_meta_round_trips(store: Any) -> None:
    """Node presence carries the node's roles and labels here."""
    payload = {"roles": ["seats"], "labels": {"zone": "eu"}}
    lease = await store.try_acquire(
        node_resource("n1"), owner="n1:a", ttl_seconds=60, meta=payload
    )
    assert lease is not None and lease.meta == payload
    read = await store.get(node_resource("n1"))
    assert read is not None and read.meta == payload
    (live,) = await store.list_live("node:")
    assert live.meta == payload


async def test_a_claim_that_says_nothing_leaves_meta_alone(store: Any) -> None:
    """A seat claim must not blank a presence row it does not own.

    Both backends implement this as "an empty payload keeps what is
    there" rather than as a per-resource rule, so a renew that forgets to
    re-send the profile does not silently un-label a node mid-flight —
    which peers would read as a node that matches no placement at all.
    """
    resource = node_resource("n1")
    await store.try_acquire(
        resource, owner="n1:a", ttl_seconds=60, meta={"roles": ["workers"]}
    )
    again = await store.try_acquire(resource, owner="n1:a", ttl_seconds=60)
    assert again is not None and again.meta == {"roles": ["workers"]}


async def test_meta_is_replaced_not_merged(store: Any) -> None:
    """A node that drops a role must stop advertising it.

    Merging would make a role impossible to remove without a restart AND
    a lease expiry, and the whole point of re-sending the profile on
    every renew is that it tracks the live process.
    """
    resource = node_resource("n1")
    await store.try_acquire(
        resource, owner="n1:a", ttl_seconds=60, meta={"roles": ["seats", "workers"]}
    )
    updated = await store.try_acquire(
        resource, owner="n1:a", ttl_seconds=60, meta={"roles": ["seats"]}
    )
    assert updated is not None and updated.meta == {"roles": ["seats"]}


async def test_a_lease_without_meta_reads_as_empty(store: Any) -> None:
    """The pre-migration row shape. ``NodeProfile.from_meta`` turns this
    into "does everything, labelled with nothing" — the old behaviour,
    which is the only safe reading of a peer that never told you."""
    lease = await store.try_acquire(seat_resource("ceo"), owner="n1:a", ttl_seconds=60)
    assert lease is not None and lease.meta == {}
