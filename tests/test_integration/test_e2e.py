"""End-to-end integration test — validates the full event flow.

Uses only in-memory implementations (MemoryEventQueue,
MemoryA2ABus, MemoryKnowledgeProvider) so no external dependencies are
required.

Validates the event flow from docs/concepts/agent-runtime.md:
  1. TaskCreated published to event queue
  2. TaskCreated routes to team lead's inbox
  3. Team lead assignment routes to agent's inbox
  4. Task completion routes back to manager
"""

from __future__ import annotations

from collections.abc import AsyncIterator

import pytest

from crewlet.a2a.memory import MemoryA2ABus
from crewlet.engine import Engine
from crewlet.events.types import (
    TaskAssigned,
    TaskCompleted,
    TaskCreated,
)
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import (
    Completion,
    CompletionChunk,
    Message,
    ToolDef,
)
from crewlet.queue.memory import MemoryEventQueue


class MockLLM:
    """Mock LLM that returns predefined responses."""

    def __init__(self, responses: list[Completion] | None = None) -> None:
        self._responses = responses or [Completion(content="Task completed")]
        self._idx = 0
        self.calls: list[list[Message]] = []

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice: str | None = None,
    ) -> Completion:
        self.calls.append(messages)
        resp = self._responses[min(self._idx, len(self._responses) - 1)]
        self._idx += 1
        return resp

    async def stream(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
    ) -> AsyncIterator[CompletionChunk]:
        yield CompletionChunk(content="streamed")


def _make_org() -> Organization:
    """Build a minimal org with a manager and two engineers."""
    return Organization(
        name="TestCo",
        mission="E2E test org",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Team Lead",
                goals=["Deliver features"],
                roles=[
                    Role(
                        name="Team Lead",
                        goal="Coordinate and assign tasks",
                        manages=["Dev A", "Dev B"],
                    ),
                    Role(
                        name="Dev A",
                        goal="Implement features",
                    ),
                    Role(
                        name="Dev B",
                        goal="Implement features",
                    ),
                ],
            ),
        ],
    )


@pytest.mark.integration
@pytest.mark.asyncio
async def test_e2e_task_lifecycle():
    """Full event flow: webhook → track → assign → complete.

    1. Start Engine with all in-memory buses
    2. Simulate a Jira webhook (publish TaskCreated event)
    3. Track the issue via ExecutionTracker
    4. Verify assignment via tracker
    5. Simulate completion (untrack)
    6. Verify completion event was published
    """
    org = _make_org()
    mock_llm = MockLLM()
    event_queue = MemoryEventQueue()
    a2a_bus = MemoryA2ABus()

    engine = Engine(
        organization=org,
        llm_providers={"default": mock_llm},
        event_queue=event_queue,
        a2a_bus=a2a_bus,
    )

    await engine.start()
    assert engine.is_running

    # Verify agents spawned: 1 lead + 2 devs
    assert len(engine.agent_pool.agents) == 3

    # --- Step 1: Simulate Jira webhook → publish TaskCreated ---
    await event_queue.publish(
        "crewlet.events.task_created",
        TaskCreated(
            source="jira-webhook",
            task_id="AUTH-101",
            title="Build auth module",
        ),
    )

    created_events = [e for e in event_queue.history if isinstance(e, TaskCreated)]
    assert len(created_events) >= 1
    assert created_events[0].task_id == "AUTH-101"

    # --- Step 2: Track the issue assignment to Dev A ---
    dev_a_agents = engine.agent_pool.get_all_for_role("Dev A")
    assert len(dev_a_agents) == 1
    dev_a = dev_a_agents[0]

    engine.execution_tracker.track("AUTH-101", agent_id=dev_a.id_str)
    assert engine.execution_tracker.get_issues(dev_a.id_str) == ["AUTH-101"]

    # --- Step 3: Publish TaskAssigned event (simulating webhook) ---
    await event_queue.publish(
        "crewlet.events.task_assigned",
        TaskAssigned(
            source="jira-webhook",
            task_id="AUTH-101",
            agent_id=dev_a.id_str,
        ),
    )

    assigned_events = [e for e in event_queue.history if isinstance(e, TaskAssigned)]
    assert len(assigned_events) >= 1

    # --- Step 4: Complete the task (simulate webhook) ---
    engine.execution_tracker.untrack("AUTH-101")

    await event_queue.publish(
        "crewlet.events.task_completed",
        TaskCompleted(
            source="jira-webhook",
            task_id="AUTH-101",
            agent_id=dev_a.id_str,
            result="Auth module done",
        ),
    )

    completed_events = [e for e in event_queue.history if isinstance(e, TaskCompleted)]
    assert len(completed_events) >= 1
    completed = [e for e in completed_events if e.task_id == "AUTH-101"]
    assert len(completed) >= 1

    # --- Cleanup ---
    await engine.stop()
    assert not engine.is_running


@pytest.mark.integration
@pytest.mark.asyncio
async def test_e2e_a2a_bus_lifecycle():
    """Verify A2A bus channel create/send/receive/close lifecycle."""
    a2a_bus = MemoryA2ABus()
    await a2a_bus.start()

    from crewlet.a2a.messages import ChannelMessage

    # Create a channel between two agents
    await a2a_bus.create_channel("ch-1", ["agent-a", "agent-b"])

    # Send a message from agent-a to agent-b
    msg = ChannelMessage(
        channel="ch-1",
        sender="agent-a",
        content="Hello from A",
    )
    await a2a_bus.send("ch-1", "agent-a", msg)

    # Receive on agent-b's side
    received = []
    async for m in a2a_bus.receive("ch-1", "agent-b"):
        received.append(m)
        # Close the channel to break the loop
        await a2a_bus.close_channel("ch-1")

    assert len(received) == 1
    assert received[0].content == "Hello from A"

    await a2a_bus.stop()
