"""Bearer-token auth for the ``/config/*`` routes.

Tier A (``config.yaml``) lists the accepted tokens under
``api.auth.tokens``.  Each entry has an ``id`` (recorded in revision
audit logs as ``created_by``) and a ``token`` (resolved from env at
API startup).  The middleware mounted on the ``/config`` prefix
extracts the bearer token, constant-time-compares against the loaded
list, and either attaches ``request.state.operator_id`` or returns
``401``.

Failed auth attempts log at WARNING via structlog with route + remote;
the candidate token value is never logged.  Successful auth logs at
DEBUG.
"""

from __future__ import annotations

import hmac
from typing import Any

from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request
from starlette.responses import JSONResponse
from starlette.types import ASGIApp

from crewlet._logging import get_logger

logger = get_logger("api.auth")


# Path prefix the middleware guards; bypass for everything else.
GUARDED_PREFIX = "/config"


class TokenLoadError(RuntimeError):
    """Raised at API startup when the configured tokens are invalid."""


def load_tokens(bootstrap: Any) -> dict[str, str]:
    """Return the validated ``{operator_id: token}`` map.

    Fail-safe behaviour:
    - When ``api.auth.disabled`` is True, returns ``{}`` (any
      ``/config/*`` request is allowed).  Loud WARNING log.
    - When ``api.auth.disabled`` is False and no tokens are listed,
      raises :class:`TokenLoadError`.
    - When a token resolves to the empty string (e.g. the env var is
      unset), raises :class:`TokenLoadError`.
    - Duplicate ``id`` entries raise :class:`TokenLoadError`.
    """
    auth = bootstrap.api.auth
    if auth.disabled:
        logger.warning(
            "api_auth_disabled",
            hint=(
                "api.auth.disabled is True — /config/* serves without "
                "authentication. Never use in production."
            ),
        )
        return {}

    if not auth.tokens:
        raise TokenLoadError(
            "api.auth.tokens is empty.  Configure at least one bearer "
            "token (or set api.auth.disabled: true for local dev)."
        )

    seen_ids: dict[str, str] = {}
    for entry in auth.tokens:
        if entry.id == "anonymous":
            # ``"anonymous"`` is the sentinel ``operator_id`` recorded
            # when ``auth.disabled`` is True.  Allowing a real token
            # with this id would collide audit attribution between
            # disabled-mode requests and real operator writes.
            raise TokenLoadError(
                "api.auth.tokens id 'anonymous' is reserved (it is the "
                "attribution string used when api.auth.disabled is True). "
                "Pick a different id."
            )
        if entry.id in seen_ids:
            raise TokenLoadError(f"Duplicate api.auth.tokens id: {entry.id!r}")
        if not entry.token:
            raise TokenLoadError(
                f"Token for id={entry.id!r} resolved to empty string — "
                f"check the env var reference."
            )
        seen_ids[entry.id] = entry.token

    logger.info("api_auth_tokens_loaded", count=len(seen_ids))
    return seen_ids


def resolve_operator(app: Any, candidate: str) -> str | None:
    """Return the ``operator_id`` a bare token authenticates as, else ``None``.

    The token comparison, in one place.  The HTTP middleware reaches it
    through :func:`check_bearer` (which peels the ``Authorization``
    header first) and the dashboard's WebSocket query channel calls it
    directly, because an operator-only query arrives as a field on a
    socket frame rather than as a header.  Both therefore accept exactly
    the same tokens, honour ``api.auth.disabled`` identically, and
    compare in constant time.
    """
    tokens: dict[str, str] | None = getattr(app.state, "auth_tokens", None)
    if not tokens:
        # When auth is disabled every caller is accepted.  Mirror the
        # "no token" case so downstream attribution lands on an explicit
        # "anonymous" label.
        if getattr(app.state, "auth_disabled", False):
            return "anonymous"
        return None
    if not candidate:
        return None
    for operator_id, expected in tokens.items():
        if hmac.compare_digest(candidate, expected):
            return operator_id
    return None


def check_bearer(request: Request) -> str | None:
    """Return the ``operator_id`` for a valid bearer header, else ``None``."""
    header = request.headers.get("authorization", "")
    if not header.lower().startswith("bearer "):
        return resolve_operator(request.app, "")
    return resolve_operator(request.app, header[len("bearer ") :].strip())


class ApiAuthMiddleware(BaseHTTPMiddleware):
    """Mounted at the app root; only guards ``GUARDED_PREFIX`` paths."""

    def __init__(self, app: ASGIApp) -> None:
        super().__init__(app)

    async def dispatch(self, request: Request, call_next: Any) -> Any:
        path = request.url.path
        if not path.startswith(GUARDED_PREFIX):
            return await call_next(request)

        operator_id = check_bearer(request)
        if operator_id is None:
            logger.warning(
                "api_auth_failed",
                route=path,
                reason="missing_or_invalid_bearer",
                remote=request.client.host if request.client else "",
            )
            return JSONResponse(
                {"error": "invalid_token"},
                status_code=401,
            )

        request.state.operator_id = operator_id
        logger.debug(
            "api_auth_ok",
            operator_id=operator_id,
            route=path,
        )
        return await call_next(request)
