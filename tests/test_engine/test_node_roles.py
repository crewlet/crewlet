"""``node.roles`` — subtracting a job from THIS node, not from the company.

Three roles, defaulting to all three, which is what one process running a
whole company has always been. Subtracting one has to actually subtract
it: a node without ``workers`` must not merely lose the duty race, it
must not enter it, or the operator's decision to run duties elsewhere
means nothing.

The failure mode this file exists for is the one no single node's config
is wrong for: a fleet assembled, node by node, into a shape where a whole
job is done by nobody. Every symptom is an absence — nothing fires,
nothing sweeps, no webhook arrives — so it is checked against live
presence and reported rather than left to be noticed in a month.
"""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).parent.parent))
from conftest import make_bootstrap, make_company, make_engine  # noqa: E402
from crewlet.config import NodeConfig  # noqa: E402
from crewlet.db.leases import MemoryLeaseStore, node_resource  # noqa: E402
from crewlet.queue.memory import MemoryEventQueue  # noqa: E402
from crewlet.seat.placement import NodeProfile, NodeRole  # noqa: E402


async def _engine(*, roles: list[str] | None = None, labels: dict | None = None, **kw):
    queue = MemoryEventQueue()
    await queue.start()
    node = NodeConfig(id="node-a")
    if roles is not None:
        node = NodeConfig(id="node-a", roles=roles, labels=labels or {})
    engine = await make_engine(
        bootstrap=make_bootstrap(node=node),
        company=make_company(name="Acme", roles=[{"name": "CEO"}, {"name": "Eng"}]),
        event_queue=queue,
        **kw,
    )
    await engine.start()
    return engine, queue


# ── the default ──────────────────────────────────────────────────────


async def test_a_node_that_says_nothing_does_everything() -> None:
    """Every deployment that exists today."""
    engine, queue = await _engine()
    try:
        assert engine.node_profile.roles == frozenset(NodeRole)
        assert engine.runs_role(NodeRole.WORKERS)
        assert await engine.claim_worker_duty("maintenance") is True
        assert set(engine._seat_host.held_handles) == {"ceo", "eng"}
    finally:
        await engine.stop()
        await queue.stop()


# ── workers ──────────────────────────────────────────────────────────


async def test_a_node_without_workers_never_claims_a_duty() -> None:
    """Not "loses the race" — does not enter it. Losing would still leave
    the duty to whichever node happened to sweep first, which is the
    behaviour the role exists to replace."""
    engine, queue = await _engine(roles=["seats", "ingress"])
    try:
        assert await engine.claim_worker_duty("maintenance") is False
        assert await engine.claim_worker_duty("scheduler") is False
        # And it left the lease alone, so a workers node can take it.
        assert await engine._seat_host.leases.get("worker:maintenance") is None
    finally:
        await engine.stop()
        await queue.stop()


async def test_a_workers_node_still_claims_normally() -> None:
    engine, queue = await _engine(roles=["workers"])
    try:
        assert await engine.claim_worker_duty("maintenance") is True
    finally:
        await engine.stop()
        await queue.stop()


# ── seats ────────────────────────────────────────────────────────────


async def test_a_node_without_seats_runs_no_agents() -> None:
    """It is still present, still reconciling config, still serving the
    API if it has ingress — it simply holds no chairs."""
    engine, queue = await _engine(roles=["ingress", "workers"])
    try:
        assert engine._seat_host.held_handles == ()
        assert engine.agent_pool.agents == []
        # Nothing attached to a seat inbox either.
        attached = {t for t, _g in queue.attachments()}
        assert not any(t.startswith("crewlet.agent.") for t in attached)
    finally:
        await engine.stop()
        await queue.stop()


async def test_a_seatless_node_is_not_in_the_seat_denominator() -> None:
    """Two live nodes, one of which will never claim: dividing by two
    leaves half the company unserved, and the sweep that did it looks
    perfectly healthy."""
    leases = MemoryLeaseStore()
    api, api_queue = await _engine(roles=["ingress"], lease_store=leases)
    try:
        # A peer that does run seats.
        await leases.try_acquire(
            node_resource("node-b"),
            owner="node-b:1",
            ttl_seconds=60,
            meta=NodeProfile(node_id="node-b").to_meta(),
        )
        plan, seat_nodes = await api._seat_host._plan(["ceo", "eng"])
        assert seat_nodes == 1
        assert plan.capacity == 0  # this node runs no seats
    finally:
        await api.stop()
        await api_queue.stop()


