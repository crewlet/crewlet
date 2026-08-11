"""Event-store read endpoints — list, single, and trace."""

from __future__ import annotations

from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet.api.routes._common import safe_store_query


async def list_events(request: Request) -> JSONResponse:
    """GET /events — recent events from the persistent store."""
    limit = int(request.query_params.get("limit", "50"))
    event_type = request.query_params.get("type")
    source = request.query_params.get("source")
    trace_id = request.query_params.get("trace_id")
    actor = request.query_params.get("actor")
    related_agent = request.query_params.get("related_agent")
    store = request.app.state.event_store
    if store is None:
        return JSONResponse([])
    events = await safe_store_query(
        store.list_events(
            limit=limit,
            event_type=event_type,
            source=source,
            trace_id=trace_id,
            actor=actor,
            related_agent=related_agent,
        ),
        [],
    )
    return JSONResponse(events)


async def get_event(request: Request) -> JSONResponse:
    """GET /events/{event_id} — single event with full payload."""
    event_id = request.path_params["event_id"]
    store = request.app.state.event_store
    if store is None:
        return JSONResponse({"error": "no event store"}, status_code=503)
    event: Any = await safe_store_query(store.get_event(event_id))
    if event is None:
        return JSONResponse({"error": "not found"}, status_code=404)
    return JSONResponse(event)


async def list_trace(request: Request) -> JSONResponse:
    """GET /events/trace/{trace_id} — all events in a trace, oldest first."""
    trace_id = request.path_params["trace_id"]
    store = request.app.state.event_store
    if store is None:
        return JSONResponse({"error": "no event store"}, status_code=503)
    events = await safe_store_query(store.list_trace(trace_id), [])
    return JSONResponse(events)
