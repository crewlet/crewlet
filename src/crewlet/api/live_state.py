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
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Any

from crewlet._logging import get_logger

logger = get_logger("api.live_state")

# How many recent (persisted-category) events the projection retains for
# the dashboard's activity feed / events view -- and the number a
# snapshot ships.  ONE number: the ring, the hydration read, and the
# snapshot all derive from it.  They used to be three (400 retained, 150
# sent, 250 kept client-side), so a tab streamed its feed up to 250 rows
# and then a refresh visibly snapped it back to 150 while 250 of the
# server's rows could never be delivered at all.
EVENT_FEED_LIMIT = 400

# Cap on the ``agent_turn_completed`` id set used to dedupe token
# accounting.  The set exists to stop a hydrated turn being counted a
# second time when the same turn also arrives on the live stream, so it
# only ever needs to cover the hydration overlap plus any re-delivery --
# a window of minutes, not the process lifetime.  It was an unbounded
# ``set`` seeded with 30 days of ids and never pruned, which is a slow
# leak in an API process that stays up for weeks.  8k ids is orders of
# magnitude beyond any real overlap and costs well under a megabyte.
_TOKEN_DEDUPE_LIMIT = 8000

# Window the LIVE spend rollup covers — the one held in memory, shipped
# in every snapshot and pushed as phases complete.  Deliberately shorter
# than ``TOKEN_WINDOW_DAYS``: "live" means what is happening now, and the
# rollup is re-aggregated from its records each time it is pushed, so its
# cost is set by how many records the window holds.  Any wider window the
# Tokens view offers is a store query (``/tokens/breakdown``) instead,
# which is not on any hot path.
LIVE_SPEND_WINDOW_HOURS = 24

# Memory and latency backstop on retained per-phase spend records.  The
# real bound is the window above; this only binds for an org emitting
# more than this in a day.  Measured: ``aggregate_phase_events`` costs
# ~13 ms over 2k records, ~32 ms over 5k and ~200 ms over 20k, so the cap
# sets the worst-case aggregation — 8k keeps it near 80 ms, affordable at
# most once a second on a background task.  Truncation drops the OLDEST
# records, so an org past the cap sees a rollup covering slightly less
# than a day rather than a wrong total.
_SPEND_RECORD_LIMIT = 8_000

# Window the projection hydrates per-agent token totals over, in days.
# Matches the dashboard's default token-rollup window
# (``/tokens/breakdown?since_days=7``) so the per-agent totals on a card
# and the rollup above it describe the same period.  The hydration call
# used to take the store's own 30-day default, so the two disagreed by
# three weeks of spend.
TOKEN_WINDOW_DAYS = 7

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

# Event types that ARE a failure by their very type, independent of any
# payload flag.  The feed row carries one ``failed`` boolean and this is
# half of how it is decided (the other half is the event's own ``failed``
# field, set on a phase or turn that died); a dashboard reads the boolean
# and never re-derives failure from a type list of its own.
_FAILURE_EVENTS = frozenset(
    {
        "task_failed",
        "llm_unavailable",
        "budget_exhausted",
        "turn.guard_breach",
    }
)

# The payload-free fields a feed row carries.  Named once because the row
# is built in two places -- live from the stream and hydrated from the
# store -- and two hand-maintained copies of a shape is how a field ends
# up present on a running dashboard and missing after a reload.
_FEED_FIELDS = (
    "id",
    "type",
    "timestamp",
    "source",
    "actor",
    "summary",
    "category",
    "trace_id",
    "span_id",
    "parent_span_id",
    "topic",
)


def _light_event(row: dict[str, Any], *, failed: bool) -> dict[str, Any]:
    """Build one payload-free feed row."""
    light: dict[str, Any] = {key: row.get(key, "") for key in _FEED_FIELDS}
    light["failed"] = failed or row.get("type", "") in _FAILURE_EVENTS
    return light


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


@dataclass
class Change:
    """What one applied event moved in the projection.

    The stream service turns this into the push envelopes a dashboard
    consumes: the changed agents' overlays, the sandbox set, the spend
    rollup.  Anything not named here did not move and is not re-sent.
    """

    agents: set[str] = field(default_factory=set)
    sandboxes: bool = False
    tokens: bool = False
    events: bool = False

    def __bool__(self) -> bool:
        return bool(self.agents or self.sandboxes or self.tokens or self.events)


