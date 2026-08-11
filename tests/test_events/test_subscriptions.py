"""Tests for event subscription routing via EventQueue."""

from __future__ import annotations

import pytest
import pytest_asyncio

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.pool import AgentPool
from crewlet.events.subscriptions import setup_subscriptions
from crewlet.events.types import (
    Event,
    ExternalNotification,
    TaskAssigned,
    TaskCompleted,
    TaskCreated,
    TaskDelegated,
)
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.queue.memory import MemoryEventQueue


def _make_org() -> Organization:
    """Create a small org: tech-lead manages developer."""
    manager_role = Role(name="tech-lead", goal="Lead", manages=["developer"])
    dev_role = Role(name="developer", goal="Code")
    return Organization(
        name="TestCo",
        units=[
            OrgUnit(
                name="eng",
                type="team",
                lead="tech-lead",
                roles=[manager_role, dev_role],
            )
        ],
    )


@pytest.fixture
def org() -> Organization:
    return _make_org()


@pytest_asyncio.fixture
async def queue() -> MemoryEventQueue:
    q = MemoryEventQueue()
    await q.start()
    yield q
    await q.stop()


@pytest_asyncio.fixture
async def pool(queue: MemoryEventQueue, org: Organization) -> AgentPool:
    return AgentPool(queue)


async def _spawn_agents(pool: AgentPool, org: Organization) -> dict[str, AgentInstance]:
    """Spawn one agent per role and return a map of role_name -> agent."""
    agents: dict[str, AgentInstance] = {}
    for role in org.all_roles():
        defn = AgentDefinition(role=role, org=org)
        agent = AgentInstance(defn, handle=role.name.lower().replace(" ", "-"))
        agent.activate()
        pool.add_agent(agent)
        agents[role.name] = agent
    return agents


# ------------------------------------------------------------------ #
# TaskCreated → team lead's inbox
# ------------------------------------------------------------------ #


@pytest.mark.asyncio
async def test_task_created_publishes_to_lead_inbox(
    queue: MemoryEventQueue, pool: AgentPool, org: Organization
) -> None:
    """TaskCreated for a developer role should publish to team lead inbox."""
    agents = await _spawn_agents(pool, org)
    lead = agents["tech-lead"]

    received: list[Event] = []

    async def capture(event: Event) -> None:
        received.append(event)

    await queue.subscribe(f"crewlet.agent.{lead.handle}.inbox", "test", capture)
    await setup_subscriptions(queue, pool, lambda: org)

    # Publish TaskCreated targeting "developer" role
    await queue.publish(
        "crewlet.events.task_created",
        TaskCreated(
            source="test", task_id="t1", title="Fix bug", target_role="developer"
        ),
    )

    assert len(received) == 1
    assert received[0].type == "task_created"
    assert received[0].task_id == "t1"


# ------------------------------------------------------------------ #
# TaskAssigned → assigned agent's inbox
# ------------------------------------------------------------------ #


@pytest.mark.asyncio
async def test_task_assigned_publishes_to_agent_inbox(
    queue: MemoryEventQueue, pool: AgentPool, org: Organization
) -> None:
    """TaskAssigned should publish to the assigned agent's inbox."""
    agents = await _spawn_agents(pool, org)
    dev = agents["developer"]

    received: list[Event] = []

    async def capture(event: Event) -> None:
        received.append(event)

    await queue.subscribe(f"crewlet.agent.{dev.handle}.inbox", "test", capture)
    await setup_subscriptions(queue, pool, lambda: org)

    await queue.publish(
        "crewlet.events.task_assigned",
        TaskAssigned(
            source="test", task_id="t1", agent_id=dev.id_str, role="developer"
        ),
    )

    assert len(received) == 1
    assert received[0].type == "task_assigned"


# ------------------------------------------------------------------ #
# TaskCompleted → manager's inbox
# ------------------------------------------------------------------ #


