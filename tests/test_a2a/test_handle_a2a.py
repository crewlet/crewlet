"""Tests for Engine._handle_a2a — A2A channel join + message read + turn trigger."""

from __future__ import annotations

from collections.abc import AsyncIterator
from typing import Any
from unittest.mock import AsyncMock

import pytest

from crewlet.a2a.memory import MemoryA2ABus
from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.engine import Engine
from crewlet.events.types import Event
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, CompletionChunk, Message, ToolDef


class _MockLLM:
    """Returns a fixed response for every completion request."""

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice: str | None = None,
    ) -> Completion:
        return Completion(content="Acknowledged via A2A")

    async def stream(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
    ) -> AsyncIterator[CompletionChunk]:
        yield CompletionChunk(content="streamed")


def _make_org() -> Organization:
    return Organization(
        name="Test Corp",
        mission="Test A2A",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="CEO",
                roles=[
                    Role(name="CEO", manages=["CTO"]),
                    Role(name="CTO"),
                ],
            )
        ],
    )


def _make_agent(org: Organization, role_name: str) -> AgentInstance:
    role = next(r for r in org.all_roles() if r.name == role_name)
    defn = AgentDefinition(role=role, org=org)
    return AgentInstance(definition=defn, handle=role_name.lower())


@pytest.fixture
def org() -> Organization:
    return _make_org()


@pytest.fixture
async def engine(org: Organization) -> AsyncIterator[Engine]:
    llm = _MockLLM()
    a2a_bus = MemoryA2ABus()
    e = Engine(organization=org, llm_providers={"default": llm}, a2a_bus=a2a_bus)
    await e.start()
    yield e
    await e.stop()


async def test_handle_a2a_reads_messages_and_triggers_turn(
    engine: Engine, org: Organization
) -> None:
    """_handle_a2a should read pending A2A messages and trigger execute_turn."""
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None

    # Open a channel and send a message (simulating what CEO would do).
    a2a_service = engine.a2a_service
    channel_id = await a2a_service.request_channel("ceo", "cto")
    await a2a_service.send(channel_id, "ceo", "Please review the PR")

    # Intercept execute_turn to capture what _handle_a2a passes.
    captured: dict[str, Any] = {}

    async def mock_execute(
        agent: AgentInstance,
        task_id: str = "",
        task_description: str = "",
        org: Any = None,
        notification_metadata: dict[str, Any] | None = None,
        *,
        event: Event | None = None,
        a2a_context: dict[str, Any] | None = None,
    ) -> str:
        captured["agent_handle"] = agent.handle
        captured["task_description"] = task_description
        captured["event"] = event
        captured["a2a_context"] = a2a_context
        return "mock response"

    engine.turn_engine.run_turn = mock_execute  # type: ignore[assignment]

    # Build the wake event as A2AService would.
    wake_event = Event(
        type="a2a_request",
        source="ceo",
        payload={"channel_id": channel_id, "requester": "ceo"},
    )

    await engine._handle_a2a(cto, wake_event)

    assert captured.get("agent_handle") == "cto"
    desc = captured.get("task_description", "")
    assert "ceo" in desc
    assert channel_id in desc
    assert "Please review the PR" in desc
    assert captured.get("event") is wake_event
    # Verify A2A context is passed through.
    a2a_ctx = captured.get("a2a_context")
    assert a2a_ctx is not None
    assert a2a_ctx["channel_id"] == channel_id
    assert a2a_ctx["requester"] == "ceo"
    assert a2a_ctx["message_count"] == 1


async def test_handle_a2a_no_messages_still_triggers_turn(
    engine: Engine, org: Organization
) -> None:
    """Even if no messages arrive within timeout, a turn should still fire."""
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None

    # Create channel but don't send any messages.
    a2a_service = engine.a2a_service
    channel_id = await a2a_service.request_channel("ceo", "cto")

    captured: dict[str, Any] = {}

    async def mock_execute(
        agent: AgentInstance,
        task_id: str = "",
        task_description: str = "",
        org: Any = None,
        notification_metadata: dict[str, Any] | None = None,
        *,
        event: Event | None = None,
        a2a_context: dict[str, Any] | None = None,
    ) -> str:
        captured["task_description"] = task_description
        return "mock"

    engine.turn_engine.run_turn = mock_execute  # type: ignore[assignment]

    wake_event = Event(
        type="a2a_request",
        source="ceo",
        payload={"channel_id": channel_id, "requester": "ceo"},
    )

    # Use a patched shorter timeout so the test doesn't wait 10s.
    import unittest.mock

    with unittest.mock.patch("asyncio.wait_for", side_effect=TimeoutError):
        await engine._handle_a2a(cto, wake_event)

    desc = captured.get("task_description", "")
    assert "hasn't sent a message yet" in desc
    assert channel_id in desc


async def test_handle_a2a_no_bus(org: Organization) -> None:
    """_handle_a2a should bail if the A2A bus is not configured."""
    llm = _MockLLM()
    engine = Engine(organization=org, llm_providers={"default": llm})
    # Don't start — bus is None.
    agent = _make_agent(org, "CTO")
    wake_event = Event(
        type="a2a_request",
        source="ceo",
        payload={"channel_id": "a2a-test123", "requester": "ceo"},
    )
    # Should not raise.
    await engine._handle_a2a(agent, wake_event)


async def test_handle_a2a_missing_channel_id(engine: Engine, org: Organization) -> None:
    """_handle_a2a should bail if channel_id is missing from payload."""
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None

    wake_event = Event(
        type="a2a_request",
        source="ceo",
        payload={"requester": "ceo"},
    )

    # Should not raise and should not trigger a turn.
    engine.turn_engine.run_turn = AsyncMock()  # type: ignore[assignment]
    await engine._handle_a2a(cto, wake_event)
    engine.turn_engine.run_turn.assert_not_called()
