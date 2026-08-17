"""Health endpoint."""

from __future__ import annotations

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet.api.streaming import build_health_envelope, build_readiness_envelope


async def health(request: Request) -> JSONResponse:
    """GET /health — liveness + ``configured`` flag + live runtime metrics.

    Always 200 while the process is alive, **including through a drain**:
    an orchestrator watching liveness must not SIGKILL a node that is
    finishing its in-flight turns.  Use ``/ready`` to steer traffic.

    ``node`` names the process that answered.  ``configured`` is ``True``
    once a Tier B company revision is active (maintained by
    :func:`crewlet.api.config_refresh.subscribe_config_refresh`).
    ``in_flight`` / ``shutting_down`` are present only when a
    :class:`~crewlet.api.runtime.NodeRuntime` is registered (the embedded
    path); they let operators watch a drain converge to 0. ``status``
    reads ``"shutting_down"`` during the drain.
    """
    return JSONResponse(build_health_envelope(request.app))


async def ready(request: Request) -> JSONResponse:
    """GET /ready — readiness for a load balancer.

    ``503`` while draining or before the first config revision applies,
    ``200`` otherwise.  The split from ``/health`` is what lets a node
    leave rotation the moment a drain starts while still reporting itself
    alive for the minutes its turns need to finish.
    """
    body, status = build_readiness_envelope(request.app)
    return JSONResponse(body, status_code=status)