@pytest.mark.asyncio
async def test_task_completed_publishes_to_manager_inbox(
    queue: MemoryEventQueue, pool: AgentPool, org: Organization
) -> None:
    """TaskCompleted from developer should publish to tech-lead inbox."""
    agents = await _spawn_agents(pool, org)
    dev = agents["developer"]
    lead = agents["tech-lead"]

    received: list[Event] = []

    async def capture(event: Event) -> None:
        received.append(event)

    await queue.subscribe(f"crewlet.agent.{lead.handle}.inbox", "test", capture)
    await setup_subscriptions(queue, pool, lambda: org)

    await queue.publish(
        "crewlet.events.task_completed",
        TaskCompleted(source="test", task_id="t1", agent_id=dev.id_str, result="done"),
    )

    assert len(received) == 1
    assert received[0].type == "task_completed"


# ------------------------------------------------------------------ #
# TaskDelegated → target agent's inbox
# ------------------------------------------------------------------ #


@pytest.mark.asyncio
async def test_task_delegated_publishes_to_target_inbox(
    queue: MemoryEventQueue, pool: AgentPool, org: Organization
) -> None:
    """TaskDelegated should publish to agents with the target role."""
    agents = await _spawn_agents(pool, org)
    dev = agents["developer"]

    received: list[Event] = []

    async def capture(event: Event) -> None:
        received.append(event)

    await queue.subscribe(f"crewlet.agent.{dev.handle}.inbox", "test", capture)
    await setup_subscriptions(queue, pool, lambda: org)

    await queue.publish(
        "crewlet.events.task_delegated",
        TaskDelegated(
            source="test",
            parent_task_id="p1",
            child_task_id="c1",
            target_role="developer",
        ),
    )

    assert len(received) == 1
    assert received[0].type == "task_delegated"


# ------------------------------------------------------------------ #
# ExternalNotification → resolved agent's inbox
# ------------------------------------------------------------------ #


@pytest.mark.asyncio
async def test_external_notification_publishes_to_agent_inbox(
    queue: MemoryEventQueue, pool: AgentPool, org: Organization
) -> None:
    """ExternalNotification should publish to the resolved agent's inbox."""
    agents = await _spawn_agents(pool, org)
    dev = agents["developer"]

    received: list[Event] = []

    async def capture(event: Event) -> None:
        received.append(event)

    await queue.subscribe(f"crewlet.agent.{dev.handle}.inbox", "test", capture)
    await setup_subscriptions(queue, pool, lambda: org)

    await queue.publish(
        "crewlet.events.external_notification",
        ExternalNotification(
            source="test",
            notification_source="slack",
            agent_id=dev.id_str,
            body="Hello",
        ),
    )

    assert len(received) == 1
    assert received[0].type == "external_notification"


# ------------------------------------------------------------------ #
# Edge cases
# ------------------------------------------------------------------ #


@pytest.mark.asyncio
async def test_task_created_no_target_role_goes_to_top_managers(
    queue: MemoryEventQueue, pool: AgentPool, org: Organization
) -> None:
    """TaskCreated without target_role should go to top-level managers."""
    agents = await _spawn_agents(pool, org)
    lead = agents["tech-lead"]

    received: list[Event] = []

    async def capture(event: Event) -> None:
        received.append(event)

    await queue.subscribe(f"crewlet.agent.{lead.handle}.inbox", "test", capture)
    await setup_subscriptions(queue, pool, lambda: org)

    await queue.publish(
        "crewlet.events.task_created",
        TaskCreated(source="test", task_id="t1", title="Unassigned"),
    )

    # tech-lead is the top manager (has no manager above)
    assert len(received) == 1


@pytest.mark.asyncio
async def test_task_assigned_unknown_agent_is_noop(
    queue: MemoryEventQueue, pool: AgentPool, org: Organization
) -> None:
    """TaskAssigned for an unknown agent_id should not crash."""
    await _spawn_agents(pool, org)
    await setup_subscriptions(queue, pool, lambda: org)

    # Should not raise
    await queue.publish(
        "crewlet.events.task_assigned",
        TaskAssigned(
            source="test", task_id="t1", agent_id="nonexistent", role="developer"
        ),
    )
