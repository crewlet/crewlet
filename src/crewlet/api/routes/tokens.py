"""Token-spend breakdown endpoint."""

from __future__ import annotations

from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet.api.live_state import TOKEN_WINDOW_DAYS
from crewlet.api.routes._common import safe_store_query

# Default rollup window.  Reads the projection's own window so the
# breakdown this endpoint computes and the one the live projection
# pushes describe the same period by construction.
DEFAULT_WINDOW_DAYS = TOKEN_WINDOW_DAYS
# Widest window a caller may ask for.  The query is a scan over the
# phase-event history, so this is the ceiling on how much of it one
# request can pull.
MAX_WINDOW_DAYS = 30
# The per-turn table is a tail of recent activity, not a full history.
DEFAULT_RECENT_TURNS = 50
MAX_RECENT_TURNS = 500


def _empty_breakdown(since_days: int, agent_role: str | None) -> dict[str, Any]:
    return {
        "since_days": since_days,
        "agent_role": agent_role or "",
        "totals": {
            "input_tokens": 0,
            "output_tokens": 0,
            "total_tokens": 0,
            "calls": 0,
        },
        "by_phase": [],
        "by_model": [],
        "by_worker": [],
        "by_agent": [],
        "by_turn": [],
        # Watermark for the dashboard's live token folding — empty means
        # "fold every live phase event" (nothing aggregated yet).
        "aggregated_through": "",
    }


async def tokens_breakdown(
    app: Any,
    *,
    since_days: int = DEFAULT_WINDOW_DAYS,
    agent_role: str | None = None,
    recent_turns: int = DEFAULT_RECENT_TURNS,
) -> dict[str, Any]:
    """Per-stage / model / worker / agent / turn spend rollup.

    Reads ``agent_phase_completed`` events from the store and aggregates
    them.  The dashboard does NOT call this for its default view — the
    live projection keeps that rollup in memory and pushes it — this is
    the path for a window other than the live one, and for any REST
    consumer.
    """
    from crewlet.api.tokens import aggregate_phase_events

    since_days = max(1, min(int(since_days), MAX_WINDOW_DAYS))
    recent_turns = max(1, min(int(recent_turns), MAX_RECENT_TURNS))

    store = app.state.event_store
    if store is None:
        return _empty_breakdown(since_days, agent_role)

    roles: list[dict[str, Any]] = app.state.agent_roles
    role_handle_map = {
        r.get("role", ""): r.get("handle", "") for r in roles if r.get("role")
    }
    events = await safe_store_query(
        store.list_phase_token_events(since_days=since_days, agent_role=agent_role),
        [],
    )
    rollup = aggregate_phase_events(
        events or [],
        role_handle_map=role_handle_map,
        recent_turns_limit=recent_turns,
    )
    return {"since_days": since_days, "agent_role": agent_role or "", **rollup}


async def get_tokens_breakdown(request: Request) -> JSONResponse:
    """GET /tokens/breakdown — per-stage / model / worker / agent / turn rollup.

    Query parameters: ``since_days`` (1..30, default 7), ``agent_role``
    (restrict to one role), ``recent_turns`` (cap on the per-turn list,
    default 50).
    """
    params = request.query_params

    def _int(name: str, default: int) -> int:
        try:
            return int(params.get(name, str(default)))
        except (TypeError, ValueError):
            return default

    return JSONResponse(
        await tokens_breakdown(
            request.app,
            since_days=_int("since_days", DEFAULT_WINDOW_DAYS),
            agent_role=params.get("agent_role") or None,
            recent_turns=_int("recent_turns", DEFAULT_RECENT_TURNS),
        )
    )
