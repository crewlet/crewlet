"""Tests for the in-memory EventStore implementation."""

from __future__ import annotations

from datetime import UTC, datetime

import pytest

from crewlet.timescaledb.memory import MemoryEventStore


@pytest.fixture
def store() -> MemoryEventStore:
    return MemoryEventStore(max_events=100)


async def test_write_and_list_events(store: MemoryEventStore) -> None:
    await store.start()
    await store.write_event(
        event_id="evt-1",
        event_type="task_created",
        source="agent-pm",
        timestamp=datetime.now(UTC),
        category="task",
        summary="Task Created: T-1",
    )
    await store.write_event(
        event_id="evt-2",
        event_type="task_completed",
        source="agent-dev",
        timestamp=datetime.now(UTC),
        category="task",
        summary="Task Completed: T-1",
    )
    events = await store.list_events()
    assert len(events) == 2
    # Newest first
    assert events[0]["id"] == "evt-2"
    assert events[1]["id"] == "evt-1"


async def test_list_events_filter_by_type(store: MemoryEventStore) -> None:
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="task_created",
        source="a",
        timestamp=datetime.now(UTC),
    )
    await store.write_event(
        event_id="e2",
        event_type="agent_spawned",
        source="b",
        timestamp=datetime.now(UTC),
    )
    events = await store.list_events(event_type="agent_spawned")
    assert len(events) == 1
    assert events[0]["type"] == "agent_spawned"


async def test_list_events_filter_by_source(store: MemoryEventStore) -> None:
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="task_created",
        source="agent-pm",
        timestamp=datetime.now(UTC),
    )
    await store.write_event(
        event_id="e2",
        event_type="task_created",
        source="agent-dev",
        timestamp=datetime.now(UTC),
    )
    events = await store.list_events(source="agent-dev")
    assert len(events) == 1
    assert events[0]["source"] == "agent-dev"


async def test_get_event(store: MemoryEventStore) -> None:
    await store.start()
    await store.write_event(
        event_id="evt-42",
        event_type="task_created",
        source="pm",
        timestamp=datetime.now(UTC),
        payload={"task_id": "T-1", "title": "Build feature"},
    )
    event = await store.get_event("evt-42")
    assert event is not None
    assert event["id"] == "evt-42"
    assert event["payload"]["task_id"] == "T-1"


async def test_get_event_not_found(store: MemoryEventStore) -> None:
    await store.start()
    assert await store.get_event("nonexistent") is None


async def test_max_events_cap(store: MemoryEventStore) -> None:
    small_store = MemoryEventStore(max_events=5)
    await small_store.start()
    for i in range(10):
        await small_store.write_event(
            event_id=f"e-{i}",
            event_type="test",
            source="test",
            timestamp=datetime.now(UTC),
        )
    events = await small_store.list_events(limit=100)
    assert len(events) == 5
    # Should keep the most recent
    assert events[0]["id"] == "e-9"


async def test_agent_states(store: MemoryEventStore) -> None:
    await store.start()
    now = datetime.now(UTC)

    # Spawn agent
    await store.write_event(
        event_id="e1",
        event_type="agent_spawned",
        source="pm",
        timestamp=now,
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )
    # Start task
    await store.write_event(
        event_id="e2",
        event_type="task_started",
        source="pm",
        timestamp=now,
        tags={"agent_id": "a-1", "task_id": "T-1"},
    )
    # Complete turn with tokens
    await store.write_event(
        event_id="e3",
        event_type="agent_turn_completed",
        source="pm",
        timestamp=now,
        payload={"input_tokens": 100, "output_tokens": 50, "total_tokens": 150},
        tags={"agent_id": "a-1"},
    )

    states = await store.get_agent_states(["PM"])
    assert "PM" in states
    assert states["PM"]["state"] == "working"
    assert states["PM"]["current_task"] == "T-1"
    # Token totals are aggregated by CompositeEventStore via
    # list_token_usage_events; per-store get_agent_states leaves them at zero.
    assert states["PM"]["input_tokens"] == 0
    assert states["PM"]["output_tokens"] == 0
    assert states["PM"]["total_tokens"] == 0


