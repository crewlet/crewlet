"""GET /config/audit — recent revision activation history.

Returns the most recent ``company_config`` revisions as a flat list
of metadata — the dashboard's Configuration tab consumes this
endpoint to render the history table.  The structural diff between
any two revisions is available via ``GET /config/revisions/{id}/diff``.
"""

from __future__ import annotations

from starlette.requests import Request
from starlette.responses import JSONResponse
from starlette.routing import Route

from crewlet.api.config_routes import _serialize_revision_meta, _store


async def get_config_audit(request: Request) -> JSONResponse:
    """GET /config/audit?limit=N — recent revision metadata."""
    try:
        limit = int(request.query_params.get("limit", "50"))
    except ValueError:
        return JSONResponse({"error": "invalid_limit"}, status_code=400)
    if limit <= 0 or limit > 500:
        return JSONResponse({"error": "invalid_limit"}, status_code=400)

    store = _store(request)
    revisions = await store.list_revisions(limit=limit)
    return JSONResponse({"revisions": [_serialize_revision_meta(r) for r in revisions]})


def build_config_audit_routes() -> list[Route]:
    return [Route("/config/audit", get_config_audit, methods=["GET"])]
