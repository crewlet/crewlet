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
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from crewlet._logging import get_logger
from crewlet.api.live_state import LiveState
from crewlet.events.types import Event

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

# Snapshot recent-events cap. Kept in step with the projection's
# ``_EVENT_BUFFER_SIZE``-bounded feed so the initial render matches
# the live view.
SNAPSHOT_EVENT_LIMIT = 150


@dataclass(slots=True)
class StreamClient:
    """Outbound WebSocket client registered with the service."""

    id: str
    queue: asyncio.Queue[str]
    drops: int = 0


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
    """
    from crewlet.timescaledb.writer import _CATEGORY_MAP

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
        "topic": topic,
        "payload": payload,
    }


def build_health_envelope(app: Any) -> dict[str, Any]:
    """Snapshot the engine-health fields the dashboard renders.

    ``in_flight`` / ``shutting_down`` are only present on the embedded
    API (where an engine reference is attached); the standalone API
    omits them.  ``status`` flips to ``"shutting_down"`` during a
    graceful drain so the dashboard's live dot tracks the drain instead
    of staying green.
    """
    engine = getattr(app.state, "engine", None)
    body: dict[str, Any] = {
        "status": "ok",
        "configured": bool(getattr(app.state, "configured", False)),
    }
    if engine is not None:
        body["in_flight"] = engine.in_flight_count
        shutting_down = engine.shutting_down or not engine.is_running
        body["shutting_down"] = shutting_down
        if shutting_down:
            body["status"] = "shutting_down"
    return body


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

    @property
    def live(self) -> LiveState:
        return self._live

    @property
    def client_count(self) -> int:
        return len(self._clients)

    # -- client lifecycle ------------------------------------------------

    def register(self) -> StreamClient:
        self._counter += 1
        client = StreamClient(
            id=f"ws-{self._counter}",
            queue=asyncio.Queue(maxsize=self._client_queue_size),
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
        self._live.apply_event(env)
        if self._clients:
            self._fan_out("event", env)

    async def emit_event(self, env: dict[str, Any]) -> None:
        """Inject an already-serialized event envelope (webhook surfaces).

        Mirrors :meth:`ingest` for events that don't flow through the
        engine event stream — the dashboard's activity feed reflects them
        in real time and the projection records them in its buffer.
        """
        self._live.apply_event(env)
        if self._clients:
            self._fan_out("event", env)

    async def broadcast(self, kind: str, data: Any) -> None:
        """Emit a non-event envelope (e.g. ``health``) to every client."""
        if self._clients:
            self._fan_out(kind, data)

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
        """Build the dashboard's initial-state bundle from memory.

        Combines health + agents (with their in-flight ``live_call``) +
        recent events + tools + org into one payload, all served from the
        in-memory projection — no database round-trip on the hot path.
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
        }

    async def hydrate(self, store: Any, roles: list[str]) -> None:
        """Seed the projection from the event store (idempotent re: tokens).

        Safe to call more than once: token / event hydration runs only on
        the first call (they *add* to running totals), while state
        hydration re-reads cleanly so the standalone path can hydrate
        baseline state after its roles arrive via config refresh.
        """
        await self._live.hydrate(store, roles, only_states=self._hydrated_once)
        self._hydrated_once = True

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
        while True:
            await asyncio.sleep(self._health_interval)
            if self._health_fn is None:
                continue
            try:
                body = self._health_fn()
            except Exception:  # pragma: no cover - defensive
                logger.exception("health_tick_failed")
                continue
            await self.broadcast("health", body)
