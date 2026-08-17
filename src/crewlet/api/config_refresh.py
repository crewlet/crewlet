"""Helper to keep the standalone API process's cached app.state in
sync with the active ``company_config`` revision.

The API process holds only serialised state (``app.state.org_data``,
``agent_roles``, ``tools_data``, ``github_webhook_secret``,
``gitlab_signing_secret``, ``plane_webhook_secret``,
``slack_signing_secrets``, ``forge_app_id``)
and a cached ``configured`` flag.  None of it is
refreshed automatically when a ``PUT /config`` writes a new
revision — without this subscription the dashboard's ``/agents``
and ``/org`` views drift stale until the API process restarts.

The subscription uses consumer group ``api-config`` (distinct from
the engine's ``engine-config``) so both processes' handlers fire on
the same event with independent work to do.
"""

from __future__ import annotations

from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import ConfigRevisionActivated, ConfigRevisionApplied
from crewlet.secrets.resolver import refresh_secret_snapshot

logger = get_logger("api.config_refresh")


def _serialize_agent_roles(payload: dict[str, Any]) -> list[dict[str, Any]]:
    """Flat agent-role view used by ``GET /agents``.

    The dashboard wants ``id`` (stable UUID for routing),
    ``name``, ``role``, ``goal``, ``handle``.  The ``id`` MUST
    match the deterministic ``derive_agent_id(org_name, handle)``
    used by the engine's ``AgentPool`` and the ``agent_diary``
    table — otherwise the dashboard's per-agent memory view links
    to a UUID with no rows.
    """
    from crewlet.db.agents import derive_agent_id
    from crewlet.org.models import slugify

    out: list[dict[str, Any]] = []
    org_name = payload.get("name", "")

    def _emit(role: dict[str, Any]) -> None:
        name = role.get("name", "")
        if not name:
            return
        if role.get("kind") == "human":
            # Human seats are org-chart members, not agents — they
            # appear in GET /org (with kind) but never in the agents
            # list: there is no AgentInstance, no diary, no turns.
            return
        handle = role.get("handle") or slugify(name)
        out.append(
            {
                "id": str(derive_agent_id(org_name, handle)) if org_name else "",
                "name": name,
                "role": name,
                "goal": role.get("goal", ""),
                "handle": handle,
            }
        )

    def _scan_unit(unit: dict[str, Any]) -> None:
        for r in unit.get("roles", []) or []:
            _emit(r)
        for child in unit.get("children", []) or []:
            _scan_unit(child)

    for r in payload.get("roles", []) or []:
        _emit(r)
    for u in payload.get("units", []) or []:
        _scan_unit(u)

    return out


def _slack_signing_secrets(payload: dict[str, Any]) -> dict[str, str]:
    """``{handle: signing_secret}`` for every Slack-enabled agent seat.

    Slack is the one inbound integration whose verification key is
    **per-agent** (one Slack app per seat) rather than one org-wide
    secret, which is why it needs a map here where GitHub / GitLab /
    Plane each need a scalar.  Without it the API process — which can run
    standalone, with no engine reference — has nothing to verify an
    inbound Slack request against at the edge.

    Handles are derived exactly as ``_serialize_agent_roles`` derives
    them, so the map is keyed by the same handle the webhook URL path
    carries.
    """
    from crewlet.config import _resolve_env_value
    from crewlet.org.models import slugify

    secrets: dict[str, str] = {}

    def _emit(role: dict[str, Any]) -> None:
        name = role.get("name", "")
        if not name or role.get("kind") == "human":
            return
        identity = (role.get("integrations") or {}).get("slack") or {}
        raw = identity.get("signing_secret") or ""
        if not raw:
            return
        resolved = str(_resolve_env_value(raw))
        if resolved:
            secrets[role.get("handle") or slugify(name)] = resolved

    def _scan_unit(unit: dict[str, Any]) -> None:
        for role in unit.get("roles", []) or []:
            _emit(role)
        for child in unit.get("children", []) or []:
            _scan_unit(child)

    for role in payload.get("roles", []) or []:
        _emit(role)
    for unit in payload.get("units", []) or []:
        _scan_unit(unit)

    return secrets


