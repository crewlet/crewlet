"""Tests that engine-originated task events skip human seats gracefully.

Human seats have no inbox and the engine never sends as itself — these
internal routing events exist to wake an agent into a turn, which a
human has no equivalent of.  When a recipient resolves to a human the
event is dropped quietly (the human is notified natively by the PM
tool / Slack where the work lives); agent routing is unaffected.
"""

from __future__ import annotations

import pytest
import pytest_asyncio

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.pool import AgentPool
from crewlet.events.subscriptions import setup_subscriptions
from crewlet.events.types import (
    Event,
    TaskAssigned,
    TaskCompleted,
    TaskCreated,
    TaskDelegated,
)
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.queue.memory import MemoryEventQueue


def _make_org() -> Organization:
    """Sarah (human) leads the unit and manages the developer agent."""
    return Organization(
        name="TestCo",
        units=[
            OrgUnit(
                name="eng",
                type="team",
                lead="Sarah Chen",
                roles=[
                    Role(
                        name="Sarah Chen",
                        kind="human",
                        email="sarah@acme.com",
                        contact={"slack_user_id": "U0HUMAN"},
                    ),
                    Role(name="developer", goal="Code"),
                ],
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
    pool = AgentPool(queue)
    for role in org.all_roles():
        if role.is_human:
            continue
        defn = AgentDefinition(role=role, org=org)
        agent = AgentInstance(defn, handle=role.get_handle())
        agent.activate()
        pool.add_agent(agent)
    return pool


async def _inbox(queue: MemoryEventQueue, handle: str) -> list[Event]:
    received: list[Event] = []

    async def capture(event: Event) -> None:
        received.append(event)

    await queue.subscribe(f"crewlet.agent.{handle}.inbox", "test", capture)
    return received


@pytest.mark.asyncio
async def test_task_created_human_lead_routes_to_target_agents(queue, pool, org):
    """With a human lead, the task falls through to the target role's
    own agents — no engine push to the human, no crash."""
    dev_inbox = await _inbox(queue, "developer")
    await setup_subscriptions(queue, pool, lambda: org)

    await queue.publish(
        "crewlet.events.task_created",
        TaskCreated(
            source="test", task_id="t1", title="Fix bug", target_role="developer"
        ),
    )

    assert [e.task_id for e in dev_inbox] == ["t1"]


@pytest.mark.asyncio
async def test_task_created_no_target_skips_human_top_manager(queue, pool, org):
    """A no-target task reaches agent top-managers only; a human
    top-level seat is skipped (it has no inbox)."""
    human_inbox = await _inbox(queue, "sarah-chen")
    await setup_subscriptions(queue, pool, lambda: org)

    await queue.publish(
        "crewlet.events.task_created",
        TaskCreated(source="test", task_id="t2", title="Plan Q3"),
    )

    assert human_inbox == []


@pytest.mark.asyncio
async def test_task_assigned_human_skipped(queue, pool, org):
    """TaskAssigned naming a human seat is dropped quietly — no inbox,
    no crash (human assignment is the PM tool's job)."""
    human_inbox = await _inbox(queue, "sarah-chen")
    await setup_subscriptions(queue, pool, lambda: org)

    await queue.publish(
        "crewlet.events.task_assigned",
        TaskAssigned(source="test", task_id="t3", agent_id="", role="Sarah Chen"),
    )

    assert human_inbox == []


@pytest.mark.asyncio
async def test_task_completed_human_manager_skipped(queue, pool, org):
    """An agent completing under a human manager routes nowhere — the
    human sees the work natively; no inbox publish, no crash."""
    human_inbox = await _inbox(queue, "sarah-chen")
    await setup_subscriptions(queue, pool, lambda: org)

    dev = pool.get_by_handle("developer")
    await queue.publish(
        "crewlet.events.task_completed",
        TaskCompleted(source="test", task_id="t4", agent_id=dev.id_str, result="done"),
    )

    assert human_inbox == []


@pytest.mark.asyncio
async def test_task_delegated_human_skipped(queue, pool, org):
    human_inbox = await _inbox(queue, "sarah-chen")
    await setup_subscriptions(queue, pool, lambda: org)

    await queue.publish(
        "crewlet.events.task_delegated",
        TaskDelegated(
            source="test",
            parent_task_id="t5",
            child_task_id="t5.1",
            target_role="Sarah Chen",
        ),
    )

    assert human_inbox == []


@pytest.mark.asyncio
async def test_agent_routing_unaffected_by_human_seats(queue, pool, org):
    """The agent inbox path still works in a mixed org."""
    dev = pool.get_by_handle("developer")
    dev_inbox = await _inbox(queue, dev.handle)
    await setup_subscriptions(queue, pool, lambda: org)

    await queue.publish(
        "crewlet.events.task_assigned",
        TaskAssigned(
            source="test", task_id="t6", agent_id=dev.id_str, role="developer"
        ),
    )

    assert [e.task_id for e in dev_inbox] == ["t6"]
