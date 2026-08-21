"""In-memory EventStore for development and testing."""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from typing import Any
from uuid import uuid4

from crewlet._logging import get_logger
from crewlet.events.types import event_failed
from crewlet.timescaledb._match import collect_related_events
from crewlet.timescaledb._time import row_key, ts_key
from crewlet.timescaledb.projections import (
    llm_history_record,
    phase_token_record,
)
from crewlet.timescaledb.repository import MAX_TRACE_EVENTS


def _as_iso(value: Any) -> str:
    """A cursor timestamp as an ISO string, whichever form it arrived in."""
    return value.isoformat() if hasattr(value, "isoformat") else str(value)


logger = get_logger("timescaledb.memory")

# Event types that affect agent state
_STATE_EVENTS = {
    "agent_spawned": "idle",
    "task_started": "working",
    "task_completed": "idle",
    "task_failed": "idle",
    "agent_terminated": "terminated",
    "agent_turn_progress": "working",
    "agent_phase_started": "working",
    "agent_phase_completed": "working",
    "reflection_completed": "idle",
    # Engine-detected failures → AFK on the dashboard.  The next
    # normal lifecycle event flips the agent back automatically.
    "llm_unavailable": "afk",
    "turn.guard_breach": "afk",
    "budget_exhausted": "afk",
}


