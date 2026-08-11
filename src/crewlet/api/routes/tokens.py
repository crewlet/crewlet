"""Token-spend breakdown endpoint."""

from __future__ import annotations

from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet.api.routes._common import safe_store_query


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


async def get_tokens_breakdown(request: Request) -> JSONResponse:
    """GET /tokens/breakdown — per-stage / model / worker / agent / turn rollup.

    Reads ``agent_phase_completed`` events and aggregates spend so the
    dashboard's Tokens view renders every breakdown from a single fetch.

    Query parameters: ``since_days`` (1..30, default 7), ``agent_role``
    (restrict to one role), ``recent_turns`` (cap on the per-turn list,
    default 50).
    """
    from crewlet.api.tokens import aggregate_phase_events

    try:
        since_days = int(request.query_params.get("since_days", "7"))
    except (TypeError, ValueError):
        since_days = 7
    since_days = max(1, min(since_days, 30))

    try:
        recent_turns = int(request.query_params.get("recent_turns", "50"))
    except (TypeError, ValueError):
        recent_turns = 50
    recent_turns = max(1, min(recent_turns, 500))

    agent_role = request.query_params.get("agent_role") or None

    store = request.app.state.event_store
    roles: list[dict[str, Any]] = request.app.state.agent_roles
    role_handle_map = {
        r.get("role", ""): r.get("handle", "") for r in roles if r.get("role")
    }

    if store is None:
        return JSONResponse(_empty_breakdown(since_days, agent_role))

    events = await safe_store_query(
        store.list_phase_token_events(since_days=since_days, agent_role=agent_role),
        [],
    )
    rollup = aggregate_phase_events(
        events or [],
        role_handle_map=role_handle_map,
        recent_turns_limit=recent_turns,
    )
    return JSONResponse(
        {
            "since_days": since_days,
            "agent_role": agent_role or "",
            **rollup,
        }
    )
