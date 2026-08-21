"""The exit criteria for seat ownership, gated.

One test per criterion, each named after the property rather than the
mechanism, because the mechanism is allowed to change and the property
is not:

* a seat handed off mid-conversation preserves order;
* a node that loses its lease starts no new turn even while its consumer
  is still attached;
* a completion for a seat reaches its owner and only its owner;
* an unclaimed seat's messages survive until someone claims it;
* and the same suite passes on the memory twin.

The last one is why every test here takes the ``fleet`` fixture instead
of building its own: the fixture is parametrized over both backends, so
each criterion runs twice and a twin that models the broker wrongly
fails here rather than certifying the bug in CI.
"""

from __future__ import annotations

import asyncio
from typing import Any

from crewlet.db.leases import LeaseError, seat_resource
from crewlet.events.types import SandboxRunCompleted
from crewlet.queue.topics import (
    agent_control_group,
    agent_control_topic,
    agent_inbox_group,
    agent_inbox_topic,
)
from crewlet.seat.host import ReleaseReason

from .conftest import Fleet, _Node, settle

# ── the fleet is a fleet ─────────────────────────────────────────────


async def test_two_nodes_split_the_company_and_each_serves_only_its_own(
    fleet: Fleet,
) -> None:
    """The precondition every other criterion rests on.

    Both halves matter. A node must hold a seat — otherwise nothing here
    is a fleet — and it must be attached to exactly the seats it holds,
    because attachment is what makes ownership mean anything: a node
    attached to a seat it does not own is the double-consumer state, and
    a node that owns one it is not attached to is a seat nobody serves.
    """
    await fleet.balance()

    held = {n.name: set(n.held) for n in fleet.nodes}
    assert held["node-a"] | held["node-b"] == set(fleet.handles)
    assert not held["node-a"] & held["node-b"]
    # ``ceil(3 / 2)`` — nobody over their share, nobody idle.
    assert sorted(len(v) for v in held.values()) == [1, 2]

    for node in fleet.nodes:
        for handle in fleet.handles:
            attached = node.attached_to(
                agent_inbox_topic(handle), agent_inbox_group(handle)
            )
            assert attached is (handle in node.held), (
                f"{node.name} attached={attached} held={handle in node.held}"
            )


# ── criterion: a handoff preserves order ─────────────────────────────


async def test_a_seat_handed_off_mid_conversation_preserves_order(
    fleet: Fleet,
) -> None:
    """Every trigger runs exactly once, in the order it was published,
    across the handoff.

    This is the criterion that killed republish-then-ack: a republished
    message goes to the topic tail while the successor replays its
    prefetched siblings from the head, so the conversation arrives
    shuffled. A plain close cannot do that — unacked messages return to
    the successor at ``redeliveryCount`` 0, in order — and this is where
    that shows.
    """
    await fleet.balance()
    handle, first, second = fleet.movable_seat()

    for marker in ("m1", "m2"):
        await fleet.publish(handle, marker)
    await settle(lambda: len(first.markers(handle)) == 2, what="the first two turns")

    # The handoff: voluntary, which is what a rebalance or a drain does.
    await first.engine._seat_host.release(handle, ReleaseReason.DRAIN)
    await second.sweep()
    assert handle in second.held

    for marker in ("m3", "m4", "m5"):
        await fleet.publish(handle, marker)
    await settle(
        lambda: len(second.markers(handle)) == 3, what="the tail after handoff"
    )

    assert first.markers(handle) == ["m1", "m2"]
    assert second.markers(handle) == ["m3", "m4", "m5"]
    # And the conversation, read as one stream, is intact: no gap, no
    # duplicate, no reordering across the seam.
    assert [m for _node, m in fleet.turns_for(handle)] == ["m1", "m2", "m3", "m4", "m5"]


