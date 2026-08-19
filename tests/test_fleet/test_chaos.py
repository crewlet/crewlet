"""Chaos: what the fleet does when a node stops existing.

The exit-criteria suite next door asks whether the fleet is *correct*
when seats move deliberately — a release, a rebalance, a drain. This one
asks whether it survives a node that does not get to say goodbye.

That is a different question, and only this shape of test can answer it.
A graceful stop drains, releases every lease and detaches every consumer;
it is the one shutdown path that cannot exercise takeover, because
nothing is left for a peer to take over *from*. A killed process leaves
leases held-and-unrenewed, deliveries fetched-and-unacked, and consumers
that simply stop reading — and every guarantee the phase makes is a
statement about that state.

Three properties, each with a failure mode that is silent in production:

- **Nothing is lost.** A trigger the dead node was working is not gone;
  it belongs to whoever takes the seat next. Losing it looks exactly
  like an agent choosing not to reply.
- **Takeover is bounded by the lease TTL, not by luck.** No peer may
  touch the seat before it lapses, and every peer may after.
- **A detached sandbox run outlives its node.** The box is real and
  still working; its completion has to reach the seat's new owner with
  the suspended conversation intact.

Runs on both backends, like everything in this directory. The lease TTL
is shortened to make the takeover window waitable — that is the only
concession, and it is a knob the production code already exposes.
"""

from __future__ import annotations

import asyncio
from typing import Any

from crewlet.db.leases import seat_resource
from crewlet.events.types import SandboxRunCompleted
from crewlet.queue.topics import (
    agent_control_group,
    agent_control_topic,
    agent_inbox_group,
    agent_inbox_topic,
)

from .conftest import Fleet, settle

# Short enough to wait out, long enough that a slow CI box does not lapse
# a lease the test still expects to be live.
_TTL = 1.0


async def _shorten(fleet: Fleet) -> None:
    """Bring the lease TTL down to something a test can outlive.

    Then HEARTBEAT, which is the half that is easy to miss: the seats
    were already claimed at the shipped 45-second TTL, and a lease's
    expiry is stamped when it is granted. Lowering the field alone
    changes what the NEXT acquire asks for and nothing about the leases
    in hand — so the takeover window would still be 45 seconds long and
    every test here would time out waiting for it.

    Everything else stays at the shipped constants. The heartbeat
    interval drops in step so a live node still renews well inside the
    TTL — the standard one-third ratio, for the standard reason.
    """
    for node in fleet.nodes:
        host = node.engine._seat_host
        host.ttl_seconds = _TTL
        host.heartbeat_seconds = _TTL / 3
        await host.heartbeat()


# ── the trigger a dead node was holding ──────────────────────────────


async def test_a_trigger_the_dead_node_was_working_is_not_lost(
    fleet: Fleet,
) -> None:
    """The criterion, stated as the failure it prevents.

    The owner dies one round into a turn. Its delivery was never acked,
    so it belongs to the seat's next owner — and if it did not, the
    outcome in production is a human's message that nobody ever answers
    and nothing anywhere records as dropped.
    """
    await fleet.balance()
    await _shorten(fleet)
    handle, victim, survivor = fleet.movable_seat()

    entered = asyncio.Event()
    hold = asyncio.Event()

    async def _blocking(agent: Any, **kwargs: Any) -> str:
        entered.set()
        await hold.wait()
        return "never gets here"

    victim.engine.turn_engine.run_turn = _blocking  # type: ignore[method-assign]

    sent = asyncio.create_task(fleet.publish(handle, "mid-turn"))
    await settle(entered.is_set, what="the owner to start the turn")

    await victim.kill(in_flight=[sent])
    assert victim.markers(handle) == [], "the killed turn never completed"

    # Nobody may take the seat until the lease lapses.
    assert await survivor.sweep() is not None
    assert handle not in survivor.held, "a peer took a seat still leased"

    await asyncio.sleep(_TTL * 1.2)
    await survivor.sweep()
    assert handle in survivor.held, "the lapsed seat was never taken over"

    await settle(
        lambda: survivor.markers(handle) == ["mid-turn"],
        what="the successor to run the abandoned trigger",
    )
    assert [m for _n, m in fleet.turns_for(handle)] == ["mid-turn"]