def _serialize_org_data(payload: dict[str, Any]) -> dict[str, Any]:
    """Minimal org dict for ``GET /org`` consumers (the dashboard)."""
    return {
        "name": payload.get("name", ""),
        "mission": payload.get("mission", ""),
        "vision": payload.get("vision", ""),
        "policies": payload.get("policies", []) or [],
        "units": payload.get("units", []) or [],
        "roles": payload.get("roles", []) or [],
    }


def _materialise_payload(
    payload: dict[str, Any],
) -> tuple[Any, Any] | tuple[None, None]:
    """Validate the payload as :class:`CompanyConfig` + build its
    :class:`Organization`, once.

    Both are cached on ``app.state`` (via :func:`_apply_payload_to_app`)
    so /schedules / /org / /agents read from a single materialised
    object — every per-helper re-validation otherwise pays for the same
    Pydantic + org-model validator pass.  Returns ``(None, None)`` on
    any validation failure so a partially-wired bootstrap revision
    can't 500 the dashboard.
    """
    try:
        from crewlet.config import CompanyConfig, config_to_organization
    except ImportError:
        return None, None
    try:
        cfg = CompanyConfig.model_validate(payload)
        org = config_to_organization(cfg)
        return cfg, org
    except Exception as exc:
        logger.warning("payload_materialise_failed", error=str(exc))
        return None, None


def _serialize_schedules(cfg: Any, org: Any) -> list[dict[str, Any]]:
    """Project an already-materialised config / org into the dashboard
    ``/schedules`` view via :func:`describe_schedules`."""
    if cfg is None or org is None:
        return []
    try:
        from crewlet.schedule import describe_schedules
    except ImportError:
        return []
    try:
        return describe_schedules(org, default_timezone=cfg.scheduling.default_timezone)
    except Exception as exc:
        logger.warning("schedules_serialize_failed", error=str(exc))
        return []


def _tools_data_from_payload(payload: dict[str, Any]) -> list[dict[str, Any]]:
    """Best-effort tools list derived from the YAML payload alone.

    The standalone API process has no live engine to query, so this
    fallback enumerates what the DB row says about tooling:

    - The ``reflect_and_persist`` / ``refresh_memory`` / ``mark_onboarded``
      learning builtins (registered unconditionally when ``learning.*``
      is enabled).
    - One entry per shared MCP server listed under ``mcp_servers``.

    Per-role MCP tools (atlassian, slack, github) are NOT visible here —
    they're derived from the ``jira:`` / ``confluence:`` / ``slack:`` /
    ``github:`` blocks and only resolved at engine boot.  The embedded
    API path (``app.state.engine`` is set) calls
    :meth:`Engine._build_tools_data` instead, which sees every live
    tool.
    """
    builtin_tool_names = [
        (
            "reflect_and_persist",
            "Capture a personal-context fact in agent memory with optional TTL",
        ),
        (
            "refresh_memory",
            "Re-read personal memories filtered by a context hint",
        ),
        ("mark_onboarded", "Mark an onboarding step complete"),
        (
            "load_tool_skill",
            "Fetch the rich body of a Tool Skill mid-task",
        ),
    ]
    out: list[dict[str, Any]] = [
        {"name": name, "description": desc, "source": "builtin"}
        for name, desc in builtin_tool_names
    ]
    for mcp_srv in payload.get("mcp_servers", []) or []:
        srv_name = mcp_srv.get("name", "mcp")
        out.append(
            {
                "name": srv_name,
                "description": f"MCP server: {srv_name}",
                "source": f"mcp:{srv_name}",
            }
        )
    return out