async def test_work_in_flight_when_the_seat_moves_is_not_lost_and_not_doubled(
    fleet: Fleet,
) -> None:
    """The handoff that happens *during* a turn, not between two.

    A fenced release abandons in-flight work rather than finishing it —
    a peer may already be running the seat, so finishing would be the
    second copy. What must not happen is the trigger vanishing: it was
    never acked, so it belongs to the seat's next owner.
    """
    await fleet.balance()
    handle, first, second = fleet.movable_seat()

    entered = asyncio.Event()
    hold = asyncio.Event()
    inner = first.engine.turn_engine.run_turn
    # The REAL in-turn fence, the one a live turn consults before every
    # round and before every side-effecting tool. Calling it is what
    # makes this an abandoned turn rather than a completed one — a stub
    # that just returns would ack the trigger and there would be nothing
    # left to hand over.
    fence = first.engine.turn_engine._fence_for(handle)
    assert fence is not None, "a fleet node's turns must be fenced"

    async def _blocking(agent: Any, **kwargs: Any) -> str:
        entered.set()
        await hold.wait()
        fence()
        return await inner(agent, **kwargs)

    first.engine.turn_engine.run_turn = _blocking  # type: ignore[method-assign]

    # From a task, not inline: the memory twin dispatches inside
    # ``publish``, so awaiting it here would block on the very handler
    # the test is holding open.
    sent = asyncio.create_task(fleet.publish(handle, "in-flight"))
    await settle(entered.is_set, what="the first node to enter the turn")

    # The lease moves while that turn is still running.
    lease = first.engine._seat_host._held[handle].lease
    await fleet.leases.release(
        seat_resource(handle), owner=lease.owner, epoch=lease.epoch
    )
    await first.engine._seat_host.heartbeat()
    assert handle not in first.held
    hold.set()
    await sent

    await second.sweep()
    assert handle in second.held
    await settle(
        lambda: second.markers(handle) == ["in-flight"],
        what="the successor to pick the abandoned trigger up",
    )
    # Exactly once, and on the successor: the first node's turn stopped
    # at the fence rather than finishing beside a peer that had already
    # taken the seat.
    assert fleet.turns_for(handle) == [(second.name, "in-flight")]


# ── criterion: losing the lease stops new turns ──────────────────────


async def test_a_node_whose_teardown_failed_starts_no_turn_while_attached(
    fleet: Fleet,
) -> None:
    """The state the ownership branch exists for, reached honestly.

    A release whose detach cannot be proven keeps the lease and keeps
    the consumer — the node is attached to a seat it no longer serves.
    That is the only *sustained* attached-but-not-owning state the design
    produces, and in it the node must refuse every delivery rather than
    run a turn a peer may already be running.
    """
    await fleet.balance()
    handle, owner, _successor = fleet.movable_seat()

    async def _stuck(topic: str, group: str) -> bool:
        raise RuntimeError("broker will not answer")

    real_detach = owner.queue.detach
    owner.queue.detach = _stuck  # type: ignore[method-assign]
    try:
        assert await owner.engine._seat_host.release(handle) is False
        assert owner.engine._seat_host.unproven_handles == (handle,)
        assert handle not in owner.held
        assert owner.attached_to(
            agent_inbox_topic(handle), agent_inbox_group(handle)
        ), "the premise: still consuming a seat it no longer owns"

        agent = owner.engine.agent_pool.get_by_handle(handle)
        assert agent is not None
        from crewlet.events.types import TaskAssigned
        from crewlet.queue.protocol import DeferDelivery

        handler = owner.engine._make_agent_handler(agent)
        try:
            await handler([TaskAssigned(source="t", task_id="x", role="Alpha")])
        except DeferDelivery:
            pass
        else:  # pragma: no cover — the criterion
            raise AssertionError("an unowned seat's delivery was accepted")
        assert owner.markers(handle) == []
    finally:
        owner.queue.detach = real_detach  # type: ignore[method-assign]


