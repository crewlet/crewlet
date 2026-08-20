"""Standalone Starlette application factory for the Crewlet API.

No Engine dependency — only an EventQueue is required for webhook
ingestion.  Agent state is derived from the EventStore (TimescaleDB
or in-memory fallback), not from queue subscriptions.

The ``/config/*`` routes (versioned company-config CRUD) are mounted
when a ``CompanyConfigStore`` is supplied and gated by
:class:`ApiAuthMiddleware`.
"""

from __future__ import annotations

import contextlib
from datetime import UTC, datetime
from typing import Any

from starlette.applications import Starlette
from starlette.middleware import Middleware
from starlette.middleware.cors import CORSMiddleware
from starlette.requests import ClientDisconnect, Request
from starlette.responses import Response

from crewlet._logging import get_logger
from crewlet.api.auth import ApiAuthMiddleware, bind_is_loopback, load_tokens
from crewlet.api.config_audit import build_config_audit_routes
from crewlet.api.config_entity_routes import build_config_entity_routes
from crewlet.api.config_refresh import (
    ConfigStateRefresher,
    prime_api_state_from_active,
)
from crewlet.api.config_routes import build_config_routes
from crewlet.api.routes import build_routes
from crewlet.api.streaming import StreamService, build_health_envelope
from crewlet.config import resolve_node_id
from crewlet.db.config_plane import ConfigPlaneStore
from crewlet.db.deliveries import (
    MemoryDeliveryDedupeStore,
    PostgresDeliveryDedupeStore,
)

logger = get_logger("api.app")


async def _client_disconnected(request: Request, exc: Exception) -> Response:
    """Handle ``ClientDisconnect`` raised while reading a request body.

    Webhook senders enforce delivery deadlines and abort requests that
    take too long; the abort surfaces as ``ClientDisconnect`` at the
    handler's first body read.  The sender is gone, so the 204 is never
    delivered — it only satisfies the exception-handler contract.
    Without this handler the disconnect escapes the app and uvicorn
    logs it as an unhandled ERROR traceback.
    """
    logger.warning(
        "client_disconnected",
        method=request.method,
        path=request.url.path,
        remote=request.client.host if request.client else "",
    )
    return Response(status_code=204)