async def test_list_token_usage_events(store: MemoryEventStore) -> None:
    await store.start()
    now = datetime.now(UTC)

    await store.write_event(
        event_id="t1",
        event_type="agent_turn_completed",
        source="pm",
        timestamp=now,
        payload={"input_tokens": 100, "output_tokens": 50, "total_tokens": 150},
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )
    await store.write_event(
        event_id="t2",
        event_type="agent_turn_completed",
        source="pm",
        timestamp=now,
        payload={"input_tokens": 7, "output_tokens": 3, "total_tokens": 10},
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )
    # Non-turn events are ignored.
    await store.write_event(
        event_id="other",
        event_type="task_started",
        source="pm",
        timestamp=now,
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )

    rows = await store.list_token_usage_events()
    assert len(rows) == 2
    by_id = {r["event_id"]: r for r in rows}
    assert by_id["t1"]["agent_id"] == "a-1"
    assert by_id["t1"]["agent_role"] == "PM"
    assert by_id["t1"]["input_tokens"] == 100
    assert by_id["t1"]["output_tokens"] == 50
    assert by_id["t1"]["total_tokens"] == 150
    assert by_id["t2"]["agent_role"] == "PM"
    assert by_id["t2"]["total_tokens"] == 10


async def test_list_phase_token_events(store: MemoryEventStore) -> None:
    """Returns per-phase rows tagged with phase, model, turn and tokens.

    Mirrors the dashboard's Tokens view contract: one row per
    ``agent_phase_completed`` event with everything the per-phase /
    per-model / per-turn rollup needs.
    """
    await store.start()
    now = datetime.now(UTC)

    await store.write_event(
        event_id="p1",
        event_type="agent_phase_completed",
        source="pm",
        timestamp=now,
        payload={
            "phase": "plan",
            "model": "claude-sonnet-4-6",
            "turn_id": "turn-1",
            "iteration": 0,
            "input_tokens": 50,
            "output_tokens": 20,
            "total_tokens": 70,
            "tool_executions": [],
        },
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )
    await store.write_event(
        event_id="p2",
        event_type="agent_phase_completed",
        source="pm",
        timestamp=now,
        payload={
            "phase": "execute",
            "model": "claude-sonnet-4-6",
            "turn_id": "turn-1",
            "iteration": 0,
            "input_tokens": 200,
            "output_tokens": 80,
            "total_tokens": 280,
        },
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )
    # Different role — verifies the ``agent_role`` filter.
    await store.write_event(
        event_id="p3",
        event_type="agent_phase_completed",
        source="dev",
        timestamp=now,
        payload={
            "phase": "plan",
            "model": "gpt-4o-mini",
            "turn_id": "turn-2",
            "input_tokens": 10,
            "output_tokens": 5,
            "total_tokens": 15,
        },
        tags={"agent_id": "a-2", "agent_role": "Dev"},
    )
    # Non-phase event ignored.
    await store.write_event(
        event_id="t1",
        event_type="agent_turn_completed",
        source="pm",
        timestamp=now,
        payload={"input_tokens": 250, "output_tokens": 100, "total_tokens": 350},
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )

    all_rows = await store.list_phase_token_events()
    assert len(all_rows) == 3
    by_id = {r["event_id"]: r for r in all_rows}
    assert by_id["p1"]["phase"] == "plan"
    assert by_id["p1"]["total_tokens"] == 70
    assert by_id["p2"]["phase"] == "execute"
    assert by_id["p2"]["total_tokens"] == 280
    assert by_id["p3"]["model"] == "gpt-4o-mini"

    pm_rows = await store.list_phase_token_events(agent_role="PM")
    assert len(pm_rows) == 2
    assert {r["event_id"] for r in pm_rows} == {"p1", "p2"}


async def test_agent_llm_history(store: MemoryEventStore) -> None:
    await store.start()
    now = datetime.now(UTC)

    await store.write_event(
        event_id="e1",
        event_type="agent_turn_completed",
        source="dev",
        timestamp=now,
        payload={
            "model": "gpt-4o",
            "prompt": "Build feature X",
            "response": "Done.",
            "input_tokens": 200,
            "output_tokens": 100,
            "total_tokens": 300,
            "tool_executions": [{"name": "write_code"}],
        },
        tags={"agent_id": "a-2"},
    )

    history = await store.get_agent_llm_history("a-2")
    assert len(history) == 1
    assert history[0]["model"] == "gpt-4o"
    assert history[0]["input_tokens"] == 200
    assert history[0]["tool_executions"] == [{"name": "write_code"}]
    assert history[0]["phase"] == "turn"


