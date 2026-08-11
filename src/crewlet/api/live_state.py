"""In-memory live-state projection for the dashboard API.

This module is the dashboard's load-bearing projection.  It consumes
the engine event stream — the same feed the
WebSocket fan-out (:mod:`crewlet.api.streaming`) reads — and maintains,
per agent role:

* the agent's live ``state`` (idle / working / afk / terminated),
* the current task / phase / iteration,
* cumulative token totals,
* and, crucially, the **in-flight LLM call** (the latest
  ``agent_turn_progress``): phase, round, model, the response accumulated
  so far, and the tool calls fired so far.

Two problems this design solves
-------------------------------

**Refresh survival.**  ``agent_turn_progress`` events are *stream-only* —
the event-store writer drops them (they're not in ``_CATEGORY_MAP``), so
the durable record of a turn only appears once the phase *completes*
(``agent_phase_completed``).  A dashboard that rebuilt agent history
from the store on every (re)connect would lose any LLM call mid-flight
when you hit refresh until its phase finished.  Holding
the in-flight call here and surfacing it in the snapshot every client
receives on connect makes the live row survive a refresh.

**No per-read database scan.**  Re-deriving agent state from a
30-day ``DISTINCT ON`` event scan on *every* ``/agents`` request, every
``/stream/snapshot``, and every WebSocket connect would not scale.
Here it is maintained incrementally from the stream and read in O(1).
The store is queried
once at startup to hydrate the baseline, and thereafter only for
genuinely historical detail (LLM history, memory, traces, token
breakdowns).

Robustness
----------

The standalone-API path receives events over Pulsar, where ordering is
only guaranteed *within* a topic.  Different event types are different
topics (``crewlet.events.task_completed`` vs
``crewlet.events.agent_phase_completed``), so a state-affecting event can
arrive out of order relative to another.  Every state transition is
therefore gated on the event timestamp: an older event can never clobber
a newer state.  The in-flight call is gated on round progression within a
turn.
"""

from __future__ import annotations

from collections import deque
from dataclasses import dataclass
from typing import Any

from crewlet._logging import get_logger

logger = get_logger("api.live_state")

# How many recent (persisted-category) events the projection retains for
# the dashboard's activity feed / events view.  Matches the snapshot
# cap so the activity feed and the initial paint agree.
_EVENT_BUFFER_SIZE = 400

# Event type → coarse agent state.  Mirrors ``_event_to_state`` in
# :mod:`crewlet.timescaledb.repository` so the live projection and the
# cold-start hydration agree on what each event means.
_EVENT_STATE: dict[str, str] = {
    "agent_spawned": "idle",
    "task_started": "working",
    "task_completed": "idle",
    "task_failed": "idle",
    "agent_terminated": "terminated",
    "agent_turn_progress": "working",
    "agent_phase_started": "working",
    "agent_phase_completed": "working",
    "reflection_completed": "idle",
    "llm_unavailable": "afk",
    "turn.guard_breach": "afk",
    "budget_exhausted": "afk",
}

# Engine-detected failure events that flip an agent to ``afk`` and carry a
# cause the dashboard renders as a status quip.
_AFK_EVENTS = frozenset({"llm_unavailable", "turn.guard_breach", "budget_exhausted"})

# Detached sandbox-run lifecycle events that feed the running-sandboxes
# panel: started → tracked; clarification → flips to awaiting-input;
# completed → dropped.
_SANDBOX_EVENTS = frozenset(
    {
        "sandbox_run_started",
        "sandbox_clarification_requested",
        "sandbox_run_completed",
    }
)


@dataclass(slots=True)
class AgentLive:
    """Live, incrementally-maintained state for one agent role."""

    role: str
    runtime_id: str = ""
    state: str = "offline"
    current_task: str | None = None
    current_phase: str | None = None
    current_iteration: int = 0
    input_tokens: int = 0
    output_tokens: int = 0
    total_tokens: int = 0
    afk_reason: str = ""
    # The in-flight LLM call (latest ``agent_turn_progress`` / the
    # ``agent_phase_started`` placeholder), or ``None`` between turns.
    live_call: dict[str, Any] | None = None
    # Timestamp (ISO-8601) of the last *state-affecting* event applied —
    # the reorder guard.
    _state_ts: str = ""

    def to_overlay(self) -> dict[str, Any]:
        """Return the live fields to overlay onto a static config row."""
        overlay: dict[str, Any] = {
            "state": self.state,
            "runtime_id": self.runtime_id,
            "current_task": self.current_task,
            "current_phase": self.current_phase,
            "current_iteration": self.current_iteration,
            "input_tokens": self.input_tokens,
            "output_tokens": self.output_tokens,
            "total_tokens": self.total_tokens,
            "live_call": self.live_call,
        }
        if self.afk_reason:
            overlay["afk_reason"] = self.afk_reason
        return overlay


