"""Three core nodes and a satellite: the topology placement exists for.

The rest of ``tests/test_fleet`` proves that seats move *freely* — any
node can take any seat, and one of them will. This file proves the
opposite property, which is the one an operator asks for by writing a
``role.placement``: that a seat runs **only** where they said, and that
saying so has a visible cost when nowhere qualifies.

The satellite here is what the fleet guide calls one: ``roles: [seats]``
and a label. It runs agents, it holds no company-wide duty, and it
terminates no inbound traffic — so a pinned seat runs there while the
scheduler, the sandbox waiter and the retention sweep stay on the core,
which is exactly the arrangement that lets a satellite sit in a network
zone the rest of the fleet is not in.
"""

from __future__ import annotations

import asyncio
import sys
from pathlib import Path
from typing import Any
from uuid import uuid4

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from conftest import make_bootstrap  # noqa: E402
from crewlet.config import (  # noqa: E402
    CompanyConfig,
    LLMProviderConfig,
    NodeConfig,
    ProvidersConfig,
)
from crewlet.db.leases import MemoryLeaseStore, worker_resource  # noqa: E402
from crewlet.db.turn_completions import MemoryTurnCompletionStore  # noqa: E402
from crewlet.engine import Engine  # noqa: E402
from crewlet.events.types import Event  # noqa: E402
from crewlet.queue.memory import MemoryEventQueue  # noqa: E402
from crewlet.queue.topics import agent_inbox_topic  # noqa: E402
from test_fleet.conftest import BACKENDS, PULSAR_URL, settle  # noqa: E402

# The satellite's label, and the selector the pinned seat carries. One
# constant so a typo cannot make the test pass by pinning nothing.
ZONE = {"zone": "eu"}


def _company(suffix: str, handles: dict[str, str]) -> CompanyConfig:
    """Four seats: three that may run anywhere, one pinned to the zone.

    Three unpinned seats over three core nodes is one each, so a fourth
    seat landing on a core node would be visible as over-capacity rather
    than hidden inside a share that had room for it anyway.
    """
    return CompanyConfig(
        name=f"Satellite {suffix}",
        mission="prove a pinned seat runs only where it was pinned",
        roles=[
            {"name": "Alpha", "handle": handles["alpha"], "goal": "lead"},
            {"name": "Beta", "handle": handles["beta"], "goal": "build"},
            {"name": "Gamma", "handle": handles["gamma"], "goal": "review"},
            {
                "name": "EU Support",
                "handle": handles["eu"],
                "goal": "answer in-zone",
                "placement": {"labels": dict(ZONE)},
            },
        ],
        providers=ProvidersConfig(
            llm={"default": LLMProviderConfig(type="openai", model="gpt-4o-mini")}
        ),
    )


class _Node:
    """One engine, plus what it was observed running."""

    def __init__(self, name: str, engine: Engine, queue: Any, journal: list) -> None:
        self.name = name
        self.engine = engine
        self.queue = queue
        self.journal = journal

    @property
    def held(self) -> tuple[str, ...]:
        return self.engine._seat_host.held_handles

    async def sweep(self) -> Any:
        return await self.engine._seat_host.sweep()

    def ran(self, handle: str) -> list[str]:
        return [m for node, h, m in self.journal if node == self.name and h == handle]


@pytest.fixture(params=BACKENDS)
async def satellite_fleet(request: pytest.FixtureRequest, monkeypatch):
    monkeypatch.setenv("OPENAI_API_KEY", "test-key-for-construction")
    suffix = uuid4().hex[:8]
    handles = {k: f"{k}{suffix}" for k in ("alpha", "beta", "gamma", "eu")}
    company = _company(suffix, handles)
    leases = MemoryLeaseStore()
    completions = MemoryTurnCompletionStore()
    seed = MemoryEventQueue() if request.param == "memory" else None
    journal: list[tuple[str, str, str]] = []

    # Three core nodes (every role, no labels) and one satellite.
    layout = [
        ("core-1", ["ingress", "seats", "workers"], {}),
        ("core-2", ["ingress", "seats", "workers"], {}),
        ("core-3", ["ingress", "seats", "workers"], {}),
        ("sat-eu", ["seats"], dict(ZONE)),
    ]
    nodes: list[_Node] = []
    for name, roles, labels in layout:
        if seed is not None:
            queue: Any = seed.client()
        else:
            from crewlet.queue.pulsar import PulsarEventQueue

            queue = PulsarEventQueue(PULSAR_URL)
        bootstrap = make_bootstrap()
        bootstrap.node = NodeConfig(id=f"{name}-{suffix}", roles=roles, labels=labels)
        engine = Engine.from_bootstrap(
            bootstrap,
            event_queue=queue,
            lease_store=leases,
            turn_completion_store=completions,
        )
        await engine.apply_config(company)
        await engine.start()
        node = _Node(name, engine, queue, journal)

        async def _record(agent: Any, _node: _Node = node, **kwargs: Any) -> str:
            event = kwargs.get("event")
            marker = str(event.payload.get("task_id", event.type)) if event else ""
            journal.append((_node.name, agent.handle, marker))
            return "recorded"

        engine.turn_engine.run_turn = _record  # type: ignore[method-assign]
        nodes.append(node)

    # The fixture starts nodes one at a time, so the first holds
    # everything until the rest arrive. Converging is the give-back plus
    # a claim — two passes, in that order.
    for _ in range(4):
        for node in nodes:
            await node.sweep()

    try:
        yield nodes, handles, leases
    finally:
        for node in reversed(nodes):
            try:
                await node.engine.stop()
            except Exception:  # pragma: no cover — teardown must not mask
                await node.engine._force_stop()


