"""Health endpoint."""

from __future__ import annotations

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet.api.streaming import build_health_envelope


async def health(request: Request) -> JSONResponse:
    """GET /health — liveness + ``configured`` flag + live runtime metrics.

    ``configured`` is ``True`` once a Tier B company revision is active
    (maintained by :func:`crewlet.api.config_refresh.
    subscribe_config_refresh`).  ``in_flight`` / ``shutting_down`` are
    present only on the embedded API (an engine reference is attached);
    they let operators watch a graceful-shutdown drain converge to 0.
    ``status`` reads ``"shutting_down"`` during the drain.
    """
    return JSONResponse(build_health_envelope(request.app))