def create_app(
    event_queue: Any,
    *,
    event_store: Any = None,
    database: Any = None,
    github_webhook_secret: str | None = None,
    gitlab_signing_secret: str | None = None,
    plane_webhook_secret: str | None = None,
    slack_signing_secrets: dict[str, str] | None = None,
    sandbox_otel_receiver: Any = None,
    forge_app_id: str = "",
    bootstrap: Any = None,
    company_config_store: Any = None,
    runtime: Any = None,
    stream: StreamService | None = None,
) -> Starlette:
    """Build a Starlette ASGI application.

    **One wiring, everywhere.**  The app learns the company from the
    active config revision (:func:`attach_config_refresh`) and learns
    what is happening from the broadcast event stream — whether it runs
    inside the engine process or on its own host.  There are deliberately
    no parameters for pre-loaded roles, org, schedules or tools: passing
    a boot-time snapshot on one path and deriving it on the other is what
    made these two the same surface with two implementations, and the
    divergence shipped three bugs.  See :mod:`crewlet.api.runtime`.

    Parameters
    ----------
    event_queue:
        An ``EventQueue`` instance for publishing webhook notifications.
    event_store:
        An ``EventStore`` instance for persistent event storage and
        agent state queries.  When ``None``, events/agents endpoints
        return empty results.
    database:
        Optional ``Database`` pool for learning-store reads
        (episodes, counterparty profiles, synthesized skills) used by
        ``/agents/{id}/memory``.  When ``None`` those sections of the
        memory endpoint return empty lists.
    github_webhook_secret:
        HMAC key for ``POST /webhooks/github`` signature verification
        (``X-Hub-Signature-256``).  ``None`` → the endpoint returns 500.
    gitlab_signing_secret:
        GitLab 19.1+ Standard-Webhooks signing token for
        ``POST /webhooks/gitlab``.  ``None`` → the endpoint returns 500.
    plane_webhook_secret:
        HMAC key for ``POST /webhooks/plane`` signature verification
        (``X-Plane-Signature``).  ``None`` → the endpoint returns 500.
    slack_signing_secrets:
        ``{handle: signing_secret}`` for Slack-enabled agent seats, used
        to verify inbound Slack webhooks at the edge.  Slack is the only
        inbound integration whose key is **per-agent** rather than
        org-wide — one Slack app per seat — so this is a map where the
        others are scalars.  Populated by ``attach_config_refresh`` in
        the standalone API process.
    bootstrap:
        Optional :class:`BootstrapConfig` (Tier A).  When supplied,
        auth tokens are loaded from ``bootstrap.api.auth`` and
        ``ApiAuthMiddleware`` is mounted in front of ``/config/*``.
    company_config_store:
        Optional :class:`CompanyConfigStore`.  Required to mount the
        ``/config/*`` routes.
    runtime:
        Optional :class:`~crewlet.api.runtime.NodeRuntime` — the single
        seam for facts only a co-located engine can answer (in-flight
        turns, drain state, the live MCP tool surface).  Registered by
        the embedded path; ``None`` on a standalone process, where those
        fields are simply omitted.
    stream:
        Optional pre-wired :class:`~crewlet.api.streaming.StreamService`.
        The engine constructs one and wires it into the event flow (so
        the same instance owns the live-state projection and the client
        fan-out); when omitted a fresh service is created so the
        ``/ws/stream`` endpoint, snapshot, and ``/agents`` live state
        still work (they just see no engine events until one is wired
        in).
    """
    auth_cfg = getattr(getattr(bootstrap, "api", None), "auth", None)
    # Same-origin by default. ``*`` let any page a logged-in operator
    # happened to visit read every endpoint this process serves.
    allowed_origins = list(getattr(auth_cfg, "allowed_origins", None) or [])
    middleware: list[Middleware] = [
        Middleware(
            CORSMiddleware,
            allow_origins=allowed_origins,
            allow_methods=["*"],
            allow_headers=["*"],
        ),
    ]

    if stream is None:
        stream = StreamService()

    routes = list(build_routes())

    auth_tokens: dict[str, str] = {}
    auth_disabled = False
    auth_anonymous_read = False
    auth_enabled = False
    if company_config_store is not None:
        routes.extend(build_config_routes())
        routes.extend(build_config_entity_routes())
        routes.extend(build_config_audit_routes())
    if bootstrap is not None:
        # Mounted whenever Tier A is present, not only alongside the
        # config routes: what it guards is a policy decision the whole
        # API is subject to (see ``requires_token``), and ``/events`` /
        # ``/agents/{id}/memory`` / ``/ws/stream`` carry full LLM
        # transcripts regardless of whether a config store is wired.
        auth_tokens = load_tokens(bootstrap)
        auth_disabled = bootstrap.api.auth.disabled
        auth_anonymous_read = bool(
            getattr(bootstrap.api.auth, "allow_anonymous_read", True)
        )
        if auth_anonymous_read and not auth_disabled:
            # Stated on every boot, because it is the default and a
            # reader should never have to infer which posture they got.
            # The LEVEL is what carries the judgement: bound to loopback
            # this is a laptop, bound to anything else the transcripts
            # are readable by whoever can reach the port — and a warning
            # that fires on every deployment alike is a warning nobody
            # reads by the third one.
            reachable = not bind_is_loopback(bootstrap.api.host)
            (logger.warning if reachable else logger.info)(
                "api_anonymous_read_enabled",
                host=bootstrap.api.host,
                hint=(
                    "api.auth.allow_anonymous_read is True — every read "
                    "route, including LLM transcripts on /events, "
                    "/agents/{id}/memory and /ws/stream, serves without "
                    "a token. Writes and /config/* still require one. "
                    "Set api.auth.allow_anonymous_read: false to guard "
                    "reads too."
                ),
            )
        auth_enabled = True
        middleware.append(Middleware(ApiAuthMiddleware))

    @contextlib.asynccontextmanager
    async def _lifespan(app: Starlette) -> Any:
        # One shared health tick fans the engine's drain/in-flight state
        # out to every connected dashboard.
        stream.start_health_tick(lambda: build_health_envelope(app))
        # Seed the live-state projection from the store so the first
        # snapshot reflects history accrued before this process started.
        role_names = [
            r.get("role", "") for r in (app.state.agent_roles or []) if r.get("role")
        ]
        try:
            await stream.hydrate(app.state.event_store, role_names)
        except Exception:
            logger.exception("stream_hydrate_failed")
        try:
            yield
        finally:
            await stream.stop_health_tick()

    app = Starlette(
        routes=routes,
        middleware=middleware,
        exception_handlers={ClientDisconnect: _client_disconnected},
        lifespan=_lifespan,
    )
    # This API process's start. Read by the health envelope, and kept
    # distinct from the engine's own start time: on the standalone
    # deployment those are two processes on two clocks.
    app.state.started_at = datetime.now(UTC).isoformat()
    app.state.event_queue = event_queue
    # Populated exclusively by ``attach_config_refresh`` from the active
    # revision — never seeded from a boot-time snapshot, on either path.
    app.state.agent_roles = []
    app.state.org_data = {}
    app.state.schedules_data = []
    app.state.tools_data = []
    app.state.event_store = event_store
    app.state.database = database
    app.state.github_webhook_secret = github_webhook_secret
    app.state.gitlab_signing_secret = gitlab_signing_secret
    app.state.plane_webhook_secret = plane_webhook_secret
    # Per-AGENT, unlike every other inbound secret: one Slack app per
    # seat means one signing secret per seat, so the edge needs a map
    # keyed by the handle the webhook URL path carries.
    app.state.slack_signing_secrets = dict(slack_signing_secrets or {})
    app.state.sandbox_otel_receiver = sandbox_otel_receiver
    # Inbound delivery dedupe. Process-local until a database is wired;
    # GitHub and GitLab had none at all before this, so every provider
    # retry and every operator replay woke the agent again.
    app.state.delivery_dedupe = (
        PostgresDeliveryDedupeStore(database)
        if database is not None
        else MemoryDeliveryDedupeStore()
    )
    app.state.forge_app_id = forge_app_id
    app.state.bootstrap = bootstrap
    app.state.company_config_store = company_config_store
    # Secret-encryption keyring (Tier A ``secrets``).  Encrypts the whole
    # config document on ``PUT /config`` writes and decrypts it when
    # refreshing app.state.  ``None`` when encryption is disabled.
    from crewlet.secrets import KeyringCipher

    app.state.secret_cipher = (
        KeyringCipher.from_config(bootstrap.secrets) if bootstrap is not None else None
    )
    app.state.auth_tokens = auth_tokens
    app.state.auth_disabled = auth_disabled
    app.state.auth_anonymous_read = auth_anonymous_read
    # Whether ApiAuthMiddleware is mounted — the WebSocket handler checks
    # this so it enforces exactly when the HTTP surface does.
    app.state.auth_enabled = auth_enabled
    app.state.runtime = runtime
    # Names the process answering /health; see build_health_envelope.
    app.state.node_id = resolve_node_id(bootstrap)
    app.state.stream = stream
    # The service needs the app to build a snapshot and to attach each
    # role's handle to the spend rollup it pushes.
    stream.bind(app)
    # ``configured`` semantics:
    # - With a ``company_config_store`` wired (Tier B path): starts
    #   False, flipped to True by ``attach_config_refresh`` once the
    #   active revision is primed (or by a later
    #   ``revision_activated`` event).
    # - Without a store (test apps / embedded APIs constructed
    #   without a Tier B config store): always True so /health stays sensible and the
    #   webhook unconfigured early-out doesn't spuriously fire on
    #   apps that have no Tier B model at all.
    app.state.configured = company_config_store is None

    return app


