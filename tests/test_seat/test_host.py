"""Contract suite for seat placement.

Runs against the memory lease twin by default and, when
``CREWLET_TEST_DSN`` is set, against a real PostgreSQL as well — the same
rule the lease suite follows, for the same reason: the placement policy
is only as trustworthy as the ownership primitive underneath it.

The properties here are the ones a wrong answer breaks in production
rather than in a test: a node that claims more than its share starves its
peers, one that drops seats on a database blip takes the company down
over a two-second outage, and one that keeps a seat after losing its
lease is a zombie running someone else's agent.
"""

from __future__ import annotations

import os
from datetime import UTC, datetime, timedelta
from typing import Any

import pytest

from crewlet.db.leases import Lease, LeaseError, MemoryLeaseStore, node_resource
from crewlet.seat.host import SeatHost, SweepResult

_TEST_DSN = os.environ.get("CREWLET_TEST_DSN", "")


# ── backends ─────────────────────────────────────────────────────────


class _RealLeases:
    """LeaseStore over a real database, one connection per test.

    pytest-asyncio gives each test a fresh event loop and an asyncpg pool
    is bound to the loop that made it, so this is per-test and closed by
    the fixture — see tests/test_db/test_leases.py, where the same shape
    is explained at length.
    """

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._db: Any = None
        self._store: Any = None

    async def _ready(self) -> Any:
        if self._store is None:
            from crewlet.db.client import Database
            from crewlet.db.leases import LeaseStore

            self._db = await Database.connect(self._dsn)
            await self._db.execute("DELETE FROM leases")
            self._store = LeaseStore(self._db)
        return self._store

    def __getattr__(self, name: str) -> Any:
        async def _call(*args: Any, **kwargs: Any) -> Any:
            store = await self._ready()
            return await getattr(store, name)(*args, **kwargs)

        return _call

    async def aclose(self) -> None:
        if self._db is not None:
            await self._db.close()
            self._db = None
            self._store = None


