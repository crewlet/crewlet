"""End-to-end two-agent integration test.

Drives the full delegation flow via the A2A surface (which is fully
in-process and therefore reliable to test without real transports):

    Agent A Plan -> Execute(calls a2a_ask, target=bob) -> Review(done)
      -> a2a_ask -> A2AService.request_channel
      -> wake event lands on bob.inbox with depth+1
      -> Agent B's TurnEngine runs Plan/Execute/Review

The test verifies that:

- Both agents' turns run end-to-end through the TurnEngine.
- Bob's inbox receives exactly one wake, and it is typed
  ``a2a_request``.
- That wake carries ``delegation_depth == A.depth + 1`` with ``alice``
  appended to ``delegation_chain``.
- Both agents emit ``AgentTurnCompleted`` with populated model-field
  summaries.
"""

from __future__ import annotations

import asyncio
from typing import Any

from crewlet.a2a.service import A2AService
from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.turn import TurnEngine
from crewlet.concurrency import ConcurrencyController
from crewlet.events.types import (
    AgentTurnCompleted,
    Event,
)
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, ToolCall
from crewlet.queue.memory import MemoryEventQueue
from crewlet.tools.builtin import register_builtin_tools
from crewlet.tools.colleague import register_colleague_tools
from crewlet.tools.registry import ToolRegistry


def _plan_submission(tools_needed=None) -> list[Completion]:
    if tools_needed is None:
        tools_needed = ["a2a_ask"]
    return [
        Completion(
            content="",
            tool_calls=[
                ToolCall(
                    id="cp",
                    name="submit_plan",
                    arguments={
                        "decision": "plan",
                        "reasoning": "need colleague help",
                        "steps": [{"intent": "search"}],
                        "tools_needed": tools_needed,
                        "success_criteria": [],
                    },
                )
            ],
        )
    ]


def _execute_a2a_ask(target: str) -> list[Completion]:
    """Execute-phase completions: hand off to a colleague by calling
    ``a2a_ask`` directly, then finish the phase with a text summary."""
    return [
        Completion(
            content="",
            tool_calls=[
                ToolCall(
                    id="ce",
                    name="a2a_ask",
                    arguments={"role_urn": target, "brief": "please advise on X"},
                )
            ],
        ),
        Completion(content="I asked Bob for input."),
    ]


def _review_done(final: str) -> list[Completion]:
    return [
        Completion(
            content="",
            tool_calls=[
                ToolCall(
                    id="cr",
                    name="submit_review",
                    arguments={"decision": "done", "final_artifact": final},
                )
            ],
        )
    ]


class _RoutedProvider:
    """Returns per-phase canned completions, separately for each agent.

    Route by handle (found in the identity section of the system
    prompt) and then by phase marker.
    """

    model = "routed"

    def __init__(self, scripts: dict[str, dict[str, list[Completion]]]):
        self._scripts = {
            handle: {phase: list(cs) for phase, cs in phases.items()}
            for handle, phases in scripts.items()
        }

    async def complete(self, messages, tools=None, **_):
        sys_text = next((m.content for m in messages if m.role == "system"), "")
        # Find agent handle in identity section.
        handle = None
        for h in self._scripts:
            matches_marker = (
                f"({h})" in sys_text or f"**{h}**" in sys_text or " at " in sys_text
            )
            if matches_marker and h in sys_text:
                handle = h
                break
        if handle is None:
            # fall through to the first handle; this is a safety net
            # for the test fixture.
            handle = next(iter(self._scripts))

        phase_bucket = self._scripts[handle]
        if "PLAN phase" in sys_text:
            queue = phase_bucket.get("plan", [])
        elif "EXECUTE phase" in sys_text:
            queue = phase_bucket.get("execute", [])
        elif "REVIEW phase" in sys_text:
            queue = phase_bucket.get("review", [])
        else:
            return Completion(content="?", tool_calls=[])
        if not queue:
            return Completion(content="(no more)", tool_calls=[])
        return queue.pop(0)