def _apply_payload_to_app(app: Any, payload: dict[str, Any]) -> None:
    """Refresh every app.state field derived from the Tier B payload.

    Called from both ``cmd_api`` boot time (after store.get_active())
    and from the Pulsar handler below.

    Tools view: when the embedded API has a live engine reference
    (``app.state.engine``), the engine's ``_build_tools_data()`` is
    the source of truth — it enumerates the live tool registry +
    per-role MCP tools (atlassian, slack, github), which the payload
    alone can't reveal.  Standalone API processes (no engine handle)
    fall back to :func:`_tools_data_from_payload`.
    """
    # Materialise once; the engine has already validated this payload
    # upstream of revision_activated, but the standalone API process
    # needs its own copy for /schedules + /org + the webhook secret
    # resolution below.  ``cfg`` / ``org`` may be ``None`` if validation
    # fails (partial bootstrap revision) -- helpers handle that.
    from crewlet.secrets import SecretCipherError, load_config, redact_payload

    cipher = getattr(app.state, "secret_cipher", None)
    # ``plain`` decrypts the stored config to its full structure. When the
    # config is stored encrypted but this process has no keyring (or the
    # sealing key is missing), fail closed: log a precise, non-secret error
    # and leave the cached state unconfigured rather than crash the API
    # process (a standalone `crewlet run api` started without Tier A
    # `secrets`).
    try:
        plain = load_config(payload, cipher)
    except SecretCipherError as exc:
        logger.error(
            "config_refresh_decrypt_failed",
            error=str(exc),
            hint="set Tier A `secrets.keys` so this process can read the "
            "encrypted config",
        )
        app.state.configured = False
        return
    # ``readable`` is the display view — structure visible, secrets masked.
    # Redact the already-decrypted ``plain`` (one decrypt pass, not two);
    # the /org view carries units + roles with per-agent mcp_env / Slack
    # secrets, so it must never receive plaintext secrets.
    readable = redact_payload(plain)
    cfg, org = _materialise_payload(plain)

    app.state.org_data = _serialize_org_data(readable)
    app.state.agent_roles = _serialize_agent_roles(readable)
    app.state.schedules_data = _serialize_schedules(cfg, org)
    engine = getattr(app.state, "engine", None)
    if engine is not None and hasattr(engine, "_build_tools_data"):
        try:
            app.state.tools_data = engine._build_tools_data()
        except Exception:
            # Defensive: engine hasn't finished spawning yet (the
            # cascade runs after this handler returns).  Fall back to
            # the thin payload-derived view so /tools doesn't 500.
            app.state.tools_data = _tools_data_from_payload(readable)
    else:
        app.state.tools_data = _tools_data_from_payload(readable)
    _set_webhook_secrets(app, plain)
    app.state.configured = True


def _set_webhook_secrets(app: Any, payload: dict[str, Any]) -> None:
    """Resolve the webhook HMAC secret and Forge JWT audience onto
    ``app.state``.

    ``payload`` is the fully-decrypted config (the whole document is
    decrypted at the read boundary before this runs), so each webhook
    secret (GitHub / GitLab / Plane) is a plaintext value or a ``${VAR}``
    reference resolved from the environment — never ciphertext.  The
    Forge app id is likewise ``${VAR}``-resolved.
    """
    from crewlet.config import _resolve_env_value

    integrations = payload.get("integrations") or {}
    raw_secret = (integrations.get("github") or {}).get("webhook_secret") or ""
    raw_forge = integrations.get("forge_app_id") or ""
    gitlab = integrations.get("gitlab") or {}
    raw_gl_signing = gitlab.get("signing_secret") or ""
    plane = integrations.get("plane") or {}
    raw_plane = plane.get("webhook_secret") or ""

    app.state.github_webhook_secret = str(_resolve_env_value(raw_secret))
    app.state.forge_app_id = str(_resolve_env_value(raw_forge))
    app.state.gitlab_signing_secret = str(_resolve_env_value(raw_gl_signing)) or None
    app.state.plane_webhook_secret = str(_resolve_env_value(raw_plane)) or None
    app.state.slack_signing_secrets = _slack_signing_secrets(payload)


