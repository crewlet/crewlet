"""Dashboard shell + static-asset serving.

The dashboard is a zero-build, modular ES-module app under
``crewlet/static/dashboard/``.  ``/dashboard`` serves the shell
(``index.html``); ``/static/{path}`` serves every module / stylesheet /
asset it imports.
"""

from __future__ import annotations

import hashlib
from pathlib import Path

from starlette.requests import Request
from starlette.responses import HTMLResponse, RedirectResponse, Response

_STATIC_DIR = Path(__file__).resolve().parent.parent.parent / "static"
_DASHBOARD_INDEX = _STATIC_DIR / "dashboard" / "index.html"

# Content types for the assets the dashboard serves.  ES modules are
# served as ``text/javascript`` (``.js`` / ``.mjs``).
_CONTENT_TYPES = {
    ".html": "text/html; charset=utf-8",
    ".js": "text/javascript; charset=utf-8",
    ".mjs": "text/javascript; charset=utf-8",
    ".css": "text/css; charset=utf-8",
    ".json": "application/json",
    ".map": "application/json",
    ".svg": "image/svg+xml",
    ".png": "image/png",
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".ico": "image/x-icon",
    ".woff": "font/woff",
    ".woff2": "font/woff2",
}


async def root(request: Request) -> RedirectResponse:
    return RedirectResponse(url="/dashboard", status_code=302)


async def dashboard(request: Request) -> Response:
    """GET /dashboard — serve the dashboard shell.

    Read fresh each request (cheap, one small file) with ``no-cache`` so
    a redeploy is picked up immediately; the module assets it pulls are
    cached separately by :func:`static_file`.
    """
    if not _DASHBOARD_INDEX.is_file():
        return HTMLResponse("<h1>Crewlet dashboard not installed</h1>", status_code=404)
    return HTMLResponse(
        _DASHBOARD_INDEX.read_text(encoding="utf-8"),
        headers={"Cache-Control": "no-cache"},
    )


async def static_file(request: Request) -> Response:
    """GET /static/{path} — serve a dashboard asset.

    Resolves the requested path against the static directory and refuses
    anything that escapes it (path traversal).  Assets are served with a
    content-hash ``ETag`` and ``Cache-Control: no-cache`` so the browser
    always revalidates: unchanged files get a cheap ``304``, but a
    redeploy is picked up on the very next request (no stale-module
    window).
    """
    rel = request.path_params["path"]
    target = (_STATIC_DIR / rel).resolve()
    try:
        target.relative_to(_STATIC_DIR.resolve())
    except ValueError:
        return Response("forbidden", status_code=403)
    if not target.is_file():
        return Response("not found", status_code=404)

    data = target.read_bytes()
    etag = f'"{hashlib.blake2b(data, digest_size=10).hexdigest()}"'
    if request.headers.get("if-none-match") == etag:
        return Response(
            status_code=304, headers={"ETag": etag, "Cache-Control": "no-cache"}
        )
    media_type = _CONTENT_TYPES.get(target.suffix.lower(), "application/octet-stream")
    return Response(
        data,
        media_type=media_type,
        headers={"ETag": etag, "Cache-Control": "no-cache"},
    )