async def test_a_store_blip_stops_new_turns_and_the_recovery_resumes_them(
    fleet: Fleet,
) -> None:
    """Freshness, not membership — and the resume that has to come with it.

    A membership check reads a snapshot refreshed on a heartbeat against
    a much longer TTL, so it can be a full TTL stale: precisely the
    window it exists to close. Admission therefore rests on proof — a
    successful renew — and the first failed one stops new turns
    immediately while the seat is KEPT, because the row is untouched by
    a blip and shedding on one would tear a healthy company down over a
    two-second outage.

    The second half is the one that is easy to forget and silent when
    missing: when the store comes back, the node still owns the seat and
    is still attached to it, so something has to start it reading again.
    Without that it is owned, attached, and deaf for the rest of its
    life.
    """
    await fleet.balance()
    handle, owner, _successor = fleet.movable_seat()
    host = owner.engine._seat_host
    topic, group = agent_inbox_topic(handle), agent_inbox_group(handle)

    class _Blip:
        """The lease store, unreachable — the real cause, not a poke at
        the freshness clock."""

        def __init__(self, inner: Any) -> None:
            self._inner = inner
            self.down = False

        async def renew(self, *a: Any, **k: Any) -> bool:
            if self.down:
                raise LeaseError("lease store unreachable")
            return await self._inner.renew(*a, **k)

        def __getattr__(self, name: str) -> Any:
            return getattr(self._inner, name)

    blip = _Blip(host.leases)
    host.leases = blip
    blip.down = True

    await host.heartbeat()
    assert handle in host.held_handles, "a blip keeps the seat; it is not lost"
    assert owner.attached_to(topic, group), "and keeps the consumer"

    await fleet.publish(handle, "during-the-blip")
    await asyncio.sleep(0.2)
    assert owner.markers(handle) == [], "no turn starts without proof of ownership"
    assert fleet.turns_for(handle) == [], "and no peer ran it either"

    # The store comes back.
    blip.down = False
    await host.heartbeat()
    assert host.may_start(handle) is not None

    await settle(
        lambda: owner.markers(handle) == ["during-the-blip"],
        what="the held trigger once ownership is provable again",
    )


# ── criterion: a completion reaches only the owner ───────────────────


async def test_a_completion_reaches_the_seats_owner_and_only_its_owner(
    fleet: Fleet,
) -> None:
    """Routing emerges from who subscribes, not from a computation.

    The control topic is per seat and attached alongside the inbox, so
    the node that owns the seat is the node that resumes its runs. Under
    the fleet-wide group this replaced, a completion landed on a
    non-owner ``(N-1)/N`` of the time — and a non-owner cannot resume a
    run whose suspended Execute conversation lives in another process.
    """
    await fleet.balance()
    handle, owner, other = fleet.movable_seat()

    topic, group = agent_control_topic(handle), agent_control_group(handle)
    assert owner.attached_to(topic, group)
    assert not other.attached_to(topic, group)

    await owner.queue.publish(
        topic,
        SandboxRunCompleted(
            source="waiter", agent_handle=handle, turn_id="t-1", sandbox_id="sbx-1"
        ),
    )
    await settle(lambda: owner.control == [(handle, "t-1")], what="the owner's control")
    assert other.control == []


async def test_a_completion_published_between_owners_waits_for_the_next_one(
    fleet: Fleet,
) -> None:
    """A detached run outlives the node that started it.

    The box is real and still working while the seat is between owners,
    so a completion published into that gap has to be held. Dropping it
    strands a finished job whose Execute loop is still suspended.
    """
    await fleet.balance()
    handle, first, second = fleet.movable_seat()

    await first.engine._seat_host.release(handle, ReleaseReason.DRAIN)
    topic, group = agent_control_topic(handle), agent_control_group(handle)
    assert not any(n.attached_to(topic, group) for n in fleet.nodes)

    await first.queue.publish(
        topic,
        SandboxRunCompleted(
            source="waiter", agent_handle=handle, turn_id="t-orphan", sandbox_id="sbx-9"
        ),
    )
    await asyncio.sleep(0.1)
    assert first.control == [] and second.control == []

    await second.sweep()
    await settle(
        lambda: second.control == [(handle, "t-orphan")],
        what="the completion to reach the seat's next owner",
    )
    assert first.control == []


# ── criterion: an unclaimed seat's mail survives ─────────────────────


