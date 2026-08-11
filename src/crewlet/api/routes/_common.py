"""Shared helpers for the API route modules."""

from __future__ import annotations

from datetime import datetime
from typing import Any

from crewlet._logging import get_logger

logger = get_logger("api.routes")


async def safe_store_query(coro: Any, fallback: Any = None) -> Any:
    """Execute an event-store query, returning *fallback* on error.

    Reads against the event store are best-effort: a missing or
    unavailable store must degrade gracefully rather than 500 the
    dashboard.
    """
    try:
        return await coro
    except Exception as exc:
        logger.exception("event_store_query_failed", error=str(exc))
        return fallback


def iso(value: Any) -> str:
    """Render a datetime / string / None as ISO-8601 (or empty string)."""
    if value is None or value == "":
        return ""
    if isinstance(value, datetime):
        return value.isoformat()
    return str(value)