async def test_agent_llm_history_includes_phase_events(
    store: MemoryEventStore,
) -> None:
    """Regression: each ``agent_phase_completed`` event must surface as
    its own history row so the dashboard can show Plan / Execute /
    Review detail.  Without this the dashboard only showed the turn
    aggregate with a generic ``default`` model label.
    """
    await store.start()
    now = datetime.now(UTC)

    await store.write_event(
        event_id="phase-plan",
        event_type="agent_phase_completed",
        source="dev",
        timestamp=now,
        payload={
            "phase": "plan",
            "turn_id": "turn-1",
            "iteration": 1,
            "model": "sonnet",
            "provider_key": "plan",
            "system_prompt": "You are a planner.",
            "user_prompt": "Answer the slack message.",
            "response": '{"decision":"direct"}',
            "input_tokens": 100,
            "output_tokens": 20,
            "total_tokens": 120,
            "tool_executions": [],
            "decision": "direct",
            "notes": "",
            "tools_available": ["submit_plan", "activate_tool"],
            "tool_catalogue": ["slack_message", "jira_comment"],
        },
        tags={"agent_id": "a-9"},
    )
    await store.write_event(
        event_id="phase-execute",
        event_type="agent_phase_completed",
        source="dev",
        timestamp=now,
        payload={
            "phase": "execute",
            "turn_id": "turn-1",
            "iteration": 1,
            "model": "haiku",
            "provider_key": "execute",
            "system_prompt": "You are an executor.",
            "user_prompt": "Answer the slack message.",
            "response": "Hi there!",
            "input_tokens": 50,
            "output_tokens": 10,
            "total_tokens": 60,
            "tool_executions": [{"name": "slack_message"}],
            "decision": "",
            "notes": "",
            "tools_available": ["slack_message", "query_knowledge"],
        },
        tags={"agent_id": "a-9"},
    )

    history = await store.get_agent_llm_history("a-9")
    assert len(history) == 2
    phases = {h["phase"] for h in history}
    assert phases == {"plan", "execute"}

    plan_row = next(h for h in history if h["phase"] == "plan")
    assert plan_row["model"] == "sonnet"
    assert plan_row["decision"] == "direct"
    assert plan_row["turn_id"] == "turn-1"
    # system_prompt + user_prompt are flattened into prompt_messages so
    # the dashboard renders them with existing prompt-section code.
    assert plan_row["prompt_messages"] == [
        {"role": "system", "content": "You are a planner."},
        {"role": "user", "content": "Answer the slack message."},
    ]

    execute_row = next(h for h in history if h["phase"] == "execute")
    assert execute_row["tool_executions"] == [{"name": "slack_message"}]
    # Plan only ships meta-tool schemas in tools=[...]; the catalogue
    # (name-only, no schema) goes in the prompt and is reported
    # separately so the dashboard doesn't misrepresent it as schemas.
    assert plan_row["tools_available"] == ["submit_plan", "activate_tool"]
    assert plan_row["tool_catalogue"] == ["slack_message", "jira_comment"]
    # Execute has no catalogue -- it gets the actual tool schemas.
    assert execute_row["tools_available"] == ["slack_message", "query_knowledge"]
    assert execute_row["tool_catalogue"] == []


async def test_agent_llm_history_judge_event_carries_host_phase_fields(
    store: MemoryEventStore,
) -> None:
    """``phase="judge"`` events from the extension judge must carry
    ``host_phase`` + ``host_iteration`` through the flattener so the
    dashboard can group them under the phase they belong to.

    These fields must surface in the LLM-history projection, not just
    on the event model — otherwise the
    dashboard sees them as missing.
    """
    await store.start()
    now = datetime.now(UTC)

    await store.write_event(
        event_id="phase-judge",
        event_type="agent_phase_completed",
        source="dev",
        timestamp=now,
        payload={
            "phase": "judge",
            "host_phase": "execute",
            "host_iteration": 2,
            "turn_id": "turn-1",
            "iteration": 2,
            "model": "haiku",
            "provider_key": "judge",
            "system_prompt": "You are the extension judge.",
            "user_prompt": "Phase: execute ...",
            "response": "extend",
            "input_tokens": 80,
            "output_tokens": 20,
            "total_tokens": 100,
            "tool_executions": [{"name": "submit_extension_decision"}],
            "decision": "extend",
            "notes": "execute: progress",
        },
        tags={"agent_id": "a-judge"},
    )

    history = await store.get_agent_llm_history("a-judge")
    assert len(history) == 1
    row = history[0]
    assert row["phase"] == "judge"
    assert row["host_phase"] == "execute"
    assert row["host_iteration"] == 2
    assert row["decision"] == "extend"


