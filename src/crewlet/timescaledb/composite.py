"""Composite event store — merges persistent storage with in-memory freshness.

Writes go to both stores.  Reads MERGE both, deduped by id with the
persistent copy winning, so a row that has not been indexed yet is still
visible and a persistent leg that answers nothing does not hand back the
memory leg's newest rows in its place.
"""

from __future__ import annotations

import contextlib
from typing import Any

from crewlet._logging import get_logger
from crewlet.timescaledb._time import row_key, ts_key
from crewlet.timescaledb.repository import MAX_TRACE_EVENTS

logger = get_logger("timescaledb.composite")


def _merge_by_id(
    primary: list[dict[str, Any]], secondary: list[dict[str, Any]]
) -> list[dict[str, Any]]:
    """Union two event lists, keeping ``primary``'s copy on a conflict."""
    seen = {str(row.get("id", "")) for row in primary if row.get("id")}
    merged = list(primary)
    merged.extend(
        row for row in secondary if not row.get("id") or str(row["id"]) not in seen
    )
    return merged


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
        """Merge both legs, deduped by id, newest first.

        This used to return the persistent leg whenever it was non-empty
        and fall through to memory otherwise.  Two problems, and paging
        turned the second into a visible one:

        * memory rows with no persistent counterpart -- events not yet
          flushed -- were invisible whenever the persistent store had
          anything at all;
        * under a cursor, an empty persistent page means "no more
          history", and falling through hands back memory's NEWEST rows.
          A reader who scrolled to the bottom would be teleported to the
          present with no error.

        Same rule the token-usage merge already uses: persistent wins on
        conflict, memory fills the gaps.
        """
        persistent: list[dict[str, Any]] = []
        with contextlib.suppress(Exception):
            persistent = await self._persistent.list_events(**kwargs)
        memory = await self._memory.list_events(**kwargs)
        merged = _merge_by_id(persistent, memory)
        merged.sort(key=row_key, reverse=True)
        limit = int(kwargs.get("limit", 50) or 50)
        return merged[:limit]

    async def get_event(self, event_id: str) -> dict[str, Any] | None:
        try:
            result = await self._persistent.get_event(event_id)
            if result is not None:
                return result
        except Exception:
            pass
        return await self._memory.get_event(event_id)

    async def list_trace(self, trace_id: str) -> list[dict[str, Any]]:
        """Merge both legs, deduped by id, OLDEST first.

        A trace is read as a causal sequence, so its order is the
        opposite of a feed's -- and a half-flushed trace showing only
        its persistent spans would read as a turn that skipped steps.
        """
        persistent: list[dict[str, Any]] = []
        with contextlib.suppress(Exception):
            persistent = await self._persistent.list_trace(trace_id)
        memory = await self._memory.list_trace(trace_id)
        merged = _merge_by_id(persistent, memory)
        merged.sort(key=row_key)
        return merged[:MAX_TRACE_EVENTS]

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
        # The satellite leg first, in full: it is capped in memory, it
        # carries the events not yet flushed, and its ids are what the
        # persistent rollup must not count twice.
        memory_events = await self._memory.list_token_usage_events()
        counted_ids: list[str] = []
        for ev in memory_events:
            event_id = str(ev.get("event_id", "") or "")
            if event_id:
                counted_ids.append(event_id)
            role = ev.get("agent_role", "") or self._id_to_role.get(
                ev.get("agent_id", ""), ""
            )
            if not role or role not in merged:
                continue
            state = merged[role]
            state["input_tokens"] += ev.get("input_tokens", 0)
            state["output_tokens"] += ev.get("output_tokens", 0)
            state["total_tokens"] += ev.get("total_tokens", 0)

        # The persistent leg is summed BY THE DATABASE. This runs on
        # every `/agents` read and every dashboard refresh, and it used
        # to pull every `agent_turn_completed` row in a 30-day window —
        # full JSONB payload included — to add three integers per row.
        # Where the store cannot do that, fall back to the row scan
        # rather than reporting no tokens at all.
        summed = False
        roll_up = getattr(self._persistent, "sum_token_usage_by_role", None)
        if roll_up is not None:
            try:
                totals = await roll_up(exclude_event_ids=counted_ids)
            except Exception:
                logger.warning("token_rollup_sql_failed", exc_info=True)
            else:
                summed = True
                for role, sums in totals.items():
                    state = merged.get(role)
                    if state is None:
                        continue
                    state["input_tokens"] += sums.get("input_tokens", 0)
                    state["output_tokens"] += sums.get("output_tokens", 0)
                    state["total_tokens"] += sums.get("total_tokens", 0)
        if not summed:
            seen = set(counted_ids)
            persistent_events: list[dict[str, Any]] = []
            with contextlib.suppress(Exception):
                persistent_events = await self._persistent.list_token_usage_events()
            for ev in persistent_events:
                if str(ev.get("event_id", "") or "") in seen:
                    continue
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
        newest_persisted_ts = max(ts_key(h["timestamp"]) for h in persistent_history)
        merged = list(persistent_history)
        for h in memory_history:
            if ts_key(h["timestamp"]) > newest_persisted_ts:
                merged.append(h)
        # Newest first across the cross-session set.
        merged.sort(key=lambda h: ts_key(h["timestamp"]), reverse=True)
        return merged[:limit]
