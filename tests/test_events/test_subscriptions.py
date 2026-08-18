"""Tests for event subscription routing via EventQueue.

Routing is an **org** function: every assertion here runs with NO agent
pool at all.  That is the point — a node routes an event to a seat it
does not run, because the topics these handlers consume have one
fleet-wide consumer group and the node that wins a delivery is rarely
the node that owns the recipient.
"""

from __future__ import annotations

import pytest
import pytest_asyncio

from crewlet.db.agents import derive_agent_id
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
from crewlet.queue.topics import agent_inbox_topic


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


async def _watch(queue: MemoryEventQueue, org: Organization, role_name: str):
    """Subscribe to a seat's inbox and return the list it fills."""
    role = org.get_role(role_name)
    assert role is not None
    received: list[Event] = []

    async def capture(event: Event) -> None:
        received.append(event)

    await queue.subscribe(agent_inbox_topic(role.get_handle()), "test", capture)
    return received


def _agent_id(org: Organization, role_name: str) -> str:
    role = org.get_role(role_name)
    assert role is not None
    return org.agent_id_for(role)


# ------------------------------------------------------------------ #
# TaskCreated → team lead's inbox
# ------------------------------------------------------------------ #


async def test_task_created_publishes_to_lead_inbox(
    queue: MemoryEventQueue, org: Organization
) -> None:
    """TaskCreated for a developer role should publish to team lead inbox."""
    received = await _watch(queue, org, "tech-lead")
    await setup_subscriptions(queue, lambda: org)

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


async def test_task_assigned_publishes_to_agent_inbox(
    queue: MemoryEventQueue, org: Organization
) -> None:
    """TaskAssigned should publish to the assigned seat's inbox."""
    received = await _watch(queue, org, "developer")
    await setup_subscriptions(queue, lambda: org)

    await queue.publish(
        "crewlet.events.task_assigned",
        TaskAssigned(
            source="test",
            task_id="t1",
            agent_id=_agent_id(org, "developer"),
            role="developer",
        ),
    )

    assert len(received) == 1
    assert received[0].type == "task_assigned"


async def test_task_assigned_routes_on_agent_id_alone(
    queue: MemoryEventQueue, org: Organization
) -> None:
    """A producer that carries only the derived id still routes.

    The id is a ``uuid5`` over (org name, handle), so the seat is
    recoverable from the org with no instance and no database.
    """
    received = await _watch(queue, org, "developer")
    await setup_subscriptions(queue, lambda: org)

    await queue.publish(
        "crewlet.events.task_assigned",
        TaskAssigned(source="test", task_id="t1", agent_id=_agent_id(org, "developer")),
    )

    assert len(received) == 1


async def test_task_assigned_agent_id_wins_over_role(
    queue: MemoryEventQueue, org: Organization
) -> None:
    """When both fields resolve and disagree, the id decides."""
    to_dev = await _watch(queue, org, "developer")
    to_lead = await _watch(queue, org, "tech-lead")
    await setup_subscriptions(queue, lambda: org)

    await queue.publish(
        "crewlet.events.task_assigned",
        TaskAssigned(
            source="test",
            task_id="t1",
            agent_id=_agent_id(org, "developer"),
            role="tech-lead",
        ),
    )

    assert len(to_dev) == 1 and to_lead == []


# ------------------------------------------------------------------ #
# TaskCompleted → manager's inbox
# ------------------------------------------------------------------ #


async def test_task_completed_publishes_to_manager_inbox(
    queue: MemoryEventQueue, org: Organization
) -> None:
    """TaskCompleted from developer should publish to tech-lead inbox."""
    received = await _watch(queue, org, "tech-lead")
    await setup_subscriptions(queue, lambda: org)

    await queue.publish(
        "crewlet.events.task_completed",
        TaskCompleted(
            source="test",
            task_id="t1",
            agent_id=_agent_id(org, "developer"),
            result="done",
        ),
    )

    assert len(received) == 1
    assert received[0].type == "task_completed"


# ------------------------------------------------------------------ #
# TaskDelegated → target agent's inbox
# ------------------------------------------------------------------ #


async def test_task_delegated_publishes_to_target_inbox(
    queue: MemoryEventQueue, org: Organization
) -> None:
    """TaskDelegated should publish to the target role's seat."""
    received = await _watch(queue, org, "developer")
    await setup_subscriptions(queue, lambda: org)

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


async def test_external_notification_publishes_to_agent_inbox(
    queue: MemoryEventQueue, org: Organization
) -> None:
    """ExternalNotification should publish to the resolved seat's inbox."""
    received = await _watch(queue, org, "developer")
    await setup_subscriptions(queue, lambda: org)

    await queue.publish(
        "crewlet.events.external_notification",
        ExternalNotification(
            source="test",
            notification_source="slack",
            agent_id=_agent_id(org, "developer"),
            body="Hello",
        ),
    )

    assert len(received) == 1
    assert received[0].type == "external_notification"


# ------------------------------------------------------------------ #
# Edge cases
# ------------------------------------------------------------------ #


async def test_task_created_no_target_role_goes_to_top_managers(
    queue: MemoryEventQueue, org: Organization
) -> None:
    """TaskCreated without target_role should go to top-level managers."""
    received = await _watch(queue, org, "tech-lead")
    await setup_subscriptions(queue, lambda: org)

    await queue.publish(
        "crewlet.events.task_created",
        TaskCreated(source="test", task_id="t1", title="Unassigned"),
    )

    # tech-lead is the top manager (has no manager above)
    assert len(received) == 1


async def test_task_assigned_unknown_seat_is_noop(
    queue: MemoryEventQueue, org: Organization
) -> None:
    """Neither field naming a seat is a warned drop, not a crash."""
    await setup_subscriptions(queue, lambda: org)

    await queue.publish(
        "crewlet.events.task_assigned",
        TaskAssigned(
            source="test", task_id="t1", agent_id="nonexistent", role="nobody"
        ),
    )


async def test_task_completed_unknown_agent_id_is_noop(
    queue: MemoryEventQueue, org: Organization
) -> None:
    """An id that names no seat drops rather than routing somewhere."""
    to_lead = await _watch(queue, org, "tech-lead")
    await setup_subscriptions(queue, lambda: org)

    await queue.publish(
        "crewlet.events.task_completed",
        TaskCompleted(
            source="test", task_id="t1", agent_id="nonexistent", result="done"
        ),
    )

    assert to_lead == []


async def test_ids_are_the_same_on_every_node(org: Organization) -> None:
    """The derivation the handlers invert has no process-local input."""
    dev = org.get_role("developer")
    assert dev is not None
    assert org.agent_id_for(dev) == str(derive_agent_id("TestCo", "developer"))
    assert org.agent_seat_by_id(org.agent_id_for(dev)) is dev