async def test_list_events_filter_by_trace_id(store: MemoryEventStore) -> None:
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="task_created",
        source="pm",
        timestamp=datetime.now(UTC),
        trace_id="trace-aaa",
    )
    await store.write_event(
        event_id="e2",
        event_type="task_started",
        source="dev",
        timestamp=datetime.now(UTC),
        trace_id="trace-bbb",
    )
    events = await store.list_events(trace_id="trace-aaa")
    assert len(events) == 1
    assert events[0]["trace_id"] == "trace-aaa"


async def test_list_trace(store: MemoryEventStore) -> None:
    await store.start()
    tid = "trace-xyz"
    await store.write_event(
        event_id="e1",
        event_type="task_created",
        source="pm",
        timestamp=datetime.now(UTC),
        trace_id=tid,
        span_id="span-1",
    )
    await store.write_event(
        event_id="e2",
        event_type="task_started",
        source="dev",
        timestamp=datetime.now(UTC),
        trace_id=tid,
        span_id="span-2",
        parent_span_id="span-1",
    )
    await store.write_event(
        event_id="e3",
        event_type="other",
        source="other",
        timestamp=datetime.now(UTC),
        trace_id="different-trace",
    )
    trace = await store.list_trace(tid)
    assert len(trace) == 2
    # Oldest first
    assert trace[0]["id"] == "e1"
    assert trace[1]["id"] == "e2"
    assert trace[1]["parent_span_id"] == "span-1"


async def test_actor_field_stored(store: MemoryEventStore) -> None:
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="task_created",
        source="pm",
        timestamp=datetime.now(UTC),
        actor="Project Manager",
    )
    events = await store.list_events()
    assert events[0]["actor"] == "Project Manager"

    event = await store.get_event("e1")
    assert event is not None
    assert event["actor"] == "Project Manager"


async def test_list_events_filter_by_actor(store: MemoryEventStore) -> None:
    """The ``actor`` filter returns only events produced by the named agent."""
    await store.start()
    now = datetime.now(UTC)
    await store.write_event(
        event_id="e1",
        event_type="task_started",
        source="pm",
        timestamp=now,
        actor="PM",
    )
    await store.write_event(
        event_id="e2",
        event_type="task_started",
        source="cto",
        timestamp=now,
        actor="CTO",
    )
    events = await store.list_events(actor="CTO")
    assert len(events) == 1
    assert events[0]["id"] == "e2"


async def test_list_events_filter_by_related_agent_direct_match(
    store: MemoryEventStore,
) -> None:
    """Matches events where the agent name appears in actor or known tags."""
    await store.start()
    now = datetime.now(UTC)
    await store.write_event(
        event_id="e1",
        event_type="agent_turn_completed",
        source="cto",
        timestamp=now,
        actor="CTO",  # actor match
    )
    await store.write_event(
        event_id="e2",
        event_type="external_notification",
        source="webhook",
        timestamp=now,
        tags={"target": "CTO"},  # tag match (target)
    )
    await store.write_event(
        event_id="e3",
        event_type="task_started",
        source="pm",
        timestamp=now,
        actor="PM",  # unrelated
    )
    events = await store.list_events(related_agent="CTO")
    ids = {e["id"] for e in events}
    assert ids == {"e1", "e2"}


async def test_list_events_filter_by_related_agent_includes_trace_siblings(
    store: MemoryEventStore,
) -> None:
    """Trace-aware: unrelated events in the same trace are also returned."""
    await store.start()
    now = datetime.now(UTC)
    # A trace with: webhook (→CTO) + CTO's turn + a system log
    await store.write_event(
        event_id="webhook",
        event_type="external_notification",
        source="webhook",
        timestamp=now,
        trace_id="tr-1",
        tags={"target": "CTO"},
    )
    await store.write_event(
        event_id="turn",
        event_type="agent_turn_completed",
        source="cto",
        timestamp=now,
        trace_id="tr-1",
        actor="CTO",
    )
    await store.write_event(
        event_id="log",
        event_type="task_started",
        source="system",
        timestamp=now,
        trace_id="tr-1",
        actor="system",  # not directly matching CTO, but shares trace
    )
    # Unrelated trace
    await store.write_event(
        event_id="other",
        event_type="task_started",
        source="pm",
        timestamp=now,
        trace_id="tr-2",
        actor="PM",
    )
    events = await store.list_events(related_agent="CTO")
    ids = {e["id"] for e in events}
    assert ids == {"webhook", "turn", "log"}
    assert "other" not in ids