async def test_a_dead_nodes_seat_is_untouchable_until_its_lease_lapses(
    fleet: Fleet,
) -> None:
    """Takeover latency is bounded by the TTL, in BOTH directions.

    Too eager and two nodes run one agent while the first is merely
    slow; too lazy and the seat is dark for longer than the lease says
    it should be. Sweeping repeatedly inside the window must claim
    nothing at all.
    """
    await fleet.balance()
    await _shorten(fleet)
    handle, victim, survivor = fleet.movable_seat()
    await victim.kill()

    for _ in range(3):
        await survivor.sweep()
        assert handle not in survivor.held
        await asyncio.sleep(_TTL / 5)

    await asyncio.sleep(_TTL)
    await survivor.sweep()
    assert handle in survivor.held


async def test_the_successor_attaches_everything_the_seat_needs(
    fleet: Fleet,
) -> None:
    """Taking a seat over is not just taking its lease.

    A node that holds the lease but never attached the inbox is a seat
    nobody serves — the same outage as no owner at all, and harder to
    see, because placement reports it as owned.
    """
    await fleet.balance()
    await _shorten(fleet)
    handle, victim, survivor = fleet.movable_seat()
    await victim.kill()
    await asyncio.sleep(_TTL * 1.2)
    await survivor.sweep()

    assert handle in survivor.held
    assert survivor.attached_to(agent_inbox_topic(handle), agent_inbox_group(handle))
    assert survivor.attached_to(
        agent_control_topic(handle), agent_control_group(handle)
    )
    assert survivor.engine.agent_pool.get_by_handle(handle) is not None


async def test_the_successor_claims_at_a_higher_epoch(fleet: Fleet) -> None:
    """The fencing token has to move, or the dead node's late writes are
    indistinguishable from its successor's.

    A lease released by lapsing is re-acquired, not renewed, and
    re-acquiring bumps the epoch — that is what fences the gap in which
    the previous owner's work was unprotected.
    """
    await fleet.balance()
    await _shorten(fleet)
    handle, victim, survivor = fleet.movable_seat()
    before = victim.engine._seat_host.epoch_for(handle)
    assert before is not None

    await victim.kill()
    await asyncio.sleep(_TTL * 1.2)
    await survivor.sweep()

    after = survivor.engine._seat_host.epoch_for(handle)
    assert after is not None and after > before


# ── mail that arrives while nobody is alive to take it ───────────────


async def test_mail_arriving_while_the_owner_is_dead_waits_for_the_successor(
    fleet: Fleet,
) -> None:
    """The window between a node dying and its lease lapsing is the one
    an operator never sees and every deploy produces."""
    await fleet.balance()
    await _shorten(fleet)
    handle, victim, survivor = fleet.movable_seat()
    await victim.kill()

    for marker in ("during-1", "during-2"):
        await fleet.publish(handle, marker)
    await asyncio.sleep(0.1)
    assert fleet.turns_for(handle) == [], "a dead node's mail was worked by someone"

    await asyncio.sleep(_TTL * 1.2)
    await survivor.sweep()
    await settle(
        lambda: survivor.markers(handle) == ["during-1", "during-2"],
        what="mail published into the takeover window",
    )


# ── a detached sandbox run outlives its node ─────────────────────────


async def test_a_sandbox_completion_survives_the_death_of_its_node(
    fleet: Fleet,
) -> None:
    """The box does not die with the process that launched it.

    A detached coding run keeps working — pushing commits, opening pull
    requests — while the node that started it is gone. Its completion
    has to reach whichever node owns the seat by the time it lands, or
    the run finishes into nothing and the suspended Execute conversation
    is stranded forever.
    """
    await fleet.balance()
    await _shorten(fleet)
    handle, victim, survivor = fleet.movable_seat()

    await victim.kill()
    topic, group = agent_control_topic(handle), agent_control_group(handle)
    assert not any(n.attached_to(topic, group) for n in fleet.nodes)

    # The run finishes while the seat is between owners.
    await survivor.queue.publish(
        topic,
        SandboxRunCompleted(
            source="waiter",
            agent_handle=handle,
            turn_id="t-orphaned-by-death",
            sandbox_id="sbx-1",
        ),
    )
    await asyncio.sleep(0.1)
    assert survivor.control == []

    await asyncio.sleep(_TTL * 1.2)
    await survivor.sweep()
    await settle(
        lambda: survivor.control == [(handle, "t-orphaned-by-death")],
        what="the completion to reach the seat's new owner",
    )


# ── the whole fleet, one node at a time ──────────────────────────────


