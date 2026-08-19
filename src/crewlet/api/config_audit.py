"""GET /config/audit — recent revision activation history.

Returns the most recent ``company_config`` revisions as a flat list
of metadata — the dashboard's Configuration tab consumes this
endpoint to render the history table.  The structural diff between
any two revisions is available via ``GET /config/revisions/{id}/diff``.
"""

from __future__ import annotations

from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse
from starlette.routing import Route

from crewlet.api.config_routes import _serialize_revision_meta, _store

# Bounds on one audit read.  The table is a history pane, not an export
# surface — ``crewlet config export`` is the path for a full dump.
DEFAULT_AUDIT_LIMIT = 50
MAX_AUDIT_LIMIT = 500


async def list_revisions(app: Any, *, limit: int = DEFAULT_AUDIT_LIMIT) -> Any:
    """Recent revision metadata, newest first.

    Shared by ``GET /config/audit`` and the WebSocket ``config_audit``
    query.
    """
    store = getattr(app.state, "company_config_store", None)
    if store is None:
        return {"revisions": []}
    limit = max(1, min(int(limit), MAX_AUDIT_LIMIT))
    revisions = await store.list_revisions(limit=limit)
    return {"revisions": [_serialize_revision_meta(r) for r in revisions]}


async def get_config_audit(request: Request) -> JSONResponse:
    """GET /config/audit?limit=N — recent revision metadata."""
    try:
        limit = int(request.query_params.get("limit", str(DEFAULT_AUDIT_LIMIT)))
    except ValueError:
        return JSONResponse({"error": "invalid_limit"}, status_code=400)
    if limit <= 0 or limit > MAX_AUDIT_LIMIT:
        return JSONResponse({"error": "invalid_limit"}, status_code=400)

    _store(request)  # raises the configured 503 when no store is wired
    return JSONResponse(await list_revisions(request.app, limit=limit))


def build_config_audit_routes() -> list[Route]:
    return [Route("/config/audit", get_config_audit, methods=["GET"])]
