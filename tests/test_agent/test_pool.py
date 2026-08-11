"""Tests for AgentPool."""

import pytest

from crewlet.agent.instance import AgentState
from crewlet.agent.pool import AgentPool
from crewlet.db.agents import derive_agent_id
from crewlet.events.types import Event
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.queue.memory import MemoryEventQueue


def make_org() -> Organization:
    return Organization(
        name="Test Corp",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Tech Lead",
                roles=[
                    Role(
                        name="Tech Lead",
                        manages=["Engineer A", "Engineer B"],
                    ),
                    Role(name="Engineer A"),
                    Role(name="Engineer B"),
                ],
            )
        ],
    )


@pytest.mark.asyncio
async def test_spawn_from_org():
    bus = MemoryEventQueue()
    await bus.start()
    pool = AgentPool(bus)
    org = make_org()

    agents = await pool.spawn_from_org(org)
    assert len(agents) == 3  # 1 lead + 2 individual engineers

    # All should be IDLE
    for agent in agents:
        assert agent.state == AgentState.IDLE
    await bus.stop()


@pytest.mark.asyncio
async def test_spawn_emits_events():
    bus = MemoryEventQueue()
    await bus.start()

    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await bus.subscribe("crewlet.events.agent_spawned", "test", handler)

    pool = AgentPool(bus)
    org = make_org()

    await pool.spawn_from_org(org)
    assert len(received) == 3
    for event in received:
        assert event.type == "agent_spawned"
    await bus.stop()


@pytest.mark.asyncio
async def test_get_all_for_role():
    bus = MemoryEventQueue()
    await bus.start()
    pool = AgentPool(bus)
    org = make_org()
    await pool.spawn_from_org(org)

    engineers_a = pool.get_all_for_role("Engineer A")
    assert len(engineers_a) == 1
    await bus.stop()


@pytest.mark.asyncio
async def test_get_by_id():
    bus = MemoryEventQueue()
    await bus.start()
    pool = AgentPool(bus)
    org = make_org()
    agents = await pool.spawn_from_org(org)

    found = pool.get_by_id(agents[0].id_str)
    assert found is not None
    assert found.id == agents[0].id
    await bus.stop()


@pytest.mark.asyncio
async def test_terminate():
    bus = MemoryEventQueue()
    await bus.start()

    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await bus.subscribe("crewlet.events.agent_terminated", "test", handler)

    pool = AgentPool(bus)
    org = make_org()
    agents = await pool.spawn_from_org(org)

    await pool.terminate(agents[0])
    assert agents[0].state == AgentState.TERMINATED
    assert len(pool.active_agents) == 2  # 3 spawned - 1 terminated

    # Terminated event emitted
    assert len(received) == 1
    await bus.stop()


@pytest.mark.asyncio
async def test_handle_failure_creates_replacement():
    bus = MemoryEventQueue()
    await bus.start()
    pool = AgentPool(bus)
    org = make_org()
    await pool.spawn_from_org(org)

    # Pick an engineer agent and make it work
    engineer = pool.get_all_for_role("Engineer A")[0]
    engineer.start_working("task-1")

    original_id = engineer.id_str
    replacement_id = await pool.handle_failure(engineer, "task-1", "crash")

    # Replacement is a fresh AgentInstance object that *preserves* the
    # agent's persistent identity -- restart-equivalent reuse, not a
    # brand-new agent.  Personal memory + onboarding markers (keyed by
    # ``agent.id`` at AGENT scope) therefore follow the replacement.
    replacement = pool.get_by_id(replacement_id)
    assert replacement is not None
    assert replacement is not engineer
    assert replacement.role_name == "Engineer A"
    assert replacement.state == AgentState.IDLE
    assert replacement.id_str == original_id
    await bus.stop()