def _mk_agent(org: Organization, role_name: str, handle: str) -> AgentInstance:
    role = org.get_role(role_name)
    defn = AgentDefinition(role=role, org=org)
    agent = AgentInstance(definition=defn, handle=handle, email=f"{handle}@acme.com")
    agent.activate()
    return agent


async def test_end_to_end_two_agent_a2a_handoff_with_depth_inheritance():
    # Org: two peers managed by a lead.
    alice_role = Role(name="Alice", handle="alice")
    bob_role = Role(name="Bob", handle="bob")
    lead_role = Role(name="Lead", handle="lead", manages=["Alice", "Bob"])
    unit = OrgUnit(
        name="Eng", type="team", lead="Lead", roles=[lead_role, alice_role, bob_role]
    )
    org = Organization(name="Acme", units=[unit])

    alice = _mk_agent(org, "Alice", "alice")
    bob = _mk_agent(org, "Bob", "bob")

    # Shared registry with builtins + colleague tools.
    registry = ToolRegistry()
    register_builtin_tools(registry)
    register_colleague_tools(registry)

    # Event queue shared by both engines.
    queue = MemoryEventQueue()
    await queue.start()

    try:
        a2a = A2AService(queue)

        # Separate per-agent concurrency controllers.
        alice_conc = ConcurrencyController(max_concurrent=4)
        bob_conc = ConcurrencyController(max_concurrent=4)

        provider = _RoutedProvider(
            {
                "alice": {
                    "plan": _plan_submission(["a2a_ask"]),
                    "execute": _execute_a2a_ask("bob"),
                    "review": _review_done("Asked Bob for input."),
                },
                "bob": {
                    "plan": _plan_submission([]),
                    "execute": [Completion(content="Sure, sounds good to me.")],
                    "review": _review_done("Bob replied."),
                },
            }
        )
        providers = {"default": provider}

        alice_engine = TurnEngine(
            llm_providers=providers,
            tool_registry=registry,
            event_queue=queue,
            concurrency=alice_conc,
            a2a_service=a2a,
        )
        bob_engine = TurnEngine(
            llm_providers=providers,
            tool_registry=registry,
            event_queue=queue,
            concurrency=bob_conc,
            a2a_service=a2a,
        )

        # Bob subscribes to his inbox: when Alice hands off, the memory
        # queue dispatches to this callback, which runs Bob's turn.
        bob_wakes: list[Event] = []

        async def bob_handler(event: Event) -> None:
            bob_wakes.append(event)
            # Bob runs his own turn in response.
            await bob_engine.run_turn(bob, event=event, org=org)

        await queue.subscribe(
            topic="crewlet.agent.bob.inbox",
            group="g-bob",
            handler=bob_handler,
        )

        # Kick off Alice's turn.
        await alice_engine.run_turn(
            alice,
            task_id="task-42",
            task_description="Decide whether to ship feature X.",
            org=org,
        )

        # Give the memory queue a moment to dispatch Bob's wake event.
        await asyncio.sleep(0.1)

        # --- assertions ---

        # Bob's inbox received exactly one wake event from Alice's a2a_ask.
        assert len(bob_wakes) == 1
        wake = bob_wakes[0]
        assert wake.type == "a2a_request"
        assert wake.delegation_depth == 1  # Alice was at depth 0 → Bob 1
        assert wake.delegation_chain == ["alice"]

        # Both agents emitted AgentTurnCompleted with the new model fields.
        turn_events = [
            e for _, e in _history_events(queue) if isinstance(e, AgentTurnCompleted)
        ]
        assert len(turn_events) >= 2
        # Every turn completion should have at least one phase model set.
        for t in turn_events:
            assert t.plan_model or t.execute_model or t.review_model
            assert t.iterations >= 1
    finally:
        await queue.stop()


def _history_events(queue: MemoryEventQueue) -> list[tuple[str, Any]]:
    """Return ``queue.history`` as a list of ``(topic, event)`` tuples.

    This helper is local to the test module and reads
    ``queue.history`` without mutating the class, so it can't leak
    into other tests.
    """
    history = getattr(queue, "history", None) or []
    out: list[tuple[str, Any]] = []
    for item in history:
        if isinstance(item, tuple):
            out.append(item)
        else:
            out.append(("", item))
    return out
