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


# How many rows of dispatch history the schedules view shows.  It is a
# recent-activity tail next to the schedule list, not an audit log —
# the full history lives in the ``scheduled_runs`` table.
RECENT_RUNS_LIMIT = 50


def schedule_projection(app: Any) -> list[dict[str, Any]]:
    """The configured schedules with ``next_run`` computed, synchronously.

    Deliberately free of I/O: the live-stream snapshot embeds this on
    every WebSocket connect, and a snapshot must never touch the
    database.  Dispatch history is the async half
    (:func:`recent_scheduled_runs`) and is fetched on demand by the
    Schedules view.
    """
    schedules: list[dict[str, Any]] = list(
        getattr(app.state, "schedules_data", []) or []
    )
    now = datetime.now(UTC)
    return [
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


async def recent_scheduled_runs(app: Any) -> list[dict[str, Any]]:
    """Recent rows from the ``scheduled_runs`` dispatch ledger."""
    return await _list_scheduled_runs(
        getattr(app.state, "database", None), limit=RECENT_RUNS_LIMIT
    )


async def schedules_payload(app: Any) -> dict[str, Any]:
    """Configured schedules + recent dispatch history."""
    return {
        "schedules": schedule_projection(app),
        "recent_runs": await recent_scheduled_runs(app),
    }


async def get_schedules(request: Request) -> JSONResponse:
    """GET /schedules — configured role/unit schedules + recent run history."""
    return JSONResponse(await schedules_payload(request.app))
