"""Tests for the queue-driven agent handler model in Engine."""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator

from crewlet.agent.instance import AgentInstance
from crewlet.engine import Engine
from crewlet.events.types import Event
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import (
    Completion,
    CompletionChunk,
    Message,
    ToolDef,
)
from crewlet.queue.memory import MemoryEventQueue
from crewlet.queue.topics import agent_inbox_group, agent_inbox_topic


class MockLLM:
    """Mock LLM that returns a canned response."""

    def __init__(self, response: str = "done") -> None:
        self._response = response
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
        return Completion(content=self._response)

    async def stream(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
    ) -> AsyncIterator[CompletionChunk]:
        yield CompletionChunk(content="streamed")


def _make_org() -> Organization:
    return Organization(
        name="QueueTest",
        mission="Test queue handlers",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Lead",
                roles=[
                    Role(name="Lead", manages=["Dev"]),
                    Role(name="Dev"),
                ],
            )
        ],
    )


def _make_engine(
    queue: MemoryEventQueue,
    llm: MockLLM | None = None,
) -> Engine:
    llm = llm or MockLLM()
    return Engine(
        organization=_make_org(),
        llm_providers={"default": llm},
        event_queue=queue,
    )


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


class TestEngineSubscribesHandlers:
    """Engine.start() subscribes a handler for every agent's inbox topic."""

    async def test_subscribes_all_agents(self) -> None:
        queue = MemoryEventQueue()
        await queue.start()
        engine = _make_engine(queue)

        await engine.start()
        try:
            agent_handles = {a.handle for a in engine.agent_pool.agents}
            # Agent inboxes use the batched per-conversation subscription.
            attached = {topic for topic, _group in queue.attachments()}

            for handle in agent_handles:
                expected_topic = agent_inbox_topic(handle)
                assert expected_topic in attached, (
                    f"Missing subscription for {expected_topic}"
                )
        finally:
            await engine.stop()

    async def test_subscription_groups_match_agent_handles(self) -> None:
        queue = MemoryEventQueue()
        await queue.start()
        engine = _make_engine(queue)

        await engine.start()
        try:
            for topic, group in queue.attachments():
                if topic.startswith("crewlet.agent."):
                    handle = topic.split(".")[2]
                    assert group == agent_inbox_group(handle)
        finally:
            await engine.stop()


class TestTaskAssignedTriggersExecuteTurn:
    """Publishing a task_assigned event to an agent's inbox triggers execute_turn()."""

    async def test_task_assigned_calls_executor(self) -> None:
        queue = MemoryEventQueue()
        await queue.start()
        llm = MockLLM(response="handled via queue")
        engine = _make_engine(queue, llm=llm)

        await engine.start()
        try:
            # Pick any agent
            agent = engine.agent_pool.agents[0]
            topic = f"crewlet.agent.{agent.handle}.inbox"

            # Patch execute_turn to track calls
            assert engine.turn_engine is not None
            execute_calls: list[tuple[AgentInstance, Event | None]] = []

            async def mock_execute(
                agent: AgentInstance,
                task_id: str = "",
                task_description: str = "",
                org=None,
                notification_metadata=None,
                *,
                event: Event | None = None,
                **_kwargs: object,
            ) -> str:
                execute_calls.append((agent, event))
                return "mocked"

            engine.turn_engine.run_turn = mock_execute  # type: ignore[assignment]

            event = Event(
                type="task_assigned",
                source="test",
                payload={
                    "task_id": "task-123",
                    "task_description": "Do something",
                },
            )
            await queue.publish(topic, event)

            # Give the dispatch loop time to process
            await asyncio.sleep(0.2)

            assert len(execute_calls) == 1
            called_agent, called_event = execute_calls[0]
            assert called_agent.id == agent.id
            assert called_event is not None
            assert called_event.type == "task_assigned"
            assert called_event.payload["task_id"] == "task-123"
        finally:
            await engine.stop()

    async def test_unknown_event_type_does_not_call_executor(self) -> None:
        queue = MemoryEventQueue()
        await queue.start()
        engine = _make_engine(queue)

        await engine.start()
        try:
            agent = engine.agent_pool.agents[0]
            topic = f"crewlet.agent.{agent.handle}.inbox"

            assert engine.turn_engine is not None
            execute_calls: list[Event] = []

            async def mock_execute(
                agent,
                task_id="",
                task_description="",
                org=None,
                notification_metadata=None,
                *,
                event=None,
            ) -> str:
                execute_calls.append(event)
                return "mocked"

            engine.turn_engine.run_turn = mock_execute  # type: ignore[assignment]

            event = Event(type="unknown_type", source="test")
            await queue.publish(topic, event)
            await asyncio.sleep(0.2)

            assert len(execute_calls) == 0
        finally:
            await engine.stop()


