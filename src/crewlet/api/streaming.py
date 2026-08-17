"""Live-stream service for the dashboard — fan-out + state projection.

``StreamService`` sits between the engine's
:class:`~crewlet.queue.protocol.EventQueue` and the ``/ws/stream``
WebSocket endpoint.  It does three jobs:

1. **Projection.**  It owns a :class:`~crewlet.api.live_state.LiveState`
   and feeds every engine event into it, so agent state — including the
   *in-flight* LLM call — is always current in memory.  This is what
   makes the dashboard's live row survive a refresh and removes the
   per-read database scan.

2. **Fan-out.**  It keeps a per-tab client registry and pushes every
   event envelope onto each client's bounded queue.  A slow tab drops
   its oldest queued envelope rather than stalling the publish path or
   other tabs.

3. **Health pulse.**  A single shared background task broadcasts a
   ``health`` envelope to every client on a fixed interval — one timer
   total, not one per connection.

Wiring: the engine wires the service in once, either via
``event_queue.add_publish_listener(stream.ingest)`` (embedded API, same
process) or ``event_queue.subscribe_stream("crewlet.events.>",
stream.ingest)`` (standalone API).  Both call :meth:`ingest`, which
filters to ``crewlet.events.*`` so internal inbox / notification traffic
never reaches dashboards.  Webhook surfaces are injected with
:meth:`emit_event`.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import time
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from crewlet import __version__
from crewlet._logging import get_logger
from crewlet.api.live_state import EVENT_FEED_LIMIT, Change, LiveState
from crewlet.config import DEFAULT_NODE_ID
from crewlet.events.types import Event, event_failed

logger = get_logger("api.streaming")

# Per-client bounded queue. Sized so a brief stall (a few seconds at
# ~50 events/sec peak) is absorbed without dropping; beyond that the
# dashboard's reconnect flow refetches the snapshot.
_DEFAULT_CLIENT_QUEUE_SIZE = 512

# Only events on this prefix are forwarded by ``ingest``.  Matches the
# wildcard the standalone API uses with ``subscribe_stream`` so both
# deployment paths fan out the same set of topics.
_EVENT_TOPIC_PREFIX = "crewlet.events."

# How often the shared health tick pushes a fresh health envelope to
# every client even when no events fired — keeps the in-flight pill,
# drain state, and live dot honest.
_HEALTH_INTERVAL_SECONDS = 5.0

# Snapshot recent-events cap.  Reads the projection's own feed limit so
# the two cannot drift: a snapshot ships exactly what the projection
# retains, which is exactly what a tab keeps.  It used to be a separate
# 150 against a 400-deep ring, so a tab's feed visibly shrank on every
# refresh and 250 retained rows were undeliverable.
SNAPSHOT_EVENT_LIMIT = EVENT_FEED_LIMIT

# How often the service checks whether the spend rollup moved.  The
# rollup is re-aggregated from the projection's records, which costs tens
# of milliseconds, so it is deliberately not computed per event: a burst
# of phase completions collapses into one push.  One second keeps the
# Tokens view live to the eye, and the work happens on the background
# tick rather than inside the publish that triggered it.
_TOKENS_PUSH_INTERVAL_SECONDS = 1.0


@dataclass(slots=True)
class StreamClient:
    """Outbound WebSocket client registered with the service."""

    id: str
    queue: asyncio.Queue[str]
    drops: int = 0
    # When this socket connected. ``drops`` resets on every reconnect, so
    # reporting the counter without the window it accumulated over would
    # be a figure with no denominator.
    connected_at: str = ""


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()


def envelope(kind: str, data: Any) -> str:
    """Encode a single broadcast envelope as a JSON string."""
    return json.dumps({"kind": kind, "data": data, "ts": _now_iso()})


def serialize_event(topic: str, event: Event) -> dict[str, Any]:
    """Render an engine ``Event`` into the dashboard envelope shape.

    Carries the same id / type / timestamp / source / actor / summary /
    category / trace context the persistent ``/events`` rows have, plus
    the full event ``payload`` and the originating ``topic`` so live
    consumers (the agent page's LLM rows) and the live-state projection
    can read every field.

    ``failed`` and ``tags`` are stamped here too, so a pushed row and a
    row read back from history are the same shape.  A client that had to
    derive either -- from the payload for ``failed``, from a
    reimplementation of ``_extract_tags`` for the routing keys -- would
    be a second copy of a rule that already exists twice.
    """
    from crewlet.timescaledb.writer import _CATEGORY_MAP, extract_tags

    payload = event.model_dump(mode="json")
    return {
        "id": str(event.id),
        "type": event.type,
        "timestamp": event.timestamp.isoformat() if event.timestamp else "",
        "source": event.source,
        "actor": event.actor,
        "summary": event.summary,
        "category": _CATEGORY_MAP.get(event.type, ""),
        "trace_id": getattr(event, "trace_id", ""),
        "span_id": getattr(event, "span_id", ""),
        "parent_span_id": getattr(event, "parent_span_id", ""),
        "failed": event_failed(event.type, payload_failed=bool(payload.get("failed"))),
        "tags": extract_tags(event),
        "topic": topic,
        "payload": payload,
    }


def _queue_backend(app: Any) -> str:
    """The event queue's backend name, for operator display only.

    Read duck-typed off the Protocol, exactly as ``in_flight_count`` is.
    Sniffing ``type(...).__name__`` instead would lie the moment a queue
    is wrapped, and the Protocol is where this codebase declares what a
    provider can answer.
    """
    return str(getattr(getattr(app.state, "event_queue", None), "backend", "") or "")


def _event_store_kind(app: Any) -> str:
    """``durable`` | ``memory`` | ``none``.

    Three-valued rather than a boolean because "a store is wired" does
    not mean "history survives a restart": with no database the CLI
    still wraps two in-memory legs in a ``CompositeEventStore``, so the
    presence check an operator would naturally make answers yes while
    every event is one process death from gone.
    """
    if getattr(app.state, "event_store", None) is None:
        return "none"
    return "durable" if getattr(app.state, "database", None) is not None else "memory"


def build_health_envelope(app: Any) -> dict[str, Any]:
    """Snapshot the engine-health fields the dashboard renders.

    This one function backs all three health surfaces -- ``GET /health``,
    the snapshot's ``health`` section, and the periodic push -- so a
    field added here reaches every one of them, and a reconnect restores
    it without a second round trip.

    ``engine`` is what makes the ABSENCE of ``in_flight`` explicit
    rather than implicit.  The standalone API has no engine to ask, and
    without the flag a client cannot tell "nothing is running" from "this
    process cannot know", so it renders a confident zero for both.

    ``status`` carries a ``unconfigured`` state now.  ``configured`` has
    been on the wire since this function was written and nothing rendered
    it, which meant an engine with no active company revision -- one
    dropping every inbound webhook -- looked exactly like a healthy idle
    one, just with empty screens.  Precedence is
    ``shutting_down > unconfigured > ok``: a draining engine is draining
    first, whatever else is true of it.

    ``node`` names the process that answered — the field that turns
    "the config apply failed" into "the config apply failed on node-2"
    once a load balancer is in front of more than one process, and the
    only way a caller can tell which one it reached.
    """
    engine = getattr(app.state, "engine", None)
    stream = getattr(app.state, "stream", None)
    configured = bool(getattr(app.state, "configured", False))
    body: dict[str, Any] = {
        "status": "ok" if configured else "unconfigured",
        "node": str(getattr(app.state, "node_id", "") or DEFAULT_NODE_ID),
        "configured": configured,
        "engine": engine is not None,
        "version": __version__,
        # The API process's own start. Deliberately separate from the
        # engine's: on the standalone deployment they are two processes
        # on two clocks, and one merged "uptime" would be the
        # two-different-windows error in a new place.
        "started_at": str(getattr(app.state, "started_at", "") or ""),
        "queue": _queue_backend(app),
        "event_store": _event_store_kind(app),
        "feed_hydrated": bool(stream is not None and stream.hydrated),
        "clients": stream.client_count if stream is not None else 0,
    }
    if engine is not None:
        body["in_flight"] = engine.in_flight_count
        body["engine_started_at"] = str(getattr(engine, "started_at", "") or "")
        shutting_down = engine.shutting_down or not engine.is_running
        body["shutting_down"] = shutting_down
        if shutting_down:
            body["status"] = "shutting_down"
    return body


def _schedule_projection(app: Any) -> list[dict[str, Any]]:
    """The configured schedules with ``next_run``, imported lazily.

    ``routes.org`` imports nothing from here, but ``routes.stream`` does,
    so keeping this import inside the call avoids wiring a cycle through
    the routes package just to embed a section in the snapshot.
    """
    from crewlet.api.routes.org import schedule_projection

    return schedule_projection(app)


class StreamService:
    """Owns the live-state projection, client fan-out, and health pulse.

    Concurrency model: all client-registry and projection mutations
    happen in purely-synchronous blocks (no ``await``), so they're atomic
    from asyncio's single-threaded loop perspective — no lock needed.
    """

    def __init__(
        self,
        *,
        live_state: LiveState | None = None,
        client_queue_size: int = _DEFAULT_CLIENT_QUEUE_SIZE,
        health_interval: float = _HEALTH_INTERVAL_SECONDS,
    ) -> None:
        self._live = live_state if live_state is not None else LiveState()
        self._clients: dict[str, StreamClient] = {}
        self._client_queue_size = client_queue_size
        self._counter = 0
        self._health_interval = health_interval
        self._health_task: asyncio.Task[Any] | None = None
        self._health_fn: Callable[[], dict[str, Any]] | None = None
        self._hydrated_once = False
        # Whether the projection was successfully seeded from history.
        # A BOOLEAN, never the exception text: asyncpg errors carry host,
        # user and DSN fragments, and ``GET /health`` is served outside
        # the auth middleware. The reason stays in the structured log.
        self._hydrated_ok = False
        # Static context the derived pushes need: the configured roles
        # (for role→handle on the spend rollup) and the app whose state
        # holds org / tools / schedules.  Set by :meth:`bind`.
        self._app: Any = None
        # Monotonic timestamp of the last spend-rollup push, for the
        # coalescing window; a pending push is flushed by the health tick
        # so the last event of a burst is never left unsent.
        self._tokens_pushed_at = 0.0
        self._tokens_dirty = False

    def bind(self, app: Any) -> None:
        """Attach the app whose state backs snapshot + derived pushes."""
        self._app = app

    @property
    def live(self) -> LiveState:
        return self._live

    @property
    def client_count(self) -> int:
        return len(self._clients)

    @property
    def hydrated(self) -> bool:
        """Whether the projection was seeded from history without error."""
        return self._hydrated_ok

    # -- client lifecycle ------------------------------------------------

    def register(self) -> StreamClient:
        self._counter += 1
        client = StreamClient(
            id=f"ws-{self._counter}",
            queue=asyncio.Queue(maxsize=self._client_queue_size),
            connected_at=_now_iso(),
        )
        self._clients[client.id] = client
        logger.debug("stream_client_registered", client_id=client.id)
        return client

    def unregister(self, client: StreamClient) -> None:
        if self._clients.pop(client.id, None) is None:
            return
        logger.debug(
            "stream_client_unregistered", client_id=client.id, drops=client.drops
        )

    # -- ingest path -----------------------------------------------------

    async def ingest(self, topic: str, event: Event) -> None:
        """Apply an engine event to the projection and fan it out.

        Single entry point for both wiring styles:
        ``add_publish_listener`` (embedded — receives *every* publish, so
        the prefix filter matters) and ``subscribe_stream`` (standalone —
        already prefix-filtered).  Always updates the projection so a
        later snapshot is current even with no clients connected; the
        fan-out itself is skipped when nobody is listening.
        """
        if not topic.startswith(_EVENT_TOPIC_PREFIX):
            return
        env = serialize_event(topic, event)
        change = self._live.apply_event(env)
        if self._clients:
            self._fan_out("event", env)
            self._push_change(change)

    async def emit_event(self, env: dict[str, Any]) -> None:
        """Inject an already-serialized event envelope (webhook surfaces).

        Mirrors :meth:`ingest` for events that don't flow through the
        engine event stream — the dashboard's activity feed reflects them
        in real time and the projection records them in its buffer.
        """
        change = self._live.apply_event(env)
        if self._clients:
            self._fan_out("event", env)
            self._push_change(change)

    async def broadcast(self, kind: str, data: Any) -> None:
        """Emit a non-event envelope (e.g. ``health``) to every client."""
        if self._clients:
            self._fan_out(kind, data)

    def push(self, kind: str, data: Any) -> None:
        """Synchronous :meth:`broadcast`, for non-async callers.

        The fan-out itself never awaits (each client has a bounded queue
        the publisher writes without blocking), so a caller that is not
        a coroutine — the config-refresh handler — does not need to be
        made one just to notify open dashboards.
        """
        if self._clients:
            self._fan_out(kind, data)

    # -- derived pushes --------------------------------------------------

    def _push_change(self, change: Change) -> None:
        """Push the *result* of applying an event, not just the event.

        This is what lets a dashboard be a mirror rather than a second
        implementation.  Every tab used to run its own copy of the
        projection's state machine, its own sandbox tracker, and its own
        token aggregation off the raw event stream; three copies, three
        sets of drift, and a refresh that disagreed with what was on
        screen a moment earlier.  Now the projection is computed once,
        here, and its changes are sent.
        """
        if change.agents:
            overlays = [
                {"role": role, **overlay}
                for role in sorted(change.agents)
                if (overlay := self._live.agent_overlay(role)) is not None
            ]
            if overlays:
                self._fan_out("agents", overlays)
        if change.sandboxes:
            self._fan_out("sandboxes", self._live.active_sandboxes())
        if change.budget:
            # Fanned out immediately rather than marked dirty: unlike the
            # spend rollup there is nothing to aggregate — the engine's
            # reporter already coalesced this to at most one report a
            # second, so the work here is a dict copy.
            self._fan_out("budget", self._live.budget())
        if change.tokens:
            # Marked only. Aggregating here would run inside the caller's
            # ``publish()`` — which, on the embedded deployment, is the
            # engine's own event loop mid-turn. The background tick owns
            # the actual rollup.
            self._tokens_dirty = True

    def _flush_tokens(self) -> None:
        """Send the spend rollup if a phase completed since the last one.

        Called from the shared background tick rather than from the
        publish path, so re-aggregating the window never delays an
        agent's tool round or a queue dispatch.
        """
        if not self._tokens_dirty or not self._clients:
            return
        self._tokens_pushed_at = time.monotonic()
        self._tokens_dirty = False
        self._fan_out("tokens", self.token_rollup())

    def token_rollup(self) -> dict[str, Any]:
        """The live spend rollup, with each role's handle attached."""
        roles: list[dict[str, Any]] = []
        if self._app is not None:
            roles = getattr(self._app.state, "agent_roles", None) or []
        handles = {
            r.get("role", ""): r.get("handle", "") for r in roles if r.get("role")
        }
        return self._live.token_rollup(handles)

    def _fan_out(self, kind: str, data: Any) -> None:
        """Synchronous fan-out — atomic from the event loop's view."""
        message = envelope(kind, data)
        for client in list(self._clients.values()):
            try:
                client.queue.put_nowait(message)
            except asyncio.QueueFull:
                # Drop the oldest queued envelope so the slow client gets
                # the freshest state rather than a stale tail.
                with contextlib.suppress(asyncio.QueueEmpty):
                    client.queue.get_nowait()
                client.drops += 1
                with contextlib.suppress(asyncio.QueueFull):
                    client.queue.put_nowait(message)

    # -- snapshot --------------------------------------------------------

    def snapshot(self, app: Any) -> dict[str, Any]:
        """Build the dashboard's whole initial state from memory.

        Every section a screen renders on first paint is here — health,
        agents (with their in-flight ``live_call``), the activity feed,
        in-flight sandbox runs, tools, org, the spend rollup, and the
        schedule list — so a dashboard opens on one WebSocket frame and
        makes no HTTP request at all.  It is still served entirely from
        the in-memory projection: no database round-trip on connect.
        """
        state = app.state
        roles: list[dict[str, Any]] = state.agent_roles or []
        return {
            "health": build_health_envelope(app),
            "agents": self._live.merge_agents(roles),
            "events": self._live.recent_events(SNAPSHOT_EVENT_LIMIT),
            "sandboxes": self._live.active_sandboxes(),
            "tools": state.tools_data,
            "org": state.org_data,
            "tokens": self.token_rollup(),
            "budget": self._live.budget(),
            "schedules": _schedule_projection(app),
        }

    async def hydrate(self, store: Any, roles: list[str]) -> None:
        """Seed the projection from the event store (idempotent re: tokens).

        Safe to call more than once: token / event hydration runs only on
        the first call (they *add* to running totals), while state
        hydration re-reads cleanly so the standalone path can hydrate
        baseline state after its roles arrive via config refresh.
        """
        seeded = await self._live.hydrate(store, roles, only_states=self._hydrated_once)
        self._hydrated_once = True
        self._hydrated_ok = seeded

    # -- shared health tick ----------------------------------------------

    def start_health_tick(self, health_fn: Callable[[], dict[str, Any]]) -> None:
        """Start the single shared health-broadcast loop."""
        self._health_fn = health_fn
        if self._health_task is None or self._health_task.done():
            self._health_task = asyncio.create_task(self._health_loop())

    async def stop_health_tick(self) -> None:
        task = self._health_task
        self._health_task = None
        if task is not None and not task.done():
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await task

    async def _health_loop(self) -> None:
        """The service's one background timer: health, and the spend push.

        Ticks at ``_TOKENS_PUSH_INTERVAL_SECONDS`` and pushes health only
        every ``health_interval``, so the rollup stays responsive without
        a second task and without the health envelope getting chattier.
        """
        elapsed = 0.0
        while True:
            await asyncio.sleep(_TOKENS_PUSH_INTERVAL_SECONDS)
            elapsed += _TOKENS_PUSH_INTERVAL_SECONDS
            self._flush_tokens()
            if elapsed < self._health_interval:
                continue
            elapsed = 0.0
            if self._health_fn is None:
                continue
            try:
                body = self._health_fn()
            except Exception:  # pragma: no cover - defensive
                logger.exception("health_tick_failed")
                continue
            await self.broadcast("health", body)
