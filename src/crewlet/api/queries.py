"""Request/response queries served over the dashboard WebSocket.

The live stream pushes *state* — agents, events, sandboxes, spend — but a
dashboard also needs to pull things on demand: the LLM history behind one
agent, the full payload of one event, a trace, a different spend window,
the configuration document.  Those used to be HTTP fetches issued from
the browser alongside the socket, which is where the two transports
disagreed: the socket updated a projection that the next fetch replaced
wholesale, and a re-fetch on every streamed round is what made the page
churn while a turn was running.

So the socket carries both directions.  A client sends::

    → {"kind": "query", "id": 7, "what": "agent", "params": {"id": "..."}}

and gets exactly one reply::

    ← {"kind": "result", "id": 7, "what": "agent", "data": {...}}
    ← {"kind": "error",  "id": 7, "what": "agent", "error": "not_found"}

Every handler here is a thin adapter over the *same* function the REST
route calls, so the two surfaces can never answer differently.  The REST
endpoints remain — they are a public read API, and the dashboard still
falls back to ``/stream/snapshot`` when a proxy refuses to upgrade a
WebSocket — but nothing the dashboard does in normal operation is an
HTTP request.

Auth: ``config``-family queries are gated exactly like their ``/config``
routes.  The client passes the operator's bearer token on the query
frame and it is compared with :func:`crewlet.api.auth.resolve_operator`
— the same constant-time comparison the HTTP middleware performs.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from typing import Any

from crewlet._logging import get_logger
from crewlet.api.auth import resolve_operator
from crewlet.api.routes.agents import agent_detail, agent_memory
from crewlet.api.routes.events import (
    DEFAULT_EVENT_LIMIT,
    event_detail,
    events_query,
    trace_events,
)
from crewlet.api.routes.org import schedules_payload
from crewlet.api.routes.tokens import (
    DEFAULT_RECENT_TURNS,
    DEFAULT_WINDOW_DAYS,
    tokens_breakdown,
)

logger = get_logger("api.queries")

__all__ = ["QueryError", "run_query"]


class QueryError(Exception):
    """A query that cannot be answered, with a machine-readable code.

    The code goes on the wire as ``error`` so the client can distinguish
    "this agent does not exist" from "you are not allowed to read this"
    without parsing prose.
    """

    def __init__(self, code: str) -> None:
        super().__init__(code)
        self.code = code


def _str(params: dict[str, Any], key: str, default: str = "") -> str:
    value = params.get(key, default)
    return value if isinstance(value, str) else default


def _int(params: dict[str, Any], key: str, default: int) -> int:
    try:
        return int(params[key])
    except (KeyError, TypeError, ValueError):
        return default


# -- handlers ---------------------------------------------------------


async def _agent(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    detail = await agent_detail(app, _str(params, "id"))
    if detail is None:
        raise QueryError("not_found")
    return detail


async def _agent_memory(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    memory = await agent_memory(app, _str(params, "id"))
    if memory is None:
        raise QueryError("not_found")
    return memory


async def _event(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    if app.state.event_store is None:
        raise QueryError("no_event_store")
    event = await event_detail(app, _str(params, "id"))
    if event is None:
        raise QueryError("not_found")
    return event


async def _events(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    # Guarded like ``_event`` and ``_trace``.  Without it a store-less
    # deployment answers an empty page, which a pager reads as "you have
    # reached the beginning of history" -- an answer that silently
    # excludes every event there is.
    if app.state.event_store is None:
        raise QueryError("no_event_store")
    return await events_query(
        app,
        limit=_int(params, "limit", DEFAULT_EVENT_LIMIT),
        event_type=_str(params, "type") or None,
        source=_str(params, "source") or None,
        category=_str(params, "category") or None,
        trace_id=_str(params, "trace_id") or None,
        actor=_str(params, "actor") or None,
        related_agent=_str(params, "related_agent") or None,
        before=_str(params, "before"),
        before_id=_str(params, "before_id"),
    )


async def _trace(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    if app.state.event_store is None:
        raise QueryError("no_event_store")
    return await trace_events(app, _str(params, "trace_id"))


async def _tokens(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    return await tokens_breakdown(
        app,
        since_days=_int(params, "since_days", DEFAULT_WINDOW_DAYS),
        agent_role=_str(params, "agent_role") or None,
        recent_turns=_int(params, "recent_turns", DEFAULT_RECENT_TURNS),
    )


async def _schedules(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    return await schedules_payload(app)


async def _integrations(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    """Configured external surfaces, their wiring, and recent traffic."""
    from crewlet.api.routes.integrations import integrations_payload

    return await integrations_payload(app)


async def _budgets(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    """Caps, the durable counter, and which scopes are being refused.

    The seat cards draw the engine-run meter because it shares a span
    with the cap it is compared against. This is the other half: the
    shared counter that actually enforces, which survives restarts and
    is the number an operator is asking about when they ask whether a
    seat is out of budget.
    """
    from crewlet.api.routes.budgets import budgets_payload

    return await budgets_payload(app)


async def _conversations(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    """What a seat already did in each thread it works.

    The same rows the engine renders into a conversation's next turn.
    Served because a context source an operator cannot read is the
    invisible second memory the CLI-agent workspace deletes on every
    call precisely to avoid — this one is engine-owned, so it can be
    shown instead.
    """
    from crewlet.api.routes.conversations import conversations_payload

    return await conversations_payload(
        app,
        handle=str(params.get("handle", "") or ""),
        key=str(params.get("key", "") or ""),
    )


async def _sandbox_runs(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    """The detached coding runs the engine is holding.

    Read from the durable row rather than from the live projection: the
    projection sweeps an entry after twelve hours and a run parked on a
    question can wait days, so the states that most need a person were
    the ones least likely to still be in memory.
    """
    from crewlet.api.routes.sandbox_runs import (
        PendingStoreUnavailable,
        sandbox_runs_payload,
    )

    try:
        return await sandbox_runs_payload(app)
    except PendingStoreUnavailable:
        raise QueryError("no_pending_store") from None


async def _fleet(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    """The lease table, over the socket.

    Fleet was the last view still reaching for ``fetch`` on its own, and
    it is where that cost was collected: the view took its HTTP client
    from a context field the shell never populated, so every poll threw
    before it reached the network and the page held its loading skeleton
    for ever. The socket channel is wired for every other view, which is
    why none of them could fail this way.

    Leases still move with no event to push, so the view still polls —
    but it polls through the one transport, and a failed poll is a
    rejected promise the view already knows how to render.
    """
    from crewlet.api.routes.fleet import FleetUnavailable, fleet_payload

    try:
        return await fleet_payload(app)
    except FleetUnavailable:
        raise QueryError("fleet_unavailable") from None


async def _config(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    from crewlet.api.config_routes import config_document

    return await config_document(app)


async def _config_entities(
    app: Any, params: dict[str, Any], _client: Any = None
) -> Any:
    """One config collection's ids, or one entity's body.

    Operator-gated like every other ``/config`` read: the fragment
    carries structure, and structure is exactly what the guard on that
    prefix exists to protect.
    """
    from crewlet.api.config_entity_routes import config_entities

    try:
        return await config_entities(app, _str(params, "kind"), _str(params, "id"))
    except LookupError:
        raise QueryError("no_active_revision") from None
    except KeyError:
        raise QueryError("not_found") from None


async def _config_audit(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    from crewlet.api.config_audit import list_revisions

    return await list_revisions(app, limit=_int(params, "limit", 50))


async def _config_diff(app: Any, params: dict[str, Any], _client: Any = None) -> Any:
    from crewlet.api.config_routes import revision_diff

    diff = await revision_diff(app, _str(params, "revision_id"))
    if diff is None:
        raise QueryError("not_found")
    return diff


async def _stream(app: Any, params: dict[str, Any], client: Any) -> Any:
    """Facts about THIS socket.

    Dropped envelopes cannot ride the health tick: ``_fan_out`` encodes
    one JSON string and puts the same string on every client's queue --
    that is the design -- so a per-client field there would force one
    encode per client per tick.  They are also not a five-second number:
    you want them when you open the health surface.

    ``connected_at`` ships with the counter because ``drops`` resets on
    every reconnect, and a monotonic counter with no window is a figure
    that cannot be read.
    """
    stream = getattr(app.state, "stream", None)
    if client is None or stream is None:
        raise QueryError("not_found")
    return {
        "client_id": client.id,
        "dropped": client.drops,
        "queued": client.queue.qsize(),
        "capacity": client.queue.maxsize,
        "connected_at": client.connected_at,
        "clients": stream.client_count,
    }


Handler = Callable[[Any, dict[str, Any], Any], Awaitable[Any]]

# ``what`` → handler.  The boolean is whether the query is operator-only
# (the ``/config`` surface); everything else is as public as the REST
# read endpoints it mirrors.
_QUERIES: dict[str, tuple[Handler, bool]] = {
    "agent": (_agent, False),
    "agent_memory": (_agent_memory, False),
    "event": (_event, False),
    "events": (_events, False),
    "trace": (_trace, False),
    "tokens": (_tokens, False),
    "schedules": (_schedules, False),
    "fleet": (_fleet, False),
    "sandbox_runs": (_sandbox_runs, False),
    "conversations": (_conversations, False),
    "budgets": (_budgets, False),
    "integrations": (_integrations, False),
    # Named ``stream``, not ``health``: a query must never share a name
    # with a push kind, or a reader of the protocol has to know which
    # direction a frame was travelling to know what it means.
    "stream": (_stream, False),
    "config": (_config, True),
    "config_audit": (_config_audit, True),
    "config_entities": (_config_entities, True),
    "config_diff": (_config_diff, True),
}


async def run_query(
    app: Any,
    what: str,
    params: dict[str, Any],
    *,
    token: str = "",
    client: Any = None,
) -> Any:
    """Answer one query, or raise :class:`QueryError` with a code."""
    entry = _QUERIES.get(what)
    if entry is None:
        raise QueryError("unknown_query")
    handler, needs_operator = entry
    if needs_operator and resolve_operator(app, token) is None:
        raise QueryError("unauthorized")
    try:
        # ``client`` is passed positionally to every handler; all but
        # ``_stream`` ignore it. Smuggling it through ``params`` instead
        # was the shorter diff and the wrong one: ``params`` comes off
        # the wire from a browser and must never be a channel for
        # server-side objects.
        return await handler(app, params, client)
    except QueryError:
        raise
    except Exception:
        logger.exception("stream_query_failed", what=what)
        raise QueryError("query_failed") from None
