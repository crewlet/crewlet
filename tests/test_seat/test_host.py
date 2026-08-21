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

import asyncio
import os
from datetime import UTC, datetime, timedelta
from typing import Any

import pytest

from crewlet.db.leases import (
    Lease,
    LeaseError,
    MemoryLeaseStore,
    node_resource,
    seat_resource,
)
from crewlet.seat.host import (
    SEAT_RELEASE_LIMIT_PER_SWEEP,
    ReleaseReason,
    SeatHost,
    SeatReleaseError,
    SweepResult,
)
from crewlet.seat.placement import NodeProfile, SeatPlacement, parse_roles

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

    async def _on_release(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        released.append(handle)

    host = _host(leases, seats=lambda: ["ceo"], on_release=_on_release)
    await host._renew_node_presence()
    await host.sweep()
    assert host.may_start("ceo") is not None

    await _expire(leases, "seat:ceo")
    lost = await host.heartbeat()

    assert lost == ("ceo",)
    assert not (host.may_start("ceo") is not None)
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
    assert host.may_start("eng") is not None

    seats.remove("eng")
    await host.sweep()
    assert not (host.may_start("eng") is not None)
    assert host.may_start("ceo") is not None


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
    assert peer.may_start("ceo") is not None


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
    assert not (host.may_start("ceo") is not None)
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
        assert host.may_start("ceo") is not None
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

    async def _on_release(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        released.append(handle)

    host = _host(leases, seats=lambda: ["ceo"], on_release=_on_release, ttl_seconds=30)
    await host._renew_node_presence()
    await host.sweep()
    assert host.may_start("ceo") is not None

    async def _boom(*args: Any, **kwargs: Any) -> bool:
        raise LeaseError("database is down")

    host.leases = _Unavailable(host.leases, renew=_boom)

    # Inside the TTL: hold on.
    assert await host.heartbeat() == ()
    assert host.may_start("ceo") is not None

    # Past the TTL: the lease has lapsed regardless of what we can see.
    host._held["ceo"].renewed_at -= 31
    assert await host.heartbeat() == ("ceo",)
    assert not (host.may_start("ceo") is not None)
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
    assert host.may_start("ceo") is not None


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
    assert host.may_start("ceo") is not None
    assert host.epoch_for("ceo") == stale.epoch + 1


# ── admission: freshness, not membership ─────────────────────────────


async def test_may_start_returns_the_epoch_while_the_renew_is_fresh(
    leases: Any,
) -> None:
    host = _host(leases, seats=lambda: ["ceo"])
    await host._renew_node_presence()
    await host.sweep()

    epoch = host.may_start("ceo")
    assert epoch is not None and epoch >= 1
    assert host.may_start("nobody") is None


async def test_may_start_refuses_once_the_last_renew_is_stale(leases: Any) -> None:
    """The correction the design review forced.

    A membership check ("is this seat in ``_held``?") reads a snapshot
    refreshed on a 15 s heartbeat against a 45 s TTL, so it can be a full
    TTL stale — precisely the window it exists to close. Freshness can be
    proven: a renew at ``t`` bought exclusivity through ``t + ttl``.
    """
    host = _host(leases, seats=lambda: ["ceo"], heartbeat_seconds=5.0)
    await host._renew_node_presence()
    await host.sweep()
    assert host.may_start("ceo") is not None

    # Age the last successful renew past one heartbeat interval.
    held = host._held["ceo"]
    held.renewed_at -= 6.0

    assert host.may_start("ceo") is None
    # The seat is still HELD — in-flight work finishes, only new turns
    # are refused — and its epoch is still available for fencing writes
    # that belong to that work.
    assert host.held_handles == ("ceo",)
    assert host.epoch_for("ceo") is not None


async def test_a_failed_renew_stops_new_turns_before_the_seat_is_dropped(
    leases: Any,
) -> None:
    """The LeaseError grace keeps the seat so in-flight work can finish,
    but it must not keep admitting NEW work on faith."""
    host = _host(leases, seats=lambda: ["ceo"], ttl_seconds=30, heartbeat_seconds=5.0)
    await host._renew_node_presence()
    await host.sweep()

    async def _boom(*_a: Any, **_k: Any) -> bool:
        raise LeaseError("store unreachable")

    host.leases = _Unavailable(leases, renew=_boom)
    await host.heartbeat()  # LeaseError: seat kept, renewed_at NOT refreshed

    assert host.held_handles == ("ceo",)  # grace holds the seat
    host._held["ceo"].renewed_at -= 6.0  # one heartbeat interval passes
    assert host.may_start("ceo") is None  # but starts nothing new


# ── release reasons ──────────────────────────────────────────────────


async def test_release_reasons_separate_loss_from_drain(leases: Any) -> None:
    """Voluntary and fenced are opposites: one has time and exclusivity,
    the other has neither."""
    assert ReleaseReason.DRAIN.fenced is False
    assert ReleaseReason.ROLE_GONE.fenced is False
    assert ReleaseReason.LEASE_LOST.fenced is True
    assert ReleaseReason.ACQUIRE_FAILED.fenced is True
    assert ReleaseReason.POSTURE.fenced is True


async def test_the_hook_is_told_why_the_seat_is_going(leases: Any) -> None:
    seen: list[tuple[str, ReleaseReason]] = []

    async def _on_release(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        seen.append((handle, reason))

    host = _host(leases, seats=lambda: ["ceo"], on_release=_on_release)
    await host._renew_node_presence()
    await host.sweep()

    await host.release("ceo")
    assert seen == [("ceo", ReleaseReason.DRAIN)]

    seen.clear()
    await host.sweep()
    host.seats = lambda: []  # the role was decommissioned
    await host.sweep()
    assert seen == [("ceo", ReleaseReason.ROLE_GONE)]


async def test_a_lost_lease_releases_with_the_fenced_reason(leases: Any) -> None:
    seen: list[ReleaseReason] = []

    async def _on_release(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        seen.append(reason)

    host = _host(leases, seats=lambda: ["ceo"], on_release=_on_release)
    await host._renew_node_presence()
    await host.sweep()

    # A peer takes it: the row moves, so our renew returns False.
    await leases.release(seat_resource("ceo"), owner=host.owner, epoch=1)
    await leases.try_acquire(seat_resource("ceo"), owner="peer", ttl_seconds=30)
    await host.heartbeat()

    assert seen == [ReleaseReason.LEASE_LOST]


# ── fail-closed release ──────────────────────────────────────────────


async def test_an_unproven_teardown_keeps_the_lease(leases: Any) -> None:
    """The single failure ownership exists to prevent.

    Releasing a lease while still consuming the seat hands a peer
    permission to run the same agent concurrently. If teardown cannot be
    PROVEN, the lease is kept and renewed instead.
    """

    async def _wont_die(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        raise SeatReleaseError("the consumer would not close")

    host = _host(leases, seats=lambda: ["ceo"], on_release=_wont_die)
    await host._renew_node_presence()
    await host.sweep()
    epoch = host.epoch_for("ceo")

    assert await host.release("ceo") is False
    # Out of _held, so nothing new starts on it...
    assert host.held_handles == ()
    assert host.may_start("ceo") is None
    # ...but still leased, and still ours.
    assert host.unproven_handles == ("ceo",)
    lease = await leases.get(seat_resource("ceo"))
    assert lease is not None and lease.owner == host.owner and lease.epoch == epoch

    # A peer cannot take it.
    assert (
        await leases.try_acquire(seat_resource("ceo"), owner="peer", ttl_seconds=30)
        is None
    )


async def test_an_unproven_seat_keeps_being_renewed(leases: Any) -> None:
    async def _wont_die(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        raise SeatReleaseError("still consuming")

    host = _host(leases, seats=lambda: ["ceo"], on_release=_wont_die, ttl_seconds=30)
    await host._renew_node_presence()
    await host.sweep()
    await host.release("ceo")

    before = (await leases.get(seat_resource("ceo"))).expires_at
    await host.heartbeat()
    after = (await leases.get(seat_resource("ceo"))).expires_at
    assert after >= before, "an unproven seat stopped being renewed"


async def test_an_unproven_seat_is_not_re_claimed_or_double_counted(
    leases: Any,
) -> None:
    """It counts against capacity: this process may still be serving it,
    so taking on more work would over-subscribe a node in trouble."""

    async def _wont_die(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        raise SeatReleaseError("still consuming")

    host = _host(leases, seats=lambda: ["ceo", "eng"], on_release=_wont_die)
    await host._renew_node_presence()
    await host.sweep()
    await host.release("ceo")

    result = await host.sweep()
    assert "ceo" not in result.claimed
    assert host.unproven_handles == ("ceo",)


async def test_an_unproven_teardown_is_retried_and_recovers(leases: Any) -> None:
    """The reason undead is a state and not a grave.

    The causes of an unproven teardown are overwhelmingly transient — a
    consumer mid-delivery, an MCP child that has not finished dying — and
    without a retry the FIRST one stranded the seat for the life of the
    process: out of ``_held`` so this node never ran it, leased so no
    peer could claim it, counted against this node's capacity forever,
    and announced exactly once in a log line that then rotated away.
    """
    failures = [True]
    calls: list[ReleaseReason] = []

    async def _wont_die_once(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        calls.append(reason)
        if failures.pop() if failures else False:
            raise SeatReleaseError("the consumer would not close")

    host = _host(leases, seats=lambda: ["ceo"], on_release=_wont_die_once)
    await host._renew_node_presence()
    await host.sweep()
    await host.release("ceo", ReleaseReason.PLACEMENT)
    assert host.unproven_handles == ("ceo",)

    await host.heartbeat()

    assert host.unproven_handles == (), "an undead seat was never retried"
    assert calls == [ReleaseReason.PLACEMENT, ReleaseReason.PLACEMENT], (
        "the retry told the hook a different reason than the release did"
    )
    # And the lease is gone, so the fleet can run the seat again.
    assert await leases.get(seat_resource("ceo")) is None or (
        await leases.try_acquire(seat_resource("ceo"), owner="peer", ttl_seconds=30)
        is not None
    )


async def test_a_retry_that_fails_keeps_the_seat_and_ages(leases: Any) -> None:
    """The only safe answer is still to hold it — but visibly.

    ``unproven_ages`` is what an alert reads: existence is normal for a
    moment, duration never is.
    """

    async def _wont_die(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        raise SeatReleaseError("still consuming")

    host = _host(leases, seats=lambda: ["ceo"], on_release=_wont_die, ttl_seconds=30)
    await host._renew_node_presence()
    await host.sweep()
    await host.release("ceo")

    await host.heartbeat()
    await host.heartbeat()

    assert host.unproven_handles == ("ceo",)
    assert host._undead["ceo"].attempts == 3, "the retry did not run every heartbeat"
    assert set(host.unproven_ages()) == {"ceo"}
    assert host.unproven_ages()["ceo"] >= 0.0
    lease = await leases.get(seat_resource("ceo"))
    assert lease is not None and lease.owner == host.owner


async def test_a_recovered_seat_can_be_claimed_again_by_this_node(
    leases: Any,
) -> None:
    """Recovery has to return the seat to the FLEET, not just to the
    dictionary it was parked in — which means releasing the lease."""
    failures = [True]

    async def _wont_die_once(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        if failures:
            failures.pop()
            raise SeatReleaseError("the consumer would not close")

    host = _host(leases, seats=lambda: ["ceo"], on_release=_wont_die_once)
    await host._renew_node_presence()
    await host.sweep()
    await host.release("ceo")
    await host.heartbeat()

    result = await host.sweep()
    assert "ceo" in result.claimed
    assert host.held_handles == ("ceo",)


async def test_an_unexpected_release_failure_also_fails_closed(leases: Any) -> None:
    """A hook that raises something unexpected is no more proof of
    teardown than one that raises the documented error."""

    async def _explodes(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        raise RuntimeError("something else entirely")

    host = _host(leases, seats=lambda: ["ceo"], on_release=_explodes)
    await host._renew_node_presence()
    await host.sweep()

    assert await host.release("ceo") is False
    assert host.unproven_handles == ("ceo",)


# ── acquire backoff ──────────────────────────────────────────────────


async def test_a_failed_acquire_is_not_retried_every_sweep(leases: Any) -> None:
    """Retrying a config-shaped failure every 5 s spins at the cost of an
    MCP fork each time, and the seat is dark throughout either way."""
    attempts: list[str] = []

    async def _fails(handle: str, lease: Lease) -> None:
        attempts.append(handle)
        raise RuntimeError("bad mcp command")

    host = _host(leases, seats=lambda: ["ceo"], on_acquire=_fails)
    await host._renew_node_presence()
    await host.sweep()
    assert attempts == ["ceo"]
    assert host.held_handles == ()

    await host.sweep()
    await host.sweep()
    assert attempts == ["ceo"], "the seat was re-claimed while backed off"


async def test_the_backoff_expires(leases: Any) -> None:
    attempts: list[str] = []

    async def _fails(handle: str, lease: Lease) -> None:
        attempts.append(handle)
        raise RuntimeError("bad mcp command")

    host = _host(
        leases, seats=lambda: ["ceo"], on_acquire=_fails, acquire_backoff_seconds=0.01
    )
    await host._renew_node_presence()
    await host.sweep()
    await asyncio.sleep(0.02)
    await host.sweep()
    assert attempts == ["ceo", "ceo"]


# ── presence and the mixed-version gate ──────────────────────────────


async def test_presence_survives_an_older_protocol_peer(leases: Any) -> None:
    """The gate exists for the rolling upgrade; it must not make the
    upgrading node invisible during one.

    A node that cannot register presence is missing from
    ``list_live("node:")``, so its peers divide the seats by a count that
    excludes it and each take a larger share — and its own capacity
    excludes it too.
    """
    await leases.try_acquire(
        seat_resource("ceo"), owner="old-node:1", ttl_seconds=30, protocol=1
    )
    host = _host(leases, node="node-new", protocol=2)

    await host._renew_node_presence()

    live = await leases.list_live("node:")
    assert [lease.resource for lease in live] == [node_resource("node-new")]
    # Seat claims are still refused — that half of the gate stands.
    result = await host.sweep()
    assert result.claimed == () and result.blocked_by_protocol == 1


# ── drain drops presence ─────────────────────────────────────────────


async def test_draining_stops_counting_toward_the_fleet_size(leases: Any) -> None:
    """A draining node that keeps its presence lease stays in every
    peer's divisor, reserving capacity for a node that will never claim
    again — its seats stay dark for the whole drain plus the ramp."""
    host = _host(leases, seats=lambda: ["ceo"])
    await host.start()
    assert len(await leases.list_live("node:")) == 1

    await host.begin_drain()
    assert await leases.list_live("node:") == []
    # And it still holds what it had, so in-flight turns finish.
    assert host.held_handles == ("ceo",)
    await host.stop()


# ── bookkeeping ──────────────────────────────────────────────────────


async def test_sweep_reports_what_it_released(leases: Any) -> None:
    """``SweepResult.lost`` is documented "for logs, tests and /health"
    and was hardcoded empty, so every consumer of it saw nothing."""
    host = _host(leases, seats=lambda: ["ceo", "eng"])
    await host._renew_node_presence()
    await host.sweep()

    host.seats = lambda: ["eng"]
    result = await host.sweep()
    assert result.lost == ("ceo",)


async def test_heartbeat_losses_reach_the_last_sweep(leases: Any) -> None:
    host = _host(leases, seats=lambda: ["ceo"])
    await host._renew_node_presence()
    await host.sweep()

    await leases.release(seat_resource("ceo"), owner=host.owner, epoch=1)
    await leases.try_acquire(seat_resource("ceo"), owner="peer", ttl_seconds=30)
    assert await host.heartbeat() == ("ceo",)

    assert host.last_sweep is not None
    assert host.last_sweep.lost == ("ceo",)


async def test_seat_locks_do_not_accumulate_forever(leases: Any) -> None:
    """One lock per handle EVER seen, including every seat a live config
    apply removed — unbounded growth in a long-lived process."""
    host = _host(leases, seats=lambda: ["ceo", "eng", "ops"])
    await host._renew_node_presence()
    await host.sweep()
    assert set(host._seat_locks) == {"ceo", "eng", "ops"}

    host.seats = lambda: ["ceo"]
    await host.sweep()
    assert set(host._seat_locks) == {"ceo"}


async def test_a_store_blip_does_not_make_every_node_claim_everything(
    leases: Any,
) -> None:
    """Assuming a fleet of one on an unreadable node count turns every
    node into "I should own everything" simultaneously. Mutual exclusion
    still prevents double ownership, but the fleet degenerates to
    whoever sweeps first taking the lot."""
    seats = ["a", "b", "c", "d"]
    host = _host(leases, node="node-a", seats=lambda: seats)
    await host._renew_node_presence()
    # A second node is live.
    await leases.try_acquire(node_resource("node-b"), owner="node-b:1", ttl_seconds=30)
    plan, live = await host._plan(seats)
    assert (plan.capacity, live) == (2, 2)

    async def _boom(*_a: Any, **_k: Any) -> list[Any]:
        raise LeaseError("store unreachable")

    host.leases = _Unavailable(leases, list_live=_boom)
    plan, live = await host._plan(seats)
    assert (plan.capacity, live) == (2, 2), "a blip widened the share to everything"


# ── giving seats back: the other half of placement ───────────────────


async def test_a_node_that_booted_alone_makes_room_when_a_peer_joins(
    leases: Any,
) -> None:
    """Scaling out has to actually move work.

    Claiming alone converges only for a fleet that shrinks. A node that
    booted first holds every seat, and a peer joining later computes a
    share it can never reach — every seat it wants is held by a node with
    no reason to let go. Without the give-back, adding a node to a
    running company does nothing at all until something dies.
    """
    a = _host(leases, "node-a")
    await a._renew_node_presence()
    await a.sweep()
    assert set(a.held_handles) == {"ceo", "eng", "ops"}

    b = _host(leases, "node-b")
    await b._renew_node_presence()

    # a notices the fleet grew and hands back its excess.
    result = await a.sweep()
    assert result.capacity == 2
    assert len(a.held_handles) == 2
    assert len(result.lost) == 1

    # And the seat it gave up is claimable, so the fleet converges.
    await b.sweep()
    assert len(b.held_handles) == 1
    assert not set(a.held_handles) & set(b.held_handles)
    assert set(a.held_handles) | set(b.held_handles) == {"ceo", "eng", "ops"}


async def test_the_give_back_settles_instead_of_ping_ponging(leases: Any) -> None:
    """Convergence, and the ceiling is what guarantees it.

    Shares are ``ceil(seats / nodes)``, so they sum to at least the seat
    count and a node at its share has no room to re-claim what it just
    shed. Sweeping both nodes repeatedly must therefore reach a split and
    stay there — a rebalance that oscillates would respawn MCP children
    on every tick forever.
    """
    a = _host(leases, "node-a", seats=lambda: [f"s{i}" for i in range(5)])
    b = _host(leases, "node-b", seats=lambda: [f"s{i}" for i in range(5)])
    await a._renew_node_presence()
    await a.sweep()  # alone: takes all five
    await b._renew_node_presence()

    for _ in range(8):
        await a.sweep()
        await b.sweep()

    held_a, held_b = set(a.held_handles), set(b.held_handles)
    assert held_a | held_b == {f"s{i}" for i in range(5)}
    assert not held_a & held_b
    assert len(held_a) <= 3 and len(held_b) <= 3

    # Stable: another round moves nothing.
    before = (a.held_handles, b.held_handles)
    await a.sweep()
    await b.sweep()
    assert (a.held_handles, b.held_handles) == before


async def test_the_give_back_is_rate_limited_like_claiming(leases: Any) -> None:
    """A release is an MCP teardown here and a spawn on whoever takes it.

    Shedding eight seats in one tick would fork the whole company's
    subprocess trees twice inside a second, for a rebalance nothing is
    waiting on — no seat is dark while it is held by the wrong node.
    """
    a = _host(leases, "node-a", seats=lambda: [f"s{i}" for i in range(10)])
    await a._renew_node_presence()
    for _ in range(4):
        await a.sweep()  # alone, claim-limited: ends up holding all ten
    assert len(a.held_handles) == 10

    for node in ("node-b", "node-c", "node-d"):
        await _host(leases, node)._renew_node_presence()

    result = await a.sweep()  # capacity is now ceil(10/4) = 3
    assert result.capacity == 3
    assert len(result.lost) == SEAT_RELEASE_LIMIT_PER_SWEEP
    assert len(a.held_handles) == 10 - SEAT_RELEASE_LIMIT_PER_SWEEP


async def test_the_shed_order_is_stable(leases: Any) -> None:
    """A rebalance has to be reproducible.

    Deliberately NOT ordered by the ``preferred`` hint, the way claiming
    is: the hint records the last node to claim a seat, so for every seat
    this node holds it names this node. Ordering by it would look like
    stickiness and do nothing.
    """
    a = _host(leases, "node-a", seats=lambda: ["ceo", "eng", "ops"])
    await a._renew_node_presence()
    await a.sweep()
    assert set(a.held_handles) == {"ceo", "eng", "ops"}
    hints = [(await leases.get(seat_resource(h))).preferred for h in a.held_handles]
    assert hints == ["node-a"] * 3, (
        "the hint names the last claimer, so it cannot rank a node's own seats"
    )

    await _host(leases, "node-b")._renew_node_presence()
    assert (await a.sweep()).lost == ("ceo",)


async def test_a_shed_that_cannot_be_proven_is_not_reported_as_room_made(
    leases: Any,
) -> None:
    """An unproven teardown keeps the lease, so the seat did not move.

    Counting it as shed would tell the fleet a rebalance made room that
    no peer can actually take — and this node would then shed a second
    seat next sweep on top of one it is still serving.
    """

    async def _stuck(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        raise SeatReleaseError("consumer will not close")

    a = _host(leases, "node-a", on_release=_stuck)
    await a._renew_node_presence()
    await a.sweep()
    assert len(a.held_handles) == 3

    await _host(leases, "node-b")._renew_node_presence()
    result = await a.sweep()
    assert result.lost == ()
    assert len(a.unproven_handles) == 1


async def test_a_draining_node_does_not_shed_twice(leases: Any) -> None:
    """``release_all`` is already giving every seat back; a capacity shed
    on top of it only slows the drain down and logs a rebalance that is
    not happening."""
    a = _host(leases, "node-a")
    await a._renew_node_presence()
    await a.sweep()
    await _host(leases, "node-b")._renew_node_presence()

    await a.begin_drain()
    result = await a.sweep()
    assert result.lost == ()
    assert len(a.held_handles) == 3


# ── admission: the blip that keeps the seat ──────────────────────────


async def test_a_failed_renew_reports_admission_lost_and_the_recovery_reports_it_back(
    leases: Any,
) -> None:
    """The signal the consumer needs, in both directions.

    A blip freezes ``renewed_at``, so admission stops being provable
    within one heartbeat while the seat itself is correctly kept — the
    row is untouched, and shedding on a two-second outage would tear a
    healthy company down. The consumer has to stop reading, and then it
    has to start again: without the second edge the node comes back
    healthy, still owning the seat, and never serves it.
    """
    calls: list[tuple[str, bool]] = []

    async def _admission(handle: str, admitted: bool) -> None:
        calls.append((handle, admitted))

    host = _host(leases, seats=lambda: ["ceo"], on_admission=_admission)
    await host._renew_node_presence()
    await host.sweep()
    assert host.held_handles == ("ceo",)

    async def _boom(*_a: Any, **_k: Any) -> bool:
        raise LeaseError("store unreachable")

    host.leases = _Unavailable(leases, renew=_boom)
    assert await host.heartbeat() == ()
    assert host.held_handles == ("ceo",), "a blip keeps the seat"
    assert calls == [("ceo", False)]

    host.leases = leases
    await host.heartbeat()
    assert calls == [("ceo", False), ("ceo", True)]


async def test_admission_is_reported_on_the_edge_not_every_heartbeat(
    leases: Any,
) -> None:
    """An hour-long outage is one call, not two hundred and forty.

    The hook stops and starts a consumer; running it per tick would
    reattach-storm a seat for the whole outage.
    """
    calls: list[tuple[str, bool]] = []

    async def _admission(handle: str, admitted: bool) -> None:
        calls.append((handle, admitted))

    host = _host(leases, seats=lambda: ["ceo"], on_admission=_admission)
    await host._renew_node_presence()
    await host.sweep()

    async def _boom(*_a: Any, **_k: Any) -> bool:
        raise LeaseError("store unreachable")

    host.leases = _Unavailable(leases, renew=_boom)
    for _ in range(4):
        await host.heartbeat()
    assert calls == [("ceo", False)]

    host.leases = leases
    for _ in range(4):
        await host.heartbeat()
    assert calls == [("ceo", False), ("ceo", True)]


async def test_a_seat_that_leaves_forgets_it_was_unproven(leases: Any) -> None:
    """A stale flag would suppress the resume after a re-claim, leaving
    a seat this node legitimately owns attached and silent."""
    calls: list[tuple[str, bool]] = []

    async def _admission(handle: str, admitted: bool) -> None:
        calls.append((handle, admitted))

    host = _host(leases, seats=lambda: ["ceo"], on_admission=_admission)
    await host._renew_node_presence()
    await host.sweep()

    async def _boom(*_a: Any, **_k: Any) -> bool:
        raise LeaseError("store unreachable")

    host.leases = _Unavailable(leases, renew=_boom)
    await host.heartbeat()
    assert calls == [("ceo", False)]

    host.leases = leases
    await host.release("ceo")
    await host.sweep()  # re-claimed, fresh attachment
    assert host.held_handles == ("ceo",)
    assert calls == [("ceo", False)], "the release already reset the flag"

    host.leases = _Unavailable(leases, renew=_boom)
    await host.heartbeat()
    assert calls == [("ceo", False), ("ceo", False)]


async def test_an_admission_hook_failure_does_not_abort_the_heartbeat(
    leases: Any,
) -> None:
    """The heartbeat is what keeps every OTHER seat on this node alive.
    Letting one seat's gate take it down turns a queue hiccup into a
    node-wide lease lapse."""

    async def _boom_hook(handle: str, admitted: bool) -> None:
        raise RuntimeError("the broker will not answer")

    host = _host(leases, seats=lambda: ["ceo", "eng"], on_admission=_boom_hook)
    await host._renew_node_presence()
    await host.sweep()

    async def _boom(*_a: Any, **_k: Any) -> bool:
        raise LeaseError("store unreachable")

    host.leases = _Unavailable(leases, renew=_boom)
    assert await host.heartbeat() == ()
    assert set(host.held_handles) == {"ceo", "eng"}


# ── draining ─────────────────────────────────────────────────────────


async def test_seats_are_released_together_not_in_a_queue(leases: Any) -> None:
    """A drain costs one bounded wait, not one per seat.

    A voluntary release waits for that seat's in-flight turn. In
    sequence, a node holding a dozen seats pays that timeout twelve
    times, and the seat that went idle FIRST stays dark for the whole
    procession — unclaimable by any peer, for no reason.
    """
    holds: dict[str, asyncio.Event] = {}
    entered: list[str] = []

    async def _slow(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        entered.append(handle)
        await holds.setdefault(handle, asyncio.Event()).wait()

    host = _host(leases, seats=lambda: ["ceo", "eng", "ops"], on_release=_slow)
    await host._renew_node_presence()
    await host.sweep()
    assert len(host.held_handles) == 3

    task = asyncio.create_task(host.release_all())
    await asyncio.sleep(0)
    await asyncio.sleep(0)
    assert sorted(entered) == ["ceo", "eng", "ops"], (
        "releases ran in sequence; the last seat waited on the first"
    )

    for event in holds.values():
        event.set()
    await task
    assert host.held_handles == ()


async def test_one_stuck_seat_does_not_strand_the_others(leases: Any) -> None:
    """An unproven teardown keeps THAT seat's lease and says nothing
    about the rest — the whole point of failing closed per seat."""

    async def _stuck(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        if handle == "eng":
            raise SeatReleaseError("consumer will not close")

    host = _host(leases, seats=lambda: ["ceo", "eng", "ops"], on_release=_stuck)
    await host._renew_node_presence()
    await host.sweep()

    await host.release_all()

    assert host.held_handles == ()
    assert host.unproven_handles == ("eng",)


async def test_a_release_that_raises_is_reported_and_isolated(leases: Any) -> None:
    """Anything other than SeatReleaseError is a bug in the hook, not a
    verdict on the seat — but it must not take the drain down with it."""

    async def _boom(handle: str, lease: Lease, reason: ReleaseReason) -> None:
        if handle == "eng":
            raise RuntimeError("hook is broken")

    host = _host(leases, seats=lambda: ["ceo", "eng", "ops"], on_release=_boom)
    await host._renew_node_presence()
    await host.sweep()

    await host.release_all()
    assert "ceo" not in host.held_handles and "ops" not in host.held_handles


# ── placement ────────────────────────────────────────────────────────


def _placed(**by_handle: SeatPlacement) -> Any:
    return lambda handle: by_handle.get(handle, SeatPlacement())


async def test_a_node_never_claims_a_seat_pinned_elsewhere(leases: Any) -> None:
    """The whole point of a pin, and the reason the protocol version
    moved: a build that ignores placement claims it and succeeds, because
    the lease is only a mutex."""
    placement = _placed(eng=SeatPlacement(node="node-b"))
    a = _host(
        leases,
        "node-a",
        seats=lambda: ["ceo", "eng"],
        placement_of=placement,
        profile=NodeProfile(node_id="node-a"),
    )
    b = _host(
        leases,
        "node-b",
        seats=lambda: ["ceo", "eng"],
        placement_of=placement,
        profile=NodeProfile(node_id="node-b"),
    )
    await a._renew_node_presence()
    await b._renew_node_presence()

    await a.sweep()
    assert "eng" not in a.held_handles
    await b.sweep()
    assert "eng" in b.held_handles


async def test_a_label_selector_matches_the_nodes_own_labels(leases: Any) -> None:
    placement = _placed(gpu=SeatPlacement(labels={"gpu": "true"}))
    big = _host(
        leases,
        "big",
        seats=lambda: ["gpu"],
        placement_of=placement,
        profile=NodeProfile(node_id="big", labels={"gpu": "true"}),
    )
    small = _host(
        leases,
        "small",
        seats=lambda: ["gpu"],
        placement_of=placement,
        profile=NodeProfile(node_id="small", labels={"gpu": "false"}),
    )
    await big._renew_node_presence()
    await small._renew_node_presence()

    await small.sweep()
    assert small.held_handles == ()
    await big.sweep()
    assert big.held_handles == ("gpu",)


async def test_a_seat_nobody_matches_is_reported_and_left_alone(leases: Any) -> None:
    """Not widened. Widening the selector is precisely what the operator
    asked the engine not to do — so the seat goes unserved and the sweep
    says so, which is the only way anyone finds out."""
    host = _host(
        leases,
        "node-a",
        seats=lambda: ["ceo", "gpu"],
        placement_of=_placed(gpu=SeatPlacement(labels={"gpu": "true"})),
        profile=NodeProfile(node_id="node-a"),
    )
    await host._renew_node_presence()
    result = await host.sweep()
    assert result.unplaceable == ("gpu",)
    assert host.held_handles == ("ceo",)


async def test_a_seat_that_stops_matching_is_handed_back(leases: Any) -> None:
    """A live apply narrows the selector under a node that is holding
    the seat. Voluntary, so the in-flight turn finishes — but it must
    happen, or an eligible peer can never take it."""
    placements: dict[str, SeatPlacement] = {}
    released: list[tuple[str, str]] = []

    async def _on_release(handle: str, _lease: Any, reason: Any) -> None:
        released.append((handle, str(reason)))

    host = _host(
        leases,
        "node-a",
        seats=lambda: ["ceo"],
        placement_of=lambda h: placements.get(h, SeatPlacement()),
        profile=NodeProfile(node_id="node-a"),
        on_release=_on_release,
    )
    await host._renew_node_presence()
    await host.sweep()
    assert host.held_handles == ("ceo",)

    placements["ceo"] = SeatPlacement(node="node-b")
    result = await host.sweep()
    assert host.held_handles == ()
    assert released == [("ceo", "placement")]
    assert result.lost == ("ceo",)


async def test_an_ingress_only_node_claims_nothing_but_is_still_present(
    leases: Any,
) -> None:
    """It must be visible to the fleet (the dashboard lists it, and it
    holds worker duties if it has that role) while never appearing in
    the seat denominator."""
    api = _host(
        leases,
        "api",
        seats=lambda: ["ceo", "eng"],
        profile=NodeProfile(node_id="api", roles=parse_roles(["ingress"])),
    )
    await api._renew_node_presence()
    result = await api.sweep()
    assert api.held_handles == ()
    assert result.capacity == 0

    seat_node = _host(leases, "node-b", seats=lambda: ["ceo", "eng"])
    await seat_node._renew_node_presence()
    result = await seat_node.sweep()
    # Two live nodes, but only one runs seats — so the share is both
    # seats, not one. Counting the API node would strand the other.
    assert result.capacity == 2
    assert set(seat_node.held_handles) == {"ceo", "eng"}


async def test_the_profile_reaches_peers_on_the_presence_lease(leases: Any) -> None:
    host = _host(
        leases,
        "node-a",
        profile=NodeProfile(
            node_id="node-a", roles=parse_roles(["seats"]), labels={"zone": "eu"}
        ),
    )
    await host._renew_node_presence()
    lease = await leases.get(node_resource("node-a"))
    assert lease is not None
    assert NodeProfile.from_meta("node-a", lease.meta) == host.profile


async def test_a_deferred_delivery_is_resumed_by_the_next_healthy_renew(
    leases: Any,
) -> None:
    """A seat that defers on freshness must not go deaf forever.

    This needs no failure at all to reach, which is what makes it worth
    a test of its own. ``may_start`` refuses once the last renew is
    older than one heartbeat interval, and consecutive renews are one
    heartbeat apart PLUS the duration of the pass — so every cycle has
    a real window where a healthy, successfully-renewing seat answers
    ``None``. A batch landing in that window raises ``DeferDelivery``
    and the queue quiesces the attachment.

    Nothing resumed it. ``_note_admission`` is edge-triggered, the
    deferral never entered the set, so the next successful renew
    reported "still admitted", short-circuited, and never called the
    hook. The node kept the lease, kept renewing, stayed attached, and
    read nothing from that inbox again until the process restarted.
    """
    calls: list[tuple[str, bool]] = []

    async def _admission(handle: str, admitted: bool) -> None:
        calls.append((handle, admitted))

    host = _host(leases, seats=lambda: ["ceo"], on_admission=_admission)
    await host._renew_node_presence()
    await host.sweep()

    # The ordinary window: last renew is older than one heartbeat.
    host._held["ceo"].renewed_at -= host.heartbeat_seconds * 2
    assert host.may_start("ceo") is None, "the premise — a healthy seat refused"
    host.note_delivery_deferred("ceo")

    # A perfectly ordinary, SUCCESSFUL heartbeat.
    await host.heartbeat()
    assert host.held_handles == ("ceo",)
    assert host.may_start("ceo") is not None
    assert calls == [("ceo", True)], (
        "the deferred consumer was never resumed, so the seat is deaf "
        "on a healthy node for the life of the process"
    )


async def test_the_in_turn_fence_does_not_claim_a_consumer_stopped(
    leases: Any,
) -> None:
    """``may_start`` has two callers with opposite consequences.

    The delivery path defers and quiesces; the in-turn fence abandons a
    turn and stops no consumer. Marking the second would leave the set
    claiming a seat is quiesced while it is still happily consuming —
    and the next real store outage would then short-circuit the very
    call that stops it, because that transition is edge-triggered too.
    """
    calls: list[tuple[str, bool]] = []

    async def _admission(handle: str, admitted: bool) -> None:
        calls.append((handle, admitted))

    host = _host(leases, seats=lambda: ["ceo"], on_admission=_admission)
    await host._renew_node_presence()
    await host.sweep()

    host._held["ceo"].renewed_at -= host.heartbeat_seconds * 2
    assert host.may_start("ceo") is None  # the fence trips; no defer call
    await host.heartbeat()
    assert calls == [], "a fence trip must not fake a consumer stop"
    assert "ceo" not in host.unproven_handles


async def test_a_draining_node_stays_out_of_the_fleet_count(leases: Any) -> None:
    """`begin_drain` drops presence for a reason, and the heartbeat put
    it straight back.

    A draining node in the divisor makes peers reserve capacity for a
    node that will never claim again. On the posture-shed path — which
    can last minutes, not seconds — that is real stranded work: with 3
    nodes and 10 seats the healthy peers each compute `ceil(10/3) = 4`,
    and 2 of the drained node's seats are claimable by nobody until it
    comes back.
    """
    host = _host(leases, seats=lambda: ["ceo"])
    await host._renew_node_presence()
    await host.sweep()
    assert len(await leases.list_live("node:")) == 1

    await host.begin_drain()
    assert await leases.list_live("node:") == [], "begin_drain kept presence"

    # The heartbeat loop keeps ticking through a drain — that is what
    # keeps held seats alive while their turns finish.
    await host.heartbeat()
    assert await leases.list_live("node:") == [], (
        "the heartbeat re-announced a draining node, putting it back in "
        "every peer's capacity divisor"
    )


async def test_resuming_from_a_shed_announces_the_node_again(leases: Any) -> None:
    """The other half: a node that shed on divergence and then converged
    has to rejoin, or it sits out until it restarts."""
    host = _host(leases, seats=lambda: ["ceo"])
    await host._renew_node_presence()
    await host.sweep()
    await host.begin_drain()
    assert await leases.list_live("node:") == []

    await host.resume_claiming()
    assert len(await leases.list_live("node:")) == 1


async def test_a_seat_whose_acquire_failed_is_not_reported_as_claimed(
    leases: Any,
) -> None:
    """`claimed` is the pass's ledger, and it counted seats nothing runs.

    The acquire hook gives a failed takeover straight back — a bad MCP
    command, a credential resolving to nothing — so the seat is released
    before the sweep returns. Appending before the hook meant it still
    burned a claim slot, still logged as claimed, and still made
    `claimed` non-empty, which suppresses the protocol-block probe: a
    stalled mixed-version upgrade then reported `blocked_by_protocol=None`
    beside a claim that never happened.
    """

    async def _boom(handle: str, lease: Any) -> None:
        raise RuntimeError("no such MCP command")

    host = _host(leases, seats=lambda: ["ceo"], on_acquire=_boom)
    await host._renew_node_presence()
    result = await host.sweep()

    assert host.held_handles == (), "the failed seat was given back"
    assert result.claimed == (), (
        f"reported {result.claimed} as claimed, but nothing runs those seats"
    )