async def test_list_events_related_agent_response_strips_tags(
    store: MemoryEventStore,
) -> None:
    """The public response must never include the internal ``tags`` key."""
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="task_started",
        source="cto",
        timestamp=datetime.now(UTC),
        actor="CTO",
        tags={"agent_role": "CTO"},
    )
    events = await store.list_events(related_agent="CTO")
    assert len(events) == 1
    assert "tags" not in events[0]


async def test_close_clears_state(store: MemoryEventStore) -> None:
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="test",
        source="test",
        timestamp=datetime.now(UTC),
    )
    await store.close()
    events = await store.list_events()
    assert events == []


async def test_get_agent_states_phase_started_sets_current_phase(
    store: MemoryEventStore,
) -> None:
    """``agent_phase_started`` updates ``current_phase`` so the dashboard
    can show which phase a working agent is currently running.

    Cleared on ``task_completed`` / ``task_failed`` (turn ended).
    """
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="agent_spawned",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
    )
    await store.write_event(
        event_id="e2",
        event_type="task_started",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM", "task_id": "T-1"},
    )
    await store.write_event(
        event_id="e3",
        event_type="agent_phase_started",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
        payload={"phase": "plan", "iteration": 1},
    )

    states = await store.get_agent_states(["PM"])
    assert states["PM"]["state"] == "working"
    assert states["PM"]["current_phase"] == "plan"
    assert states["PM"]["current_iteration"] == 1

    # Phase progresses to execute -> review with iteration tagging.
    await store.write_event(
        event_id="e4",
        event_type="agent_phase_started",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
        payload={"phase": "execute", "iteration": 1},
    )
    states = await store.get_agent_states(["PM"])
    assert states["PM"]["current_phase"] == "execute"

    # Turn ends -> current_phase clears so the dashboard doesn't show
    # a stale "in review" chip on an idle agent.
    await store.write_event(
        event_id="e5",
        event_type="task_completed",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
    )
    states = await store.get_agent_states(["PM"])
    assert states["PM"]["state"] == "idle"
    assert states["PM"]["current_phase"] is None
    assert states["PM"]["current_iteration"] == 0


async def test_get_agent_states_aux_phase_keeps_agent_working(
    store: MemoryEventStore,
) -> None:
    """Auxiliary phase events DO drive state to ``working`` so the
    dashboard shows the agent as live during reflection.

    The trailing ``reflection_completed`` sentinel (covered by the
    next test) flips the agent back to idle when the pass finishes.
    """
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="agent_spawned",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
    )
    await store.write_event(
        event_id="e2",
        event_type="task_started",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM", "task_id": "T-1"},
    )
    await store.write_event(
        event_id="e3",
        event_type="task_completed",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
    )
    # PersistDecider aux phase event — keeps the agent "working".
    await store.write_event(
        event_id="e4",
        event_type="agent_phase_completed",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
        payload={"phase": "auxiliary", "worker": "persist_decider"},
    )

    states = await store.get_agent_states(["PM"])
    assert states["PM"]["state"] == "working"


async def test_get_agent_states_reflection_completed_flips_to_idle(
    store: MemoryEventStore,
) -> None:
    """``reflection_completed`` is the sentinel emitted by ReflectEngine
    when its worker dispatch finishes; it flips the agent back to idle
    after any aux phase events the workers produced."""
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="agent_spawned",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
    )
    await store.write_event(
        event_id="e2",
        event_type="task_completed",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
    )
    await store.write_event(
        event_id="e3",
        event_type="agent_phase_completed",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
        payload={"phase": "auxiliary", "worker": "persist_decider"},
    )
    await store.write_event(
        event_id="e4",
        event_type="reflection_completed",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
    )

    states = await store.get_agent_states(["PM"])
    assert states["PM"]["state"] == "idle"
    assert states["PM"]["current_task"] is None