async def test_a_rolling_restart_loses_no_trigger(fleet: Fleet) -> None:
    """The shape every deploy has, compressed.

    Each node dies in turn while traffic keeps arriving, and the company
    ends up having handled every trigger exactly once. Nothing here
    tolerates a loss: at-least-once delivery plus an owner-only
    subscription is supposed to mean a trigger survives any single
    node's death, and this is the assertion that says so.
    """
    await fleet.balance()
    await _shorten(fleet)
    handle, first, second = fleet.movable_seat()

    await fleet.publish(handle, "before")
    await settle(lambda: first.markers(handle) == ["before"], what="the first turn")

    await first.kill()
    await fleet.publish(handle, "during")
    await asyncio.sleep(_TTL * 1.2)
    await second.sweep()
    await settle(
        lambda: "during" in second.markers(handle),
        what="the successor to take over and work the backlog",
    )

    await fleet.publish(handle, "after")
    await settle(
        lambda: second.markers(handle) == ["during", "after"],
        what="the successor to keep serving",
    )

    markers = [m for _n, m in fleet.turns_for(handle)]
    assert markers == ["before", "during", "after"]
    assert len(markers) == len(set(markers)), "a trigger was worked twice"


async def test_a_killed_nodes_lease_is_never_released_by_the_killer(
    fleet: Fleet,
) -> None:
    """The premise the whole file rests on, asserted rather than assumed.

    If ``kill`` released anything, every test above would be measuring a
    graceful handoff wearing a chaos costume — and would pass whether or
    not takeover works at all.
    """
    await fleet.balance()
    await _shorten(fleet)
    handle, victim, _survivor = fleet.movable_seat()

    await victim.kill()

    lease = await fleet.leases.get(seat_resource(handle))
    assert lease is not None
    assert lease.owner == victim.engine._incarnation, (
        "the killed node handed its lease back; that is a graceful stop"
    )


# ── the completion ledger ────────────────────────────────────────────


async def test_a_turn_that_finished_is_not_re_run_by_the_successor(
    fleet: Fleet,
) -> None:
    """The window the ledger exists for, and the only one.

    A turn completes, its outbound effects ship — the Slack reply is
    posted, the Jira comment is filed — and the node dies before the
    delivery is acked. At-least-once then hands the trigger to the
    seat's next owner. Without a record of what finished, that node
    re-runs the whole turn and the person on the other end gets the
    same answer twice, from an agent that has no idea it already spoke.
    """
    await fleet.balance()
    await _shorten(fleet)
    handle, victim, survivor = fleet.movable_seat()

    await fleet.publish(handle, "answered-once")
    await settle(
        lambda: victim.markers(handle) == ["answered-once"],
        what="the owner to finish the turn",
    )
    assert await fleet.completions.completed(
        handle, [str(e.id) for e in fleet.published_ids(handle)]
    ), "a finished turn recorded nothing"

    # The ack never lands: the node dies and the trigger comes back.
    await victim.kill()
    for event in fleet.published_ids(handle):
        await survivor.queue.publish(agent_inbox_topic(handle), event)

    await asyncio.sleep(_TTL * 1.2)
    await survivor.sweep()
    await asyncio.sleep(0.3)

    assert survivor.markers(handle) == [], (
        "the successor re-ran a turn that had already answered"
    )
    assert [m for _n, m in fleet.turns_for(handle)] == ["answered-once"]


async def test_an_unrecorded_trigger_is_still_run(fleet: Fleet) -> None:
    """The other half, and the one that matters more.

    A ledger that short-circuits too eagerly turns at-least-once into
    at-most-once for exactly the case redelivery exists for: a node that
    died BEFORE finishing leaves no record, and its trigger must run.
    Losing it looks identical to an agent choosing not to reply.
    """
    await fleet.balance()
    await _shorten(fleet)
    handle, victim, survivor = fleet.movable_seat()

    entered = asyncio.Event()
    hold = asyncio.Event()

    async def _blocking(agent: Any, **kwargs: Any) -> str:
        entered.set()
        await hold.wait()
        return "never gets here"

    victim.engine.turn_engine.run_turn = _blocking  # type: ignore[method-assign]
    sent = asyncio.create_task(fleet.publish(handle, "never-finished"))
    await settle(entered.is_set, what="the owner to start the turn")
    await victim.kill(in_flight=[sent])

    await asyncio.sleep(_TTL * 1.2)
    await survivor.sweep()
    await settle(
        lambda: survivor.markers(handle) == ["never-finished"],
        what="the successor to run the unfinished trigger",
    )