class TestConcurrencyControllerLimitsHandlers:
    """ConcurrencyController limits simultaneous handler execution."""

    async def test_concurrency_limits_parallel_handlers(self) -> None:
        """Invoke two agent handlers concurrently and verify only one
        LLM call runs at a time (max_concurrent=1).

        MemoryEventQueue dispatches handlers sequentially, so we call
        the handlers directly via ``asyncio.gather`` to exercise real
        concurrent scheduling against the ConcurrencyController.
        """
        queue = MemoryEventQueue()
        await queue.start()

        # Track concurrent LLM calls to verify serialisation.
        concurrent_count = 0
        max_concurrent_seen = 0
        lock = asyncio.Lock()

        class SlowLLM:
            """LLM that sleeps, letting us measure concurrency."""

            def __init__(self) -> None:
                self.calls: list[list[Message]] = []

            async def complete(
                self,
                messages: list[Message],
                tools: list[ToolDef] | None = None,
                temperature: float = 0.7,
                max_tokens: int | None = None,
                tool_choice: str | None = None,
            ) -> Completion:
                nonlocal concurrent_count, max_concurrent_seen
                async with lock:
                    concurrent_count += 1
                    max_concurrent_seen = max(max_concurrent_seen, concurrent_count)
                await asyncio.sleep(0.15)
                async with lock:
                    concurrent_count -= 1
                self.calls.append(messages)
                return Completion(content="done")

            async def stream(
                self,
                messages: list[Message],
                tools: list[ToolDef] | None = None,
            ) -> AsyncIterator[CompletionChunk]:
                yield CompletionChunk(content="streamed")

        engine = Engine(
            organization=_make_org(),
            llm_providers={"default": SlowLLM()},
            event_queue=queue,
            max_concurrent=1,  # Only 1 agent at a time
        )

        await engine.start()
        try:
            agents = engine.agent_pool.agents
            assert len(agents) >= 2

            # Build handlers and events, then invoke concurrently
            # so that both handlers race for the semaphore.
            coros = []
            for agent in agents:
                handler = engine._make_agent_handler(agent)
                ev = Event(
                    type="task_assigned",
                    source="test",
                    payload={
                        "task_id": f"task-{agent.handle}",
                        "task_description": f"work for {agent.handle}",
                    },
                )
                coros.append(handler([ev]))

            await asyncio.gather(*coros)

            assert max_concurrent_seen == 1, (
                f"Expected max 1 concurrent, saw {max_concurrent_seen}"
            )
        finally:
            await engine.stop()


class TestNotificationTriggersExecuteTurn:
    """Publishing a notification event to an agent's inbox triggers a turn."""

    async def test_notification_calls_executor(self) -> None:
        queue = MemoryEventQueue()
        await queue.start()
        engine = _make_engine(queue)

        await engine.start()
        try:
            agent = engine.agent_pool.agents[0]
            topic = f"crewlet.agent.{agent.handle}.inbox"

            assert engine.turn_engine is not None
            execute_calls: list[Event | None] = []

            async def mock_execute(
                agent,
                task_id="",
                task_description="",
                org=None,
                notification_metadata=None,
                *,
                event=None,
            ) -> str:
                execute_calls.append(event)
                return "notified"

            engine.turn_engine.run_turn = mock_execute  # type: ignore[assignment]

            event = Event(
                type="notification",
                source="external",
                payload={
                    "task_id": "notif-task-1",
                    "task_description": "Handle incoming webhook",
                },
            )
            await queue.publish(topic, event)
            await asyncio.sleep(0.2)

            assert len(execute_calls) == 1
            assert execute_calls[0] is not None
            assert execute_calls[0].type == "notification"
        finally:
            await engine.stop()