async def test_get_agent_states_main_phase_still_drives_state(
    store: MemoryEventStore,
) -> None:
    """A non-auxiliary ``agent_phase_completed`` (i.e. plan / execute /
    review) DOES flip the agent to ``working`` — same as the aux-phase
    case, just from a different code path.  The ``payload.phase`` is
    not consulted any more by the in-memory store."""
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="agent_spawned",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
    )
    await store.write_event(
        event_id="e2",
        event_type="agent_phase_completed",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
        payload={"phase": "plan"},
    )

    states = await store.get_agent_states(["PM"])
    assert states["PM"]["state"] == "working"


async def test_get_agent_states_guard_breach_flips_to_afk_with_reason(
    store: MemoryEventStore,
) -> None:
    """``turn.guard_breach`` flips the agent to ``afk`` and the
    payload's ``kind`` surfaces as ``afk_reason`` for the dashboard
    quip."""
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="agent_spawned",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
    )
    await store.write_event(
        event_id="e2",
        event_type="turn.guard_breach",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
        payload={"kind": "stall", "detail": "x"},
    )

    states = await store.get_agent_states(["PM"])
    assert states["PM"]["state"] == "afk"
    assert states["PM"]["afk_reason"] == "stall"


async def test_get_agent_states_llm_unavailable_uses_event_type_as_reason(
    store: MemoryEventStore,
) -> None:
    """``llm_unavailable`` (no ``kind`` field) falls back to the event
    type itself as the AFK reason."""
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="agent_spawned",
        source="Eng",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "eng-1", "agent_role": "Eng"},
    )
    await store.write_event(
        event_id="e2",
        event_type="llm_unavailable",
        source="Eng",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "eng-1", "agent_role": "Eng"},
        payload={"provider_chain": ["openai"], "attempt_count": 1},
    )

    states = await store.get_agent_states(["Eng"])
    assert states["Eng"]["state"] == "afk"
    assert states["Eng"]["afk_reason"] == "llm_unavailable"


async def test_get_agent_states_afk_cleared_after_normal_event(
    store: MemoryEventStore,
) -> None:
    """A subsequent lifecycle event flips the agent back and drops
    the stale AFK reason — the dashboard chip disappears automatically."""
    await store.start()
    await store.write_event(
        event_id="e1",
        event_type="agent_spawned",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
    )
    await store.write_event(
        event_id="e2",
        event_type="turn.guard_breach",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM"},
        payload={"kind": "stall"},
    )
    await store.write_event(
        event_id="e3",
        event_type="task_started",
        source="PM",
        timestamp=datetime.now(UTC),
        tags={"agent_id": "pm-1", "agent_role": "PM", "task_id": "t-99"},
    )

    states = await store.get_agent_states(["PM"])
    assert states["PM"]["state"] == "working"
    assert "afk_reason" not in states["PM"]


# --- Keyset paging ----------------------------------------------------
#
# The in-memory store has to answer a cursor exactly as the persistent
# one does, or the same dashboard reads differently on the two backends
# — and it reads differently by silently skipping rows, which is the
# kind of wrong nobody reports.


async def _seed(store: MemoryEventStore, n: int, *, base_minute: int = 0) -> None:
    await store.start()
    for i in range(n):
        await store.write_event(
            event_id=f"e{i:02d}",
            event_type="task_created",
            source="pm",
            timestamp=datetime(2026, 4, 1, 12, base_minute + i, tzinfo=UTC),
            category="task",
            summary=f"event {i}",
        )


async def test_a_cursor_returns_only_older_rows(store: MemoryEventStore) -> None:
    await _seed(store, 6)
    page = await store.list_events(limit=3)
    assert [e["id"] for e in page] == ["e05", "e04", "e03"]

    oldest = page[-1]
    nxt = await store.list_events(limit=3, before=(oldest["timestamp"], oldest["id"]))
    assert [e["id"] for e in nxt] == ["e02", "e01", "e00"]


async def test_the_cursor_is_exclusive(store: MemoryEventStore) -> None:
    """The row a caller already holds must not come back again."""
    await _seed(store, 3)
    page = await store.list_events(limit=3)
    oldest = page[-1]
    nxt = await store.list_events(before=(oldest["timestamp"], oldest["id"]))
    assert oldest["id"] not in [e["id"] for e in nxt]