async def attach_config_refresh(
    app: Starlette, *, poll: bool = True
) -> ConfigStateRefresher | None:
    """Prime ``app``'s cached state and attach its config reconciler.

    Called once after the process starts the event_queue and connects to
    the DB.  Idempotent — safe to call multiple times; the second call
    returns the refresher the first one attached.

    ``poll=False`` attaches the broadcast nudge but starts no loop: the
    caller drives :meth:`ConfigStateRefresher.refresh_if_changed` itself.
    That is the merged-node path, where the engine already polls the
    activation pointer and a second loop would only let the two halves of
    one process disagree about which epoch they are on.

    Returns the refresher (``None`` when there is no config store to
    reconcile against, e.g. a programmatic embed).
    """
    existing = getattr(app.state, "config_refresher", None)
    if existing is not None:
        return existing

    store = app.state.company_config_store
    await prime_api_state_from_active(app, store)

    # Roles are now populated (the standalone API learns them from the
    # active revision) — hydrate the live-state projection's baseline so
    # ``/agents`` and the snapshot reflect prior sessions immediately.
    stream = getattr(app.state, "stream", None)
    if stream is not None:
        role_names = [
            r.get("role", "") for r in (app.state.agent_roles or []) if r.get("role")
        ]
        with contextlib.suppress(Exception):
            await stream.hydrate(app.state.event_store, role_names)

    if store is None:
        return None

    database = getattr(app.state, "database", None)
    plane = ConfigPlaneStore(database) if database is not None else None
    refresher = ConfigStateRefresher(app, store, plane)
    # The prime above already reflects the active revision, so record the
    # epoch it satisfied.  Skipping this would make the first tick
    # re-derive state that is already current.
    await refresher.refresh_if_changed()
    await refresher.start(app.state.event_queue, poll=poll)
    app.state.config_refresher = refresher
    return refresher


async def detach_config_refresh(app: Starlette) -> None:
    """Stop the reconciler attached by :func:`attach_config_refresh`."""
    refresher = getattr(app.state, "config_refresher", None)
    if refresher is None:
        return
    app.state.config_refresher = None
    await refresher.stop()