class TestExecutorEventParameter:
    """execute_turn() extracts task details from the Event payload."""

    async def test_event_payload_provides_task_details(self) -> None:
        queue = MemoryEventQueue()
        await queue.start()
        llm = MockLLM(response="completed from event")
        engine = _make_engine(queue, llm=llm)

        await engine.start()
        try:
            assert engine.turn_engine is not None
            agent = engine.agent_pool.agents[0]
            topic = f"crewlet.agent.{agent.handle}.inbox"

            event = Event(
                type="task_assigned",
                source="test",
                payload={
                    "task_id": "evt-task-1",
                    "task_description": "Build feature X",
                },
            )
            await queue.publish(topic, event)
            await asyncio.sleep(0.3)

            # Verify the LLM was called (the turn engine ran a turn)
            assert len(llm.calls) >= 1
            # The task description should appear in at least one
            # phase's input messages (Plan carries it verbatim in the
            # user message).
            found = any(
                "Build feature X" in (m.content or "")
                for call in llm.calls
                for m in call
            )
            assert found, (
                "Task description from event payload not found in LLM messages"
            )
        finally:
            await engine.stop()

    async def test_explicit_args_override_event(self) -> None:
        """When both explicit args and event are given, explicit wins."""
        from crewlet.agent.definition import AgentDefinition
        from crewlet.agent.turn import TurnEngine
        from crewlet.queue.memory import MemoryEventQueue
        from crewlet.tools.registry import ToolRegistry

        event_queue = MemoryEventQueue()
        await event_queue.start()
        try:
            llm = MockLLM()
            turn_engine = TurnEngine(
                llm_providers={"default": llm},
                tool_registry=ToolRegistry(),
                event_queue=event_queue,
            )

            org = _make_org()
            role = org.get_role("Dev")
            assert role is not None
            defn = AgentDefinition(role=role, org=org)
            agent = AgentInstance(defn, handle="dev")
            agent.activate()

            event = Event(
                type="task_assigned",
                source="test",
                payload={
                    "task_id": "event-id",
                    "task_description": "event desc",
                },
            )

            await turn_engine.run_turn(
                agent,
                task_id="explicit-id",
                task_description="explicit desc",
                event=event,
            )

            # Explicit args take priority — "explicit desc" should be
            # in the messages (in the Plan phase's user message), not
            # "event desc"
            assert len(llm.calls) >= 1
            found = any(
                "explicit desc" in (m.content or "") for call in llm.calls for m in call
            )
            assert found
        finally:
            await event_queue.stop()


