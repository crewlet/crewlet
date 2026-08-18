"""EventStore protocol — abstract interface for persistent event storage."""

from __future__ import annotations

from datetime import datetime
from typing import Any, Protocol, runtime_checkable


@runtime_checkable
class EventStore(Protocol):
    """Protocol for persistent event storage.

    Implementations: ``TimescaleDBEventStore`` (TimescaleDB/PostgreSQL)
    and ``MemoryEventStore`` (in-memory fallback for dev/test).
    """

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
        """Persist a single event."""
        ...

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
        """Return recent events (without payload), newest first.

        Ordered by ``(timestamp, id)`` DESCENDING.  The id is the
        tiebreak and is not optional: burst writes share a timestamp at
        microsecond resolution, and a cursor over a non-unique key skips
        or repeats whatever collided with it.

        Every row carries a ``failed`` boolean.  It is derived from the
        stored ``failed`` tag, because this read never returns payloads;
        events written before the writer began stamping that tag read
        back as not-failed.

        *before* is an exclusive keyset cursor ``(timestamp, event_id)``:
        return only rows strictly older than it.  A page SHORTER than
        *limit* means the history is exhausted -- except under
        *related_agent*, which over-fetches and post-filters, so that
        surface only knows it is done when a page returns zero rows.

        *related_agent* is a broad filter: matches events where the agent
        name appears in ``actor``, or in tags (``agent_role``, ``target``,
        ``recipient``, ``sender``).  It also pulls in every event sharing
        a trace with a direct match, so a caller must dedupe by id.

        Implementations differ in how far back they reach: the persistent
        store has a fixed retention floor (``EVENT_HISTORY_DAYS``), the
        in-memory one is bounded by row count instead.  A cursor that
        crosses either simply returns nothing more.
        """
        ...

    async def get_event(self, event_id: str) -> dict[str, Any] | None:
        """Return a single event with full payload, or ``None``."""
        ...

    async def list_trace(self, trace_id: str) -> list[dict[str, Any]]:
        """Return all events in a trace, OLDEST first, without payloads.

        Ordered by ``(timestamp, id)`` ascending, because a trace is read
        as a causal sequence.  Capped at ``MAX_TRACE_EVENTS``, keeping
        the oldest rows -- the root is what explains a trace -- so a
        caller seeing exactly that many should say the view is truncated.
        Rows carry ``failed``, as in :meth:`list_events`.
        """
        ...

    async def get_agent_states(
        self, agent_roles: list[str]
    ) -> dict[str, dict[str, Any]]:
        """Return live state for each agent role.

        Returns ``{role: {state, current_task, input_tokens, ...}}``.
        Token totals are populated by ``CompositeEventStore`` via
        :meth:`list_token_usage_events`; individual stores leave them at zero.
        """
        ...

    async def list_token_usage_events(
        self, *, since_days: int = 30
    ) -> list[dict[str, Any]]:
        """Return one record per ``agent_turn_completed`` event.

        Each record carries ``event_id``, ``agent_id``, ``agent_role``,
        ``input_tokens``, ``output_tokens``, ``total_tokens`` and
        ``timestamp``.  Used by :class:`CompositeEventStore` to
        deduplicate token counts across overlapping stores by
        ``event_id`` and attribute them to a role without having to
        round-trip through a separate ``agent_id → role`` index (which
        can miss runtime_ids from earlier sessions).
        """
        ...

    async def list_phase_token_events(
        self, *, since_days: int = 30, agent_role: str | None = None
    ) -> list[dict[str, Any]]:
        """Return per-phase token records from ``agent_phase_completed``.

        One row per phase invocation (Plan / Execute / Review /
        Sub-agent / Auxiliary / Judge). Carries the fields the
        dashboard's "Tokens" view needs to roll up spend by phase,
        model, agent, worker or turn:

        ``event_id``, ``timestamp``, ``agent_id``, ``agent_role``,
        ``phase``, ``host_phase``, ``worker``, ``model``, ``turn_id``,
        ``iteration``, ``input_tokens``, ``output_tokens``,
        ``total_tokens``, ``tool_executions``.

        Used by :class:`CompositeEventStore.list_phase_token_events` to
        dedupe across overlapping stores by ``event_id`` (mirroring the
        :meth:`list_token_usage_events` pattern).
        """
        ...

    async def get_agent_llm_history(
        self, agent_id: str, *, limit: int = 50
    ) -> list[dict[str, Any]]:
        """Return recent LLM invocation records for an agent."""
        ...

    async def start(self) -> None:
        """Initialize the store (connect, etc.)."""
        ...

    async def close(self) -> None:
        """Shut down the store (close connections, etc.)."""
        ...