async def test_rows_sharing_a_timestamp_are_broken_by_id(
    store: MemoryEventStore,
) -> None:
    """Burst writes share a timestamp at microsecond resolution.

    A cursor over a non-unique key skips or repeats whatever collided
    with it, and the reader gets no error either way.
    """
    await store.start()
    same = datetime(2026, 4, 1, 12, 0, tzinfo=UTC)
    for eid in ("a", "b", "c"):
        await store.write_event(
            event_id=eid,
            event_type="task_created",
            source="pm",
            timestamp=same,
            category="task",
            summary=eid,
        )
    first = await store.list_events(limit=1)
    assert first[0]["id"] == "c"
    rest = await store.list_events(before=(same, "c"))
    assert [e["id"] for e in rest] == ["b", "a"]


async def test_order_is_by_time_not_by_insertion(store: MemoryEventStore) -> None:
    """Backfilled writes are real: a webhook replay, a gap re-read.

    Ordering by insertion puts them at the head, which under a cursor
    shows up as rows appearing above rows already scrolled past.
    """
    await store.start()
    for eid, minute in (("late", 5), ("early", 1), ("middle", 3)):
        await store.write_event(
            event_id=eid,
            event_type="task_created",
            source="pm",
            timestamp=datetime(2026, 4, 1, 12, minute, tzinfo=UTC),
            category="task",
            summary=eid,
        )
    assert [e["id"] for e in await store.list_events()] == [
        "late",
        "middle",
        "early",
    ]


async def test_naive_and_aware_timestamps_order_together(
    store: MemoryEventStore,
) -> None:
    """``write_event`` stores whatever encoding its caller passed.

    Compared as raw strings, ``"...+00:00"`` sorts after the same
    instant written naive, so the two interleave wrongly.
    """
    await store.start()
    await store.write_event(
        event_id="aware",
        event_type="task_created",
        source="pm",
        timestamp=datetime(2026, 4, 1, 12, 5, tzinfo=UTC),
        category="task",
        summary="aware",
    )
    await store.write_event(
        event_id="naive",
        event_type="task_created",
        source="pm",
        timestamp=datetime(2026, 4, 1, 12, 9),
        category="task",
        summary="naive",
    )
    assert [e["id"] for e in await store.list_events()] == ["naive", "aware"]


async def test_a_short_page_is_the_end(store: MemoryEventStore) -> None:
    await _seed(store, 2)
    assert len(await store.list_events(limit=10)) == 2


async def test_filter_by_category(store: MemoryEventStore) -> None:
    """The Activity view pushes its category pills into the query.

    Filtering a paged list client-side silently excludes: a 50-row page
    holding 2 matches reads as "only 2 exist".
    """
    await store.start()
    for eid, category in (("t", "task"), ("s", "system"), ("t2", "task")):
        await store.write_event(
            event_id=eid,
            event_type="x",
            source="pm",
            timestamp=datetime(2026, 4, 1, 12, 0, tzinfo=UTC),
            category=category,
            summary=eid,
        )
    rows = await store.list_events(category="task")
    assert {e["id"] for e in rows} == {"t", "t2"}


async def test_every_row_carries_the_failure_flag(store: MemoryEventStore) -> None:
    """One rule for failure, whichever backend answered."""
    await store.start()
    await store.write_event(
        event_id="dead",
        event_type="agent_phase_completed",
        source="pm",
        timestamp=datetime(2026, 4, 1, 12, 1, tzinfo=UTC),
        category="system",
        summary="died",
        tags={"failed": "true"},
    )
    await store.write_event(
        event_id="typed",
        event_type="llm_unavailable",
        source="pm",
        timestamp=datetime(2026, 4, 1, 12, 2, tzinfo=UTC),
        category="system",
        summary="no provider",
    )
    await store.write_event(
        event_id="fine",
        event_type="agent_phase_completed",
        source="pm",
        timestamp=datetime(2026, 4, 1, 12, 3, tzinfo=UTC),
        category="system",
        summary="ok",
    )
    flags = {e["id"]: e["failed"] for e in await store.list_events()}
    assert flags == {"dead": True, "typed": True, "fine": False}


async def test_a_trace_is_oldest_first(store: MemoryEventStore) -> None:
    """A trace is read as a causal sequence, so it runs the other way."""
    await store.start()
    for eid, minute in (("third", 3), ("first", 1), ("second", 2)):
        await store.write_event(
            event_id=eid,
            event_type="task_created",
            source="pm",
            timestamp=datetime(2026, 4, 1, 12, minute, tzinfo=UTC),
            category="task",
            summary=eid,
            trace_id="tr-1",
        )
    assert [e["id"] for e in await store.list_trace("tr-1")] == [
        "first",
        "second",
        "third",
    ]
