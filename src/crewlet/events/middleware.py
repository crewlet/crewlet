"""Event middleware pipeline for filtering and transforming events."""

from __future__ import annotations

from collections.abc import Callable, Coroutine
from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import Event

EventMiddleware = Callable[[Event], Coroutine[Any, Any, Event | None]]

logger = get_logger("events.middleware")


async def logging_middleware(event: Event) -> Event | None:
    """Log every event that passes through the pipeline."""
    logger.debug(
        "event_passthrough",
        event_type=event.type,
        source=event.source,
        event_id=str(event.id)[:8],
    )
    return event
