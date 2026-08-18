"""TimescaleDB-backed EventStore implementation.

Stores all engine events in a PostgreSQL table managed as a TimescaleDB
hypertable (``crewlet_events``).  Common filterable dimensions
(``event_type``, ``source``, ``category``, ``trace_id``, ``agent_id``,
``agent_role``, ``task_id``, ``channel_id``, ``sender``) are stored as
dedicated columns so queries don't have to parse JSON.  Everything else
lives in a ``JSONB`` ``tags`` column and in the serialized ``payload``.
The hypertable is partitioned by the ``event_time`` column.

The table is created and registered as a hypertable by the standard SQL
migration runner (see ``db/migrations/001_initial.sql``) — this
repository only reads and writes rows.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Any

from crewlet._logging import get_logger
from crewlet.db._jsonb import decode_jsonb_dict
from crewlet.timescaledb._match import collect_related_events
from crewlet.timescaledb.projections import (
    llm_history_record,
    phase_token_record,
)

if TYPE_CHECKING:
    from crewlet.db.client import Database

logger = get_logger("timescaledb.repository")

EVENTS_TABLE = "crewlet_events"


class TimescaleDBEventStore:
    """Persistent event store backed by a TimescaleDB hypertable."""

    def __init__(self, db: Database) -> None:
        self._db = db

    async def start(self) -> None:
        # The table is created by the shared SQL migration runner; no
        # DDL is executed here.  Just log that the store is ready.
        logger.info("timescaledb_event_store_ready")

    async def close(self) -> None:
        # The underlying ``Database`` is owned by the caller (engine /
        # API process) — closing the connection pool is their job.
        logger.info("timescaledb_event_store_closed")

    # -- Write path --------------------------------------------------------

    async def write_event(
        self,
        *,
        event_id: str,
        event_type: str,
        source: str,
        timestamp: datetime,
        category: str = "",
        payload: dict[str, Any] | None = None,
        summary: str = "",
        actor: str = "",
        trace_id: str = "",
        span_id: str = "",
        parent_span_id: str = "",
        tags: dict[str, str] | None = None,
    ) -> None:
        tag_map = dict(tags or {})
        # ``ON CONFLICT DO NOTHING`` makes observability writes idempotent:
        # if the same (event_time, event_id) pair is written twice (e.g.
        # a publish retry or a replay), the second write is silently
        # dropped instead of raising a ``UniqueViolationError`` that the
        # publish-listener's except block would then log as a spurious
        # "event_write_failed".
        await self._db.execute(
            f"""
            INSERT INTO {EVENTS_TABLE} (
                event_time, event_id, event_type, source, category,
                trace_id, span_id, parent_span_id,
                agent_id, agent_role, task_id, channel_id, sender,
                summary, actor, tags, payload
            )
            VALUES (
                $1, $2, $3, $4, $5,
                $6, $7, $8,
                $9, $10, $11, $12, $13,
                $14, $15, $16::jsonb, $17::jsonb
            )
            ON CONFLICT (event_time, event_id) DO NOTHING
            """,
            _to_aware_utc(timestamp),
            event_id,
            event_type,
            source,
            category,
            trace_id,
            span_id,
            parent_span_id,
            tag_map.get("agent_id", ""),
            tag_map.get("agent_role", ""),
            tag_map.get("task_id", ""),
            tag_map.get("channel_id", ""),
            tag_map.get("sender", ""),
            summary,
            actor,
            json.dumps(tag_map),
            json.dumps(payload or {}),
        )

    # -- Read path ---------------------------------------------------------

    async def list_events(
        self,
        *,
        limit: int = 50,
        event_type: str | None = None,
        source: str | None = None,
        trace_id: str | None = None,
        actor: str | None = None,
        related_agent: str | None = None,
    ) -> list[dict[str, Any]]:
        conditions = ["event_time >= now() - INTERVAL '30 days'"]
        params: list[Any] = []
        if event_type:
            params.append(event_type)
            conditions.append(f"event_type = ${len(params)}")
        if source:
            params.append(source)
            conditions.append(f"source = ${len(params)}")
        if trace_id:
            params.append(trace_id)
            conditions.append(f"trace_id = ${len(params)}")
        if actor:
            params.append(actor)
            conditions.append(f"actor = ${len(params)}")
        where = " AND ".join(conditions)

        # related_agent is filtered post-query because it matches across
        # the actor field plus several keys in the ``tags`` JSONB.
        # Over-fetch to compensate.
        fetch_limit = min(limit * 5, 500) if related_agent else limit

        # ``payload`` is intentionally omitted — ``list_events`` strips
        # it from the response anyway, and for 30 days of events the
        # JSONB column can be large.  Avoid the wire transfer.
        sql = (
            "SELECT event_time, event_id, event_type, source, category, "
            "summary, actor, trace_id, span_id, parent_span_id, tags "
            f"FROM {EVENTS_TABLE} "
            f"WHERE {where} "
            "ORDER BY event_time DESC "
            f"LIMIT {int(fetch_limit)}"
        )
        rows = await self._db.execute(sql, *params)
        results = [_row_to_event(row) for row in rows]
        if related_agent:
            results = collect_related_events(results, related_agent, limit)
        # Strip internal tags from list response (consistent with MemoryEventStore)
        return [{k: v for k, v in r.items() if k != "tags"} for r in results]

    async def list_trace(self, trace_id: str) -> list[dict[str, Any]]:
        """Return all events in a trace, ordered by timestamp (oldest first)."""
        # ``payload`` is intentionally omitted for the same reason as in
        # ``list_events`` — the response strips it anyway.
        sql = (
            "SELECT event_time, event_id, event_type, source, category, "
            "summary, actor, trace_id, span_id, parent_span_id, tags "
            f"FROM {EVENTS_TABLE} "
            "WHERE trace_id = $1 "
            "AND event_time >= now() - INTERVAL '30 days' "
            "ORDER BY event_time ASC, event_id ASC"
        )
        rows = await self._db.execute(sql, trace_id)
        return [
            {k: v for k, v in _row_to_event(row).items() if k != "tags"} for row in rows
        ]

    async def get_event(self, event_id: str) -> dict[str, Any] | None:
        sql = (
            "SELECT event_time, event_id, event_type, source, category, "
            "summary, actor, trace_id, span_id, parent_span_id, "
            "tags, payload "
            f"FROM {EVENTS_TABLE} "
            "WHERE event_id = $1 "
            "ORDER BY event_time DESC LIMIT 1"
        )
        row = await self._db.fetchrow(sql, event_id)
        if row is None:
            return None
        result = _row_to_event(row)
        result.pop("tags", None)
        result["payload"] = _parse_payload(row)
        return result

    # -- Agent state queries ------------------------------------------------

    async def get_agent_states(
        self, agent_roles: list[str]
    ) -> dict[str, dict[str, Any]]:
        """Derive live agent state from stored events, one entry per role.

        Both queries use ``DISTINCT ON (agent_role)`` with an explicit
        ``ORDER BY agent_role, event_time DESC, event_id DESC`` so the
        most recent ``runtime_id`` and state-affecting event for each
        role are picked deterministically.  The ``event_id DESC``
        tiebreaker is essential: if two rows share the same
        ``event_time`` for the same role (possible under burst writes
        at microsecond resolution), Postgres would otherwise pick an
        arbitrary row and undermine the determinism guarantee.

        Postgres doesn't have ClickHouse's ``argMax`` aggregate;
        ``DISTINCT ON`` is the Postgres-native way to "pick the latest
        row per group".  ``agent_roles`` is pushed down to the SQL
        ``WHERE`` clause so we don't pull rows for agents the caller
        doesn't care about.
        """
        if not agent_roles:
            return {}

        roles = list(agent_roles)

        # Step 1: latest runtime_id per role.
        sql_agents = f"""
            SELECT DISTINCT ON (agent_role) agent_role, agent_id AS runtime_id
            FROM {EVENTS_TABLE}
            WHERE agent_role = ANY($1::text[])
              AND agent_id <> ''
              AND event_time >= now() - INTERVAL '30 days'
            ORDER BY agent_role, event_time DESC, event_id DESC
        """
        rows = await self._db.execute(sql_agents, roles)
        states: dict[str, dict[str, Any]] = {}
        for row in rows:
            role = row.get("agent_role", "")
            runtime_id = row.get("runtime_id", "")
            if role and runtime_id:
                states[role] = {
                    "state": "idle",
                    "runtime_id": runtime_id,
                    "current_task": None,
                    "current_phase": None,
                    "current_iteration": 0,
                    "input_tokens": 0,
                    "output_tokens": 0,
                    "total_tokens": 0,
                }

        if not states:
            return states

        # Step 2: latest state-affecting event per role.  Same
        # ``event_id DESC`` tiebreaker as step 1 for the same reason —
        # avoids nondeterminism when two state-changing events for the
        # same role land in the same microsecond.
        # ``phase=auxiliary`` invocations (PersistDecider,
        # CounterpartyProfiler, etc.) fire AFTER the turn's
        # ``task_completed`` and intentionally drive the state to
        # "working" so the dashboard shows the agent as live during
        # reflection.  ``reflection_completed`` is the sentinel emitted
        # by ReflectEngine at the end of the pass that flips the agent
        # back to "idle" — it sits in this allowlist for that reason.
        # The trailing three event types (``llm_unavailable``,
        # ``turn.guard_breach``, ``budget_exhausted``) flip the agent
        # into ``afk`` so the dashboard surfaces the failure cause.
        # The payload's ``kind`` field (for guard breaches) is read
        # out as ``afk_reason`` for the per-cause status quip.
        sql_state = f"""
            SELECT DISTINCT ON (agent_role)
                agent_role,
                event_type AS latest_type,
                task_id   AS latest_task,
                payload
            FROM {EVENTS_TABLE}
            WHERE event_type IN (
                    'task_started','task_completed','task_failed',
                    'agent_terminated','agent_turn_progress',
                    'agent_phase_started','agent_phase_completed',
                    'reflection_completed',
                    'llm_unavailable','turn.guard_breach','budget_exhausted'
                )
              AND agent_role = ANY($1::text[])
              AND event_time >= now() - INTERVAL '7 days'
            ORDER BY agent_role, event_time DESC, event_id DESC
        """
        rows = await self._db.execute(sql_state, list(states.keys()))
        for row in rows:
            role = row.get("agent_role", "")
            if role not in states:
                continue
            etype = row.get("latest_type", "")
            new_state = _event_to_state(etype)
            states[role]["state"] = new_state
            if etype == "task_started":
                states[role]["current_task"] = row.get("latest_task", "")
            elif etype in ("task_completed", "task_failed"):
                states[role]["current_task"] = None
                states[role]["current_phase"] = None
                states[role]["current_iteration"] = 0
            if new_state == "afk":
                payload = _parse_payload(row)
                states[role]["afk_reason"] = payload.get("kind", "") or etype
            else:
                states[role].pop("afk_reason", None)

        # Step 2b: AFK is sticky until the agent does real work again —
        # or is respawned, which is a NEW instance of the seat and cannot
        # inherit what stopped the last one (otherwise the hold outlives
        # an engine restart and a healthy seat reads as broken).
        # Every engine-detected failure publishes its AFK event and then
        # ``TaskFailed`` microseconds later, so step 2 — which just takes
        # the latest state-affecting event — always saw ``task_failed``
        # and reported a healthy ``idle`` seat.  The rule that actually
        # holds (and the one the live projection applies) is: an agent is
        # AFK when its newest event among {failure, real work} is a
        # failure.  ``task_completed`` / ``task_failed`` are deliberately
        # NOT "real work" here — they are the failure's own epilogue.
        # ``agent_spawned`` and ``agent_terminated`` are in the set for
        # the opposite reason: neither is a failure, so either one being
        # newest breaks the hold.  Without them a respawned seat reads as
        # AFK forever, and a terminated one reads as AFK rather than gone.
        sql_afk = f"""
            SELECT DISTINCT ON (agent_role)
                agent_role,
                event_type AS latest_type,
                payload
            FROM {EVENTS_TABLE}
            WHERE event_type IN (
                    'llm_unavailable','turn.guard_breach','budget_exhausted',
                    'task_started','agent_phase_started','agent_turn_progress',
                    'agent_spawned','agent_terminated'
                )
              AND agent_role = ANY($1::text[])
              AND event_time >= now() - INTERVAL '7 days'
            ORDER BY agent_role, event_time DESC, event_id DESC
        """
        for row in await self._db.execute(sql_afk, list(states.keys())):
            role = row.get("agent_role", "")
            etype = row.get("latest_type", "")
            if role not in states or etype not in _AFK_EVENT_TYPES:
                continue
            payload = _parse_payload(row)
            states[role]["state"] = "afk"
            states[role]["afk_reason"] = payload.get("kind", "") or etype

        # Step 3: latest agent_phase_started per role for the live
        # ``current_phase`` / ``current_iteration``.  Done as a separate
        # query (rather than crammed into step 2) because the latest
        # state-affecting event is often a later one -- e.g. a
        # ``agent_phase_completed`` that closes the same phase.  Only
        # set on rows where the agent is currently working; for any
        # other end-state (idle, terminated, afk) the step-2 clearer
        # above already nulled it.
        sql_phase = f"""
            SELECT DISTINCT ON (agent_role)
                agent_role,
                payload
            FROM {EVENTS_TABLE}
            WHERE event_type = 'agent_phase_started'
              AND agent_role = ANY($1::text[])
              AND event_time >= now() - INTERVAL '7 days'
            ORDER BY agent_role, event_time DESC, event_id DESC
        """
        phase_rows = await self._db.execute(sql_phase, list(states.keys()))
        for row in phase_rows:
            role = row.get("agent_role", "")
            if role not in states or states[role]["state"] != "working":
                continue
            payload = _parse_payload(row)
            states[role]["current_phase"] = payload.get("phase") or None
            states[role]["current_iteration"] = payload.get("iteration", 0)

        # Token totals are intentionally left at zero here.  Aggregation
        # happens at the CompositeEventStore layer via
        # ``list_token_usage_events`` so events that are written to both
        # the persistent and memory stores aren't double-counted.
        return states

    async def list_token_usage_events(
        self, *, since_days: int = 30
    ) -> list[dict[str, Any]]:
        """Return per-event token records for ``agent_turn_completed``."""
        sql = (
            "SELECT event_time, event_id, agent_id, agent_role, payload "
            f"FROM {EVENTS_TABLE} "
            "WHERE event_type = 'agent_turn_completed' "
            f"AND event_time >= now() - INTERVAL '{int(since_days)} days'"
        )
        rows = await self._db.execute(sql)
        results: list[dict[str, Any]] = []
        for row in rows:
            payload = _parse_payload(row)
            results.append(
                {
                    "event_id": row.get("event_id", ""),
                    "agent_id": row.get("agent_id", ""),
                    "agent_role": row.get("agent_role", ""),
                    "input_tokens": payload.get("input_tokens", 0),
                    "output_tokens": payload.get("output_tokens", 0),
                    "total_tokens": payload.get("total_tokens", 0),
                    "timestamp": _format_time(row.get("event_time")),
                }
            )
        return results

    async def list_phase_token_events(
        self, *, since_days: int = 30, agent_role: str | None = None
    ) -> list[dict[str, Any]]:
        """Return per-phase token rows from ``agent_phase_completed``.

        Used by the dashboard's Tokens view to roll up LLM spend by
        phase / model / worker / agent / turn across the whole org.
        Mirrors :meth:`list_token_usage_events` but reads phase-level
        rather than turn-level events so callers can break down spend
        within a single turn.
        """
        conditions = [
            "event_type = 'agent_phase_completed'",
            f"event_time >= now() - INTERVAL '{int(since_days)} days'",
        ]
        params: list[Any] = []
        if agent_role:
            params.append(agent_role)
            conditions.append(f"agent_role = ${len(params)}")
        where = " AND ".join(conditions)
        sql = (
            "SELECT event_time, event_id, agent_id, agent_role, payload "
            f"FROM {EVENTS_TABLE} "
            f"WHERE {where}"
        )
        rows = await self._db.execute(sql, *params)
        results: list[dict[str, Any]] = []
        for row in rows:
            payload = _parse_payload(row)
            results.append(
                phase_token_record(
                    event_id=row.get("event_id", ""),
                    timestamp=_format_time(row.get("event_time")),
                    agent_id=row.get("agent_id", ""),
                    agent_role=row.get("agent_role", ""),
                    payload=payload,
                )
            )
        return results

    async def get_agent_llm_history(
        self, agent_id: str, *, limit: int = 50
    ) -> list[dict[str, Any]]:
        """Return recent LLM invocation records for an agent.

        Includes one row per phase (``agent_phase_completed``) so the
        dashboard can show Plan / Execute / Review / Sub-agent detail,
        plus a whole-turn aggregate row (``agent_turn_completed``).
        """
        sql = (
            "SELECT event_type, event_time, payload "
            f"FROM {EVENTS_TABLE} "
            "WHERE event_type IN ('agent_turn_completed', 'agent_phase_completed') "
            "AND agent_id = $1 "
            "AND event_time >= now() - INTERVAL '7 days' "
            "ORDER BY event_time DESC "
            f"LIMIT {int(limit)}"
        )
        rows = await self._db.execute(sql, agent_id)
        history: list[dict[str, Any]] = []
        for row in rows:
            payload = _parse_payload(row)
            event_type = row.get("event_type", "")
            timestamp = _format_time(row.get("event_time"))
            history.append(llm_history_record(event_type, timestamp, payload))
        return history


def _row_to_event(row: dict[str, Any]) -> dict[str, Any]:
    """Convert a query row to a standard event dict (without payload).

    When the row includes the ``tags`` JSONB column it's passed through
    so post-query ``related_agent`` filtering via
    ``collect_related_events`` can see the full tag dict.  Callers that
    return events to clients should strip the ``tags`` key before
    serializing.
    """
    raw_tags = decode_jsonb_dict(row.get("tags"))
    tags: dict[str, str] = {str(k): str(v) for k, v in raw_tags.items()}
    return {
        "id": row.get("event_id", ""),
        "type": row.get("event_type", ""),
        "source": row.get("source", ""),
        "timestamp": _format_time(row.get("event_time")),
        "category": row.get("category", ""),
        "summary": row.get("summary", ""),
        "actor": row.get("actor", ""),
        "trace_id": row.get("trace_id", ""),
        "span_id": row.get("span_id", ""),
        "parent_span_id": row.get("parent_span_id", ""),
        "tags": tags,
    }


# Engine-detected failure events that put an agent in ``afk``.  Shared
# by the state map below and the sticky-AFK pass in
# ``get_agent_states`` so the two can't disagree about what counts.
_AFK_EVENT_TYPES = frozenset(
    {"llm_unavailable", "turn.guard_breach", "budget_exhausted"}
)


def _event_to_state(event_type: str) -> str:
    """Map event type to agent state string."""
    return {
        "agent_spawned": "idle",
        "task_started": "working",
        "task_completed": "idle",
        "task_failed": "idle",
        "agent_terminated": "terminated",
        "agent_turn_progress": "working",
        "agent_phase_started": "working",
        "agent_phase_completed": "working",
        "reflection_completed": "idle",
        # Engine-detected failures → AFK on the dashboard. The next
        # normal lifecycle event (task_started / task_completed)
        # flips the agent back automatically — no manual reset.
        "llm_unavailable": "afk",
        "turn.guard_breach": "afk",
        "budget_exhausted": "afk",
    }.get(event_type, "offline")


def _format_time(t: Any) -> str:
    """Format a timestamp value to a UTC ISO string.

    The events table stores ``TIMESTAMPTZ`` and asyncpg returns
    timezone-aware ``datetime`` values for such columns.  We normalize
    to UTC before serializing so consumers don't have to guess the
    timezone on the wire.
    """
    if t is None:
        return ""
    if isinstance(t, datetime):
        t = t.replace(tzinfo=UTC) if t.tzinfo is None else t.astimezone(UTC)
        return t.isoformat()
    return str(t)


def _parse_payload(row: dict[str, Any]) -> dict[str, Any]:
    """Parse the payload field from a query row.

    The column is ``JSONB``, which asyncpg returns as a decoded ``dict``
    by default.  Fall back to JSON decoding a string for defensive
    compatibility with fake clients and future driver changes.
    """
    return decode_jsonb_dict(row.get("payload"))


def _to_aware_utc(dt: datetime) -> datetime:
    """Return *dt* normalized to a timezone-aware UTC datetime.

    asyncpg accepts both naive and aware datetimes for ``TIMESTAMPTZ``
    columns, but aware is unambiguous — we always pass UTC.
    """
    if dt.tzinfo is None:
        return dt.replace(tzinfo=UTC)
    return dt.astimezone(UTC)