async def test_an_unclaimed_seats_mail_waits_until_someone_claims_it(
    fleet: Fleet,
) -> None:
    """The invariant that makes owner-only attachment safe at all.

    Under a competing-consumer group the subscription is what retains
    messages, and it exists whether or not anyone is attached. Without
    the invariant the first publish to a seat nobody has ever subscribed
    is dropped on the floor: no dead letter, no producer error, nothing
    to alert on — and a lease gap, a claim ramp or a full fleet restart
    all produce exactly that window.
    """
    await fleet.balance()
    handle, first, second = fleet.movable_seat()

    await first.engine._seat_host.release(handle, ReleaseReason.DRAIN)
    assert not any(h == handle for n in fleet.nodes for h in n.held)

    for marker in ("q1", "q2", "q3"):
        await fleet.publish(handle, marker)
    await asyncio.sleep(0.1)
    assert fleet.turns_for(handle) == [], "nobody owns the seat; nobody may work it"

    await second.sweep()
    assert handle in second.held
    await settle(
        lambda: second.markers(handle) == ["q1", "q2", "q3"],
        what="the held mail to reach the seat's new owner, in order",
    )
    assert first.markers(handle) == []


async def test_a_full_fleet_restart_does_not_lose_a_seats_mail(
    fleet: Fleet,
) -> None:
    """The window is not hypothetical: it is every deploy.

    Both nodes down, mail arriving, then a node comes back. What the
    company must not do is start its day having silently lost whatever
    was said to it overnight.
    """
    await fleet.balance()
    handle = fleet.handles[0]

    for node in fleet.nodes:
        await node.engine._seat_host.release_all(ReleaseReason.DRAIN)
    assert not any(n.held for n in fleet.nodes)

    for marker in ("overnight-1", "overnight-2"):
        await fleet.publish(handle, marker)

    # The fleet comes back. Which node ends up with this seat is a
    # placement detail — the criterion is that SOMEBODY gets the mail,
    # in the order it arrived.
    await fleet.balance()
    owner = fleet.owner_of(handle)
    await settle(
        lambda: owner.markers(handle) == ["overnight-1", "overnight-2"],
        what="mail published while the whole fleet was dark",
    )
    assert [m for _node, m in fleet.turns_for(handle)] == [
        "overnight-1",
        "overnight-2",
    ]


# ── the two nodes never both run one seat ────────────────────────────


async def test_no_trigger_is_ever_worked_by_both_nodes(fleet: Fleet) -> None:
    """The property the whole phase exists for, stated as one assertion.

    Ownership churn while triggers keep arriving is the shape a rolling
    deploy has. Duplication is bounded, not zero — nothing can un-send
    an outbound effect — but the *engine* must never hand one trigger to
    two live seats, because they would each run a turn against the same
    conversation.
    """
    await fleet.balance()
    handle, _owner, _successor = fleet.movable_seat()

    for round_no in range(3):
        owner = fleet.owner_of(handle)
        successor = fleet.other_than(owner)
        await fleet.publish(handle, f"r{round_no}")
        await settle(
            lambda o=owner, r=round_no: f"r{r}" in o.markers(handle),
            what=f"round {round_no} to run on its owner",
        )
        await owner.engine._seat_host.release(handle, ReleaseReason.DRAIN)
        await successor.sweep()

    markers = [m for _node, m in fleet.turns_for(handle)]
    assert markers == ["r0", "r1", "r2"]
    assert len(markers) == len(set(markers)), "a trigger ran twice"


def test_the_criteria_run_on_both_backends(fleet: _Node | Fleet) -> None:
    """Meta, and load-bearing.

    "The same suite passes on the memory twin" is itself an exit
    criterion, so a fixture that silently degenerated to one backend
    would let every test above pass while gating half of what they
    claim. This asserts the parametrization is real.
    """
    assert isinstance(fleet, Fleet)
    assert len(fleet.nodes) == 2
    assert len({n.engine._incarnation for n in fleet.nodes}) == 2
    assert len({n.engine._node_id for n in fleet.nodes}) == 2
