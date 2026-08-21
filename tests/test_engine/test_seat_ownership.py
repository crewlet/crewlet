"""Seat ownership: what a node establishes, and what it hands back.

The property under test throughout is that **a seat is not a thing you
can half-own**. Establishing one means its instance, its budget cap, its
per-role MCP children and its interrupted sandbox run are all in place
BEFORE its inbox consumer attaches; giving it up means the consumer is
gone before anything else is torn down, and the lease is not released
until that is proven.

These run on the memory-backed placement path, which is the degenerate
case of the fleet path rather than a separate one — the same
``_acquire_seat`` / ``_release_seat`` hooks, the same ordering.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parent.parent))
from conftest import make_company, make_engine  # noqa: E402
from crewlet.queue.memory import MemoryEventQueue  # noqa: E402
from crewlet.queue.topics import (  # noqa: E402
    agent_inbox_group,
    agent_inbox_topic,
)
from crewlet.seat.host import ReleaseReason, SeatReleaseError  # noqa: E402


async def _engine(roles: list[dict] | None = None):
    queue = MemoryEventQueue()
    await queue.start()
    engine = await make_engine(
        company=make_company(
            name="Acme", roles=roles or [{"name": "CEO"}, {"name": "Eng"}]
        ),
        event_queue=queue,
    )
    await engine.start()
    return engine, queue


# ── establishing a seat ──────────────────────────────────────────────


async def test_a_started_node_owns_and_attaches_its_seats() -> None:
    engine, queue = await _engine()
    try:
        handles = {r.get_handle() for r in engine.org.all_roles()}
        assert set(engine._seat_host.held_handles) == handles
        attached = {t for t, _g in queue.attachments()}
        for handle in handles:
            assert agent_inbox_topic(handle) in attached
        # And the pool holds exactly the seats this node owns.
        assert {a.handle for a in engine.agent_pool.agents} == handles
    finally:
        await engine.stop()
        await queue.stop()


async def test_every_seats_subscription_exists_even_unowned() -> None:
    """The invariant an unowned seat rests on.

    Without it, the first publish to a never-subscribed seat is dropped
    on the floor: no dead letter, no producer error, nothing to alert
    on. It has to hold for seats this node does not run, which is why it
    is established once for the whole company rather than per owner.
    """
    engine, queue = await _engine()
    try:
        handle = "eng"
        # Simulate the seat moving elsewhere: this node lets it go, but
        # the subscription — and the mail — must survive.
        await engine._seat_host.release(handle)
        assert (agent_inbox_topic(handle), agent_inbox_group(handle)) not in (
            queue.attachments()
        )

        from crewlet.events.types import Event

        await queue.publish(agent_inbox_topic(handle), Event(type="held"))
        assert [
            e.type
            for e in queue.backlog(agent_inbox_topic(handle), agent_inbox_group(handle))
        ] == ["held"]
    finally:
        await engine.stop()
        await queue.stop()


async def test_the_consumer_attaches_LAST() -> None:
    """The ordering the whole hook exists for.

    A seat that starts receiving work before its per-role MCP children
    are up runs its first turn with an empty tool surface. The live-add
    path used to do exactly that — subscribe, then spawn MCP — while
    boot did the reverse.
    """
    engine, queue = await _engine(roles=[{"name": "CEO"}])
    try:
        order: list[str] = []

        async def _respawn(role):
            order.append("mcp")

        async def _subscribe(agent, epoch=0):
            order.append("subscribe")

        engine._respawn_role_mcp = _respawn  # type: ignore[method-assign]
        engine._subscribe_agent_inbox = _subscribe  # type: ignore[method-assign]

        lease = engine._seat_host._held["ceo"].lease
        await engine.agent_pool.terminate(engine.agent_pool.agents[0])
        await engine._acquire_seat("ceo", lease)

        assert order == ["mcp", "subscribe"]
    finally:
        await engine.stop()
        await queue.stop()


async def test_a_failed_acquire_does_not_leave_the_seat_claimed() -> None:
    """A seat that looks owned to the fleet while nothing runs it is
    worse than an unowned one: no peer will try."""
    engine, queue = await _engine(roles=[{"name": "CEO"}])
    try:
        await engine._seat_host.release_all()

        async def _boom(role):
            raise RuntimeError("mcp command not found")

        engine._respawn_role_mcp = _boom  # type: ignore[method-assign]
        engine._tier_b_done = True

        await engine._seat_host.sweep()
        assert engine._seat_host.held_handles == ()
    finally:
        await engine.stop()
        await queue.stop()


# ── giving a seat back ───────────────────────────────────────────────


async def test_releasing_a_seat_detaches_it_and_terminates_the_instance() -> None:
    engine, queue = await _engine()
    try:
        await engine._seat_host.release("eng", ReleaseReason.DRAIN)

        assert "eng" not in engine._seat_host.held_handles
        assert "eng" not in engine._subscribed_inboxes
        assert (agent_inbox_topic("eng"), agent_inbox_group("eng")) not in (
            queue.attachments()
        )
        assert engine.agent_pool.get_by_handle("eng") is None
    finally:
        await engine.stop()
        await queue.stop()


async def test_a_detach_that_cannot_be_proven_keeps_the_lease() -> None:
    """Releasing a lease while still consuming the seat hands a peer
    permission to run the same agent concurrently. A lease held too long
    costs latency; one released too early costs correctness."""
    engine, queue = await _engine(roles=[{"name": "CEO"}])
    try:

        async def _stuck(topic: str, group: str) -> bool:
            raise RuntimeError("broker will not answer")

        queue.detach = _stuck  # type: ignore[method-assign]

        assert await engine._seat_host.release("ceo") is False
        assert engine._seat_host.unproven_handles == ("ceo",)
        # Still attached, and the bookkeeping says so.
        assert "ceo" in engine._subscribed_inboxes
    finally:
        del queue.detach
        await engine.stop()
        await queue.stop()


async def test_the_release_hook_raises_the_error_the_host_understands() -> None:
    engine, queue = await _engine(roles=[{"name": "CEO"}])
    try:

        async def _stuck(topic: str, group: str) -> bool:
            raise RuntimeError("broker will not answer")

        queue.detach = _stuck  # type: ignore[method-assign]
        lease = engine._seat_host._held["ceo"].lease

        with pytest.raises(SeatReleaseError):
            await engine._release_seat("ceo", lease, ReleaseReason.DRAIN)
    finally:
        del queue.detach
        await engine.stop()
        await queue.stop()


# ── admission ────────────────────────────────────────────────────────


async def test_a_delivery_for_an_unowned_seat_is_deferred() -> None:
    from crewlet.events.types import TaskAssigned
    from crewlet.queue.protocol import DeferDelivery

    engine, queue = await _engine()
    try:
        agent = engine.agent_pool.get_by_handle("eng")
        assert agent is not None
        handler = engine._make_agent_handler(agent)

        # Simulate the lease moving away while the consumer is still
        # attached — the window the ownership branch exists to close.
        engine._seat_host._held.pop("eng")

        with pytest.raises(DeferDelivery):
            await handler([TaskAssigned(source="t", task_id="t1", role="Eng")])
    finally:
        await engine.stop()
        await queue.stop()


async def test_a_stale_renew_stops_new_turns_while_the_seat_is_still_held() -> None:
    """Freshness, not membership: a renew at ``t`` proves exclusivity
    through ``t + ttl``, and nothing else does."""
    from crewlet.events.types import TaskAssigned
    from crewlet.queue.protocol import DeferDelivery

    engine, queue = await _engine()
    try:
        agent = engine.agent_pool.get_by_handle("eng")
        assert agent is not None
        handler = engine._make_agent_handler(agent)

        held = engine._seat_host._held["eng"]
        held.renewed_at -= engine._seat_host.heartbeat_seconds + 1

        with pytest.raises(DeferDelivery):
            await handler([TaskAssigned(source="t", task_id="t1", role="Eng")])
        # The seat is STILL held — in-flight work finishes, only new
        # turns are refused.
        assert "eng" in engine._seat_host.held_handles
    finally:
        await engine.stop()
        await queue.stop()


# ── drain ordering ───────────────────────────────────────────────────


async def test_stop_releases_seats_before_pausing_delivery() -> None:
    """Order matters more than it looks.

    ``pause_delivery`` is one-way and node-wide: past it this node serves
    nothing, while its heartbeat keeps renewing every lease it holds. The
    seats are then blackholed for the whole drain — unservable here,
    unclaimable anywhere else.
    """
    engine, queue = await _engine()
    order: list[str] = []

    original_pause = queue.pause_delivery
    original_release = engine._seat_host.release_all

    async def _pause() -> None:
        order.append("pause_delivery")
        await original_pause()

    async def _release(reason=ReleaseReason.DRAIN) -> None:
        order.append("release_seats")
        await original_release(reason)

    queue.pause_delivery = _pause  # type: ignore[method-assign]
    engine._seat_host.release_all = _release  # type: ignore[method-assign]

    await engine.stop()
    await queue.stop()

    # ``SeatHost.stop`` releases again on its way down; what matters is
    # that nothing was paused before the first release.
    assert order[0] == "release_seats"
    assert "pause_delivery" in order
    assert order.index("release_seats") < order.index("pause_delivery")


async def test_a_decommissioned_role_loses_its_subscription() -> None:
    """The destructive half. A retired seat's inbox must not accumulate
    undeliverable events forever — and deleting it must not depend on
    which node happened to run it."""
    engine, queue = await _engine()
    try:
        topic, group = agent_inbox_topic("eng"), agent_inbox_group("eng")
        assert queue.backlog(topic, group) == []

        await engine.apply_config(make_company(name="Acme", roles=[{"name": "CEO"}]))

        assert "eng" not in engine._seat_host.held_handles
        # Gone entirely: a publish now retains nothing.
        from crewlet.events.types import Event

        await queue.publish(topic, Event(type="orphan"))
        assert queue.backlog(topic, group) == []
    finally:
        await engine.stop()
        await queue.stop()


# ── singleton duties ─────────────────────────────────────────────────


async def test_every_company_wide_worker_is_gated_on_a_duty() -> None:
    """The gap this closes.

    Work that belongs to the COMPANY rather than to a seat is not merely
    wasteful on every node — it races. N reapers expire the same paused
    box, N clustering passes write N sets of near-identical auto-drafted
    skills, N sweeps delete the same rows. Each of these claims a
    ``worker:{duty}`` lease per tick instead.

    Asserted as a set so a NEW company-wide loop cannot be added without
    someone deciding, in this test, whether it is one of these.
    """
    engine, queue = await _engine()
    try:
        claimed: list[str] = []
        original = engine.claim_worker_duty

        async def _record(duty: str, ttl_seconds: float = 45.0) -> bool:
            claimed.append(duty)
            return await original(duty, ttl_seconds)

        engine.claim_worker_duty = _record  # type: ignore[method-assign]

        # Every gate the engine wires, driven directly: the loops behind
        # them tick on intervals measured in minutes.
        await engine._ensure_seat_subscriptions()
        engine._start_maintenance_worker()
        for worker in (
            engine._maintenance_worker,
            engine._sandbox_waiter,
            engine._scheduler,
            engine._skill_curator_worker,
        ):
            if worker is None:
                continue
            gate = getattr(worker, "_may_tick", None) or worker._holds_duty
            await gate()

        assert "seat-subscriptions" in claimed
    finally:
        await engine.stop()
        await queue.stop()


async def test_a_duty_is_held_by_exactly_one_node() -> None:
    """The property the lease buys. Two engines on one placement table
    must not both believe they hold the same duty at the same time."""
    from crewlet.db.leases import MemoryLeaseStore

    leases = MemoryLeaseStore()
    engine, queue = await _engine()
    peer, peer_queue = await _engine()
    try:
        engine._seat_host.leases = leases
        peer._seat_host.leases = leases

        first = await engine.claim_worker_duty("maintenance")
        second = await peer.claim_worker_duty("maintenance")
        assert (first, second) == (True, False)
    finally:
        await peer.stop()
        await peer_queue.stop()
        await engine.stop()
        await queue.stop()


# ── coalescing must not bypass the sandbox resume ────────────────────


async def test_a_coalesced_partition_still_checks_for_a_parked_answer() -> None:
    """A digest is dispatched like any other trigger.

    The clarification check lives in ``_dispatch_inbox_event``, and the
    coalesced path used to call ``_handle_notification`` directly — so a
    parked run whose answer arrived ALONGSIDE a follow-up message never
    resumed. That is not an exotic interleaving: a threaded reply plus
    one more line is exactly what a human answering a question looks
    like, and the cost is a real sandbox sitting paused until the reaper
    kills it, with the human's answer filed as unrelated chatter.
    """
    from crewlet.events.types import ExternalNotification

    engine, queue = await _engine(roles=[{"name": "CEO"}])
    try:
        asked: list[str] = []

        class _Coordinator:
            async def try_resume_from_answer(self, agent, event) -> bool:
                asked.append(event.type)
                return True  # this WAS the answer; nothing else should run

        engine._sandbox_coordinator = _Coordinator()

        # Non-None so the handler passes its no-turn-engine park. Never
        # called: the resume claims the digest first, and teardown puts
        # it back before ``stop`` walks the real shutdown path.
        engine.turn_engine = object()  # type: ignore[assignment]
        handled: list[object] = []
        engine._handle_notification = lambda a, e: handled.append(e)  # type: ignore[assignment]

        agent = engine.agent_pool.get_by_handle("ceo")
        assert agent is not None
        conversation = {"conversation": "slack:C1:9"}
        await engine._make_agent_handler(agent)(
            [
                ExternalNotification(
                    source="slack",
                    notification_source="slack",
                    body="yes, use the staging bucket",
                    metadata=conversation,
                ),
                ExternalNotification(
                    source="slack",
                    notification_source="slack",
                    body="...and squash the commits",
                    metadata=conversation,
                ),
            ]
        )

        assert asked, "the coalesced digest never reached the resume check"
        assert handled == [], "the answer was handled as an unrelated message"
    finally:
        engine.turn_engine = None
        await engine.stop()
        await queue.stop()


async def test_taking_a_duty_does_not_block_this_node_from_claiming_seats() -> None:
    """A duty lease carries THIS build's protocol, not the store default.

    The mixed-version gate refuses to claim any seat while a live lease
    is held at a lower protocol — that is what makes a rolling upgrade
    converge. A duty lease written at the default protocol is such a
    lease, so a node that took a duty could not then claim a single
    seat, on a fleet where it is the only node. Invisible while
    ``PROTOCOL_VERSION`` was 1, and immediate the moment it moved.
    """
    from crewlet.db.leases import PROTOCOL_VERSION, worker_resource

    engine, queue = await _engine()
    try:
        assert await engine.claim_worker_duty("maintenance") is True
        lease = await engine._seat_host.leases.get(worker_resource("maintenance"))
        assert lease is not None
        assert lease.protocol == PROTOCOL_VERSION

        # And the gate it feeds still lets this node claim.
        await engine._seat_host.release_all()
        await engine._seat_host.sweep()
        assert engine._seat_host.held_handles, (
            "a duty lease blocked this node from claiming its own seats"
        )
    finally:
        await engine.stop()
        await queue.stop()
