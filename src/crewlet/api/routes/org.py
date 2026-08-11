"""Organization, schedules, and tools read endpoints."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet._logging import get_logger

logger = get_logger("api.routes")


async def get_org(request: Request) -> JSONResponse:
    """GET /org — organization hierarchy from the active config."""
    return JSONResponse(request.app.state.org_data)


async def list_tools(request: Request) -> JSONResponse:
    """GET /tools — builtin + MCP tool descriptors."""
    return JSONResponse(request.app.state.tools_data)


def _next_run_iso(cron_str: str, tz_str: str, now: datetime) -> str:
    """Next UTC fire time for a cron expression, ISO-8601 (or empty)."""
    if not cron_str:
        return ""
    try:
        from zoneinfo import ZoneInfo

        from crewlet.schedule.cron import next_fire, parse_cron

        cron = parse_cron(cron_str)
        tz = ZoneInfo(tz_str or "UTC")
    except Exception:
        return ""
    nxt = next_fire(cron, after_utc=now, tz=tz)
    return nxt.isoformat() if nxt is not None else ""


async def _list_scheduled_runs(
    database: Any, *, limit: int = 50
) -> list[dict[str, Any]]:
    """Recent rows from the ``scheduled_runs`` dispatch ledger."""
    if database is None:
        return []
    try:
        from crewlet.db.client import Database
        from crewlet.schedule.store import ScheduledRunStore
    except ImportError:  # pragma: no cover - defensive
        return []
    if not isinstance(database, Database):
        return []
    try:
        return await ScheduledRunStore(database).list_recent(limit)
    except Exception:
        logger.exception("scheduled_runs_query_failed")
        return []


async def get_schedules(request: Request) -> JSONResponse:
    """GET /schedules — configured role/unit schedules + recent run history.

    The configured list is resolved once at startup
    (``app.state.schedules_data``); ``next_run`` is computed per request
    from each cron expression, and the recent dispatch history is read
    from the ``scheduled_runs`` ledger (empty without a database).
    """
    schedules: list[dict[str, Any]] = list(
        getattr(request.app.state, "schedules_data", []) or []
    )
    now = datetime.now(UTC)
    enriched = [
        {
            **s,
            "next_run": (
                _next_run_iso(s.get("cron", ""), s.get("timezone", "") or "UTC", now)
                if s.get("enabled", True)
                else ""
            ),
        }
        for s in schedules
    ]
    database = getattr(request.app.state, "database", None)
    recent = await _list_scheduled_runs(database, limit=50)
    return JSONResponse({"schedules": enriched, "recent_runs": recent})
