"""GET /budgets — caps, what has actually been spent against them, and
which scopes are being refused.

Three numbers describe a token budget and they cover three different
spans, which is why the dashboard could only ever show one of them
honestly:

* the **cap** is config — it comes from the active company revision;
* **durable usage** is the shared counter in ``token_budget_usage``, is
  written by every process running the company, and survives restarts.
  It is what the engine actually enforces against;
* the **live meter** is per engine RUN. It is pushed to the dashboard as
  ``budget_reported`` and resets when the process does.

A seat card can draw a bar because the live meter and the cap it is
compared against share a span. Nothing could draw the more useful
picture — "this seat has burned 94% of its cap over the last three days,
across two restarts" — because the durable half was reachable only from
`crewlet budgets show`. This is that half.

Exhaustion is reported as the engine records it: ``refused_at``, the
moment a charge was turned away. Not ``used >= max``. ``TokenBudget``
refuses a charge that would exceed the cap and increments nothing, so a
seat charged in 3k-token rounds against a 100k cap stalls near 99k and
never compares equal to its own maximum — a ratio test shows a
permanently blocked seat at 97% and calls it healthy.
"""

from __future__ import annotations

from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet._logging import get_logger

logger = get_logger("api.routes.budgets")


def _iso(value: Any) -> str:
    try:
        return value.isoformat() if value is not None else ""
    except Exception:
        return ""


def _caps(app: Any) -> tuple[int, dict[str, dict[str, Any]]]:
    """The configured caps, from the active revision's projection.

    ``agent_roles`` is the same projection ``GET /agents`` reads, so the
    handle derivation and the human-seat exclusion are shared rather than
    re-walked here — a seat whose id this derived differently would show
    a cap that belongs to nobody.
    """
    org_data = getattr(app.state, "org_data", None) or {}
    org_cap = int(org_data.get("token_budget") or 0)
    seats: dict[str, dict[str, Any]] = {}
    for role in getattr(app.state, "agent_roles", None) or []:
        agent_id = str(role.get("id") or "")
        if not agent_id:
            continue
        seats[agent_id] = {
            "agent_id": agent_id,
            "role": str(role.get("name") or role.get("role") or ""),
            "handle": str(role.get("handle") or ""),
            "max_tokens": int(role.get("token_budget") or 0),
        }
    return org_cap, seats


async def budgets_payload(app: Any) -> dict[str, Any]:
    """Caps, durable usage and live refusals, in one answer."""
    from crewlet.db.budgets import ORG_SCOPE, agent_scope
    from crewlet.db.client import Database

    org_cap, seats = _caps(app)

    used_by_scope: dict[str, dict[str, Any]] = {}
    durable = False
    db = getattr(app.state, "database", None)
    if isinstance(db, Database):
        from crewlet.db.budgets import PostgresBudgetUsageStore

        try:
            for row in await PostgresBudgetUsageStore(db).list_usage():
                used_by_scope[row["scope"]] = row
            durable = True
        except Exception:
            # A counter that cannot be read is not a counter that reads
            # zero. `durable` stays false so the surface says it could
            # not see the shared usage rather than drawing every seat at
            # the bottom of its cap.
            logger.exception("budget_usage_read_failed")

    # The live meter, as the engine last reported it. Present only on a
    # node with an engine in the process; a standalone API has none.
    #
    # The org half sits on the meter itself; the per-seat halves ride on
    # each agent's overlay, because a seat that loses its meter has to
    # lose its bar, and the projection expresses that by clearing the
    # field on the agent rather than by omitting a row from the meter.
    from crewlet.api.routes.agents import agents_payload

    stream = getattr(app.state, "stream", None)
    meter = stream.live.budget() if stream is not None else {}
    live_org = (meter or {}).get("org") or {}
    live_agents = {
        str(row.get("id") or ""): row.get("budget") or {}
        for row in agents_payload(app)
        if row.get("budget")
    }

    def _scope_row(scope: str) -> dict[str, Any]:
        row = used_by_scope.get(scope) or {}
        return {
            "used_tokens": int(row.get("used_tokens") or 0),
            "updated_at": _iso(row.get("updated_at")),
        }

    org_durable = _scope_row(ORG_SCOPE)
    seat_rows = []
    for agent_id, seat in seats.items():
        durable_row = _scope_row(agent_scope(agent_id))
        live_row = live_agents.get(agent_id) or {}
        seat_rows.append(
            {
                **seat,
                "durable_used": durable_row["used_tokens"],
                "durable_updated_at": durable_row["updated_at"],
                "live_used": int(live_row.get("used") or 0) if live_row else None,
                "live_max": int(live_row.get("max") or 0) if live_row else None,
                "refused_at": str(live_row.get("refused_at") or ""),
            }
        )
    seat_rows.sort(key=lambda s: (-s["durable_used"], str(s["role"])))

    return {
        # False means "the shared counter could not be read", which is a
        # different statement from "nothing has been spent".
        "durable": durable,
        "org": {
            "max_tokens": org_cap,
            "durable_used": org_durable["used_tokens"],
            "durable_updated_at": org_durable["updated_at"],
            "live_used": int(live_org.get("used") or 0) if live_org else None,
            "live_max": int(live_org.get("max") or 0) if live_org else None,
            "refused_at": str(live_org.get("refused_at") or ""),
        },
        "seats": seat_rows,
    }


async def get_budgets(request: Request) -> JSONResponse:
    """GET /budgets — the REST twin of the ``budgets`` query."""
    return JSONResponse(await budgets_payload(request.app))
