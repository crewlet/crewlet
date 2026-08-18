"""Live-stream endpoints — snapshot (REST) + WebSocket.

The WebSocket handshake pushes a snapshot built entirely from the
in-memory :class:`~crewlet.api.streaming.StreamService` projection (no
database round-trip), then streams ``event`` envelopes as they happen.
A single shared health tick on the service pushes ``health`` envelopes
to every client, so there is no per-connection timer.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse
from starlette.websockets import WebSocket, WebSocketDisconnect

from crewlet._logging import get_logger
from crewlet.api.queries import QueryError, run_query
from crewlet.api.routes.org import schedule_projection
from crewlet.api.streaming import build_health_envelope, envelope

logger = get_logger("api.routes")

# Queries a single socket may have in flight at once.  Queries run
# concurrently so a store scan cannot stall the live feed, but each one
# can take a database connection from a pool the engine shares — an
# unbounded fan-out from one tab would starve the engine's own writes.
# Four covers the most a dashboard screen issues at once (the agent page
# opens with three) and makes a burst queue rather than pile up.
_MAX_INFLIGHT_QUERIES = 4


def _stream(app: Any) -> Any:
    return getattr(app.state, "stream", None)


def _fallback_snapshot(app: Any) -> dict[str, Any]:
    """Snapshot from static config alone (no stream service wired)."""
    state = app.state
    return {
        "health": build_health_envelope(app),
        "agents": [dict(r) for r in (state.agent_roles or [])],
        "events": [],
        "sandboxes": [],
        "tools": state.tools_data,
        "org": state.org_data,
        "tokens": {},
        "schedules": schedule_projection(app),
    }


def _snapshot(app: Any) -> dict[str, Any]:
    stream = _stream(app)
    return stream.snapshot(app) if stream is not None else _fallback_snapshot(app)


async def stream_snapshot(request: Request) -> JSONResponse:
    """GET /stream/snapshot — single-shot bundle of every live view.

    The dashboard's initial paint in one round-trip and the REST
    fallback when a WebSocket can't be established (corporate proxies).
    """
    return JSONResponse(_snapshot(request.app))


async def _send(ws: WebSocket, message: str, lock: asyncio.Lock) -> bool:
    """Send a pre-encoded envelope; return ``False`` on disconnect.

    Serializes concurrent senders (queue drain + pong reply) through
    ``lock`` so frames never interleave on one socket — ASGI's single-
    sender contract raises ``RuntimeError`` otherwise.
    """
    async with lock:
        try:
            await ws.send_text(message)
            return True
        except WebSocketDisconnect:
            return False
        except RuntimeError as exc:
            logger.debug("ws_send_failed", error=str(exc))
            return False


async def stream_websocket(websocket: WebSocket) -> None:
    """WS /ws/stream — stream engine events + health to a dashboard tab.

    This is the dashboard's whole transport — state comes down it and
    requests go up it, so a running dashboard makes no HTTP request at
    all.

    Protocol (JSON, one envelope per text frame)::

        ← {"kind": "snapshot",  "data": {health, agents, events, sandboxes,
                                         org, tools, tokens, schedules}}
        ← {"kind": "event",     "data": <event row + payload>}
        ← {"kind": "agents",    "data": [<changed agent overlay>, …]}
        ← {"kind": "sandboxes", "data": [<in-flight sandbox run>, …]}
        ← {"kind": "tokens",    "data": <spend rollup>}
        ← {"kind": "health",    "data": {status, in_flight?, shutting_down?}}
        ← {"kind": "result",    "id": N, "what": "...", "data": <any>}
        ← {"kind": "error",     "id": N, "what": "...", "error": "code"}
        ← {"kind": "pong"}
        → {"kind": "ping"}
        → {"kind": "query", "id": N, "what": "...", "params": {…},
                            "token": "<operator bearer, config queries only>"}

    The ``agents`` / ``sandboxes`` / ``tokens`` envelopes carry the
    *result* of applying an event to the server's projection.  A client
    renders them; it does not re-derive them from the raw ``event``
    stream, which is what every tab used to do — three private copies of
    the projection, each drifting its own way.

    Backpressure is per-client: the service drops a slow tab's oldest
    queued envelope rather than stalling the publish path.  The dashboard
    dedups streamed envelopes against the snapshot by event id, so the
    register-before-snapshot ordering is harmless.
    """
    await websocket.accept()
    stream = _stream(websocket.app)
    lock = asyncio.Lock()

    # Register BEFORE building the snapshot so events published during
    # the build window also reach this client (deduped by id downstream).
    client = stream.register() if stream is not None else None

    if not await _send(websocket, envelope("snapshot", _snapshot(websocket.app)), lock):
        if client is not None and stream is not None:
            stream.unregister(client)
        return

    inbound_task: asyncio.Task[Any] | None = None
    get_task: asyncio.Task[str] | None = None

    query_slots = asyncio.Semaphore(_MAX_INFLIGHT_QUERIES)

    async def _answer_query(parsed: dict[str, Any]) -> None:
        """Run one client query and send its single reply frame.

        Never raises: a query task that died with an exception would
        leave the client waiting for a reply that is never coming, and
        the socket has no other way to say so.
        """
        req_id = parsed.get("id")
        what = str(parsed.get("what", ""))
        params = parsed.get("params")
        if not isinstance(params, dict):
            params = {}
        token = parsed.get("token")
        try:
            async with query_slots:
                data = await run_query(
                    websocket.app,
                    what,
                    params,
                    token=token if isinstance(token, str) else "",
                )
            body = json.dumps(
                {"kind": "result", "id": req_id, "what": what, "data": data}
            )
        except QueryError as exc:
            body = json.dumps(
                {"kind": "error", "id": req_id, "what": what, "error": exc.code}
            )
        except asyncio.CancelledError:
            raise
        except Exception:
            # Serialisation blew up, or a handler raised outside
            # ``run_query``'s own guard.
            logger.exception("stream_query_reply_failed", what=what)
            body = json.dumps(
                {"kind": "error", "id": req_id, "what": what, "error": "query_failed"}
            )
        await _send(websocket, body, lock)

    async def _drain_inbound() -> None:
        """Read client → server frames (``ping`` and ``query``)."""
        pending: set[asyncio.Task[None]] = set()
        try:
            while True:
                try:
                    msg = await websocket.receive_text()
                except WebSocketDisconnect:
                    return
                try:
                    parsed = json.loads(msg)
                except (json.JSONDecodeError, ValueError):
                    continue
                if not isinstance(parsed, dict):
                    continue
                kind = parsed.get("kind")
                if kind == "ping":
                    if not await _send(websocket, envelope("pong", None), lock):
                        return
                elif kind == "query":
                    # Queries run concurrently with each other and with
                    # the push stream: one slow read (an agent's LLM
                    # history is a database scan) must not stall the
                    # live feed or the tab's other requests behind it.
                    task = asyncio.create_task(_answer_query(parsed))
                    pending.add(task)
                    task.add_done_callback(pending.discard)
        finally:
            for task in pending:
                task.cancel()

    try:
        inbound_task = asyncio.create_task(_drain_inbound())
        if client is None:
            # No stream wired: keep the connection open for inbound only.
            await inbound_task
            return
        # Race the per-client queue read against the inbound task so a
        # quiet org doesn't park us on queue.get() after the socket dies.
        while True:
            if get_task is None or get_task.done():
                get_task = asyncio.ensure_future(client.queue.get())
            done, _pending = await asyncio.wait(
                {get_task, inbound_task}, return_when=asyncio.FIRST_COMPLETED
            )
            if inbound_task in done:
                return
            message = get_task.result()
            get_task = None
            if not await _send(websocket, message, lock):
                return
    except (WebSocketDisconnect, asyncio.CancelledError):
        return
    finally:
        for task in (inbound_task, get_task):
            if task is not None and not task.done():
                task.cancel()
        with contextlib.suppress(Exception):
            if client is not None and stream is not None:
                stream.unregister(client)