class MemoryEventStore:
    """In-memory event store (capped at *max_events*).

    Drop-in replacement for ``TimescaleDBEventStore`` when persistence
    is not configured.  Events are lost on restart.
    """

    def __init__(self, max_events: int = 1000) -> None:
        self._events: list[dict[str, Any]] = []
        self._max = max_events
        # Mirrors the PG repository's ``ON CONFLICT (event_time,
        # event_id) DO NOTHING``: the queue's deferral/requeue paths
        # REPUBLISH events (same id) and rely on the event store being
        # idempotent — without dedup, every deferral cycle would append
        # another copy and evict genuine events from the ring.
        self._seen_ids: set[str] = set()

    async def start(self) -> None:
        logger.info("memory_event_store_started", max_events=self._max)

    async def close(self) -> None:
        self._events.clear()
        self._seen_ids.clear()

    async def write_event(
        self,
        *,
        event_id: str = "",
        event_type: str,
        source: str,
        timestamp: datetime | None = None,
        category: str = "",
        payload: dict[str, Any] | None = None,
        summary: str = "",
        actor: str = "",
        trace_id: str = "",
        span_id: str = "",
        parent_span_id: str = "",
        tags: dict[str, str] | None = None,
    ) -> None:
        if not event_id:
            event_id = str(uuid4())
        if event_id in self._seen_ids:
            return  # idempotent on event_id, like the PG upsert
        self._seen_ids.add(event_id)
        self._events.append(
            {
                "id": event_id,
                "type": event_type,
                "source": source,
                "timestamp": (timestamp or datetime.now(UTC)).isoformat(),
                "category": category,
                "payload": payload,
                "summary": summary,
                "actor": actor,
                "trace_id": trace_id,
                "span_id": span_id,
                "parent_span_id": parent_span_id,
                "tags": tags or {},
            }
        )
        if len(self._events) > self._max:
            evicted = self._events[: len(self._events) - self._max]
            del self._events[: len(self._events) - self._max]
            for entry in evicted:
                self._seen_ids.discard(entry["id"])

    async def list_events(
        self,
        *,
        limit: int = 50,
        event_type: str | None = None,
        source: str | None = None,
        category: str | None = None,
        trace_id: str | None = None,
        actor: str | None = None,
        related_agent: str | None = None,
        before: tuple[Any, str] | None = None,
    ) -> list[dict[str, Any]]:
        results = self._events
        if event_type:
            results = [e for e in results if e["type"] == event_type]
        if source:
            results = [e for e in results if e["source"] == source]
        if category:
            results = [e for e in results if e.get("category") == category]
        if trace_id:
            results = [e for e in results if e.get("trace_id") == trace_id]
        if actor:
            results = [e for e in results if e.get("actor") == actor]
        if before is not None:
            before_ts, before_id = before
            cursor = (ts_key(_as_iso(before_ts)), str(before_id))
            results = [e for e in results if row_key(e) < cursor]
        if related_agent:
            results = collect_related_events(results, related_agent, limit)
            return [self._light(e) for e in results]
        # Ordered by (instant, id) descending, NOT by insertion.  The two
        # differ whenever a write is backfilled -- a webhook replay, the
        # Mattermost since= gap re-read -- and under a cursor that shows
        # up as rows appearing above rows the reader already scrolled
        # past.  ``ts_key`` is what makes the comparison safe across the
        # naive and aware encodings ``write_event`` accepts.
        ordered = sorted(results, key=row_key, reverse=True)
        return [self._light(e) for e in ordered[:limit]]

    async def count_events_by_source(
        self, *, since_hours: int = 24
    ) -> list[dict[str, Any]]:
        """Per (source, type) counts over the ring.

        The twin of the persistent store's GROUP BY, and bounded by the
        ring like everything else here: on a store-less deployment this
        describes the last ``max_events``, not the last ``since_hours``.
        That is the same limit every other read on this backend carries.
        """
        # Compared through ts_key for the same reason every other read
        # here is: the ring holds both naive and aware encodings of the
        # same instant, and comparing those lexicographically orders one
        # after the other.
        cutoff = ts_key(
            (datetime.now(UTC) - timedelta(hours=int(since_hours))).isoformat()
        )
        buckets: dict[tuple[str, str, str], dict[str, Any]] = {}
        for event in self._events:
            at = ts_key(str(event.get("timestamp") or ""))
            if at and at < cutoff:
                continue
            source = str(event.get("source") or "")
            if not source:
                continue
            key = (
                source,
                str(event.get("type") or ""),
                str(event.get("category") or ""),
            )
            bucket = buckets.setdefault(
                key,
                {
                    "source": key[0],
                    "event_type": key[1],
                    "category": key[2],
                    "count": 0,
                    "last_at": None,
                },
            )
            bucket["count"] += 1
            if at and (bucket["last_at"] is None or at > bucket["last_at"]):
                bucket["last_at"] = at
        return list(buckets.values())

    @staticmethod
    def _light(event: dict[str, Any]) -> dict[str, Any]:
        """One list-view row: no payload, no tags, plus ``failed``.

        The persistent store derives the same flag in ``_row_to_event``;
        both must, or the same event reads as a failure on one backend
        and not on the other.
        """
        row = {k: v for k, v in event.items() if k not in ("payload", "tags")}
        tags = event.get("tags") or {}
        row["failed"] = event_failed(
            str(event.get("type", "")),
            tag_failed=str(tags.get("failed", "")) == "true",
        )
        return row

    async def get_event(self, event_id: str) -> dict[str, Any] | None:
        for e in self._events:
            if e["id"] == event_id:
                return {k: v for k, v in e.items() if k != "tags"}
        return None

    async def list_trace(self, trace_id: str) -> list[dict[str, Any]]:
        """Return all events in a trace, ordered by timestamp (oldest first).

        Sorted and capped exactly as the persistent store does, so a
        trace reads the same on either backend.
        """
        results = [e for e in self._events if e.get("trace_id") == trace_id]
        ordered = sorted(results, key=row_key)
        return [self._light(e) for e in ordered[:MAX_TRACE_EVENTS]]

    async def get_agent_states(
        self, agent_roles: list[str]
    ) -> dict[str, dict[str, Any]]:
        """Derive live agent state by replaying events in order."""
        states: dict[str, dict[str, Any]] = {}

        for event in self._events:
            tags = event.get("tags", {})
            agent_id = tags.get("agent_id", "")
            role = tags.get("agent_role", "") or event.get("source", "")
            etype = event["type"]

            if etype == "agent_spawned" and role:
                states[role] = {
                    "state": "idle",
                    "runtime_id": agent_id,
                    "current_task": None,
                    "current_phase": None,
                    "current_iteration": 0,
                    "input_tokens": 0,
                    "output_tokens": 0,
                    "total_tokens": 0,
                }
            elif etype in _STATE_EVENTS and agent_id:
                for _r, s in states.items():
                    if s.get("runtime_id") == agent_id:
                        new_state = _STATE_EVENTS[etype]
                        # AFK is sticky until the agent does real work
                        # again.  Every engine-detected failure publishes
                        # its AFK event and then ``TaskFailed`` an
                        # instant later, so taking the newest event at
                        # face value reported a healthy ``idle`` seat
                        # microseconds after the failure that stopped it.
                        # Mirrors the same rule in the SQL backend and in
                        # the live projection.
                        if s.get("state") == "afk" and etype in (
                            "task_completed",
                            "task_failed",
                        ):
                            new_state = "afk"
                        s["state"] = new_state
                        if etype == "task_started":
                            s["current_task"] = tags.get("task_id", "")
                        elif etype in ("task_completed", "task_failed"):
                            s["current_task"] = None
                            # Turn ended -- the agent is no longer in any
                            # phase.  Clear so the dashboard doesn't show
                            # a stale "in review" badge on an idle agent.
                            s["current_phase"] = None
                            s["current_iteration"] = 0
                        elif etype == "agent_phase_started":
                            payload = event.get("payload", {}) or {}
                            s["current_phase"] = payload.get("phase") or None
                            s["current_iteration"] = payload.get("iteration", 0)
                        # Track the AFK reason for the dashboard quip.
                        # ``turn.guard_breach`` carries the specific
                        # ``kind`` in its payload (stall / max_iter / ...);
                        # other AFK events use their type as the reason.
                        if (
                            new_state == "afk"
                            and etype in _STATE_EVENTS
                            and (_STATE_EVENTS[etype] == "afk")
                        ):
                            payload = event.get("payload", {}) or {}
                            s["afk_reason"] = payload.get("kind", "") or etype
                        elif new_state != "afk":
                            # Normal lifecycle event — clear any stale
                            # AFK reason so the dashboard chip goes away.
                            s.pop("afk_reason", None)
                        break
        return states

    async def list_token_usage_events(
        self, *, since_days: int = 30
    ) -> list[dict[str, Any]]:
        """Return per-event token records for ``agent_turn_completed``.

        ``since_days`` is accepted for protocol parity but ignored — the
        in-memory store is bounded by ``max_events`` rather than time.
        """
        del since_days
        results: list[dict[str, Any]] = []
        for event in self._events:
            if event.get("type") != "agent_turn_completed":
                continue
            payload = event.get("payload", {}) or {}
            tags = event.get("tags", {}) or {}
            results.append(
                {
                    "event_id": event.get("id", ""),
                    "agent_id": tags.get("agent_id", ""),
                    "agent_role": tags.get("agent_role", ""),
                    "input_tokens": payload.get("input_tokens", 0),
                    "output_tokens": payload.get("output_tokens", 0),
                    "total_tokens": payload.get("total_tokens", 0),
                    "timestamp": event.get("timestamp", ""),
                }
            )
        return results

    async def list_phase_token_events(
        self, *, since_days: int = 30, agent_role: str | None = None
    ) -> list[dict[str, Any]]:
        """Return per-phase token rows from ``agent_phase_completed``.

        ``since_days`` is accepted for protocol parity but ignored — the
        in-memory store is bounded by ``max_events`` rather than time.
        ``agent_role`` filters via the ``agent_role`` tag.
        """
        del since_days
        results: list[dict[str, Any]] = []
        for event in self._events:
            if event.get("type") != "agent_phase_completed":
                continue
            tags = event.get("tags", {}) or {}
            role = tags.get("agent_role", "")
            if agent_role and role != agent_role:
                continue
            payload = event.get("payload", {}) or {}
            results.append(
                phase_token_record(
                    event_id=event.get("id", ""),
                    timestamp=event.get("timestamp", ""),
                    agent_id=tags.get("agent_id", ""),
                    agent_role=role,
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
        history: list[dict[str, Any]] = []
        for event in reversed(self._events):
            if event["type"] not in ("agent_turn_completed", "agent_phase_completed"):
                continue
            tags = event.get("tags", {})
            if tags.get("agent_id") != agent_id:
                continue
            payload = event.get("payload", {}) or {}
            history.append(
                llm_history_record(event["type"], event["timestamp"], payload)
            )
            if len(history) >= limit:
                break
        return history
