"""Delegation-depth plumbing end-to-end.

Covers the critical edge: when Agent A asks Agent B
via a colleague-surface tool, the event that wakes B must carry
``delegation_depth = A.delegation_depth + 1`` and
``delegation_chain = [*A.chain, A.handle]``.

Most straightforward path: A2A, because the A2AService runs in-process
and the a2a_ask tool has direct access to A's TurnContext.
"""

from __future__ import annotations

from dataclasses import dataclass

from crewlet.a2a.memory import MemoryA2ABus
from crewlet.a2a.service import A2AService
from crewlet.agent.turn_context import TurnContext
from crewlet.events.types import Event
from crewlet.queue.memory import MemoryEventQueue
from crewlet.tools.colleague import register_colleague_tools
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry


@dataclass
class _AgentStub:
    handle: str = "alice"
    id_str: str = "id-alice"
    role_name: str = "Engineer"
    input_tokens: int = 0
    output_tokens: int = 0


async def test_a2a_request_event_carries_depth_and_chain():
    """a2a_ask should wake Bob with depth+1 and A's handle appended."""
    queue = MemoryEventQueue()
    await queue.start()
    try:
        # Capture events published to Bob's inbox.
        received: list[tuple[str, Event]] = []

        async def _handler(event: Event) -> None:
            received.append(("bob_inbox", event))

        await queue.subscribe(
            topic="crewlet.agent.bob.inbox",
            group="g-bob",
            handler=_handler,
        )

        bus = MemoryA2ABus()
        a2a = A2AService(bus, queue)

        registry = ToolRegistry()
        register_colleague_tools(registry)
        tool = registry.get("a2a_ask")
        assert tool is not None

        # Build an AgentContext as the TurnEngine does -- attach the
        # TurnContext so a2a_ask can read depth/chain.
        alice = _AgentStub(handle="alice")
        turn = TurnContext(
            agent=alice,  # type: ignore[arg-type]
            org=None,
            delegation_depth=1,
            delegation_chain=["ceo"],
        )
        ctx = AgentContext(
            agent_id="id-alice",
            agent_handle="alice",
            role="Engineer",
            event_queue=queue,
            a2a_service=a2a,
        )
        ctx.__dict__["turn_context"] = turn

        result = await tool.execute(
            {"role_urn": "bob", "brief": "pls review PR 42"}, ctx
        )
        assert result.success
        # Give the memory queue's dispatcher a moment.
        import asyncio

        await asyncio.sleep(0.05)

        assert received, "Bob's inbox received no event"
        topic, event = received[0]
        assert topic == "bob_inbox"
        assert event.type == "a2a_request"
        assert event.delegation_depth == 2  # 1 + 1
        assert event.delegation_chain == ["ceo", "alice"]
        assert event.parent_turn_id == turn.turn_id
    finally:
        await queue.stop()


async def test_a2a_request_with_no_turn_context_uses_defaults():
    """Backwards compatibility: a2a_ask without TurnContext uses depth=0."""
    queue = MemoryEventQueue()
    await queue.start()
    try:
        received: list[Event] = []

        async def _handler(event: Event) -> None:
            received.append(event)

        await queue.subscribe(
            topic="crewlet.agent.bob.inbox",
            group="g-bob",
            handler=_handler,
        )
        bus = MemoryA2ABus()
        a2a = A2AService(bus, queue)
        registry = ToolRegistry()
        register_colleague_tools(registry)
        tool = registry.get("a2a_ask")
        assert tool is not None
        ctx = AgentContext(
            agent_id="id-alice",
            agent_handle="alice",
            role="Engineer",
            event_queue=queue,
            a2a_service=a2a,
        )
        # No turn_context attached.
        await tool.execute({"role_urn": "bob", "brief": "hi"}, ctx)
        import asyncio

        await asyncio.sleep(0.05)
        assert received
        event = received[0]
        assert event.delegation_depth == 1  # starts at 0 -> +1 when forwarded
        assert event.delegation_chain == ["alice"]
    finally:
        await queue.stop()
