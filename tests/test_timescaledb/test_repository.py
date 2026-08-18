"""Unit tests for the TimescaleDB repository's SQL wiring.

These tests use a lightweight ``FakeDatabase`` stand-in instead of a
real PostgreSQL server.  They verify the SQL the repository emits and
the parameters it passes — behaviour that's independent of the engine
version and easy to regress.

End-to-end validation against a real TimescaleDB server is done
separately as part of the review workflow; this file keeps the unit
tests fast and hermetic.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any

import pytest

from crewlet.timescaledb.repository import (
    EVENTS_TABLE,
    TimescaleDBEventStore,
    _event_to_state,
)


class FakeDatabase:
    """Records every query the repository issues.

    ``execute``/``fetchrow``/``fetchval`` all collect calls in the
    same ``queries`` list so tests can inspect the SQL and parameters
    in order.  Canned responses can be queued via ``responses`` for
    ``execute``/``fetchrow`` to return.
    """

    def __init__(self) -> None:
        self.queries: list[tuple[str, tuple[Any, ...]]] = []
        self.responses: list[Any] = []

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        self.queries.append((query, args))
        if self.responses:
            resp = self.responses.pop(0)
            if resp is None:
                return []
            return resp
        return []

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        self.queries.append((query, args))
        if self.responses:
            return self.responses.pop(0)
        return None

    async def fetchval(self, query: str, *args: Any) -> Any:
        self.queries.append((query, args))
        if self.responses:
            return self.responses.pop(0)
        return None


@pytest.fixture
def db() -> FakeDatabase:
    return FakeDatabase()


@pytest.fixture
def store(db: FakeDatabase) -> TimescaleDBEventStore:
    return TimescaleDBEventStore(db)  # type: ignore[arg-type]


async def test_start_is_a_noop(store: TimescaleDBEventStore, db: FakeDatabase) -> None:
    """Schema creation is owned by the migration runner, not the store."""
    await store.start()
    assert db.queries == []


async def test_write_event_is_idempotent_on_conflict(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """Duplicate (event_time, event_id) inserts must be silently dropped.

    The PK on
    ``(event_time, event_id)`` would raise ``UniqueViolationError`` on
    retry publishes, and the publish listener's ``except Exception``
    block would then log it as a spurious ``event_write_failed``.
    ``ON CONFLICT DO NOTHING`` makes the write idempotent so
    legitimate retries don't spam the logs.
    """
    await store.write_event(
        event_id="e1",
        event_type="task_started",
        source="pm",
        timestamp=datetime(2026, 4, 12, 12, 0, 0, tzinfo=UTC),
    )
    sql, _ = db.queries[0]
    assert "ON CONFLICT (event_time, event_id) DO NOTHING" in sql


async def test_write_event_inserts_row_with_expected_shape(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    await store.write_event(
        event_id="e1",
        event_type="task_started",
        source="pm",
        timestamp=datetime(2026, 4, 12, 12, 0, 0, tzinfo=UTC),
        category="task",
        payload={"task_id": "T-1"},
        summary="Task started",
        actor="PM",
        trace_id="trace-a",
        span_id="span-a",
        parent_span_id="",
        tags={
            "agent_id": "a-1",
            "agent_role": "PM",
            "task_id": "T-1",
            "requester": "boss",
        },
    )
    assert len(db.queries) == 1
    sql, args = db.queries[0]
    assert f"INSERT INTO {EVENTS_TABLE}" in sql
    # Positional asyncpg parameters (all 17 columns).
    assert len(args) == 17
    time_val, event_id, event_type, source, category = args[:5]
    trace_id, span_id, parent_span_id = args[5:8]
    agent_id, agent_role, task_id, channel_id, sender = args[8:13]
    summary, actor, tags_json, payload_json = args[13:17]

    assert event_id == "e1"
    assert event_type == "task_started"
    assert trace_id == "trace-a"
    assert span_id == "span-a"
    assert parent_span_id == ""
    assert agent_id == "a-1"
    assert agent_role == "PM"
    assert task_id == "T-1"
    assert channel_id == ""
    assert sender == ""
    assert summary == "Task started"
    assert actor == "PM"
    assert source == "pm"
    assert category == "task"
    # Timestamps are aware UTC for asyncpg TIMESTAMPTZ binding.
    assert isinstance(time_val, datetime)
    assert time_val.tzinfo is not None
    # JSONB columns are serialized to JSON strings by the store; the
    # map keeps all tags including those not promoted to columns.
    tag_map = json.loads(tags_json)
    assert tag_map["requester"] == "boss"
    assert tag_map["agent_id"] == "a-1"
    # Payload is serialized JSON.
    assert json.loads(payload_json) == {"task_id": "T-1"}


async def test_list_trace_sql_uses_event_id_tiebreaker(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """Trace queries must be deterministic when events share a timestamp."""
    await store.list_trace("trace-xyz")
    sql, args = db.queries[0]
    assert "trace_id = $1" in sql
    assert "ORDER BY event_time ASC, event_id ASC" in sql
    assert args == ("trace-xyz",)


async def test_list_events_does_not_fetch_payload(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """``list_events`` strips ``payload`` from the response, so the SQL
    must not SELECT it — otherwise we're transferring the full JSONB
    blob over the wire for every listed event only to throw it away.
    """
    await store.list_events()
    sql, _ = db.queries[0]
    # The SELECT clause must not include the payload column.
    select_clause = sql.split("FROM", 1)[0]
    assert "payload" not in select_clause
    # ``tags`` is still fetched so ``collect_related_events`` can filter
    # by tag keys post-query.
    assert "tags" in select_clause


async def test_list_trace_does_not_fetch_payload(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """Same rationale as ``list_events``: ``list_trace`` strips payload."""
    await store.list_trace("trace-abc")
    sql, _ = db.queries[0]
    select_clause = sql.split("FROM", 1)[0]
    assert "payload" not in select_clause


async def test_list_events_parameterizes_filters(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    await store.list_events(event_type="task_started", source="pm", trace_id="t")
    sql, args = db.queries[0]
    assert "event_type = $1" in sql
    assert "source = $2" in sql
    assert "trace_id = $3" in sql
    assert args == ("task_started", "pm", "t")


async def test_list_events_actor_filter_uses_sql_where(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """``actor`` filter must be pushed down to SQL (not post-query)."""
    await store.list_events(actor="Agent CEO", limit=25)
    sql, args = db.queries[0]
    assert "actor = $1" in sql
    assert args == ("Agent CEO",)
    # Without related_agent the fetch_limit must equal the caller's limit.
    assert "LIMIT 25" in sql


async def test_list_events_related_agent_over_fetches(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """``related_agent`` filter requires over-fetching for post-query matching."""
    # Caller asks for 20 rows — the SQL should fetch 100 (5x) to leave
    # enough candidates for ``collect_related_events`` to pick from.
    await store.list_events(related_agent="PM", limit=20)
    sql, _ = db.queries[0]
    # related_agent itself is NOT a SQL filter — it's post-query.
    assert "related_agent" not in sql
    assert "LIMIT 100" in sql


async def test_list_events_related_agent_over_fetch_capped_at_500(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """Over-fetch is capped so a huge limit can't blow up the query."""
    await store.list_events(related_agent="PM", limit=1000)
    sql, _ = db.queries[0]
    assert "LIMIT 500" in sql


