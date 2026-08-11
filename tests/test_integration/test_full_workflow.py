"""Integration tests — full company workflows with all subsystems."""

from collections.abc import AsyncIterator

import pytest

from crewlet.agent.pool import AgentPool
from crewlet.engine import Engine
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import (
    Completion,
    CompletionChunk,
    Message,
    ToolDef,
)
from crewlet.queue.memory import MemoryEventQueue
from crewlet.task.delegation import DelegationHandler
from crewlet.task.models import Task, TaskStatus


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


def make_org() -> Organization:
    return Organization(
        name="Acme AI",
        mission="Build intelligent products",
        policies=["Code review required", "Document decisions"],
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                children=[
                    OrgUnit(
                        name="Backend",
                        type="team",
                        lead="Tech Lead",
                        goals=["Ship MVP"],
                        roles=[
                            Role(
                                name="Tech Lead",
                                goal="Coordinate team",
                                manages=["Engineer A", "Engineer B"],
                                tools=[
                                    "send_message",
                                    "create_task",
                                    "delegate",
                                ],
                            ),
                            Role(
                                name="Engineer A",
                                goal="Build features",
                                tools=[
                                    "send_message",
                                    "update_task",
                                ],
                            ),
                            Role(
                                name="Engineer B",
                                goal="Build features",
                                tools=[
                                    "send_message",
                                    "update_task",
                                ],
                            ),
                        ],
                    )
                ],
            ),
            OrgUnit(
                name="Product",
                type="department",
                children=[
                    OrgUnit(
                        name="PM",
                        type="team",
                        lead="Product Manager",
                        roles=[
                            Role(
                                name="Product Manager",
                                goal="Define requirements",
                                tools=["send_message", "create_task"],
                            ),
                        ],
                    )
                ],
            ),
        ],
    )


@pytest.mark.asyncio
async def test_engine_full_lifecycle():
    """Full engine lifecycle: start → track issue → stop."""
    mock_llm = MockLLM()
    engine = Engine(
        organization=make_org(),
        llm_providers={"default": mock_llm},
    )

    await engine.start()
    assert engine.is_running
    assert len(engine.agent_pool.agents) == 4  # 1 lead + 2 eng + 1 pm

    # Simulate a Jira webhook assigning an issue to the Tech Lead
    lead = engine.agent_pool.get_all_for_role("Tech Lead")[0]
    engine.execution_tracker.track("AUTH-1", agent_id=lead.id_str)

    issues = engine.execution_tracker.get_issues(lead.id_str)
    assert "AUTH-1" in issues

    # Simulate issue completion via webhook
    engine.execution_tracker.untrack("AUTH-1")
    assert engine.execution_tracker.get_issues(lead.id_str) == []

    await engine.stop()
    assert not engine.is_running


@pytest.mark.asyncio
async def test_delegation_flow():
    """Task delegation: parent creates subtasks, completion rolls up."""
    bus = MemoryEventQueue()
    await bus.start()
    pool = AgentPool(bus)
    org = make_org()
    await pool.spawn_from_org(org)

    tasks: dict[str, Task] = {}
    handler = DelegationHandler(org, pool, bus)

    parent = Task(title="Build feature", target_role="Tech Lead")
    tasks[parent.id_str] = parent

    sub1 = Task(title="Design API", target_role="Engineer A")
    sub2 = Task(title="Write tests", target_role="Engineer B")

    await handler.delegate(parent, sub1, tasks)
    await handler.delegate(parent, sub2, tasks)

    assert parent.status == TaskStatus.DELEGATED
    assert len(parent.children_ids) == 2
    # Subtasks are always QUEUED — team lead must assign them explicitly
    assert sub1.status == TaskStatus.QUEUED
    assert sub2.status == TaskStatus.QUEUED

    # Complete subtasks
    sub1.status = TaskStatus.COMPLETED
    sub2.status = TaskStatus.COMPLETED

    assert await handler.check_parent_completion(parent, tasks) is True
    assert parent.status == TaskStatus.COMPLETED
