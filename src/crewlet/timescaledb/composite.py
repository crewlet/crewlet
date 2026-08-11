"""Composite event store — merges persistent storage with in-memory freshness.

Writes go to both stores.  Reads prefer the persistent (TimescaleDB)
store but fall back to the memory store for data that hasn't been
indexed yet (eventual consistency).
"""

from __future__ import annotations

import contextlib
from datetime import UTC, datetime
from typing import Any

from crewlet._logging import get_logger

logger = get_logger("timescaledb.composite")


def _ts_key(ts: str) -> str:
    """Normalize a timestamp string to a timezone-naive UTC key for deduplication.

    Handles both aware (``2026-04-07T03:25:04.729224+00:00``) and naive
    (``2026-04-07T03:25:04.729224``) ISO 8601 strings that represent the
    same instant, preventing duplicate entries when memory and the
    persistent store return the same turn with different timezone
    encodings.
    """
    try:
        dt = datetime.fromisoformat(ts)
        if dt.tzinfo is not None:
            dt = dt.astimezone(UTC).replace(tzinfo=None)
        return dt.isoformat()
    except ValueError:
        return ts


class CompositeEventStore:
    """Event store that writes to both persistent and memory, reads merged."""

    def __init__(self, persistent: Any, memory: Any) -> None:
        self._persistent = persistent
        self._memory = memory
        # Maps runtime_id → role for cross-session ID resolution.
        self._id_to_role: dict[str, str] = {}
        # Maps role → set of all known runtime_ids (across sessions).
        self._role_ids: dict[str, set[str]] = {}

    async def start(self) -> None:
        await self._persistent.start()
        await self._memory.start()

    async def close(self) -> None:
        await self._persistent.close()
        await self._memory.close()

    async def write_event(self, **kwargs: Any) -> None:
        """Write to both stores."""
        await self._memory.write_event(**kwargs)
        try:
            await self._persistent.write_event(**kwargs)
        except Exception as exc:
            logger.warning("persistent_write_failed", error=str(exc))

    async def list_events(self, **kwargs: Any) -> list[dict[str, Any]]:
        """List events — prefer persistent store for full history."""
        try:
            result = await self._persistent.list_events(**kwargs)
            if result:
                return result
        except Exception:
            pass
        return await self._memory.list_events(**kwargs)

    async def get_event(self, event_id: str) -> dict[str, Any] | None:
        try:
            result = await self._persistent.get_event(event_id)
            if result is not None:
                return result
        except Exception:
            pass
        return await self._memory.get_event(event_id)

    async def list_trace(self, trace_id: str) -> list[dict[str, Any]]:
        try:
            result = await self._persistent.list_trace(trace_id)
            if result:
                return result
        except Exception:
            pass
        return await self._memory.list_trace(trace_id)

    async def get_agent_states(
        self, agent_roles: list[str]
    ) -> dict[str, dict[str, Any]]:
        """Merge agent states from both stores.

        Builds an index of role → runtime_ids so that
        ``get_agent_llm_history`` can query all IDs for a role.  Token
        totals are computed via :meth:`list_token_usage_events` so that
        events written to both stores are counted exactly once.
        """
        persistent_states: dict[str, dict[str, Any]] = {}
        with contextlib.suppress(Exception):
            persistent_states = await self._persistent.get_agent_states(agent_roles)

        memory_states = await self._memory.get_agent_states(agent_roles)

        # Track all runtime_ids we see per role.
        for role, state in persistent_states.items():
            rid = state.get("runtime_id", "")
            if rid:
                self._id_to_role[rid] = role
                self._role_ids.setdefault(role, set()).add(rid)
        for role, state in memory_states.items():
            rid = state.get("runtime_id", "")
            if rid:
                self._id_to_role[rid] = role
                self._role_ids.setdefault(role, set()).add(rid)

        # Memory has the freshest state; use it as the base and fall back
        # to persistent for roles that haven't emitted events this session.
        merged = dict(memory_states)
        for role, p_state in persistent_states.items():
            if role not in merged:
                merged[role] = p_state

        # Reset token counters — the per-store ``get_agent_states`` calls
        # leave them at zero, but be defensive in case of stale state.
        for state in merged.values():
            state["input_tokens"] = 0
            state["output_tokens"] = 0
            state["total_tokens"] = 0

        # Aggregate tokens from deduplicated turn events.
        #
        # Attribute tokens to a role via the ``agent_role`` column on
        # the token event itself rather than via the ``_id_to_role``
        # index.  The index only captures the single ``runtime_id``
        # exposed by each per-store ``get_agent_states`` call, so
        # events from earlier sessions (different runtime_id, same
        # role) would be silently dropped.  The ``agent_role`` column
        # is populated by ``EventStoreWriter._extract_tags`` for every
        # event that carries a ``role`` attribute, so it's reliable
        # for cross-session aggregation.  Fall back to the index only
        # for rows whose event carried no ``role`` attribute (empty
        # ``agent_role``).
        token_events = await self.list_token_usage_events()
        for ev in token_events:
            role = ev.get("agent_role", "") or self._id_to_role.get(
                ev.get("agent_id", ""), ""
            )
            if not role or role not in merged:
                continue
            state = merged[role]
            state["input_tokens"] += ev.get("input_tokens", 0)
            state["output_tokens"] += ev.get("output_tokens", 0)
            state["total_tokens"] += ev.get("total_tokens", 0)

        return merged

    async def list_token_usage_events(
        self, *, since_days: int = 30
    ) -> list[dict[str, Any]]:
        """Merge token-usage events from both stores, deduped by ``event_id``.

        Persistent wins on conflict — memory entries with the same
        ``event_id`` are dropped, while memory entries with no persistent
        counterpart (e.g. not yet flushed to the persistent store) are
        kept.
        """
        persistent_events: list[dict[str, Any]] = []
        with contextlib.suppress(Exception):
            persistent_events = await self._persistent.list_token_usage_events(
                since_days=since_days
            )
        memory_events = await self._memory.list_token_usage_events(
            since_days=since_days
        )

        seen: set[str] = set()
        merged: list[dict[str, Any]] = []
        for ev in persistent_events:
            eid = ev.get("event_id", "")
            if eid:
                seen.add(eid)
            merged.append(ev)
        for ev in memory_events:
            eid = ev.get("event_id", "")
            if eid and eid in seen:
                continue
            merged.append(ev)
        return merged

    async def list_phase_token_events(
        self, *, since_days: int = 30, agent_role: str | None = None
    ) -> list[dict[str, Any]]:
        """Merge per-phase token events from both stores, deduped by ``event_id``.

        Same dedupe rule as :meth:`list_token_usage_events`: persistent
        wins on conflict, memory entries with no persistent counterpart
        (not yet flushed) are kept.
        """
        persistent_events: list[dict[str, Any]] = []
        with contextlib.suppress(Exception):
            persistent_events = await self._persistent.list_phase_token_events(
                since_days=since_days, agent_role=agent_role
            )
        memory_events = await self._memory.list_phase_token_events(
            since_days=since_days, agent_role=agent_role
        )

        seen: set[str] = set()
        merged: list[dict[str, Any]] = []
        for ev in persistent_events:
            eid = ev.get("event_id", "")
            if eid:
                seen.add(eid)
            merged.append(ev)
        for ev in memory_events:
            eid = ev.get("event_id", "")
            if eid and eid in seen:
                continue
            merged.append(ev)
        return merged

    async def get_agent_llm_history(
        self, agent_id: str, *, limit: int = 50
    ) -> list[dict[str, Any]]:
        """Return LLM history across sessions.

        Queries both stores.  When the requested ``agent_id`` belongs
        to a known role, also queries the persistent store for every
        other runtime_id that has emitted ``agent_turn_completed``
        events for the same role — so the dashboard's history list
        shows every session the agent has run, not just the current
        one.

        Discovery of cross-session runtime_ids comes from the actual
        ``list_token_usage_events`` stream rather than the
        ``_role_ids`` cache.  The cache is populated lossily by
        ``get_agent_states`` (only one ``runtime_id`` per role per
        call surfaces from the per-store step-1 query), which would
        miss any historical session whose ``runtime_id`` happened
        not to be the one iterated last.
        """
        # Resolve the role for the requested agent_id, then discover
        # every runtime_id that has ever emitted a turn for that role.
        role = self._id_to_role.get(agent_id, "")
        all_ids: set[str] = {agent_id}
        if role:
            with contextlib.suppress(Exception):
                token_events = await self.list_token_usage_events()
                for ev in token_events:
                    if ev.get("agent_role", "") == role and (
                        aid := ev.get("agent_id", "")
                    ):
                        all_ids.add(aid)
            # Keep the cache hot for any callers that still depend on it.
            all_ids.update(self._role_ids.get(role, set()))

        # Query memory for current session history.
        memory_history = await self._memory.get_agent_llm_history(agent_id, limit=limit)

        # Query persistent store for all known IDs (current + historical).
        persistent_history: list[dict[str, Any]] = []
        for aid in all_ids:
            with contextlib.suppress(Exception):
                batch = await self._persistent.get_agent_llm_history(aid, limit=limit)
                persistent_history.extend(batch)

        if not persistent_history:
            # Persistent store has nothing yet (buffer not flushed) —
            # fall back to memory.
            return memory_history

        # The persistent store is the source of truth.  Only supplement
        # with memory entries that are strictly newer than the most
        # recent persisted entry — these are turns that were published
        # but not yet flushed.
        newest_persisted_ts = max(_ts_key(h["timestamp"]) for h in persistent_history)
        merged = list(persistent_history)
        for h in memory_history:
            if _ts_key(h["timestamp"]) > newest_persisted_ts:
                merged.append(h)
        # Newest first across the cross-session set.
        merged.sort(key=lambda h: _ts_key(h["timestamp"]), reverse=True)
        return merged[:limit]