class TestInboxCoalescing:
    """Multi-event same-conversation partitions trigger ONE digest turn."""

    @staticmethod
    def _slack_notification(
        agent_handle: str, *, sender: str, salient: str, ts: str
    ) -> Event:
        from crewlet.events.types import ExternalNotification

        return ExternalNotification(
            notification_source="slack",
            source_event_type="message",
            agent_id=agent_handle,
            sender=sender,
            subject="Slack message",
            body=f"enriched prompt: {salient}",
            salient_body=salient,
            metadata={
                "channel": "C42",
                "thread_ts": "1718.001",
                "ts": ts,
                "slack_user_id": f"U-{sender}",
                "event_type": "message",
            },
        )

    async def test_batched_partition_runs_one_turn_with_digest(self) -> None:
        queue = MemoryEventQueue()
        await queue.start()
        engine = _make_engine(queue)

        await engine.start()
        try:
            agent = engine.agent_pool.agents[0]
            assert engine.turn_engine is not None
            turn_events: list[Event] = []

            async def mock_run_turn(agent, *, event=None, org=None, **kwargs) -> str:
                turn_events.append(event)
                return "mocked"

            engine.turn_engine.run_turn = mock_run_turn  # type: ignore[assignment]

            handler = engine._make_agent_handler(agent)
            events = [
                self._slack_notification(
                    agent.handle, sender="Alice", salient="first", ts="1718.002"
                ),
                self._slack_notification(
                    agent.handle, sender="Bob", salient="second", ts="1718.003"
                ),
            ]
            await handler(events)

            assert len(turn_events) == 1, "two same-thread events must be ONE turn"
            trigger = turn_events[0]
            assert trigger.type == "external_notification"
            assert "## Coalesced updates (2 events" in trigger.body
            assert "first" in trigger.body
            assert [m.sender for m in trigger.messages] == ["Alice", "Bob"]

            # The merge is observable: a NotificationsCoalesced event
            # was published for the dashboard / event store.
            coalesced = [
                e for e in queue.history if e.type == "notifications_coalesced"
            ]
            assert len(coalesced) == 1
            assert coalesced[0].count == 2
            assert coalesced[0].conversation_key == "slack:C42:1718.001"
        finally:
            await engine.stop()

    async def test_lingered_publishes_coalesce_end_to_end(self) -> None:
        """Through the real queue path: a linger window merges burst
        publishes into one turn."""
        queue = MemoryEventQueue()
        await queue.start()
        engine = _make_engine(queue)
        engine._inbox_batch_options.linger_seconds = 0.05

        await engine.start()
        try:
            agent = engine.agent_pool.agents[0]
            topic = f"crewlet.agent.{agent.handle}.inbox"
            assert engine.turn_engine is not None
            turn_events: list[Event] = []

            async def mock_run_turn(agent, *, event=None, org=None, **kwargs) -> str:
                turn_events.append(event)
                return "mocked"

            engine.turn_engine.run_turn = mock_run_turn  # type: ignore[assignment]

            for i, sender in enumerate(("Alice", "Bob", "Carol")):
                await queue.publish(
                    topic,
                    self._slack_notification(
                        agent.handle,
                        sender=sender,
                        salient=f"msg {i}",
                        ts=f"1718.00{i + 2}",
                    ),
                )
            await asyncio.sleep(0.2)

            assert len(turn_events) == 1
            assert len(turn_events[0].messages) == 3
        finally:
            await engine.stop()

    async def test_distinct_conversations_stay_separate_turns(self) -> None:
        queue = MemoryEventQueue()
        await queue.start()
        engine = _make_engine(queue)

        await engine.start()
        try:
            agent = engine.agent_pool.agents[0]
            assert engine.turn_engine is not None
            turn_events: list[Event] = []

            async def mock_run_turn(agent, *, event=None, org=None, **kwargs) -> str:
                turn_events.append(event)
                return "mocked"

            engine.turn_engine.run_turn = mock_run_turn  # type: ignore[assignment]

            handler = engine._make_agent_handler(agent)
            thread_a = self._slack_notification(
                agent.handle, sender="Alice", salient="a", ts="1.2"
            )
            thread_b = self._slack_notification(
                agent.handle, sender="Bob", salient="b", ts="9.9"
            )
            thread_b.metadata["thread_ts"] = "9.1"  # different conversation

            # The queue partitions by key, so each conversation arrives
            # as its own handler call.
            await handler([thread_a])
            await handler([thread_b])

            assert len(turn_events) == 2
            assert all(not e.messages for e in turn_events)  # no digests
        finally:
            await engine.stop()