def _by_name(nodes: list[_Node]) -> dict[str, _Node]:
    return {n.name: n for n in nodes}


# ── the pin holds ────────────────────────────────────────────────────


async def test_the_pinned_seat_runs_on_the_satellite_and_nowhere_else(
    satellite_fleet,
) -> None:
    nodes, handles, _ = satellite_fleet
    sat = _by_name(nodes)["sat-eu"]
    assert handles["eu"] in sat.held
    for node in nodes:
        if node is not sat:
            assert handles["eu"] not in node.held, (
                f"{node.name} claimed a seat pinned to another node's labels"
            )


async def test_a_trigger_for_a_pinned_seat_reaches_it(satellite_fleet) -> None:
    """Published from a core node, run on the satellite. The wake travels
    the seat's inbox, so the publisher never has to know where the seat
    is — which is what makes a satellite a placement decision rather than
    a routing one."""
    nodes, handles, _ = satellite_fleet
    core = _by_name(nodes)["core-1"]
    sat = _by_name(nodes)["sat-eu"]
    await core.queue.publish(
        agent_inbox_topic(handles["eu"]),
        Event(
            type="task_assigned",
            source="test",
            payload={"task_id": "in-zone", "task_description": "in-zone"},
        ),
    )
    await settle(
        lambda: sat.ran(handles["eu"]) == ["in-zone"],
        what="the satellite to run the pinned seat's trigger",
    )


async def test_the_unpinned_seats_are_shared_across_every_seat_node(
    satellite_fleet,
) -> None:
    """A satellite runs seats, so it is eligible for unpinned ones too —
    a real property, and the one to be deliberate about: if a seat needs
    the core's network, pin it to the core rather than assuming a
    satellite will not take it."""
    nodes, handles, _ = satellite_fleet
    unpinned = {handles["alpha"], handles["beta"], handles["gamma"]}
    held = {h for node in nodes for h in node.held}
    assert unpinned <= held, "an unpinned seat went unclaimed"

    # The shares, per placement group: three unpinned seats over four
    # eligible nodes is ceil(3/4) == 1 each, and the pinned group is
    # ceil(1/1) == 1 for the satellite alone. So a core node holds at
    # most one seat and the satellite at most two — and the satellite's
    # second is its pinned one, never a core node's share.
    by_name = _by_name(nodes)
    for name in ("core-1", "core-2", "core-3"):
        assert len(by_name[name].held) <= 1, f"{name} exceeded its share"
    assert len(by_name["sat-eu"].held) <= 2


# ── the satellite holds no company-wide duty ─────────────────────────


async def test_the_satellite_never_takes_a_singleton_duty(satellite_fleet) -> None:
    """It has no ``workers`` role, so it does not merely lose the race —
    it does not enter it. That is what keeps the scheduler, the sandbox
    waiter and the retention sweep on nodes that can reach the rest of
    the world."""
    nodes, _handles, leases = satellite_fleet
    sat = _by_name(nodes)["sat-eu"]
    core = _by_name(nodes)["core-1"]

    assert await sat.engine.claim_worker_duty("sandbox-waiter") is False
    assert await core.engine.claim_worker_duty("sandbox-waiter") is True

    lease = await leases.get(worker_resource("sandbox-waiter"))
    assert lease is not None
    assert lease.owner.startswith("core-1")


# ── the cost of a pin, made visible ──────────────────────────────────


async def test_a_pinned_seat_goes_unserved_when_its_node_leaves(
    satellite_fleet,
) -> None:
    """The engine will not widen a selector to keep a seat running —
    widening it is exactly what the operator asked it not to do. So the
    seat stops being served, and the sweep says so rather than letting
    three healthy-looking nodes imply otherwise.
    """
    nodes, handles, _ = satellite_fleet
    sat = _by_name(nodes)["sat-eu"]
    await sat.engine.stop()
    nodes.remove(sat)

    # Let the satellite's presence lapse, then sweep the survivors.
    await asyncio.sleep(0.05)
    results = []
    for node in nodes:
        results.append(await node.sweep())

    for node in nodes:
        assert handles["eu"] not in node.held, (
            f"{node.name} took a seat it does not match"
        )
    assert any(handles["eu"] in r.unplaceable for r in results), (
        "nothing reported the seat as unservable"
    )