# ── labels reach the fleet ───────────────────────────────────────────


async def test_labels_are_advertised_on_the_presence_lease() -> None:
    """A peer decides eligibility from these, so they have to travel."""
    engine, queue = await _engine(roles=["seats"], labels={"zone": "eu"})
    try:
        lease = await engine._seat_host.leases.get(node_resource("node-a"))
        assert lease is not None
        profile = NodeProfile.from_meta("node-a", lease.meta)
        assert profile.labels == {"zone": "eu"}
        assert profile.roles == frozenset({NodeRole.SEATS})
    finally:
        await engine.stop()
        await queue.stop()


# ── the shape nobody's config is wrong for ───────────────────────────


async def test_a_fleet_with_nobody_on_workers_says_so(caplog: Any) -> None:
    """No node is misconfigured; the fleet is. Every symptom is an
    absence — nothing scheduled fires, no sandbox run is collected, no
    retention sweep runs — so nothing raises and nothing surfaces."""
    engine, queue = await _engine(roles=["seats", "ingress"])
    try:
        with caplog.at_level("WARNING"):
            await engine._seat_host.sweep()
        assert any("fleet_role_unmanned" in r.message for r in caplog.records)
        assert any("workers" in r.message for r in caplog.records)
    finally:
        await engine.stop()
        await queue.stop()


async def test_the_unmanned_warning_fires_on_the_edge_only(caplog: Any) -> None:
    """A fleet missing a role for an hour says so once, not 720 times."""
    engine, queue = await _engine(roles=["seats"])
    try:
        with caplog.at_level("WARNING"):
            await engine._seat_host.sweep()
        first = sum("fleet_role_unmanned" in r.message for r in caplog.records)
        assert first >= 1
        caplog.clear()
        with caplog.at_level("WARNING"):
            await engine._seat_host.sweep()
            await engine._seat_host.sweep()
        assert not any("fleet_role_unmanned" in r.message for r in caplog.records)
    finally:
        await engine.stop()
        await queue.stop()


# ── ingress ──────────────────────────────────────────────────────────


async def test_a_node_without_ingress_does_not_bind_the_api_port() -> None:
    """One Tier A file has to work for every shape in the fleet.

    `api.port` is set once and shipped everywhere — that is the point of
    a shared config — so the port must be gated on the ROLE rather than
    on being unset. A node without `ingress` that bound it anyway would
    answer webhooks the operator routed elsewhere and put a dashboard on
    an address nothing is meant to reach.

    The satellite guide tells operators to rely on exactly this, which
    is why it is asserted rather than assumed.
    """
    from crewlet.seat.placement import NodeRole

    bootstrap = make_bootstrap(node=NodeConfig(id="sat-eu", roles=["seats"]))
    bootstrap.api.port = 8099
    engine = await make_engine(
        bootstrap=bootstrap,
        company=make_company(name="Acme", roles=[{"name": "CEO"}]),
        event_queue=MemoryEventQueue(),
    )
    try:
        # Asserted BEFORE `start`, deliberately: `node_profile` used to
        # answer with every role until `start` resolved Tier A, so a
        # caller asking this question early was told a seats-only node
        # was an ingress node.
        assert engine.runs_role(NodeRole.INGRESS) is False
        await engine.start()
        assert engine.runs_role(NodeRole.INGRESS) is False
        # The gate `run()` applies, spelled out: a positive port is not
        # enough on its own.
        assert engine._api_port == 8099
        assert not (engine._api_port > 0 and engine.runs_role(NodeRole.INGRESS))
    finally:
        await engine.stop()


async def test_an_ingress_node_with_the_same_config_would_bind_it() -> None:
    """The other half — otherwise the test above passes on a port that
    is simply never bound by anybody."""
    from crewlet.seat.placement import NodeRole

    bootstrap = make_bootstrap(node=NodeConfig(id="core-1"))
    bootstrap.api.port = 8099
    engine = await make_engine(
        bootstrap=bootstrap,
        company=make_company(name="Acme", roles=[{"name": "CEO"}]),
        event_queue=MemoryEventQueue(),
    )
    try:
        await engine.start()
        assert engine.runs_role(NodeRole.INGRESS) is True
        assert engine._api_port > 0 and engine.runs_role(NodeRole.INGRESS)
    finally:
        await engine.stop()
