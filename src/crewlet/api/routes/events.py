"""Event-store read endpoints — list, single, and trace.

Each endpoint's body is a plain ``(app, ...) -> data`` function so the
WebSocket query channel (:mod:`crewlet.api.queries`) can answer with the
same object the REST route returns, rather than a second implementation
that drifts.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet.api.routes._common import safe_store_query

# Default and maximum rows one ``/events`` read returns.  The dashboard
# reads the live feed off the stream and uses this only for filtered
# history (a trace, one agent's tail), where a few hundred rows is
# already more than a screen; the ceiling keeps a hand-crafted
# ``?limit=`` from turning into an unbounded scan.
DEFAULT_EVENT_LIMIT = 50
MAX_EVENT_LIMIT = 500


def parse_cursor(before: str, before_id: str) -> tuple[Any, str] | None:
    """Build the keyset cursor from its two wire fields.

    An unparseable timestamp yields no cursor rather than an error: this
    is a best-effort read path (see ``safe_store_query``), and answering
    the first page is a better failure than answering nothing.  The
    caller can tell, because the page it gets back starts at the top.
    """
    if not before:
        return None
    try:
        return (datetime.fromisoformat(before), before_id)
    except ValueError:
        return None


async def events_query(
    app: Any,
    *,
    limit: int = DEFAULT_EVENT_LIMIT,
    event_type: str | None = None,
    source: str | None = None,
    category: str | None = None,
    trace_id: str | None = None,
    actor: str | None = None,
    related_agent: str | None = None,
    before: str = "",
    before_id: str = "",
) -> list[dict[str, Any]]:
    """Recent events from the persistent store, newest first.

    ``before`` / ``before_id`` are the exclusive keyset cursor: pass the
    oldest row a caller already holds to get the page beneath it.
    """
    store = app.state.event_store
    if store is None:
        return []
    # Clamped, not rejected: this is a scan over the event history and an
    # unbounded ``?limit=`` is a way to ask the database for everything.
    # The ceiling is documented in docs/reference/api-endpoints.md so a
    # paginating caller knows the page size it will actually get.
    limit = max(0, min(int(limit), MAX_EVENT_LIMIT))
    return await safe_store_query(
        store.list_events(
            limit=limit,
            event_type=event_type,
            source=source,
            category=category,
            trace_id=trace_id,
            actor=actor,
            related_agent=related_agent,
            before=parse_cursor(before, before_id),
        ),
        [],
    )


async def event_detail(app: Any, event_id: str) -> dict[str, Any] | None:
    """One event with its full payload, or ``None``."""
    store = app.state.event_store
    if store is None:
        return None
    return await safe_store_query(store.get_event(event_id))


async def trace_events(app: Any, trace_id: str) -> list[dict[str, Any]]:
    """Every event in one trace, oldest first."""
    store = app.state.event_store
    if store is None:
        return []
    return await safe_store_query(store.list_trace(trace_id), [])


async def list_events(request: Request) -> JSONResponse:
    """GET /events — recent events from the persistent store.

    Answers 503 with no store wired, matching its two siblings.  It used
    to answer 200 with ``[]``, which is indistinguishable from "you have
    reached the beginning of history" -- fine when the response was one
    unpaged page, wrong now that a caller pages until it runs out.
    """
    if request.app.state.event_store is None:
        return JSONResponse({"error": "no event store"}, status_code=503)
    params = request.query_params
    try:
        limit = int(params.get("limit", str(DEFAULT_EVENT_LIMIT)))
    except (TypeError, ValueError):
        limit = DEFAULT_EVENT_LIMIT
    return JSONResponse(
        await events_query(
            request.app,
            limit=limit,
            event_type=params.get("type"),
            source=params.get("source"),
            category=params.get("category"),
            trace_id=params.get("trace_id"),
            actor=params.get("actor"),
            related_agent=params.get("related_agent"),
            before=params.get("before", ""),
            before_id=params.get("before_id", ""),
        )
    )


async def get_event(request: Request) -> JSONResponse:
    """GET /events/{event_id} — single event with full payload."""
    if request.app.state.event_store is None:
        return JSONResponse({"error": "no event store"}, status_code=503)
    event = await event_detail(request.app, request.path_params["event_id"])
    if event is None:
        return JSONResponse({"error": "not found"}, status_code=404)
    return JSONResponse(event)


async def list_trace(request: Request) -> JSONResponse:
    """GET /events/trace/{trace_id} — all events in a trace, oldest first."""
    if request.app.state.event_store is None:
        return JSONResponse({"error": "no event store"}, status_code=503)
    trace_id = request.path_params["trace_id"]
    return JSONResponse(await trace_events(request.app, trace_id))
