"""Tests for the composite (memory + persistent) event store."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

import pytest

from crewlet.timescaledb._time import ts_key
from crewlet.timescaledb.composite import CompositeEventStore
from crewlet.timescaledb.memory import MemoryEventStore


class FakePersistent:
    """Persistent store stand-in that records calls and can simulate failure."""

    def __init__(self) -> None:
        self.events: list[dict[str, Any]] = []
        self.state: dict[str, dict[str, Any]] = {}
        self.llm_history: dict[str, list[dict[str, Any]]] = {}
        self.token_events: list[dict[str, Any]] = []
        self.fail_writes = False

    async def start(self) -> None:
        pass

    async def close(self) -> None:
        pass

    async def write_event(self, **kwargs: Any) -> None:
        if self.fail_writes:
            raise RuntimeError("persistent store down")
        self.events.append(kwargs)

    def _row(self, event: dict[str, Any]) -> dict[str, Any]:
        """One stored write, in the shape the protocol returns.

        The fake used to hand back its raw ``write_event`` kwargs, which
        carry ``event_id`` / ``event_time`` rather than the ``id`` /
        ``timestamp`` every real store returns. Nothing noticed while the
        composite passed rows straight through; a merge that has to order
        and dedupe them does.
        """
        when = event.get("event_time")
        return {
            "id": event.get("event_id", ""),
            "type": event.get("event_type", event.get("type", "")),
            "source": event.get("source", ""),
            "category": event.get("category", ""),
            "summary": event.get("summary", ""),
            "actor": event.get("actor", ""),
            "trace_id": event.get("trace_id", ""),
            "timestamp": when.isoformat() if hasattr(when, "isoformat") else "",
            "failed": False,
        }

    async def list_events(self, **kwargs: Any) -> list[dict[str, Any]]:
        rows = [self._row(e) for e in self.events]
        if before := kwargs.get("before"):
            before_ts, before_id = before
            cursor = (ts_key(before_ts.isoformat()), str(before_id))
            rows = [r for r in rows if (ts_key(r["timestamp"]), r["id"]) < cursor]
        rows.sort(key=lambda r: (ts_key(r["timestamp"]), r["id"]), reverse=True)
        return rows[: kwargs.get("limit", 50)]

    async def get_event(self, event_id: str) -> dict[str, Any] | None:
        for e in self.events:
            if e.get("event_id") == event_id:
                return {"id": event_id, **e}
        return None

    async def list_trace(self, trace_id: str) -> list[dict[str, Any]]:
        rows = [self._row(e) for e in self.events if e.get("trace_id") == trace_id]
        rows.sort(key=lambda r: (ts_key(r["timestamp"]), r["id"]))
        return rows

    async def get_agent_states(
        self, agent_roles: list[str]
    ) -> dict[str, dict[str, Any]]:
        return self.state

    async def list_token_usage_events(
        self, *, since_days: int = 30
    ) -> list[dict[str, Any]]:
        del since_days
        return list(self.token_events)

    async def list_phase_token_events(
        self, *, since_days: int = 30, agent_role: str | None = None
    ) -> list[dict[str, Any]]:
        del since_days
        del agent_role
        return []

    async def get_agent_llm_history(
        self, agent_id: str, *, limit: int = 50
    ) -> list[dict[str, Any]]:
        return self.llm_history.get(agent_id, [])[:limit]


@pytest.fixture
def memory() -> MemoryEventStore:
    return MemoryEventStore()


@pytest.fixture
def persistent() -> FakePersistent:
    return FakePersistent()


@pytest.fixture
def composite(
    persistent: FakePersistent, memory: MemoryEventStore
) -> CompositeEventStore:
    return CompositeEventStore(persistent, memory)


# --------------------------------------------------------------------------- #
# ts_key helper
# --------------------------------------------------------------------------- #


def test_ts_key_normalizes_aware_and_naive_to_same_value() -> None:
    """Ensures composite dedup works across driver timezone differences."""
    assert ts_key("2026-04-12T12:00:00.123456+00:00") == ts_key(
        "2026-04-12T12:00:00.123456"
    )


def test_ts_key_falls_back_to_raw_on_parse_error() -> None:
    assert ts_key("not-a-date") == "not-a-date"


# --------------------------------------------------------------------------- #
# Write paths
# --------------------------------------------------------------------------- #


async def test_write_event_dual_writes(
    composite: CompositeEventStore, persistent: FakePersistent, memory: MemoryEventStore
) -> None:
    await composite.start()
    await composite.write_event(
        event_id="e1",
        event_type="task_created",
        source="pm",
        timestamp=datetime.now(UTC),
    )
    assert len(persistent.events) == 1
    memory_events = await memory.list_events()
    assert len(memory_events) == 1


async def test_write_event_tolerates_persistent_failure(
    composite: CompositeEventStore, persistent: FakePersistent, memory: MemoryEventStore
) -> None:
    # Persistent failures must not block memory writes.
    # Observability writes are fire-and-forget.
    persistent.fail_writes = True
    await composite.start()
    await composite.write_event(
        event_id="e1",
        event_type="task_created",
        source="pm",
        timestamp=datetime.now(UTC),
    )
    memory_events = await memory.list_events()
    assert len(memory_events) == 1
    assert persistent.events == []


# --------------------------------------------------------------------------- #
# Read paths
# --------------------------------------------------------------------------- #


async def test_list_events_prefers_persistent_store(
    composite: CompositeEventStore, persistent: FakePersistent, memory: MemoryEventStore
) -> None:
    await composite.start()
    # Only persistent has an event
    persistent.events.append({"event_id": "p1", "type": "task_created", "source": "pm"})
    result = await composite.list_events(limit=10)
    assert len(result) == 1


async def test_list_events_falls_back_to_memory_when_persistent_empty(
    composite: CompositeEventStore, persistent: FakePersistent, memory: MemoryEventStore
) -> None:
    await composite.start()
    # Only memory has an event
    await memory.write_event(
        event_id="m1",
        event_type="task_created",
        source="pm",
        timestamp=datetime.now(UTC),
    )
    result = await composite.list_events(limit=10)
    assert len(result) == 1
    assert result[0]["id"] == "m1"


async def test_list_events_merges_both_legs(
    composite: CompositeEventStore, persistent: FakePersistent, memory: MemoryEventStore
) -> None:
    """A row not yet flushed is still a row.

    The read used to return the persistent leg whenever it was non-empty,
    so anything written but not yet indexed was invisible for as long as
    the persistent store had anything at all.
    """
    await composite.start()
    persistent.events.append(
        {
            "event_id": "p1",
            "event_type": "task_created",
            "source": "pm",
            "event_time": datetime(2026, 4, 1, 12, 1, tzinfo=UTC),
        }
    )
    await memory.write_event(
        event_id="m1",
        event_type="task_created",
        source="pm",
        timestamp=datetime(2026, 4, 1, 12, 2, tzinfo=UTC),
    )
    result = await composite.list_events(limit=10)
    assert [e["id"] for e in result] == ["m1", "p1"], "newest first, both legs"


async def test_list_events_dedupes_with_persistent_winning(
    composite: CompositeEventStore, persistent: FakePersistent, memory: MemoryEventStore
) -> None:
    await composite.start()
    when = datetime(2026, 4, 1, 12, 0, tzinfo=UTC)
    persistent.events.append(
        {
            "event_id": "dup",
            "event_type": "task_created",
            "source": "persistent",
            "event_time": when,
        }
    )
    await memory.write_event(
        event_id="dup", event_type="task_created", source="memory", timestamp=when
    )
    result = await composite.list_events(limit=10)
    assert len(result) == 1
    assert result[0]["source"] == "persistent"


async def test_an_exhausted_page_does_not_jump_back_to_the_present(
    composite: CompositeEventStore, persistent: FakePersistent, memory: MemoryEventStore
) -> None:
    """The bug paging would have exposed.

    Under a cursor an empty persistent page means "no more history".
    Falling through to memory hands back its NEWEST rows, teleporting a
    reader who scrolled to the bottom straight to the present -- with no
    error, and looking exactly like real data.
    """
    await composite.start()
    await memory.write_event(
        event_id="recent",
        event_type="task_created",
        source="pm",
        timestamp=datetime(2026, 4, 1, 12, 30, tzinfo=UTC),
    )
    # The persistent leg has nothing older than the cursor.
    page = await composite.list_events(
        limit=10, before=(datetime(2026, 4, 1, 12, 0, tzinfo=UTC), "x")
    )
    assert page == [], "an exhausted page returned rows newer than the cursor"


async def test_list_trace_merges_both_legs_oldest_first(
    composite: CompositeEventStore, persistent: FakePersistent, memory: MemoryEventStore
) -> None:
    """A half-flushed trace showing only its persistent spans would read
    as a turn that skipped steps."""
    await composite.start()
    persistent.events.append(
        {
            "event_id": "p1",
            "event_type": "task_created",
            "source": "pm",
            "trace_id": "tr",
            "event_time": datetime(2026, 4, 1, 12, 1, tzinfo=UTC),
        }
    )
    await memory.write_event(
        event_id="m1",
        event_type="task_completed",
        source="pm",
        timestamp=datetime(2026, 4, 1, 12, 2, tzinfo=UTC),
        trace_id="tr",
    )
    assert [e["id"] for e in await composite.list_trace("tr")] == ["p1", "m1"]


# --------------------------------------------------------------------------- #
# Token dedup across stores
# --------------------------------------------------------------------------- #


@pytest.fixture
def dual_memory_composite() -> CompositeEventStore:
    """Composite wired with two MemoryEventStores.

    Mirrors the ``cli.py`` dual-publish-listener topology where every
    event lands in both the memory leg and the persistent leg with the
    same ``event_id``.  Used to test that token aggregation deduplicates
    by ``event_id`` instead of double-counting.
    """
    return CompositeEventStore(
        MemoryEventStore(max_events=100),  # persistent leg
        MemoryEventStore(max_events=100),  # memory leg
    )


async def _write_to_both(store: CompositeEventStore, **event_kwargs: Any) -> None:
    """Mirror the dual-publish-listener wiring used in cli.py."""
    await store._memory.write_event(**event_kwargs)  # type: ignore[arg-type]
    await store._persistent.write_event(**event_kwargs)  # type: ignore[arg-type]


async def test_get_agent_states_does_not_double_count_tokens(
    dual_memory_composite: CompositeEventStore,
) -> None:
    """Regression: events written to both stores must be counted once.

    cli.py registers two publish listeners — one writing to a memory
    store and one writing to the persistent store — so every event lands in both
    stores with the same ``event_id``.  ``CompositeEventStore`` must
    deduplicate when aggregating per-role token totals.
    """
    composite = dual_memory_composite
    await composite.start()
    now = datetime.now(UTC)

    await _write_to_both(
        composite,
        event_id="spawn-1",
        event_type="agent_spawned",
        source="agent-cto",
        timestamp=now,
        payload={"agent_id": "a-1", "role": "CTO"},
        tags={"agent_id": "a-1", "agent_role": "CTO"},
    )
    await _write_to_both(
        composite,
        event_id="turn-1",
        event_type="agent_turn_completed",
        source="agent-cto",
        timestamp=now,
        payload={
            "input_tokens": 113527,
            "output_tokens": 448,
            "total_tokens": 113975,
        },
        tags={"agent_id": "a-1", "agent_role": "CTO"},
    )

    states = await composite.get_agent_states(["CTO"])
    assert "CTO" in states
    cto = states["CTO"]
    assert cto["input_tokens"] == 113527
    assert cto["output_tokens"] == 448
    assert cto["total_tokens"] == 113975


async def test_list_token_usage_events_dedupes_by_event_id(
    dual_memory_composite: CompositeEventStore,
) -> None:
    composite = dual_memory_composite
    await composite.start()
    now = datetime.now(UTC)

    # Same event in both stores — should appear once.
    for backing in (composite._memory, composite._persistent):  # type: ignore[attr-defined]
        await backing.write_event(
            event_id="t1",
            event_type="agent_turn_completed",
            source="agent-pm",
            timestamp=now,
            payload={"input_tokens": 10, "output_tokens": 2, "total_tokens": 12},
            tags={"agent_id": "a-1"},
        )

    # Memory-only event (not yet flushed to persistent) — must be kept.
    await composite._memory.write_event(  # type: ignore[attr-defined]
        event_id="t2",
        event_type="agent_turn_completed",
        source="agent-pm",
        timestamp=now,
        payload={"input_tokens": 5, "output_tokens": 1, "total_tokens": 6},
        tags={"agent_id": "a-1"},
    )

    rows = await composite.list_token_usage_events()
    ids = sorted(r["event_id"] for r in rows)
    assert ids == ["t1", "t2"]
    totals = sum(r["total_tokens"] for r in rows)
    assert totals == 18


async def test_list_phase_token_events_dedupes_by_event_id(
    dual_memory_composite: CompositeEventStore,
) -> None:
    """Per-phase token events follow the same dedup rules as turn events.

    Persistent wins on conflict (by ``event_id``); memory entries with
    no persistent counterpart (not-yet-flushed) are kept.  The
    dashboard's Tokens view depends on this so spend isn't
    double-counted when both stores carry the same phase row.
    """
    composite = dual_memory_composite
    await composite.start()
    now = datetime.now(UTC)

    # Same event in both stores — should appear once.
    for backing in (composite._memory, composite._persistent):  # type: ignore[attr-defined]
        await backing.write_event(
            event_id="phase-1",
            event_type="agent_phase_completed",
            source="agent-pm",
            timestamp=now,
            payload={
                "phase": "plan",
                "model": "m",
                "turn_id": "t-1",
                "input_tokens": 10,
                "output_tokens": 2,
                "total_tokens": 12,
            },
            tags={"agent_id": "a-1", "agent_role": "PM"},
        )

    # Memory-only event — must be kept.
    await composite._memory.write_event(  # type: ignore[attr-defined]
        event_id="phase-2",
        event_type="agent_phase_completed",
        source="agent-pm",
        timestamp=now,
        payload={
            "phase": "execute",
            "model": "m",
            "turn_id": "t-1",
            "input_tokens": 100,
            "output_tokens": 30,
            "total_tokens": 130,
        },
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )

    rows = await composite.list_phase_token_events()
    ids = sorted(r["event_id"] for r in rows)
    assert ids == ["phase-1", "phase-2"]
    totals = sum(r["total_tokens"] for r in rows)
    assert totals == 142


async def test_get_agent_states_includes_unflushed_memory_tokens(
    dual_memory_composite: CompositeEventStore,
) -> None:
    """Memory-only turns (not yet in persistent) still contribute tokens."""
    composite = dual_memory_composite
    await composite.start()
    now = datetime.now(UTC)

    await _write_to_both(
        composite,
        event_id="spawn-1",
        event_type="agent_spawned",
        source="agent-pm",
        timestamp=now,
        payload={"agent_id": "a-1", "role": "PM"},
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )
    # Flushed turn — both stores.
    await _write_to_both(
        composite,
        event_id="t1",
        event_type="agent_turn_completed",
        source="agent-pm",
        timestamp=now,
        payload={"input_tokens": 10, "output_tokens": 2, "total_tokens": 12},
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )
    # Unflushed turn — memory only.
    await composite._memory.write_event(  # type: ignore[attr-defined]
        event_id="t2",
        event_type="agent_turn_completed",
        source="agent-pm",
        timestamp=now,
        payload={"input_tokens": 4, "output_tokens": 1, "total_tokens": 5},
        tags={"agent_id": "a-1", "agent_role": "PM"},
    )

    states = await composite.get_agent_states(["PM"])
    pm = states["PM"]
    assert pm["input_tokens"] == 14
    assert pm["output_tokens"] == 3
    assert pm["total_tokens"] == 17


async def test_get_agent_states_sums_tokens_across_session_restarts(
    dual_memory_composite: CompositeEventStore,
) -> None:
    """Regression guard: tokens from earlier sessions (different
    runtime_id, same
    role) must be included in ``get_agent_states``.

    Attribution must go via the ``agent_role``
    column that ``list_token_usage_events`` carries directly.  Keying
    off a per-session ``_id_to_role`` index — which only contains the
    single ``runtime_id`` each per-store
    ``get_agent_states`` call exposes — would silently drop earlier
    sessions' runtime_ids, losing cross-session history on the
    dashboard.
    """
    composite = dual_memory_composite
    await composite.start()
    now = datetime.now(UTC)

    # Session 1: agent "a-old" runs as PM and completes a turn.
    await _write_to_both(
        composite,
        event_id="s1-spawn",
        event_type="agent_spawned",
        source="agent-pm",
        timestamp=now,
        payload={"agent_id": "a-old", "role": "PM"},
        tags={"agent_id": "a-old", "agent_role": "PM"},
    )
    await _write_to_both(
        composite,
        event_id="s1-turn",
        event_type="agent_turn_completed",
        source="agent-pm",
        timestamp=now,
        payload={"input_tokens": 100, "output_tokens": 50, "total_tokens": 150},
        tags={"agent_id": "a-old", "agent_role": "PM"},
    )

    # Session 2: new runtime_id for the same role.
    await _write_to_both(
        composite,
        event_id="s2-spawn",
        event_type="agent_spawned",
        source="agent-pm",
        timestamp=now,
        payload={"agent_id": "a-new", "role": "PM"},
        tags={"agent_id": "a-new", "agent_role": "PM"},
    )
    await _write_to_both(
        composite,
        event_id="s2-turn",
        event_type="agent_turn_completed",
        source="agent-pm",
        timestamp=now,
        payload={"input_tokens": 200, "output_tokens": 100, "total_tokens": 300},
        tags={"agent_id": "a-new", "agent_role": "PM"},
    )

    states = await composite.get_agent_states(["PM"])
    pm = states["PM"]
    # Both turns must contribute: 150 + 300 — regardless of which
    # runtime_id happened to surface last from the per-store
    # get_agent_states step-1 dict.  A per-session index would yield
    # either 150 or 300, never 450.
    assert pm["input_tokens"] == 300, pm
    assert pm["output_tokens"] == 150, pm
    assert pm["total_tokens"] == 450, pm


async def test_get_agent_llm_history_returns_turns_across_session_restarts(
    dual_memory_composite: CompositeEventStore,
) -> None:
    """Regression: ``get_agent_llm_history`` must include turns from every
    session the role has run, not just the current ``runtime_id``.

    Mirrors the dashboard flow: routes.py calls ``get_agent_states`` to
    pick the current ``runtime_id``, then asks the composite for that
    runtime_id's LLM history.  The composite discovers cross-session
    runtime_ids via
    ``list_token_usage_events`` filtered by ``agent_role``; expanding
    only via ``self._role_ids`` — the runtime_ids that
    happened to surface from the per-store ``get_agent_states`` step-1
    query — would drop older sessions for the same role from the
    history list.
    """
    composite = dual_memory_composite
    await composite.start()
    now = datetime.now(UTC)

    for aid, model in [("a-old", "gpt-4o-mini"), ("a-new", "gpt-4o")]:
        await _write_to_both(
            composite,
            event_id=f"spawn-{aid}",
            event_type="agent_spawned",
            source="agent-pm",
            timestamp=now,
            payload={"agent_id": aid, "role": "PM"},
            tags={"agent_id": aid, "agent_role": "PM"},
        )
        await _write_to_both(
            composite,
            event_id=f"turn-{aid}",
            event_type="agent_turn_completed",
            source="agent-pm",
            timestamp=now,
            payload={
                "model": model,
                "input_tokens": 100,
                "output_tokens": 50,
                "total_tokens": 150,
                "prompt": "",
                "response": "",
                "tool_executions": [],
            },
            tags={"agent_id": aid, "agent_role": "PM"},
        )

    # Mirror the routes.py flow: get_agent_states first to discover the
    # current runtime_id, then ask for that agent_id's history.
    states = await composite.get_agent_states(["PM"])
    current_runtime_id = states["PM"]["runtime_id"]
    history = await composite.get_agent_llm_history(current_runtime_id, limit=50)

    # Both sessions' turns must appear in the history — the older
    # one must not be silently dropped just because its runtime_id
    # never made it into ``_role_ids``.
    models = sorted(h.get("model", "") for h in history)
    assert models == ["gpt-4o", "gpt-4o-mini"], history


# --------------------------------------------------------------------------- #
# LLM history dedup (memory supplements persistent only when strictly newer)
# --------------------------------------------------------------------------- #


async def test_get_agent_llm_history_dedup_by_timestamp(
    composite: CompositeEventStore, persistent: FakePersistent, memory: MemoryEventStore
) -> None:
    """Memory entries are only included if strictly newer than persisted ones."""
    await composite.start()
    # Populate index so composite knows the role for this agent_id
    persistent.state = {
        "PM": {
            "state": "idle",
            "runtime_id": "a-1",
            "current_task": None,
            "input_tokens": 0,
            "output_tokens": 0,
            "total_tokens": 0,
        }
    }
    await composite.get_agent_states(["PM"])

    # Persistent has one turn
    persistent.llm_history["a-1"] = [
        {
            "timestamp": "2026-04-12T12:00:00+00:00",
            "model": "gpt-4o",
            "input_tokens": 100,
            "output_tokens": 50,
            "total_tokens": 150,
            "tool_executions": [],
            "prompt": "",
            "prompt_messages": [],
            "response": "",
        }
    ]
    # Memory has the same turn (timestamp match) plus a newer one
    await memory.write_event(
        event_id="t1",
        event_type="agent_turn_completed",
        source="pm",
        timestamp=datetime(2026, 4, 12, 12, 0, 0, tzinfo=UTC),
        payload={"input_tokens": 100},
        tags={"agent_id": "a-1"},
    )
    await memory.write_event(
        event_id="t2",
        event_type="agent_turn_completed",
        source="pm",
        timestamp=datetime(2026, 4, 12, 12, 1, 0, tzinfo=UTC),
        payload={"input_tokens": 200},
        tags={"agent_id": "a-1"},
    )

    hist = await composite.get_agent_llm_history("a-1", limit=10)
    # Should have persisted turn + the newer memory turn, but NOT the duplicate
    assert len(hist) == 2
    timestamps = sorted(h["timestamp"] for h in hist)
    assert timestamps[0].startswith("2026-04-12T12:00:00")
    assert timestamps[1].startswith("2026-04-12T12:01:00")