BACKENDS = [
    pytest.param(MemoryLeaseStore, id="memory"),
    pytest.param(
        lambda: _RealLeases(_TEST_DSN),
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
async def leases(request: pytest.FixtureRequest) -> Any:
    built = request.param()
    yield built
    if isinstance(built, _RealLeases):
        await built.aclose()


def _host(leases: Any, node: str = "node-a", **kw: Any) -> SeatHost:
    return SeatHost(
        leases=leases,
        owner=kw.pop("owner", f"{node}:incarnation"),
        node_id=node,
        seats=kw.pop("seats", lambda: ["ceo", "eng", "ops"]),
        **kw,
    )


# ── fair share ───────────────────────────────────────────────────────


async def test_a_single_node_takes_every_seat(leases: Any) -> None:
    """The degenerate case is the common one and must stay boring."""
    host = _host(leases)
    await host._renew_node_presence()
    result = await host.sweep()
    assert result.live_nodes == 1
    assert result.capacity == 3
    assert set(host.held_handles) == {"ceo", "eng", "ops"}


async def test_two_nodes_split_the_seats(leases: Any) -> None:
    """``ceil(seats / live nodes)`` — computed identically by both, from
    the same table, with no coordinator and no gossip."""
    a = _host(leases, "node-a")
    b = _host(leases, "node-b")
    await a._renew_node_presence()
    await b._renew_node_presence()

    ra = await a.sweep()
    rb = await b.sweep()

    assert ra.live_nodes == 2 and rb.live_nodes == 2
    assert ra.capacity == 2 and rb.capacity == 2
    assert len(a.held_handles) == 2
    # Whatever a did not take is claimable by b; no seat is held twice.
    assert not set(a.held_handles) & set(b.held_handles)
    assert len(a.held_handles) + len(b.held_handles) == 3


async def test_capacity_widens_when_a_peer_dies(leases: Any) -> None:
    """A dead node stops counting toward the divisor as soon as its
    presence lease lapses — which is what lets survivors widen instead of
    leaving the seats dark."""
    a = _host(leases, "node-a")
    b = _host(leases, "node-b")
    await a._renew_node_presence()
    await b._renew_node_presence()
    await a.sweep()
    assert (await a.sweep()).capacity == 2

    # node-b dies: its presence lease lapses.
    await _expire(leases, node_resource("node-b"))
    result = await a.sweep()
    assert result.live_nodes == 1
    assert result.capacity == 3
    assert set(a.held_handles) == {"ceo", "eng", "ops"}


async def test_a_node_never_exceeds_its_share(leases: Any) -> None:
    a = _host(leases, "node-a", seats=lambda: [f"s{i}" for i in range(10)])
    for node in ("node-a", "node-b", "node-c"):
        await _host(leases, node)._renew_node_presence()

    # Sweep repeatedly; the cap must hold regardless of how many passes.
    for _ in range(6):
        await a.sweep()
    assert len(a.held_handles) == 4  # ceil(10 / 3)


# ── claim rate ───────────────────────────────────────────────────────


async def test_claims_are_rate_limited_per_sweep(leases: Any) -> None:
    """The limiter is MCP spawn cost, not the lease.

    A node absorbing a dead peer's seats all at once would fork that many
    subprocess trees in a single tick.
    """
    seats = [f"s{i}" for i in range(10)]
    host = _host(leases, seats=lambda: seats, claim_limit=3)
    await host._renew_node_presence()

    assert len((await host.sweep()).claimed) == 3
    assert len((await host.sweep()).claimed) == 3
    assert len((await host.sweep()).claimed) == 3
    assert len(host.held_handles) == 9
    assert len((await host.sweep()).claimed) == 1
    assert len(host.held_handles) == 10


# ── stickiness ───────────────────────────────────────────────────────


async def test_preferred_seats_are_tried_first(leases: Any) -> None:
    """A restart should land seats back where their MCP children were."""
    seats = ["alpha", "beta", "gamma", "delta"]
    first = _host(leases, "node-a", seats=lambda: seats, claim_limit=2)
    await first._renew_node_presence()
    await first.sweep()
    warm = set(first.held_handles)
    assert len(warm) == 2
    await first.release_all()

    # Same node id, new incarnation — a restart.
    again = _host(
        leases,
        "node-a",
        owner="node-a:second-incarnation",
        seats=lambda: seats,
        claim_limit=2,
    )
    await again._renew_node_presence()
    await again.sweep()
    assert set(again.held_handles) == warm


async def test_a_stale_hint_never_blocks_a_claim(leases: Any) -> None:
    """The hint outlives the node that set it.

    Treating a foreign ``preferred`` as a reason to wait would strand
    every seat a dead node used to hold — permanently.
    """
    seats = ["alpha", "beta"]
    dead = _host(leases, "dead-node", seats=lambda: seats)
    await dead._renew_node_presence()
    await dead.sweep()
    assert set(dead.held_handles) == {"alpha", "beta"}
    for handle in ("alpha", "beta"):
        await _expire(leases, f"seat:{handle}")
    await _expire(leases, node_resource("dead-node"))

    survivor = _host(leases, "node-b", seats=lambda: seats)
    await survivor._renew_node_presence()
    await survivor.sweep()
    assert set(survivor.held_handles) == {"alpha", "beta"}


# ── losing seats ─────────────────────────────────────────────────────


async def test_a_lost_lease_drops_the_seat_immediately(leases: Any) -> None:
    """Lost means gone: a peer may already be running it, so everything
    this node does for it from here is a zombie's work."""
    released: list[str] = []

    async def _on_release(handle: str, lease: Lease) -> None:
        released.append(handle)

    host = _host(leases, seats=lambda: ["ceo"], on_release=_on_release)
    await host._renew_node_presence()
    await host.sweep()
    assert host.owns("ceo")

    await _expire(leases, "seat:ceo")
    lost = await host.heartbeat()

    assert lost == ("ceo",)
    assert not host.owns("ceo")
    assert host.epoch_for("ceo") is None
    assert released == ["ceo"]


async def test_a_database_blip_does_NOT_drop_seats(leases: Any) -> None:
    """The distinction the whole design turns on.

    ``renew`` returning False means the lease is definitively gone.
    ``LeaseError`` means the STORE could not be reached, which says
    nothing about ownership — the row is untouched and still held.
    Conflating them tears a healthy node's whole company down over a
    two-second blip, during which no peer could claim the seats anyway.
    """
    host = _host(leases, seats=lambda: ["ceo", "eng"])
    await host._renew_node_presence()
    await host.sweep()
    held_before = set(host.held_handles)
    assert held_before

    async def _boom(*args: Any, **kwargs: Any) -> bool:
        raise LeaseError("database is down")

    host.leases = _Unavailable(host.leases, renew=_boom)
    lost = await host.heartbeat()

    assert lost == ()
    assert set(host.held_handles) == held_before


class _Unavailable:
    """Wraps a lease backend, making chosen methods raise LeaseError."""

    def __init__(self, inner: Any, **overrides: Any) -> None:
        self._inner = inner
        self._overrides = overrides

    def __getattr__(self, name: str) -> Any:
        if name in self._overrides:
            return self._overrides[name]
        return getattr(self._inner, name)


# ── the org changing underneath ──────────────────────────────────────


async def test_a_decommissioned_role_is_released(leases: Any) -> None:
    """Seats are read fresh each sweep: a live config apply can delete a
    role, and holding its lease afterwards would look like ownership of
    something that no longer exists."""
    seats = ["ceo", "eng"]
    host = _host(leases, seats=lambda: list(seats))
    await host._renew_node_presence()
    await host.sweep()
    assert host.owns("eng")

    seats.remove("eng")
    await host.sweep()
    assert not host.owns("eng")
    assert host.owns("ceo")


# ── draining ─────────────────────────────────────────────────────────


async def test_draining_stops_claiming_but_keeps_holding(leases: Any) -> None:
    """The first half of a graceful shutdown: no new work, but the seats
    in hand keep their leases alive so their turns can finish."""
    seats = ["a", "b", "c"]
    host = _host(leases, seats=lambda: seats, claim_limit=1)
    await host._renew_node_presence()
    await host.sweep()
    held = set(host.held_handles)
    assert len(held) == 1

    await host.begin_drain()
    result = await host.sweep()

    assert result.claimed == ()
    assert set(host.held_handles) == held
    assert await host.heartbeat() == ()


async def test_release_all_frees_seats_for_a_peer(leases: Any) -> None:
    host = _host(leases, "node-a", seats=lambda: ["ceo"])
    await host._renew_node_presence()
    await host.sweep()
    await host.release_all()
    assert host.held_handles == ()

    peer = _host(leases, "node-b", seats=lambda: ["ceo"])
    await peer._renew_node_presence()
    await peer.sweep()
    assert peer.owns("ceo")


# ── hooks ────────────────────────────────────────────────────────────


async def test_a_failed_takeover_gives_the_seat_back(leases: Any) -> None:
    """A seat whose takeover pipeline failed must not stay claimed.

    It would read as owned to the whole fleet while nothing actually runs
    it — the seat would simply go dark until the process restarted.
    """
    calls: list[str] = []

    async def _explode(handle: str, lease: Lease) -> None:
        calls.append(handle)
        raise RuntimeError("MCP spawn failed")

    host = _host(leases, seats=lambda: ["ceo"], on_acquire=_explode)
    await host._renew_node_presence()
    await host.sweep()

    assert calls == ["ceo"]
    assert not host.owns("ceo")
    lease = await leases.get("seat:ceo")
    assert lease is None or lease.owner == ""


async def test_the_epoch_is_exposed_for_fencing(leases: Any) -> None:
    """Every write made on a seat's behalf carries this. A write without
    it is a write a zombie can also make."""
    host = _host(leases, seats=lambda: ["ceo"])
    await host._renew_node_presence()
    await host.sweep()
    assert host.epoch_for("ceo") == 1
    assert host.epoch_for("nobody") is None


# ── mixed-version visibility ─────────────────────────────────────────


async def test_an_older_protocol_peer_is_reported_not_just_silent(
    leases: Any,
) -> None:
    """A node stalled by the upgrade gate must be distinguishable from
    one whose peers simply hold every seat — otherwise a rolling upgrade
    that has wedged looks exactly like a healthy full fleet."""
    old = _host(leases, "old-node", seats=lambda: ["ceo"], protocol=1)
    await old._renew_node_presence()
    await old.sweep()

    new = _host(leases, "new-node", seats=lambda: ["ceo", "eng"], protocol=2)
    result = await new.sweep()

    assert result.claimed == ()
    assert result.blocked is True
    assert result.blocked_by_protocol == 1


async def test_nothing_to_claim_is_not_reported_as_blocked(leases: Any) -> None:
    """The other half: peers holding everything is normal, not a stall."""
    a = _host(leases, "node-a", seats=lambda: ["ceo"])
    await a._renew_node_presence()
    await a.sweep()

    b = _host(leases, "node-b", seats=lambda: ["ceo"])
    await b._renew_node_presence()
    result = await b.sweep()

    assert result.claimed == ()
    assert result.blocked is False


# ── lifecycle ────────────────────────────────────────────────────────


async def test_start_claims_before_the_loops_spin(leases: Any) -> None:
    """Boot must not be eventually-consistent: the first sweep runs
    synchronously inside ``start()`` so a node is useful the moment it
    reports started."""
    host = _host(leases, seats=lambda: ["ceo"], sweep_seconds=99, heartbeat_seconds=99)
    await host.start()
    try:
        assert host.owns("ceo")
    finally:
        await host.stop()
    assert host.held_handles == ()


async def test_stop_releases_presence_so_peers_re_divide(leases: Any) -> None:
    a = _host(leases, "node-a", seats=lambda: ["ceo"], sweep_seconds=99)
    b = _host(leases, "node-b", seats=lambda: ["ceo"], sweep_seconds=99)
    await a.start()
    await b._renew_node_presence()
    assert (await b.sweep()).live_nodes == 2

    await a.stop()
    assert (await b.sweep()).live_nodes == 1


async def _expire(leases: Any, resource: str) -> None:
    """Lapse a lease without sleeping, on whichever backend is in play."""
    if isinstance(leases, MemoryLeaseStore):
        leases._rows[resource]["expires_at"] = datetime.now(UTC) - timedelta(seconds=1)
        return
    await leases._db.execute(
        "UPDATE leases SET expires_at = now() - interval '1 second' "
        "WHERE resource = $1",
        resource,
    )


def test_sweep_result_reports_blocked_only_when_a_floor_is_set() -> None:
    assert SweepResult(held=0, capacity=1, live_nodes=1).blocked is False
    assert (
        SweepResult(held=0, capacity=1, live_nodes=1, blocked_by_protocol=1).blocked
        is True
    )


async def test_an_unreachable_store_drops_the_seat_once_the_TTL_HAS_lapsed(
    leases: Any,
) -> None:
    """Keeping a seat through a blip is right; keeping it forever is not.

    ``LeaseError`` says nothing about ownership, so a short outage must
    not shed seats no peer could claim anyway. But the row's TTL runs out
    on wall-clock time whether or not this node can see it — past that
    the lease HAS lapsed and a peer may already be running the agent.
    Holding on from there is how one unreachable database becomes two
    nodes serving one seat.
    """
    released: list[str] = []

    async def _on_release(handle: str, lease: Lease) -> None:
        released.append(handle)

    host = _host(leases, seats=lambda: ["ceo"], on_release=_on_release, ttl_seconds=30)
    await host._renew_node_presence()
    await host.sweep()
    assert host.owns("ceo")

    async def _boom(*args: Any, **kwargs: Any) -> bool:
        raise LeaseError("database is down")

    host.leases = _Unavailable(host.leases, renew=_boom)

    # Inside the TTL: hold on.
    assert await host.heartbeat() == ()
    assert host.owns("ceo")

    # Past the TTL: the lease has lapsed regardless of what we can see.
    host._held["ceo"].renewed_at -= 31
    assert await host.heartbeat() == ("ceo",)
    assert not host.owns("ceo")
    assert released == ["ceo"]


async def test_a_successful_renew_refreshes_the_grace_window(leases: Any) -> None:
    """The deadline is measured from the last SUCCESSFUL renew, so a node
    that keeps renewing never accumulates its way into a false drop."""
    host = _host(leases, seats=lambda: ["ceo"], ttl_seconds=30)
    await host._renew_node_presence()
    await host.sweep()

    host._held["ceo"].renewed_at -= 29
    assert await host.heartbeat() == ()
    # The successful renew reset the clock.
    assert await host.heartbeat() == ()
    assert host.owns("ceo")


async def test_a_reclaim_between_heartbeat_and_release_is_not_torn_down(
    leases: Any,
) -> None:
    """The heartbeat/sweep race, pinned at the point it actually happens.

    Both loops are independent tasks and both hooks are long. The window
    is INSIDE the awaited renew: the heartbeat is carrying a lease object
    it read before the await, and a sweep can re-claim the same seat at a
    new epoch while it is suspended there. Tearing that down would leave
    the seat owned in the lease table and dead in this process, with
    nothing to notice.
    """
    host = _host(leases, seats=lambda: ["ceo"])
    await host._renew_node_presence()
    await host.sweep()
    stale = host._held["ceo"].lease
    holder = type(host._held["ceo"])

    async def _lost_but_reclaimed(*args: Any, **kwargs: Any) -> bool:
        # The sweep lands here — while the heartbeat is awaiting us.
        host._held["ceo"] = holder(
            lease=Lease(
                resource=stale.resource,
                owner=stale.owner,
                epoch=stale.epoch + 1,
                expires_at=stale.expires_at,
            ),
            handle="ceo",
            renewed_at=host._held["ceo"].renewed_at,
        )
        return False  # the OLD epoch is genuinely gone

    host.leases = _Unavailable(host.leases, renew=_lost_but_reclaimed)
    lost = await host.heartbeat()

    assert lost == (), "the heartbeat tore down a newer claim it did not own"
    assert host.owns("ceo")
    assert host.epoch_for("ceo") == stale.epoch + 1