@pytest.mark.asyncio
async def test_handle_failure_resets_working_agent():
    bus = MemoryEventQueue()
    await bus.start()
    pool = AgentPool(bus)
    org = make_org()
    await pool.spawn_from_org(org)

    engineer = pool.get_all_for_role("Engineer A")[0]
    engineer.start_working("task-1")
    assert engineer.state == AgentState.WORKING

    await pool.handle_failure(engineer, "task-1", "error")

    # Failed agent should be terminated (not left as zombie)
    assert engineer.state == AgentState.TERMINATED
    assert engineer.current_task_id is None
    await bus.stop()


@pytest.mark.asyncio
async def test_spawn_assigns_deterministic_ids():
    """Spawned agents take their UUID from
    ``derive_agent_id(org.name, handle)``, so anything keyed by it
    (personal memory, onboarding markers) survives restart."""
    bus = MemoryEventQueue()
    await bus.start()

    pool = AgentPool(bus)
    org = make_org()
    agents = await pool.spawn_from_org(org)

    by_handle = {a.handle: a for a in agents}
    for handle, agent in by_handle.items():
        assert agent.id == derive_agent_id(org.name, handle)
    await bus.stop()


@pytest.mark.asyncio
async def test_spawn_is_idempotent_across_restarts():
    """Second pool spawning the same org gets the same ids -- the
    "engine restart" scenario this fix is built around."""
    bus = MemoryEventQueue()
    await bus.start()

    org = make_org()
    pool_a = AgentPool(bus)
    first = await pool_a.spawn_from_org(org)
    pool_b = AgentPool(bus)
    second = await pool_b.spawn_from_org(org)

    by_handle_a = {a.handle: a.id for a in first}
    by_handle_b = {a.handle: a.id for a in second}
    assert by_handle_a == by_handle_b
    await bus.stop()


@pytest.mark.asyncio
async def test_handle_failure_emits_event():
    bus = MemoryEventQueue()
    await bus.start()

    # Subscribe only after spawn to capture just the failure-recovery event
    received: list[Event] = []

    pool = AgentPool(bus)
    org = make_org()
    await pool.spawn_from_org(org)

    # Wait for spawn events to be processed

    async def handler(event: Event) -> None:
        received.append(event)

    await bus.subscribe("crewlet.events.agent_spawned", "test-failure", handler)

    engineer = pool.get_all_for_role("Engineer A")[0]
    engineer.start_working("task-1")

    replacement_id = await pool.handle_failure(engineer, "task-1", "oops")

    # Should have emitted an AgentSpawned event for the replacement
    assert len(received) == 1
    event = received[0]
    assert event.type == "agent_spawned"
    assert event.agent_id == replacement_id
    assert event.source == "pool.failure_recovery"
    await bus.stop()


@pytest.mark.asyncio
async def test_spawn_from_org_skips_human_seats():
    bus = MemoryEventQueue()
    await bus.start()
    try:
        pool = AgentPool(bus)
        org = Organization(
            name="Test Corp",
            units=[
                OrgUnit(
                    name="Core",
                    type="team",
                    lead="Sarah Chen",
                    roles=[
                        Role(
                            name="Sarah Chen",
                            kind="human",
                            email="sarah@acme.com",
                            contact={"slack_user_id": "U0HUMAN"},
                        ),
                        Role(name="Engineer"),
                    ],
                )
            ],
        )
        spawned = await pool.spawn_from_org(org)
        assert [a.role_name for a in spawned] == ["Engineer"]
        assert pool.get_by_handle("sarah-chen") is None
        assert pool.get_all_for_role("Sarah Chen") == []
    finally:
        await bus.stop()


@pytest.mark.asyncio
async def test_spawn_role_rejects_human_seat():
    bus = MemoryEventQueue()
    await bus.start()
    try:
        pool = AgentPool(bus)
        org = make_org()
        human = Role(
            name="Sarah Chen",
            kind="human",
            email="sarah@acme.com",
            contact={"slack_user_id": "U0HUMAN"},
        )
        with pytest.raises(ValueError, match="human seat"):
            await pool.spawn_role(human, org)
    finally:
        await bus.stop()