async def test_list_events_strips_tags_from_response(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """Public ``list_events`` result must not leak the internal ``tags`` key."""
    db.responses = [
        [
            {
                "event_id": "e1",
                "event_type": "task_started",
                "source": "pm",
                "event_time": None,
                "category": "task",
                "summary": "",
                "actor": "",
                "trace_id": "",
                "span_id": "",
                "parent_span_id": "",
                "tags": {"agent_role": "PM"},
                "payload": {},
            }
        ]
    ]
    events = await store.list_events()
    assert len(events) == 1
    assert "tags" not in events[0]


async def test_list_token_usage_events_sql_shape(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """Token-usage query filters by turn events and respects ``since_days``."""
    await store.list_token_usage_events(since_days=7)
    sql, _ = db.queries[0]
    assert "event_type = 'agent_turn_completed'" in sql
    assert "INTERVAL '7 days'" in sql
    # Selects the dedicated agent_id AND agent_role columns so the
    # composite can aggregate tokens per role without a separate
    # agent_id → role lookup (which loses cross-session history).
    assert "SELECT event_time, event_id, agent_id, agent_role, payload" in sql


async def test_list_token_usage_events_parses_payload(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """Token-usage rows carry event_id, agent_id, agent_role, and tokens."""
    db.responses = [
        [
            {
                "event_time": None,
                "event_id": "turn-1",
                "agent_id": "a-1",
                "agent_role": "PM",
                # asyncpg decodes JSONB to a dict.
                "payload": {
                    "input_tokens": 100,
                    "output_tokens": 50,
                    "total_tokens": 150,
                },
            }
        ]
    ]
    rows = await store.list_token_usage_events()
    assert len(rows) == 1
    assert rows[0]["event_id"] == "turn-1"
    assert rows[0]["agent_id"] == "a-1"
    assert rows[0]["agent_role"] == "PM"
    assert rows[0]["input_tokens"] == 100
    assert rows[0]["output_tokens"] == 50
    assert rows[0]["total_tokens"] == 150


async def test_list_phase_token_events_sql_shape(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """Per-phase token query filters by phase event type and ``since_days``.

    Used by ``/tokens/breakdown`` to roll up LLM spend by phase /
    model / worker / agent / turn for the dashboard's Tokens view.
    """
    await store.list_phase_token_events(since_days=14)
    sql, _ = db.queries[0]
    assert "event_type = 'agent_phase_completed'" in sql
    assert "INTERVAL '14 days'" in sql
    # Selects the dedicated agent_id AND agent_role columns so the
    # aggregator can attribute spend per role without a separate
    # agent_id → role lookup (same rationale as list_token_usage_events).
    assert "SELECT event_time, event_id, agent_id, agent_role, payload" in sql


async def test_list_phase_token_events_filters_by_role(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """Per-phase token query honours ``agent_role`` for per-agent rollups."""
    await store.list_phase_token_events(since_days=7, agent_role="PM")
    sql, args = db.queries[0]
    assert "agent_role = $1" in sql
    assert args == ("PM",)


async def test_list_phase_token_events_parses_payload(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """Phase-token rows carry every field the rollup needs to group by."""
    db.responses = [
        [
            {
                "event_time": None,
                "event_id": "phase-1",
                "agent_id": "a-1",
                "agent_role": "PM",
                "payload": {
                    "phase": "plan",
                    "host_phase": "",
                    "worker": "",
                    "model": "claude-sonnet-4-6",
                    "turn_id": "turn-1",
                    "iteration": 0,
                    "input_tokens": 100,
                    "output_tokens": 50,
                    "total_tokens": 150,
                    "tool_executions": [{"tool_name": "submit_plan"}],
                },
            }
        ]
    ]
    rows = await store.list_phase_token_events()
    assert len(rows) == 1
    row = rows[0]
    assert row["event_id"] == "phase-1"
    assert row["agent_id"] == "a-1"
    assert row["agent_role"] == "PM"
    assert row["phase"] == "plan"
    assert row["model"] == "claude-sonnet-4-6"
    assert row["turn_id"] == "turn-1"
    assert row["input_tokens"] == 100
    assert row["output_tokens"] == 50
    assert row["total_tokens"] == 150
    # Deliberately absent: the breakdown aggregation never reads tool
    # output, and the live projection holds these records resident for
    # its rollup window — an uncapped tool result carried here would be
    # carried in memory for the life of the window.
    assert "tool_executions" not in row


async def test_get_agent_states_leaves_tokens_at_zero(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """Per-store ``get_agent_states`` does not aggregate token counts.

    ``CompositeEventStore`` aggregates via
    ``list_token_usage_events`` to avoid double-counting events that
    are written to both the memory and persistent stores.
    """
    db.responses = [
        # Step 1: latest runtime_id per role.
        [{"agent_role": "PM", "runtime_id": "a-1"}],
        # Step 2: latest state-affecting event per role.
        [{"agent_role": "PM", "latest_type": "task_started", "latest_task": "T-1"}],
    ]
    states = await store.get_agent_states(["PM"])
    assert "PM" in states
    pm = states["PM"]
    assert pm["state"] == "working"
    assert pm["current_task"] == "T-1"
    assert pm["runtime_id"] == "a-1"
    assert pm["input_tokens"] == 0
    assert pm["output_tokens"] == 0
    assert pm["total_tokens"] == 0


async def test_get_agent_states_groups_by_role_deterministically(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """``get_agent_states`` must pick the latest row per role deterministically.

    PostgreSQL doesn't have ClickHouse's ``argMax`` aggregate, so the
    repository uses ``DISTINCT ON (agent_role)`` with an explicit
    ``ORDER BY agent_role, event_time DESC, event_id DESC``.  The
    ``event_id DESC`` tiebreaker is essential — without it, rows
    sharing the same ``event_time`` for the same role would be picked
    arbitrarily by Postgres, breaking determinism.
    """
    db.responses = [
        # Step 1: latest runtime_id per role (one entry per requested role).
        [
            {"agent_role": "PM", "runtime_id": "pm-runtime"},
            {"agent_role": "CTO", "runtime_id": "cto-runtime"},
        ],
        # Step 2: latest state-affecting event per role.
        [
            {"agent_role": "PM", "latest_type": "task_started", "latest_task": "T-1"},
        ],
    ]
    states = await store.get_agent_states(["PM", "CTO"])
    sql_step1, args_step1 = db.queries[0]
    sql_step2, args_step2 = db.queries[1]

    # Step 1 must use DISTINCT ON (agent_role) with the full tiebreaker
    # chain so time ties within a role are broken deterministically.
    assert "DISTINCT ON (agent_role)" in sql_step1
    assert "ORDER BY agent_role, event_time DESC, event_id DESC" in sql_step1
    # Caller-supplied roles get pushed down to the WHERE clause.
    assert "agent_role = ANY($1::text[])" in sql_step1
    assert args_step1 == (["PM", "CTO"],)

    # Step 2 also uses DISTINCT ON with the same tiebreaker.
    assert "DISTINCT ON (agent_role)" in sql_step2
    assert "ORDER BY agent_role, event_time DESC, event_id DESC" in sql_step2
    assert "event_type" in sql_step2

    # Returned dict carries the deterministic runtime_id from step 1
    # and the latest state from step 2.
    assert states["PM"]["runtime_id"] == "pm-runtime"
    assert states["PM"]["state"] == "working"
    assert states["PM"]["current_task"] == "T-1"
    assert states["CTO"]["runtime_id"] == "cto-runtime"
    # No step-2 row for CTO → state stays at the initial idle.
    assert states["CTO"]["state"] == "idle"


async def test_get_agent_states_empty_roles_returns_empty(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """An empty ``agent_roles`` list short-circuits without hitting the DB."""
    states = await store.get_agent_states([])
    assert states == {}
    assert db.queries == []


async def test_write_event_populates_tags_json_column(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """Tags that aren't promoted to dedicated columns still land in the JSONB."""
    await store.write_event(
        event_id="e1",
        event_type="external_notification",
        source="webhook",
        timestamp=datetime(2026, 4, 12, tzinfo=UTC),
        tags={
            "agent_id": "a-1",
            "agent_role": "CTO",
            "target": "CTO",  # promoted-column set is (agent_id, agent_role,
            "requester": "boss",  # task_id, channel_id, sender) — the rest
            "sender": "ali",  # stay in the JSONB for related_agent matching
        },
    )
    _, args = db.queries[0]
    tags_json = args[15]
    tag_map = json.loads(tags_json)
    # Tags JSONB retains EVERY key, including those promoted to columns,
    # so post-query ``collect_related_events`` can see the full picture.
    assert tag_map == {
        "agent_id": "a-1",
        "agent_role": "CTO",
        "target": "CTO",
        "requester": "boss",
        "sender": "ali",
    }


async def test_get_agent_states_includes_aux_phase_and_reflection_sentinel(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """ReflectEngine workers (PersistDecider, CounterpartyProfiler,
    SkillSynthesizer, SkillRefiner, ``summarize_episodes``) reuse
    ``agent_phase_completed`` with ``phase=auxiliary`` so the dashboard
    can show the agent as live during reflection — those events keep
    the agent in ``working`` state.  The ``reflection_completed``
    event ReflectEngine emits at the end of every pass is what flips
    the agent back to ``idle``.

    This pins both into the SQL allowlist against accidental removal.
    """
    db.responses = [
        [{"agent_role": "PM", "runtime_id": "pm-runtime"}],
        [{"agent_role": "PM", "latest_type": "reflection_completed"}],
    ]
    states = await store.get_agent_states(["PM"])
    sql_step2, _ = db.queries[1]
    assert "agent_phase_completed" in sql_step2
    assert "reflection_completed" in sql_step2
    # The previous ``phase=auxiliary`` exclusion must be gone — aux
    # phases now drive state to "working" intentionally.
    assert "auxiliary" not in sql_step2
    assert states["PM"]["state"] == "idle"


async def test_get_agent_states_aux_phase_keeps_agent_working(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """When the latest state-affecting event for a role is an aux phase
    (i.e. reflection still running) the derived state is ``working``."""
    db.responses = [
        [{"agent_role": "PM", "runtime_id": "pm-runtime"}],
        [{"agent_role": "PM", "latest_type": "agent_phase_completed"}],
    ]
    states = await store.get_agent_states(["PM"])
    assert states["PM"]["state"] == "working"


# -- AFK derivation -----------------------------------------------------------


@pytest.mark.parametrize(
    "event_type",
    ["llm_unavailable", "turn.guard_breach", "budget_exhausted"],
)
def test_event_to_state_failure_events_map_to_afk(event_type: str) -> None:
    """Engine-detected failures flip the agent into the dashboard's
    ``afk`` state.  The quip + cause are derived from the event payload
    when ``get_agent_states`` walks the latest events."""
    assert _event_to_state(event_type) == "afk"


def test_event_to_state_normal_lifecycle_unchanged() -> None:
    """The existing lifecycle mappings still work — the new AFK
    entries don't shadow anything."""
    assert _event_to_state("agent_spawned") == "idle"
    assert _event_to_state("task_started") == "working"
    assert _event_to_state("task_completed") == "idle"
    assert _event_to_state("agent_terminated") == "terminated"
    assert _event_to_state("totally_unknown") == "offline"


async def test_get_agent_states_afk_with_guard_breach_payload(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """``turn.guard_breach`` flips the agent to ``afk`` and the ``kind``
    from the event payload surfaces as ``afk_reason`` so the dashboard
    can pick a cause-specific quip."""
    db.responses = [
        [{"agent_role": "PM", "runtime_id": "pm-runtime"}],
        [
            {
                "agent_role": "PM",
                "latest_type": "turn.guard_breach",
                "payload": {"kind": "stall"},
            }
        ],
    ]
    states = await store.get_agent_states(["PM"])
    assert states["PM"]["state"] == "afk"
    assert states["PM"]["afk_reason"] == "stall"


async def test_get_agent_states_afk_falls_through_for_llm_unavailable(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """For ``llm_unavailable`` (no ``kind`` field on the event) the
    reason falls back to the event type itself."""
    db.responses = [
        [{"agent_role": "Eng", "runtime_id": "eng-runtime"}],
        [
            {
                "agent_role": "Eng",
                "latest_type": "llm_unavailable",
                "payload": {"provider_chain": ["openai"], "attempt_count": 1},
            }
        ],
    ]
    states = await store.get_agent_states(["Eng"])
    assert states["Eng"]["state"] == "afk"
    assert states["Eng"]["afk_reason"] == "llm_unavailable"


async def test_get_agent_states_afk_cleared_after_normal_event(
    store: TimescaleDBEventStore, db: FakeDatabase
) -> None:
    """A normal lifecycle event flips the agent back to ``working`` /
    ``idle`` and the stale AFK reason is dropped — the dashboard chip
    disappears automatically without manual reset."""
    db.responses = [
        [{"agent_role": "PM", "runtime_id": "pm-runtime"}],
        [{"agent_role": "PM", "latest_type": "task_started"}],
    ]
    states = await store.get_agent_states(["PM"])
    assert states["PM"]["state"] == "working"
    assert "afk_reason" not in states["PM"]
