"""In-memory EventStore for development and testing."""

from __future__ import annotations

from datetime import datetime
from typing import Any
from uuid import uuid4

from crewlet._logging import get_logger
from crewlet.timescaledb._match import collect_related_events

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
        from datetime import UTC

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
        trace_id: str | None = None,
        actor: str | None = None,
        related_agent: str | None = None,
    ) -> list[dict[str, Any]]:
        results = self._events
        if event_type:
            results = [e for e in results if e["type"] == event_type]
        if source:
            results = [e for e in results if e["source"] == source]
        if trace_id:
            results = [e for e in results if e.get("trace_id") == trace_id]
        if actor:
            results = [e for e in results if e.get("actor") == actor]
        if related_agent:
            results = collect_related_events(results, related_agent, limit)
            return [
                {k: v for k, v in e.items() if k not in ("payload", "tags")}
                for e in results
            ]
        # Newest first, strip payload/tags for list view
        return [
            {k: v for k, v in e.items() if k not in ("payload", "tags")}
            for e in reversed(results[-limit:])
        ][:limit]

    async def get_event(self, event_id: str) -> dict[str, Any] | None:
        for e in self._events:
            if e["id"] == event_id:
                return {k: v for k, v in e.items() if k != "tags"}
        return None

    async def list_trace(self, trace_id: str) -> list[dict[str, Any]]:
        """Return all events in a trace, ordered by timestamp (oldest first)."""
        results = [e for e in self._events if e.get("trace_id") == trace_id]
        return [
            {k: v for k, v in e.items() if k not in ("payload", "tags")}
            for e in results
        ]

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
                        if new_state == "afk":
                            payload = event.get("payload", {}) or {}
                            s["afk_reason"] = payload.get("kind", "") or etype
                        else:
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
                {
                    "event_id": event.get("id", ""),
                    "timestamp": event.get("timestamp", ""),
                    "agent_id": tags.get("agent_id", ""),
                    "agent_role": role,
                    "phase": payload.get("phase", ""),
                    "host_phase": payload.get("host_phase", ""),
                    "worker": payload.get("worker", ""),
                    "model": payload.get("model", "")
                    or payload.get("provider_key", ""),
                    "turn_id": payload.get("turn_id", ""),
                    "iteration": int(payload.get("iteration", 0) or 0),
                    "input_tokens": int(payload.get("input_tokens", 0) or 0),
                    "output_tokens": int(payload.get("output_tokens", 0) or 0),
                    "total_tokens": int(payload.get("total_tokens", 0) or 0),
                    "tool_executions": payload.get("tool_executions", []) or [],
                }
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
            history.append(_llm_history_record(event["type"], event, payload))
            if len(history) >= limit:
                break
        return history


def _llm_history_record(
    event_type: str, event: dict[str, Any], payload: dict[str, Any]
) -> dict[str, Any]:
    """Normalize a turn/phase event into the history record the API returns.

    Phase events (``agent_phase_completed``) carry ``system_prompt`` +
    ``user_prompt`` separately; turn-completed events carry a single
    ``prompt`` string and a ``prompt_messages`` list.  Both are
    flattened to the same shape so the dashboard renders them
    uniformly.
    """
    if event_type == "agent_phase_completed":
        system_prompt = payload.get("system_prompt", "")
        user_prompt = payload.get("user_prompt", "")
        messages: list[dict[str, str]] = []
        if system_prompt:
            messages.append({"role": "system", "content": system_prompt})
        if user_prompt:
            messages.append({"role": "user", "content": user_prompt})
        return {
            "timestamp": event["timestamp"],
            "model": payload.get("model", "") or payload.get("provider_key", ""),
            "phase": payload.get("phase", ""),
            "host_phase": payload.get("host_phase", ""),
            "host_iteration": payload.get("host_iteration", 0),
            # Set on phase=="auxiliary" rows so the dashboard can show
            # which learning worker (persist_decider, skill_synthesizer,
            # counterparty_profiler, …) made the call.
            "worker": payload.get("worker", ""),
            "turn_id": payload.get("turn_id", ""),
            "iteration": payload.get("iteration", 0),
            "decision": payload.get("decision", ""),
            "notes": payload.get("notes", ""),
            # The event that triggered this turn — the dashboard renders
            # it as the LLM invocation's source.
            "trigger": payload.get("trigger", {}) or {},
            "prompt": user_prompt,
            "prompt_messages": messages,
            "response": payload.get("response", ""),
            "input_tokens": payload.get("input_tokens", 0),
            "output_tokens": payload.get("output_tokens", 0),
            "total_tokens": payload.get("total_tokens", 0),
            "tool_executions": payload.get("tool_executions", []),
            "tools_available": payload.get("tools_available", []),
            "tool_catalogue": payload.get("tool_catalogue", []),
        }
    return {
        "timestamp": event["timestamp"],
        "model": payload.get("model", ""),
        "phase": "turn",
        "turn_id": payload.get("turn_id", ""),
        "iteration": 0,
        "decision": "",
        "notes": "",
        "trigger": payload.get("trigger", {}) or {},
        "prompt": payload.get("prompt", ""),
        "prompt_messages": payload.get("prompt_messages", []),
        "response": payload.get("response", ""),
        "input_tokens": payload.get("input_tokens", 0),
        "output_tokens": payload.get("output_tokens", 0),
        "total_tokens": payload.get("total_tokens", 0),
        "tool_executions": payload.get("tool_executions", []),
        "tools_available": [],
        "tool_catalogue": [],
    }