def _remember(seen: dict[str, None], key: str) -> None:
    """Record ``key`` in a bounded, insertion-ordered id set."""
    seen[key] = None
    while len(seen) > _TOKEN_DEDUPE_LIMIT:
        seen.pop(next(iter(seen)))


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
    # The most recent phase failure, or ``None``.  Set from a failed
    # ``agent_phase_completed`` and cleared the moment the agent starts
    # real work again, so the dashboard can say *why* a seat stopped
    # rather than leaving a call on screen that never answers.
    last_error: dict[str, Any] | None = None
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
            "last_error": self.last_error,
            # Always present, even when empty. The overlay is now MERGED
            # into a client's row rather than replacing it, so an omitted
            # key reads as "unchanged" — leaving a recovered agent
            # wearing the reason it was AFK for.
            "afk_reason": self.afk_reason,
        }
        return overlay


class LiveState:
    """A reactive, in-memory mirror of every agent's current state.

    Fed one event at a time via :meth:`apply_event`; read in O(1) via
    :meth:`merge_agents` / :meth:`recent_events`.  All mutation happens in
    synchronous blocks (no ``await``), so the projection is atomic from
    the single-threaded event loop's perspective — no lock required.
    """

    def __init__(self, *, event_buffer_size: int = EVENT_FEED_LIMIT) -> None:
        self._agents: dict[str, AgentLive] = {}
        # In-flight detached sandbox jobs, keyed by kick-off turn_id.
        # Populated from the SandboxRun* lifecycle events (see
        # _apply_sandbox); read by active_sandboxes() for the dashboard.
        self._active_sandboxes: dict[str, dict[str, Any]] = {}
        # Chronological ring buffer of persisted-category events.
        self._events: deque[dict[str, Any]] = deque(maxlen=event_buffer_size)
        # ``agent_turn_completed`` event ids already counted toward token
        # totals — prevents double-counting hydrated turns against
        # streamed ones.  A ``dict`` used as an insertion-ordered set so
        # the oldest id evicts first once past ``_TOKEN_DEDUPE_LIMIT``.
        self._counted_token_ids: dict[str, None] = {}
        # ``agent_phase_completed`` ids already folded into the spend
        # rollup — the rollup's analogue of the above.
        self._counted_phase_ids: dict[str, None] = {}
        # Per-phase spend records inside the rollup window, in the shape
        # ``aggregate_phase_events`` consumes: hydrated from the store at
        # startup and appended live.  Holding the *records* rather than a
        # folded rollup means the aggregation has exactly one
        # implementation (``api.tokens.aggregate_phase_events``) instead
        # of the three it had — the endpoint's, a re-implementation in
        # the browser, and whatever a reconnect left behind.
        self._phase_spend: deque[dict[str, Any]] = deque(maxlen=_SPEND_RECORD_LIMIT)

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

    def recent_events(self, limit: int = EVENT_FEED_LIMIT) -> list[dict[str, Any]]:
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

    def apply_event(self, envelope: dict[str, Any]) -> Change:
        """Update the projection from one serialized event envelope.

        ``envelope`` is the dashboard envelope shape produced by
        :func:`crewlet.api.streaming.serialize_event` (``type`` /
        ``payload`` / ``timestamp`` / ``category`` / …).  Reading off the
        serialized payload keeps engine events and webhook events on the
        same code path.

        Returns a :class:`Change` naming what this event moved, so the
        stream service can push the *result* of the projection rather
        than the raw event.  That is the whole point: a dashboard should
        mirror this projection, not re-implement it — every client used
        to run its own copy of the state machine below, its own sandbox
        tracking, and its own token aggregation, and each copy drifted
        from this one in its own way.
        """
        etype = envelope.get("type", "")
        payload = envelope.get("payload") or {}
        change = Change()

        # The in-flight call is stream-only: update it, but never let it
        # into the persisted-event buffer.
        if etype == "agent_turn_progress":
            role = self._apply_progress(envelope, payload)
            if role:
                change.agents.add(role)
            return change

        # Everything else that carries a category is a persisted event —
        # mirror it into the activity buffer (this is what ``/events``
        # and the snapshot's recent-activity feed show).
        if envelope.get("category"):
            self._record_event(envelope)
            change.events = True

        # Detached sandbox lifecycle: maintain the in-flight set, then stop
        # (these don't drive an agent's run-state machine below).
        if etype in _SANDBOX_EVENTS:
            self._apply_sandbox(etype, envelope, payload)
            change.sandboxes = True
            return change

        if etype == "agent_phase_completed":
            change.tokens = self._fold_spend(envelope, payload)

        role = payload.get("role") or payload.get("agent_role") or ""
        if not role:
            return change
        agent = self._ensure_agent(role)
        if agent_id := payload.get("agent_id", ""):
            agent.runtime_id = agent_id

        if etype == "agent_turn_completed":
            self._add_turn_tokens(agent, envelope, payload)
            change.agents.add(role)

        if self._apply_state(agent, etype, envelope, payload):
            change.agents.add(role)
        return change

    def _apply_state(
        self,
        agent: AgentLive,
        etype: str,
        envelope: dict[str, Any],
        payload: dict[str, Any],
    ) -> bool:
        """Apply a state-affecting event, gated on the reorder guard.

        Returns whether the agent moved, so the caller knows to push it.
        """
        if etype not in _EVENT_STATE:
            return False
        ts = str(envelope.get("timestamp", ""))
        # Reorder guard: a strictly-older event must not clobber newer
        # state.  Equal timestamps are allowed through (same-instant
        # bursts) — the later-applied wins, matching the store's
        # ``event_id DESC`` tiebreak closely enough for the dashboard.
        if ts and agent._state_ts and ts < agent._state_ts:
            return False
        if ts:
            agent._state_ts = ts

        if etype == "agent_spawned":
            # A spawn is a NEW instance of the seat, so whatever stopped
            # the last one is not this one's state.  Without this the
            # sticky-AFK hold outlives an engine restart and a healthy
            # seat renders as broken until it happens to do some work.
            if agent.state in ("offline", "terminated", "afk"):
                agent.state = "idle"
                agent.afk_reason = ""
                agent.last_error = None
                agent.live_call = None
        elif etype == "task_started":
            agent.state = "working"
            agent.current_task = payload.get("task_id") or None
            agent.afk_reason = ""
            agent.last_error = None
        elif etype in ("task_completed", "task_failed"):
            agent.current_task = None
            agent.current_phase = None
            agent.current_iteration = 0
            # A task that failed says so.  ``TaskFailed`` carries the
            # error and nothing else recorded it, so a task that died for
            # a reason the engine does not treat as AFK -- an unhandled
            # handler exception, a rejected delegation -- left the seat
            # looking like a healthy idle one, with the cause visible
            # only as one line in the feed.
            if etype == "task_failed":
                agent.last_error = {
                    "kind": "task_failed",
                    "message": payload.get("error", "") or "",
                    "phase": "",
                    "turn_id": payload.get("turn_id", "") or "",
                    "at": str(envelope.get("timestamp", "")),
                    "event_id": envelope.get("id", ""),
                }
            # An engine-detected failure publishes its AFK event
            # (``llm_unavailable`` / ``budget_exhausted`` / a guard
            # breach) and ``TaskFailed`` microseconds apart, in that
            # order.  Forcing ``idle`` here would erase the cause the
            # instant it was set — which is why an agent whose provider
            # died still showed as a healthy idle seat, and why a reload
            # showed the same.  A seat only leaves AFK when it does real
            # work again (``task_started`` / ``agent_phase_started``).
            if agent.state != "afk":
                agent.state = "idle"
                agent.afk_reason = ""
                agent.live_call = None
        elif etype == "agent_phase_started":
            agent.state = "working"
            agent.afk_reason = ""
            # A new phase is real forward progress: whatever killed the
            # last one is history now.
            agent.last_error = None
            agent.current_phase = payload.get("phase") or None
            agent.current_iteration = int(payload.get("iteration", 0) or 0)
            self._begin_live_call(agent, payload)
        elif etype == "agent_phase_completed":
            agent.state = "working"
            agent.afk_reason = ""
            if payload.get("failed"):
                self._record_phase_failure(agent, envelope, payload)
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
            # A call already frozen as failed is the most informative
            # thing on the agent's page — the prompt it died on, the
            # tools that had run, the error. The AFK event that follows
            # a failed phase would otherwise wipe it a moment later.
            if not (agent.live_call or {}).get("failed"):
                agent.live_call = None
            # ``llm_unavailable`` / ``budget_exhausted`` / a guard breach
            # each carry their own reason; keep it where the dashboard
            # reads failures from, so one panel explains every stop.
            agent.last_error = {
                "kind": payload.get("last_error_kind") or payload.get("kind") or etype,
                "message": str(
                    payload.get("last_error")
                    or payload.get("detail")
                    or payload.get("error")
                    or ""
                ),
                "phase": agent.current_phase or "",
                "turn_id": payload.get("turn_id", ""),
                "at": str(envelope.get("timestamp", "")),
                "event_id": envelope.get("id", ""),
            }
        return True

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

    def _apply_progress(self, envelope: dict[str, Any], payload: dict[str, Any]) -> str:
        """Fold an ``agent_turn_progress`` round into the in-flight call.

        Returns the role whose live call moved, or ``""`` when the round
        was stale and dropped.
        """
        role = payload.get("role") or payload.get("agent_role") or ""
        if not role:
            return ""
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
                return ""
        elif cur is not None and ts and str(cur.get("updated_at", "")) > ts:
            return ""

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
        return role

    def _record_phase_failure(
        self, agent: AgentLive, envelope: dict[str, Any], payload: dict[str, Any]
    ) -> None:
        """Remember a failed phase, and freeze its call instead of clearing it.

        A phase that dies mid-call is exactly when an operator most wants
        to see the call.  Clearing ``live_call`` here would blank the row
        the moment the failure lands; instead the call is stamped
        ``failed`` and kept, carrying whatever the phase managed —
        prompt, partial response, tool calls — with the error attached.
        """
        error = {
            "kind": payload.get("error_kind", "") or "error",
            "message": payload.get("error", "") or "",
            "phase": payload.get("phase", "") or "",
            "turn_id": payload.get("turn_id", "") or "",
            "at": str(envelope.get("timestamp", "")),
            "event_id": envelope.get("id", ""),
        }
        agent.last_error = error
        call = agent.live_call
        if call is not None and self._same_call(
            call,
            payload.get("turn_id", ""),
            payload.get("phase", ""),
            int(payload.get("iteration", 0) or 0),
        ):
            call["in_progress"] = False
            call["failed"] = True
            call["error"] = error
            call["response"] = payload.get("response", "") or call.get("response", "")
            call["model"] = payload.get("model", "") or call.get("model", "")
            call["tool_executions"] = payload.get("tool_executions") or call.get(
                "tool_executions", []
            )

    def _finish_live_call(self, agent: AgentLive, payload: dict[str, Any]) -> None:
        """Clear the in-flight call when its phase completes.

        Only clears when the completed phase matches the live call — a
        late ``agent_phase_completed`` for a prior phase must not wipe a
        newer phase's live row.
        """
        cur = agent.live_call
        if cur is None:
            return
        # A failed phase deliberately keeps its (frozen) call on screen —
        # see ``_record_phase_failure``.  Only a clean completion clears.
        if payload.get("failed"):
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
            _remember(self._counted_token_ids, event_id)
        agent.input_tokens += int(payload.get("input_tokens", 0) or 0)
        agent.output_tokens += int(payload.get("output_tokens", 0) or 0)
        agent.total_tokens += int(payload.get("total_tokens", 0) or 0)

    # -- spend rollup ----------------------------------------------------

    def _fold_spend(self, envelope: dict[str, Any], payload: dict[str, Any]) -> bool:
        """Record one completed phase's spend; return whether it counted.

        Deduped by event id so a re-delivered envelope can't inflate the
        rollup, and window-pruned so a long-lived process doesn't keep
        aggregating spend that has aged out of the window.
        """
        event_id = envelope.get("id", "")
        if event_id and event_id in self._counted_phase_ids:
            return False
        if event_id:
            _remember(self._counted_phase_ids, event_id)
        timestamp = str(envelope.get("timestamp", ""))
        self._phase_spend.append(
            {
                "event_id": event_id,
                "timestamp": timestamp,
                "agent_id": payload.get("agent_id", ""),
                "agent_role": payload.get("role", "") or payload.get("agent_role", ""),
                "phase": payload.get("phase", ""),
                "host_phase": payload.get("host_phase", ""),
                "worker": payload.get("worker", ""),
                "model": payload.get("model", "") or payload.get("provider_key", ""),
                "turn_id": payload.get("turn_id", ""),
                "iteration": int(payload.get("iteration", 0) or 0),
                "input_tokens": int(payload.get("input_tokens", 0) or 0),
                "output_tokens": int(payload.get("output_tokens", 0) or 0),
                "total_tokens": int(payload.get("total_tokens", 0) or 0),
            }
        )
        self._prune_spend(timestamp)
        return True

    def _prune_spend(self, now_iso: str) -> None:
        """Drop spend records that have aged out of the live window.

        Order-independent by construction.  Popping from the left is only
        correct while the deque is timestamp-ordered, and it is not
        reliably: the API subscribes to the event stream before it
        hydrates, so a live event can land ahead of the older records
        hydration then appends behind it.  One recent record at the head
        is enough to make a head-popping loop exit immediately and never
        prune again — the window would silently stop being a window.

        The sweep runs only when there is something to drop, so the
        common case costs one pass of timestamp comparisons and no
        allocation.
        """
        if not now_iso:
            return
        try:
            now = datetime.fromisoformat(now_iso)
        except ValueError:
            return
        cutoff = (now - timedelta(hours=LIVE_SPEND_WINDOW_HOURS)).isoformat()
        if not any(r["timestamp"] < cutoff for r in self._phase_spend):
            return
        kept = [r for r in self._phase_spend if r["timestamp"] >= cutoff]
        self._phase_spend.clear()
        self._phase_spend.extend(kept)

    def token_rollup(
        self, role_handle_map: dict[str, str] | None = None
    ) -> dict[str, Any]:
        """The spend breakdown over the live window, in the API's shape."""
        from crewlet.api.tokens import aggregate_phase_events

        rollup = aggregate_phase_events(
            list(self._phase_spend), role_handle_map=role_handle_map or {}
        )
        return {
            "since_days": max(1, LIVE_SPEND_WINDOW_HOURS // 24),
            "agent_role": "",
            **rollup,
        }

    # -- buffer ----------------------------------------------------------

    def _record_event(self, envelope: dict[str, Any]) -> None:
        """Append a light (payload-free) copy of an event to the buffer."""
        payload = envelope.get("payload") or {}
        self._events.append(_light_event(envelope, failed=bool(payload.get("failed"))))

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
        await self._hydrate_spend(store)
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
            token_events = await store.list_token_usage_events(
                since_days=TOKEN_WINDOW_DAYS
            )
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
                _remember(self._counted_token_ids, event_id)

    async def _hydrate_spend(self, store: Any) -> None:
        """Seed the spend rollup so the first snapshot is not blank."""
        try:
            records = await store.list_phase_token_events(
                since_days=max(1, LIVE_SPEND_WINDOW_HOURS // 24)
            )
        except Exception as exc:  # pragma: no cover - defensive
            logger.warning("live_state_hydrate_spend_failed", error=str(exc))
            return
        for record in sorted(records, key=lambda r: r.get("timestamp", "")):
            if event_id := record.get("event_id", ""):
                if event_id in self._counted_phase_ids:
                    continue
                _remember(self._counted_phase_ids, event_id)
            self._phase_spend.append(record)
        # Hydration runs after the stream is already live, so anything
        # that arrived in between sits ahead of these older records.
        # Restore the ordering the window reads best.
        ordered = sorted(self._phase_spend, key=lambda r: r["timestamp"])
        self._phase_spend.clear()
        self._phase_spend.extend(ordered)

    async def _hydrate_events(self, store: Any) -> None:
        try:
            events = await store.list_events(limit=self._events.maxlen or 400)
        except Exception as exc:  # pragma: no cover - defensive
            logger.warning("live_state_hydrate_events_failed", error=str(exc))
            return
        # ``list_events`` is newest-first; the buffer is chronological.
        # It never selects the payload column, so the failure flag comes
        # off the ``failed`` tag the writer stamps (see
        # ``EventStoreWriter._extract_tags``) -- without it a reloaded
        # dashboard would show every historical failure as a success.
        for row in reversed(events):
            tags = row.get("tags") or {}
            self._events.append(_light_event(row, failed=tags.get("failed") == "true"))