async def subscribe_config_refresh(app: Any, store: Any, event_queue: Any) -> None:
    """Subscribe the API process to ``crewlet.config.revision_activated``
    and ``crewlet.config.revision_applied``.

    Consumer group ``api-config``; on every ``revision_activated``,
    reads the activated revision and updates ``app.state`` fields the
    dashboard consumes.  On every ``revision_applied`` with
    ``status=error``, checks whether the prior state was unconfigured
    and (in that case) flips ``app.state.configured`` back to False so
    ``/health`` reports honestly and the webhook unconfigured
    early-out keeps firing.

    Idempotent: tracks an ``_api_config_refresh_attached`` flag on
    ``app.state`` so callers can invoke this safely from both the
    embedded (engine + API in same process) and standalone API paths
    without double-subscribing.
    """
    if event_queue is None or store is None:
        logger.debug("skip_config_refresh_subscription", reason="missing_deps")
        return
    if getattr(app.state, "_api_config_refresh_attached", False):
        logger.debug("skip_config_refresh_subscription", reason="already_attached")
        return

    async def _on_activated(event: ConfigRevisionActivated) -> None:
        from uuid import UUID

        try:
            revision_id = UUID(event.revision_id)
        except (ValueError, TypeError):
            logger.warning(
                "api_revision_activated_invalid_id",
                revision_id=event.revision_id,
            )
            return
        revision = await store.get_revision(revision_id)
        if revision is None:
            # Engine handler logs its own warning; ours is a no-op.
            return
        # Same rule as the engine's apply path: re-read the secret store
        # before resolving this revision's ${VAR}s, so a rotated webhook
        # secret takes effect here without an API restart.
        await refresh_secret_snapshot()
        _apply_payload_to_app(app, revision.payload)
        logger.info(
            "api_state_refreshed_from_revision",
            revision_id=event.revision_id,
        )

    async def _on_applied(event: ConfigRevisionApplied) -> None:
        if event.status == "ok":
            # Engine finished the spawn cascade — tool_registry +
            # per-role MCP tools (atlassian, slack, github) are now
            # populated.  In the embedded path the API has a live
            # engine reference; re-derive ``tools_data`` from it so
            # ``GET /tools`` reflects every discovered MCP tool, not
            # just the thin payload-derived list.
            engine = getattr(app.state, "engine", None)
            if engine is not None and hasattr(engine, "_build_tools_data"):
                try:
                    app.state.tools_data = engine._build_tools_data()
                    logger.info(
                        "api_tools_data_refreshed_from_engine",
                        revision_id=event.revision_id,
                        tool_count=len(app.state.tools_data),
                    )
                except Exception as exc:
                    logger.warning(
                        "api_tools_data_refresh_failed",
                        revision_id=event.revision_id,
                        error=str(exc),
                    )
            return
        # Engine failed mid-apply and rolled back.  If there's no
        # active revision in the DB the engine is unconfigured; flip
        # ``configured`` back so /health and the webhook drop are
        # honest.  When the prior state was already configured (i.e.
        # this was a second-or-later apply that failed), leave
        # ``configured=True`` — the engine rolled back to the prior
        # working state.
        active = await store.get_active()
        if active is None:
            app.state.configured = False
            logger.warning(
                "api_configured_reset_after_apply_error",
                revision_id=event.revision_id,
                error=event.error,
            )

    await event_queue.subscribe(
        topic="crewlet.config.revision_activated",
        group="api-config",
        handler=_on_activated,
    )
    await event_queue.subscribe(
        topic="crewlet.config.revision_applied",
        group="api-config",
        handler=_on_applied,
    )
    app.state._api_config_refresh_attached = True
    logger.info("api_subscribed_to_revision_events")


async def prime_api_state_from_active(app: Any, store: Any) -> None:
    """Pull the active revision (if any) at app construction so
    ``GET /agents`` / ``/org`` don't return empty on first request
    when the engine was already configured before the API started.
    """
    if store is None:
        return
    revision = await store.get_active()
    if revision is None:
        app.state.configured = False
        return
    await refresh_secret_snapshot()
    _apply_payload_to_app(app, revision.payload)