class LiveState:
    """A reactive, in-memory mirror of every agent's current state.

    Fed one event at a time via :meth:`apply_event`; read in O(1) via
    :meth:`merge_agents` / :meth:`recent_events`.  All mutation happens in
    synchronous blocks (no ``await``), so the projection is atomic from
    the single-threaded event loop's perspective — no lock required.
    """

    def __init__(self, *, event_buffer_size: int = _EVENT_BUFFER_SIZE) -> None:
        self._agents: dict[str, AgentLive] = {}
        # In-flight detached sandbox jobs, keyed by kick-off turn_id.
        # Populated from the SandboxRun* lifecycle events (see
        # _apply_sandbox); read by active_sandboxes() for the dashboard.
        self._active_sandboxes: dict[str, dict[str, Any]] = {}
        # Chronological ring buffer of persisted-category events.
        self._events: deque[dict[str, Any]] = deque(maxlen=event_buffer_size)
        # ``agent_turn_completed`` event ids already counted toward token
        # totals — prevents double-counting hydrated turns against
        # streamed ones.
        self._counted_token_ids: set[str] = set()

    # -- read side -------------------------------------------------------

    def merge_agents(self, static_roles: list[dict[str, Any]]) -> list[dict[str, Any]]:
        """Overlay live state onto each static config role row.

        Roles with no live entry yet are returned as-is (the dashboard
        renders them ``offline``).  Order follows ``static_roles``.
        """
        out: list[dict[str, Any]] = []
        for role in static_roles:
            merged = dict(role)
            live = self._agents.get(role.get("role", ""))
            if live is not None:
                merged.update(live.to_overlay())
            out.append(merged)
        return out

    def agent_overlay(self, role: str) -> dict[str, Any] | None:
        """Return the live overlay for one role, or ``None``."""
        live = self._agents.get(role)
        return live.to_overlay() if live is not None else None

    def runtime_id_for(self, role: str) -> str:
        live = self._agents.get(role)
        return live.runtime_id if live is not None else ""

    def recent_events(self, limit: int = _EVENT_BUFFER_SIZE) -> list[dict[str, Any]]:
        """Return recent events, newest first, capped at ``limit``."""
        events = list(self._events)
        events.reverse()
        return events[:limit]

    def active_sandboxes(self) -> list[dict[str, Any]]:
        """In-flight detached sandbox jobs, oldest-first.

        Oldest-first so the longest-running job (most likely to need
        attention, e.g. one blocked on a clarification) sorts to the top
        of the dashboard panel.
        """
        return sorted(
            self._active_sandboxes.values(), key=lambda s: s.get("started_at", "")
        )

    # -- write side ------------------------------------------------------

    def apply_event(self, envelope: dict[str, Any]) -> None:
        """Update the projection from one serialized event envelope.

        ``envelope`` is the dashboard envelope shape produced by
        :func:`crewlet.api.streaming.serialize_event` (``type`` /
        ``payload`` / ``timestamp`` / ``category`` / …).  Reading off the
        serialized payload keeps engine events and webhook events on the
        same code path.
        """
        etype = envelope.get("type", "")
        payload = envelope.get("payload") or {}

        # The in-flight call is stream-only: update it, but never let it
        # into the persisted-event buffer.
        if etype == "agent_turn_progress":
            self._apply_progress(envelope, payload)
            return

        # Everything else that carries a category is a persisted event —
        # mirror it into the activity buffer (this is what ``/events``
        # and the snapshot's recent-activity feed show).
        if envelope.get("category"):
            self._record_event(envelope)

        # Detached sandbox lifecycle: maintain the in-flight set, then stop
        # (these don't drive an agent's run-state machine below).
        if etype in _SANDBOX_EVENTS:
            self._apply_sandbox(etype, envelope, payload)
            return

        role = payload.get("role") or payload.get("agent_role") or ""
        if not role:
            return
        agent = self._ensure_agent(role)
        if agent_id := payload.get("agent_id", ""):
            agent.runtime_id = agent_id

        if etype == "agent_turn_completed":
            self._add_turn_tokens(agent, envelope, payload)

        self._apply_state(agent, etype, envelope, payload)

    def _apply_state(
        self,
        agent: AgentLive,
        etype: str,
        envelope: dict[str, Any],
        payload: dict[str, Any],
    ) -> None:
        """Apply a state-affecting event, gated on the reorder guard."""
        if etype not in _EVENT_STATE:
            return
        ts = str(envelope.get("timestamp", ""))
        # Reorder guard: a strictly-older event must not clobber newer
        # state.  Equal timestamps are allowed through (same-instant
        # bursts) — the later-applied wins, matching the store's
        # ``event_id DESC`` tiebreak closely enough for the dashboard.
        if ts and agent._state_ts and ts < agent._state_ts:
            return
        if ts:
            agent._state_ts = ts

        if etype == "agent_spawned":
            if agent.state in ("offline", "terminated"):
                agent.state = "idle"
        elif etype == "task_started":
            agent.state = "working"
            agent.current_task = payload.get("task_id") or None
            agent.afk_reason = ""
        elif etype in ("task_completed", "task_failed"):
            agent.state = "idle"
            agent.current_task = None
            agent.current_phase = None
            agent.current_iteration = 0
            agent.afk_reason = ""
            agent.live_call = None
        elif etype == "agent_phase_started":
            agent.state = "working"
            agent.afk_reason = ""
            agent.current_phase = payload.get("phase") or None
            agent.current_iteration = int(payload.get("iteration", 0) or 0)
            self._begin_live_call(agent, payload)
        elif etype == "agent_phase_completed":
            agent.state = "working"
            agent.afk_reason = ""
            self._finish_live_call(agent, payload)
        elif etype == "reflection_completed":
            agent.state = "idle"
            agent.current_phase = None
            agent.current_iteration = 0
            agent.live_call = None
        elif etype == "agent_terminated":
            agent.state = "terminated"
            agent.live_call = None
        elif etype in _AFK_EVENTS:
            agent.state = "afk"
            agent.afk_reason = payload.get("kind") or etype
            agent.live_call = None

    # -- sandbox runs ----------------------------------------------------

    def _apply_sandbox(
        self,
        etype: str,
        envelope: dict[str, Any],
        payload: dict[str, Any],
    ) -> None:
        """Maintain the in-flight sandbox set from one SandboxRun* event."""
        turn_id = payload.get("turn_id", "")
        if not turn_id:
            return
        if etype == "sandbox_run_completed":
            self._active_sandboxes.pop(turn_id, None)
            return
        if etype == "sandbox_run_started":
            self._active_sandboxes[turn_id] = {
                "turn_id": turn_id,
                "role": payload.get("role", ""),
                "agent_handle": payload.get("agent_handle", ""),
                "agent_id": payload.get("agent_id", ""),
                "coding_agent": payload.get("coding_agent", ""),
                "sandbox_id": payload.get("sandbox_id", ""),
                "task": payload.get("task", ""),
                "status": "running",
                "started_at": str(envelope.get("timestamp", "")),
            }
            return
        # sandbox_clarification_requested → flip to awaiting-input. The
        # started event may have been missed (API started mid-run), so
        # synthesize a minimal entry rather than dropping the signal.
        entry = self._active_sandboxes.setdefault(
            turn_id,
            {
                "turn_id": turn_id,
                "role": payload.get("role", ""),
                "agent_handle": payload.get("agent_handle", ""),
                "agent_id": payload.get("agent_id", ""),
                "coding_agent": payload.get("coding_agent", ""),
                "sandbox_id": payload.get("sandbox_id", ""),
                "task": "",
                "started_at": str(envelope.get("timestamp", "")),
            },
        )
        entry["status"] = "awaiting_input"
        entry["question"] = payload.get("question", "")
        entry["audience"] = payload.get("audience", "")

    # -- in-flight call --------------------------------------------------

    def _begin_live_call(self, agent: AgentLive, payload: dict[str, Any]) -> None:
        """Seed a placeholder in-flight call when a phase opens.

        The placeholder makes the live row appear the instant a phase
        starts; ``agent_turn_progress`` rounds then fill in the model,
        response, and tool calls.
        """
        agent.live_call = {
            "turn_id": payload.get("turn_id", ""),
            "phase": payload.get("phase", ""),
            "iteration": int(payload.get("iteration", 0) or 0),
            "model": "",
            # The event that triggered this turn (carried on the phase
            # event) so a refresh mid-call shows the live row's source.
            "trigger": payload.get("trigger", {}) or {},
            "prompt": "",
            "prompt_messages": [],
            "response": "",
            "input_tokens": 0,
            "output_tokens": 0,
            "total_tokens": 0,
            "tool_executions": [],
            "round_num": -1,
            "rounds": 0,
            "in_progress": True,
            "updated_at": payload.get("timestamp", ""),
        }

    def _apply_progress(
        self, envelope: dict[str, Any], payload: dict[str, Any]
    ) -> None:
        """Fold an ``agent_turn_progress`` round into the in-flight call."""
        role = payload.get("role") or payload.get("agent_role") or ""
        if not role:
            return
        agent = self._ensure_agent(role)
        if agent_id := payload.get("agent_id", ""):
            agent.runtime_id = agent_id
        if agent.state not in ("working",):
            agent.state = "working"
            agent.afk_reason = ""

        turn_id = payload.get("turn_id", "")
        phase = payload.get("phase", "")
        iteration = int(payload.get("iteration", 0) or 0)
        round_num = int(payload.get("round_num", 0) or 0)
        ts = str(envelope.get("timestamp", ""))

        cur = agent.live_call
        # Accept the update when there's no live call, when it's a newer
        # round of the same (turn, phase, iteration), or when it belongs
        # to a different/newer call.  A stale earlier round for the same
        # call is ignored.
        if cur is not None and self._same_call(cur, turn_id, phase, iteration):
            if round_num < int(cur.get("round_num", -1)):
                return
        elif cur is not None and ts and str(cur.get("updated_at", "")) > ts:
            return

        agent.current_phase = phase or agent.current_phase
        agent.current_iteration = iteration
        # Prefer the round's own trigger; fall back to the placeholder's
        # (seeded from agent_phase_started) so the source never blanks out
        # mid-call.
        prev_trigger = cur.get("trigger") if cur is not None else None
        agent.live_call = {
            "turn_id": turn_id,
            "phase": phase,
            "iteration": iteration,
            "trigger": (payload.get("trigger") or prev_trigger or {}),
            "model": payload.get("model", ""),
            "prompt": payload.get("prompt", ""),
            "prompt_messages": payload.get("prompt_messages", []),
            "response": payload.get("response", ""),
            "input_tokens": int(payload.get("input_tokens", 0) or 0),
            "output_tokens": int(payload.get("output_tokens", 0) or 0),
            "total_tokens": int(payload.get("total_tokens", 0) or 0),
            "tool_executions": payload.get("tool_executions", []),
            "round_num": round_num,
            "rounds": round_num + 1,
            "in_progress": True,
            "updated_at": ts,
        }

    def _finish_live_call(self, agent: AgentLive, payload: dict[str, Any]) -> None:
        """Clear the in-flight call when its phase completes.

        Only clears when the completed phase matches the live call — a
        late ``agent_phase_completed`` for a prior phase must not wipe a
        newer phase's live row.
        """
        cur = agent.live_call
        if cur is None:
            return
        if self._same_call(
            cur,
            payload.get("turn_id", ""),
            payload.get("phase", ""),
            int(payload.get("iteration", 0) or 0),
        ):
            agent.live_call = None

    @staticmethod
    def _same_call(
        call: dict[str, Any], turn_id: str, phase: str, iteration: int
    ) -> bool:
        return (
            call.get("turn_id", "") == turn_id
            and call.get("phase", "") == phase
            and int(call.get("iteration", 0) or 0) == iteration
        )

    # -- tokens ----------------------------------------------------------

    def _add_turn_tokens(
        self, agent: AgentLive, envelope: dict[str, Any], payload: dict[str, Any]
    ) -> None:
        """Accumulate a completed turn's tokens, deduped by event id."""
        event_id = envelope.get("id", "")
        if event_id and event_id in self._counted_token_ids:
            return
        if event_id:
            self._counted_token_ids.add(event_id)
        agent.input_tokens += int(payload.get("input_tokens", 0) or 0)
        agent.output_tokens += int(payload.get("output_tokens", 0) or 0)
        agent.total_tokens += int(payload.get("total_tokens", 0) or 0)

    # -- buffer ----------------------------------------------------------

    def _record_event(self, envelope: dict[str, Any]) -> None:
        """Append a light (payload-free) copy of an event to the buffer."""
        self._events.append(
            {
                "id": envelope.get("id", ""),
                "type": envelope.get("type", ""),
                "timestamp": envelope.get("timestamp", ""),
                "source": envelope.get("source", ""),
                "actor": envelope.get("actor", ""),
                "summary": envelope.get("summary", ""),
                "category": envelope.get("category", ""),
                "trace_id": envelope.get("trace_id", ""),
                "span_id": envelope.get("span_id", ""),
                "parent_span_id": envelope.get("parent_span_id", ""),
                "topic": envelope.get("topic", ""),
            }
        )

    def _ensure_agent(self, role: str) -> AgentLive:
        agent = self._agents.get(role)
        if agent is None:
            agent = AgentLive(role=role)
            self._agents[role] = agent
        return agent

    # -- hydration -------------------------------------------------------

    async def hydrate(
        self, store: Any, roles: list[str], *, only_states: bool = False
    ) -> None:
        """Seed the projection from the event store at startup.

        Reads the baseline agent states, the recent-events tail, and the
        token-event ids so the very first snapshot already reflects
        history accumulated before this process started.  Best-effort:
        any store error leaves the projection empty and it rebuilds from
        the live stream.

        ``only_states`` re-reads just the baseline agent states — used by
        the standalone API to hydrate state once its roles arrive via
        config refresh, *after* an initial role-less hydrate already
        seeded the (role-independent) token and event buffers.  Token and
        event hydration *add* to running totals, so they must run exactly
        once.
        """
        if store is None:
            return
        await self._hydrate_states(store, roles)
        if only_states:
            return
        await self._hydrate_tokens(store)
        await self._hydrate_events(store)

    async def _hydrate_states(self, store: Any, roles: list[str]) -> None:
        if not roles:
            return
        try:
            states = await store.get_agent_states(roles)
        except Exception as exc:  # pragma: no cover - defensive
            logger.warning("live_state_hydrate_states_failed", error=str(exc))
            return
        for role, st in states.items():
            agent = self._ensure_agent(role)
            agent.state = st.get("state", "offline")
            agent.runtime_id = st.get("runtime_id", "") or ""
            agent.current_task = st.get("current_task")
            agent.current_phase = st.get("current_phase")
            agent.current_iteration = int(st.get("current_iteration", 0) or 0)
            if "afk_reason" in st:
                agent.afk_reason = st.get("afk_reason", "") or ""

    async def _hydrate_tokens(self, store: Any) -> None:
        try:
            token_events = await store.list_token_usage_events()
        except Exception as exc:  # pragma: no cover - defensive
            logger.warning("live_state_hydrate_tokens_failed", error=str(exc))
            return
        for ev in token_events:
            role = ev.get("agent_role", "")
            if not role:
                continue
            agent = self._ensure_agent(role)
            agent.input_tokens += int(ev.get("input_tokens", 0) or 0)
            agent.output_tokens += int(ev.get("output_tokens", 0) or 0)
            agent.total_tokens += int(ev.get("total_tokens", 0) or 0)
            if event_id := ev.get("event_id", ""):
                self._counted_token_ids.add(event_id)

    async def _hydrate_events(self, store: Any) -> None:
        try:
            events = await store.list_events(limit=self._events.maxlen or 400)
        except Exception as exc:  # pragma: no cover - defensive
            logger.warning("live_state_hydrate_events_failed", error=str(exc))
            return
        # ``list_events`` is newest-first; the buffer is chronological.
        for row in reversed(events):
            self._events.append(
                {
                    "id": row.get("id", ""),
                    "type": row.get("type", ""),
                    "timestamp": row.get("timestamp", ""),
                    "source": row.get("source", ""),
                    "actor": row.get("actor", ""),
                    "summary": row.get("summary", ""),
                    "category": row.get("category", ""),
                    "trace_id": row.get("trace_id", ""),
                    "span_id": row.get("span_id", ""),
                    "parent_span_id": row.get("parent_span_id", ""),
                    "topic": row.get("topic", ""),
                }
            )
