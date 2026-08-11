"""Tests for task orchestration via ExecutionTracker (mocked Jira workflow).

All task lifecycle management
lives in the external PM tool (Jira, Linear, etc.); the engine keeps
only agent ↔ issue orchestration.  These tests verify
that the ExecutionTracker correctly orchestrates agent ↔ issue mappings,
dependencies, and DACI metadata — simulating a Jira-backed workflow.
"""

import pytest
import pytest_asyncio

from crewlet.agent.pool import AgentPool
from crewlet.events.types import Event, TaskAssigned, TaskCreated
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.queue.memory import MemoryEventQueue
from crewlet.task.tracker import ExecutionTracker


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


@pytest_asyncio.fixture
async def bus():
    q = MemoryEventQueue()
    await q.start()
    yield q
    await q.stop()


@pytest.fixture
def org():
    return make_org()


@pytest_asyncio.fixture
async def pool(bus: MemoryEventQueue, org: Organization):
    pool = AgentPool(bus)
    await pool.spawn_from_org(org)
    return pool


@pytest.fixture
def tracker():
    return ExecutionTracker()


# --- Simulated Jira webhook: issue assigned ---


@pytest.mark.asyncio
async def test_jira_issue_assigned_tracks_agent(
    tracker: ExecutionTracker, pool: AgentPool
):
    """When Jira assigns an issue, the tracker maps it to the agent."""
    agent = pool.get_all_for_role("Engineer A")[0]
    tracker.track("BACK-101", agent_id=agent.id_str)

    assert tracker.get_agent("BACK-101") == agent.id_str
    assert "BACK-101" in tracker.get_issues(agent.id_str)


@pytest.mark.asyncio
async def test_jira_issue_reassigned(tracker: ExecutionTracker, pool: AgentPool):
    """When Jira reassigns an issue, the tracker updates the mapping."""
    agent_a = pool.get_all_for_role("Engineer A")[0]
    agent_b = pool.get_all_for_role("Engineer B")[0]

    tracker.track("BACK-101", agent_id=agent_a.id_str)
    tracker.track("BACK-101", agent_id=agent_b.id_str)

    assert tracker.get_agent("BACK-101") == agent_b.id_str
    assert "BACK-101" not in tracker.get_issues(agent_a.id_str)
    assert "BACK-101" in tracker.get_issues(agent_b.id_str)


# --- Simulated Jira webhook: issue resolved ---


@pytest.mark.asyncio
async def test_jira_issue_resolved_untracks(tracker: ExecutionTracker, pool: AgentPool):
    """When Jira resolves an issue, the tracker removes it."""
    agent = pool.get_all_for_role("Engineer A")[0]
    tracker.track("BACK-101", agent_id=agent.id_str)
    tracker.untrack("BACK-101")

    assert tracker.get_issues(agent.id_str) == []
    assert tracker.tracked_count == 0


# --- Dependencies (simulated Jira issue links) ---


@pytest.mark.asyncio
async def test_jira_dependency_blocks_until_resolved(
    tracker: ExecutionTracker,
):
    """A dependency blocks until the blocker issue is resolved (untracked)."""
    tracker.track("BACK-100", agent_id="agent-1")
    tracker.track("BACK-101", agent_id="agent-2")
    tracker.add_dependency("BACK-101", blocked_by="BACK-100")

    assert tracker.dependencies_met("BACK-101") is False

    # Jira resolves the blocker
    tracker.untrack("BACK-100")
    assert tracker.dependencies_met("BACK-101") is True


@pytest.mark.asyncio
async def test_jira_multiple_dependencies(tracker: ExecutionTracker):
    """All blockers must be resolved before dependencies are met."""
    tracker.track("DEP-1")
    tracker.track("DEP-2")
    tracker.track("MAIN-1", agent_id="agent-1")
    tracker.add_dependency("MAIN-1", blocked_by="DEP-1")
    tracker.add_dependency("MAIN-1", blocked_by="DEP-2")

    assert tracker.dependencies_met("MAIN-1") is False
    tracker.untrack("DEP-1")
    assert tracker.dependencies_met("MAIN-1") is False
    tracker.untrack("DEP-2")
    assert tracker.dependencies_met("MAIN-1") is True


# --- Event publishing (simulated Jira webhook → event queue) ---


@pytest.mark.asyncio
async def test_jira_webhook_publishes_task_created(bus: MemoryEventQueue):
    """Simulated Jira webhook publishes TaskCreated for routing."""
    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await bus.subscribe("crewlet.events.task_created", "test-jira", handler)

    # Simulate what the notification service does when a Jira webhook arrives
    await bus.publish(
        "crewlet.events.task_created",
        TaskCreated(
            source="jira_webhook",
            task_id="BACK-101",
            title="Implement API endpoint",
            target_role="Engineer A",
        ),
    )

    assert len(received) == 1
    assert received[0].task_id == "BACK-101"
    assert received[0].title == "Implement API endpoint"


@pytest.mark.asyncio
async def test_jira_webhook_publishes_task_assigned(bus: MemoryEventQueue):
    """Simulated Jira assignment webhook publishes TaskAssigned."""
    received: list[Event] = []

    async def handler(event: Event) -> None:
        received.append(event)

    await bus.subscribe("crewlet.events.task_assigned", "test-jira", handler)

    await bus.publish(
        "crewlet.events.task_assigned",
        TaskAssigned(
            source="jira_webhook",
            task_id="BACK-101",
            agent_id="agent-123",
            role="Engineer A",
        ),
    )

    assert len(received) == 1
    assert received[0].agent_id == "agent-123"