class TestInboxCoalescingDegrade:
    """Unmergeable partitions degrade to per-event dispatch, not DLQ."""

    async def test_coalesce_failure_falls_back_to_per_event_dispatch(self) -> None:
        """A malformed constituent (naive timestamp from an extension
        producer) breaks the merge sort; the partition must degrade to
        per-event turns instead of failing wholesale."""
        from datetime import datetime

        queue = MemoryEventQueue()
        await queue.start()
        engine = _make_engine(queue)

        await engine.start()
        try:
            agent = engine.agent_pool.agents[0]
            assert engine.turn_engine is not None
            turn_events: list[Event] = []

            async def mock_run_turn(agent, *, event=None, org=None, **kwargs) -> str:
                turn_events.append(event)
                return "mocked"

            engine.turn_engine.run_turn = mock_run_turn  # type: ignore[assignment]

            good = TestInboxCoalescing._slack_notification(
                agent.handle, sender="Alice", salient="first", ts="1718.002"
            )
            bad = TestInboxCoalescing._slack_notification(
                agent.handle, sender="Bob", salient="second", ts="1718.003"
            )
            bad.timestamp = datetime(2026, 6, 10, 12, 0, 0)  # naive — unsortable

            handler = engine._make_agent_handler(agent)
            await handler([good, bad])
            # Memory queue requeues inline, so by now both events ran as
            # individual turns — neither was dropped, neither digested.
            assert len(turn_events) == 2
            assert all(not e.messages for e in turn_events)
        finally:
            await engine.stop()

    async def test_heterogeneous_partition_requeues_tail(self) -> None:
        """A mixed-type partition (key-scheme bug) processes the first
        event in this ack scope and requeues the rest as independent
        inbox messages — completed work is never replayed by a later
        event's failure."""
        queue = MemoryEventQueue()
        await queue.start()
        engine = _make_engine(queue)

        await engine.start()
        try:
            agent = engine.agent_pool.agents[0]
            assert engine.turn_engine is not None
            turn_events: list[Event] = []

            async def mock_run_turn(agent, *, event=None, org=None, **kwargs) -> str:
                turn_events.append(event)
                return "mocked"

            engine.turn_engine.run_turn = mock_run_turn  # type: ignore[assignment]

            notification = TestInboxCoalescing._slack_notification(
                agent.handle, sender="Alice", salient="hello", ts="1718.002"
            )
            task = Event(
                type="task_assigned",
                source="test",
                payload={"task_id": "t-1", "task_description": "do the thing"},
            )

            handler = engine._make_agent_handler(agent)
            await handler([notification, task])

            # Both events ran exactly once.  The tail is requeued BEFORE
            # the head dispatches (a requeue failure must abort the
            # partition before any work runs), and the memory queue
            # dispatches the requeued copy inline — so the tail's turn
            # lands first here; on Pulsar it would land on a later drain.
            assert len(turn_events) == 2
            assert {e.type for e in turn_events} == {
                "external_notification",
                "task_assigned",
            }
        finally:
            await engine.stop()

    async def test_requeue_failure_aborts_before_any_turn(self) -> None:
        """Requeue-before-dispatch: when the tail can't be requeued, the
        partition fails BEFORE the head's turn runs — redelivery re-runs
        everything from scratch instead of replaying a completed turn."""
        queue = MemoryEventQueue()
        await queue.start()
        engine = _make_engine(queue)

        await engine.start()
        try:
            agent = engine.agent_pool.agents[0]
            assert engine.turn_engine is not None
            turn_events: list[Event] = []

            async def mock_run_turn(agent, *, event=None, org=None, **kwargs) -> str:
                turn_events.append(event)
                return "mocked"

            engine.turn_engine.run_turn = mock_run_turn  # type: ignore[assignment]

            async def broken_publish(topic: str, event: Event) -> None:
                raise RuntimeError("broker down")

            engine.event_queue.publish = broken_publish  # type: ignore[assignment]

            notification = TestInboxCoalescing._slack_notification(
                agent.handle, sender="Alice", salient="hello", ts="1718.002"
            )
            task = Event(
                type="task_assigned",
                source="test",
                payload={"task_id": "t-1", "task_description": "do the thing"},
            )
            handler = engine._make_agent_handler(agent)

            import pytest

            with pytest.raises(RuntimeError, match="broker down"):
                await handler([notification, task])
            assert turn_events == []  # the head never ran
        finally:
            # Remove the shadowing instance attribute so class lookup
            # resumes — assigning ``queue.publish`` back would re-read
            # the instance dict and reinstall the broken stub, leaving
            # engine.stop()'s teardown publishing against it.
            del engine.event_queue.publish
            await engine.stop()

    async def test_same_id_duplicates_collapse_to_one_turn(self) -> None:
        """At-least-once edges (requeue copy + redelivered original) put
        two copies of one event in a drain — they must dedupe to a
        single turn, not coalesce into a fake 2-event digest or run the
        degrade path."""
        queue = MemoryEventQueue()
        await queue.start()
        engine = _make_engine(queue)

        await engine.start()
        try:
            agent = engine.agent_pool.agents[0]
            assert engine.turn_engine is not None
            turn_events: list[Event] = []

            async def mock_run_turn(agent, *, event=None, org=None, **kwargs) -> str:
                turn_events.append(event)
                return "mocked"

            engine.turn_engine.run_turn = mock_run_turn  # type: ignore[assignment]

            original = TestInboxCoalescing._slack_notification(
                agent.handle, sender="Alice", salient="hello", ts="1718.002"
            )
            copy = original.model_copy(deep=True)

            handler = engine._make_agent_handler(agent)
            await handler([original, copy])

            assert len(turn_events) == 1
            assert not turn_events[0].messages  # no fake digest
        finally:
            await engine.stop()
