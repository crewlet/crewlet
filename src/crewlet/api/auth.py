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


# The config surface. Always guarded, and never eligible for
# ``allow_anonymous_read`` — reading it exposes the whole company
# document, and writing it changes the company.
GUARDED_PREFIX = "/config"

# Routes served without a bearer token, because they authenticate by
# other means or because a client must reach them to obtain a token.
#
# - ``/health`` / ``/ready``: probes. An orchestrator has no token, and a
#   liveness check that 401s is a liveness check that fails.
# - ``/webhooks/``: every one verifies a provider HMAC before doing
#   anything, which is a stronger check than a shared bearer token.
#   Includes the Slack OAuth landing page, which a browser reaches
#   mid-install with no token in hand.
# - ``/otlp/``: the per-run signed token in the path IS the credential.
# - the dashboard shell + its assets: the page that prompts for the
#   token cannot itself require one. It ships no data — every byte it
#   renders comes from an authenticated fetch.
UNGUARDED_EXACT = frozenset({"/", "/dashboard", "/favicon.ico"})
UNGUARDED_PREFIXES = ("/health", "/ready", "/webhooks/", "/otlp/", "/static/")

# Methods treated as reads for ``allow_anonymous_read``.
READ_METHODS = frozenset({"GET", "HEAD", "OPTIONS"})


def is_unguarded_path(path: str) -> bool:
    """Whether ``path`` is served without a bearer token."""
    return path in UNGUARDED_EXACT or path.startswith(UNGUARDED_PREFIXES)


class TokenLoadError(RuntimeError):
    """Raised at API startup when the configured tokens are invalid."""


def load_tokens(bootstrap: Any) -> dict[str, str]:
    """Return the validated ``{operator_id: token}`` map.

    Fail-safe behaviour:
    - When ``api.auth.disabled`` is True, returns ``{}`` (every request
      is allowed).  Loud WARNING log.
    - When no tokens are listed and neither ``disabled`` nor
      ``allow_anonymous_read`` is set, raises :class:`TokenLoadError`.
      Serving the API requires an explicit auth decision: it carries
      LLM transcripts, and silence used to mean "open".
    - When a token resolves to the empty string (e.g. the env var is
      unset), raises :class:`TokenLoadError`.
    - Duplicate ``id`` entries raise :class:`TokenLoadError`.
    """
    auth = bootstrap.api.auth
    if auth.disabled:
        logger.warning(
            "api_auth_disabled",
            hint=(
                "api.auth.disabled is True — every route, including LLM "
                "transcripts on /events and /agents/{id}/memory, serves "
                "without authentication. Never use in production."
            ),
        )
        return {}

    if not auth.tokens:
        if getattr(auth, "allow_anonymous_read", False):
            # An explicit decision: reads are open, writes and /config
            # are refused outright since no token can ever match.
            return {}
        raise TokenLoadError(
            "api.auth.tokens is empty.  The API serves LLM transcripts "
            "(/events, /agents/{id}/memory, /ws/stream), so it needs an "
            "explicit decision: configure at least one bearer token, or "
            "set api.auth.disabled: true (local dev) or "
            "api.auth.allow_anonymous_read: true (network-isolated)."
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


def requires_token(app_state: Any, *, path: str, method: str) -> bool:
    """Whether this request must carry a valid bearer token.

    Note what is deliberately NOT exempted: ``/events``,
    ``/agents/{id}/memory`` and ``/ws/stream`` all serve full LLM
    transcripts, and used to be open because auth was scoped to
    ``/config``.
    """
    if is_unguarded_path(path):
        return False
    if path.startswith(GUARDED_PREFIX):
        return True
    if getattr(app_state, "auth_anonymous_read", False):
        return method.upper() not in READ_METHODS
    return True


class ApiAuthMiddleware(BaseHTTPMiddleware):
    """Mounted at the app root; guards every route bar the exemptions.

    HTTP only — Starlette's ``BaseHTTPMiddleware`` never sees a WebSocket
    scope, so ``/ws/stream`` authenticates inside its own handler (see
    :func:`crewlet.api.routes.stream.websocket_auth_failed`).
    """

    def __init__(self, app: ASGIApp) -> None:
        super().__init__(app)

    async def dispatch(self, request: Request, call_next: Any) -> Any:
        path = request.url.path
        if not requires_token(request.app.state, path=path, method=request.method):
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
