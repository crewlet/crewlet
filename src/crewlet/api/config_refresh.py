"""Keeps an API surface's cached ``app.state`` in sync with the active
``company_config`` revision.

The API holds only serialised state (``app.state.org_data``,
``agent_roles``, ``tools_data``, ``github_webhook_secret``,
``gitlab_signing_secret``, ``plane_webhook_secret``,
``confluence_webhook_secret``, ``jira_webhook_secret``,
``slack_signing_secrets``,
``forge_app_id``)
and a cached ``configured`` flag.  None of it refreshes on its own when
a ``PUT /config`` writes a new revision — without this the dashboard's
``/agents`` and ``/org`` views drift stale until the process restarts,
and, far worse, a rotated webhook signing secret is never picked up, so
inbound HMAC verification fails against every delivery.

**This used to be a durable competing-consumer subscription** (group
``api-config``).  With more than one API process that meant exactly one
of them refreshed and the rest served the previous company forever —
the same fan-out bug the engine had under ``engine-config``, and fixing
only one of the two would have left the two halves of a node with
different rotation semantics.

The replacement is a reconciler over the activation pointer
(:mod:`crewlet.db.config_plane`):

* :meth:`ConfigStateRefresher.refresh_if_changed` is one tick — read the
  pointer, and re-derive ``app.state`` from the active revision when the
  epoch moved.  Because it re-reads rather than replays, a process that
  was down for an activation catches up on its next tick.
* :meth:`ConfigStateRefresher.run` is the loop, used by the standalone
  API process.  A merged node does **not** start it: the engine drives
  ``refresh_if_changed`` from its own reconcile tick, so one process
  polls the pointer once rather than twice and its two halves can never
  disagree about which epoch they are on.

A broadcast ``revision_activated`` nudge shortens the common case; it is
never load-bearing (an ephemeral stream consumer starts at the latest
message, so anything published while a process reconnects is gone).
"""

from __future__ import annotations

import asyncio
import contextlib
from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import ConfigRevisionApplied
from crewlet.secrets.resolver import refresh_secret_snapshot
from crewlet.tools.registry import BUILTIN_ORIGIN, extension_origin, mcp_origin

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
        placement = role.get("placement") or {}
        out.append(
            {
                "id": str(derive_agent_id(org_name, handle)) if org_name else "",
                "name": name,
                "role": name,
                "goal": role.get("goal", ""),
                "handle": handle,
                # The configured cap (0 = unlimited).  Carried so the
                # agent page can name a seat's budget with no engine
                # attached -- the live meter is engine-only, the cap is
                # config and must render either way.
                "token_budget": int(role.get("token_budget", 0) or 0),
                # Where the seat may run, for the fleet view. Derived
                # here so it inherits this function's handle derivation
                # and its human-seat exclusion — a second walk over the
                # payload would have to repeat both, and a seat whose
                # handle it derived differently is a seat the fleet view
                # reports as unplaceable forever.
                "placement": {
                    "node": str(placement.get("node") or ""),
                    "labels": {
                        str(k): str(v)
                        for k, v in (placement.get("labels") or {}).items()
                    },
                }
                if isinstance(placement, dict) and placement
                else {},
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
        # The org-wide token cap (0 = unlimited).  Per-role caps ride
        # through inside ``units`` / ``roles``, which are passed
        # verbatim; the org one is a top-level field and was being
        # dropped.
        "token_budget": int(payload.get("token_budget", 0) or 0),
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
    - One entry per configured extension.

    The MCP-server and extension entries are surface markers, not tools:
    what each contributes is known only once a process has actually
    launched the server or imported the module.  They are listed anyway
    because the alternative is a Tools screen that shows four builtins
    and no sign that the company has any other tool surface at all — an
    operator reading it cannot tell a company with ten MCP servers and
    an extension from one with none.

    Per-role MCP tools (atlassian, slack, github) are NOT visible here —
    they're derived from the ``jira:`` / ``confluence:`` / ``slack:`` /
    ``github:`` blocks and only resolved at engine boot.  A process with
    a :class:`~crewlet.api.runtime.NodeRuntime` registered asks it
    instead, and sees every live tool.
    """
    # This list is a CLAIM about what agents can call, and it is read on
    # a node that has no engine to ask — so it has to answer from the
    # payload rather than from a constant. The learning tools below are
    # registered by ``Engine`` only when ``learning.reflect.enabled``
    # holds AND the storage is a real Database (``AgentDiary`` needs the
    # ``agent_diary`` table). Advertising them unconditionally told an
    # operator on a standalone API, or on a company with learning off,
    # that four tools were available which no agent could ever call.
    learning = payload.get("learning") or {}
    reflect = learning.get("reflect") or {}
    # Absent means the default, which is on — the same reading
    # ``Engine`` takes.
    learning_on = learning.get("enabled", True) and reflect.get("enabled", True)

    builtin_tool_names: list[tuple[str, str]] = [
        (
            "load_tool_skill",
            "Fetch the rich body of a Tool Skill mid-task",
        ),
    ]
    if learning_on:
        builtin_tool_names[:0] = [
            (
                "reflect_and_persist",
                "Capture a personal-context fact in agent memory with optional TTL",
            ),
            (
                "refresh_memory",
                "Re-read personal memories filtered by a context hint",
            ),
            ("mark_onboarded", "Mark an onboarding step complete"),
        ]
    out: list[dict[str, Any]] = [
        {"name": name, "description": desc, "source": BUILTIN_ORIGIN}
        for name, desc in builtin_tool_names
    ]
    for mcp_srv in payload.get("mcp_servers", []) or []:
        srv_name = mcp_srv.get("name", "mcp")
        out.append(
            {
                "name": srv_name,
                "description": f"MCP server: {srv_name}",
                "source": mcp_origin(srv_name),
            }
        )
    for entry in payload.get("extensions", []) or []:
        # ``extensions:`` entries are single-key ``{name: {settings}}``
        # maps; anything else was rejected by ``parse_extensions``.
        if not isinstance(entry, dict) or len(entry) != 1:
            continue
        ext_name = next(iter(entry))
        out.append(
            {
                "name": ext_name,
                "description": f"Extension: {ext_name}",
                "source": extension_origin(ext_name),
            }
        )
    return out


def _apply_payload_to_app(app: Any, payload: dict[str, Any]) -> None:
    """Refresh every app.state field derived from the Tier B payload.

    Called from both ``cmd_api`` boot time (after store.get_active())
    and from the Pulsar handler below.

    Tools view: when a :class:`~crewlet.api.runtime.NodeRuntime` is
    registered (an engine in this process), it is the source of truth —
    it enumerates the live tool registry + per-role MCP tools
    (atlassian, slack, github), which the payload alone can't reveal.
    Without one, or before the spawn cascade has run, this falls back to
    :func:`_tools_data_from_payload`.
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
    app.state.integrations_data = _serialize_integrations(readable)
    # ``NodeRuntime.tools_data`` returns [] rather than raising when the
    # spawn cascade hasn't run yet (it runs after this handler returns),
    # so an empty result is the signal to use the payload-derived view.
    runtime = getattr(app.state, "runtime", None)
    live_tools = runtime.tools_data() if runtime is not None else []
    app.state.tools_data = live_tools or _tools_data_from_payload(readable)
    _set_webhook_secrets(app, plain)
    app.state.configured = True
    _broadcast_config_change(app)


def _broadcast_config_change(app: Any) -> None:
    """Tell connected dashboards the org they are showing just changed.

    A revision activation rewrites the org chart, the seat list, the tool
    surface, and the schedules under every open tab.  Nothing told them:
    a dashboard picked the new shape up only if the operator happened to
    reload, so a tab could sit for hours showing seats that no longer
    exist.  These are the same envelopes the snapshot carries, so a
    client applies them with the code it already has.
    """
    stream = getattr(app.state, "stream", None)
    if stream is None:
        return
    from crewlet.api.routes.agents import agents_payload
    from crewlet.api.routes.org import schedule_projection

    # ``seats``, not ``agents``: an ``agents`` envelope is a set of
    # CHANGED overlays and a client merges it per role, which can add and
    # update but never remove. A revision that deletes a role has to be
    # able to take its card off the screen, so the whole list goes as its
    # own kind and replaces.
    for kind, data in (
        ("org", app.state.org_data),
        ("tools", app.state.tools_data),
        ("seats", agents_payload(app)),
        ("schedules", {"schedules": schedule_projection(app)}),
    ):
        try:
            stream.push(kind, data)
        except Exception:  # pragma: no cover - defensive
            logger.exception("config_change_broadcast_failed", kind=kind)


def _serialize_integrations(payload: dict[str, Any]) -> dict[str, dict[str, Any]]:
    """The SHAPE of each configured integration, never its credentials.

    Only the three fields an operator needs to recognise a surface —
    whether it is on, which host or workspace it points at, and nothing
    else. Built from the already-redacted payload, and then narrowed
    again here: a block copied wholesale would carry whatever key a
    future integration adds, and the redactor's key-name heuristic is a
    denylist, so the safe default is to name what goes out.
    """
    integrations = payload.get("integrations") or {}
    out: dict[str, dict[str, Any]] = {}
    for key, block in integrations.items():
        if not isinstance(block, dict):
            continue
        out[str(key)] = {
            "enabled": block.get("enabled", True) is not False,
            "url": str(block.get("url") or block.get("base_url") or ""),
            "workspace": str(
                block.get("workspace")
                or block.get("team")
                or block.get("project")
                or ""
            ),
        }
    return out


def _set_webhook_secrets(app: Any, payload: dict[str, Any]) -> None:
    """Resolve the webhook HMAC secret and Forge JWT audience onto
    ``app.state``.

    ``payload`` is the fully-decrypted config (the whole document is
    decrypted at the read boundary before this runs), so each webhook
    secret (GitHub / GitLab / Plane / Confluence / Jira) is a plaintext
    value or
    a ``${VAR}`` reference resolved from the environment — never
    ciphertext.  The Forge app id is likewise ``${VAR}``-resolved.
    """
    from crewlet.config import _resolve_env_value

    integrations = payload.get("integrations") or {}
    raw_secret = (integrations.get("github") or {}).get("webhook_secret") or ""
    raw_forge = integrations.get("forge_app_id") or ""
    gitlab = integrations.get("gitlab") or {}
    raw_gl_signing = gitlab.get("signing_secret") or ""
    plane = integrations.get("plane") or {}
    raw_plane = plane.get("webhook_secret") or ""
    confluence = integrations.get("confluence") or {}
    raw_confluence = confluence.get("webhook_secret") or ""
    jira = integrations.get("jira") or {}
    raw_jira = jira.get("webhook_secret") or ""

    app.state.github_webhook_secret = str(_resolve_env_value(raw_secret))
    app.state.forge_app_id = str(_resolve_env_value(raw_forge))
    app.state.gitlab_signing_secret = str(_resolve_env_value(raw_gl_signing)) or None
    app.state.plane_webhook_secret = str(_resolve_env_value(raw_plane)) or None
    app.state.confluence_webhook_secret = (
        str(_resolve_env_value(raw_confluence)) or None
    )
    app.state.jira_webhook_secret = str(_resolve_env_value(raw_jira)) or None
    app.state.slack_signing_secrets = _slack_signing_secrets(payload)


class ConfigStateRefresher:
    """Reconciles ``app.state`` against the activation pointer.

    One tick is :meth:`refresh_if_changed`; :meth:`run` is the loop a
    standalone API process owns.  A merged node drives the tick from the
    engine's reconcile loop instead — see the module docstring.
    """

    def __init__(self, app: Any, store: Any, plane: Any) -> None:
        self._app = app
        self._store = store
        self._plane = plane
        self._epoch = 0
        self._nudge = asyncio.Event()
        self._task: asyncio.Task[Any] | None = None
        self._unsubscribes: list[Any] = []

    @property
    def epoch(self) -> int:
        """The activation epoch ``app.state`` currently reflects."""
        return self._epoch

    def nudge(self) -> None:
        """Ask the loop to reconcile now instead of at the next tick."""
        self._nudge.set()

    async def refresh_if_changed(self) -> bool:
        """Re-derive ``app.state`` when the activation pointer moved.

        Returns whether anything was refreshed.  Cheap and safe to call
        on every tick: the common path is one indexed ``MAX(epoch)``.

        Note this follows the **activation pointer**, not the local
        engine's apply outcome, and deliberately so.  These fields decide
        whether inbound HMAC verification succeeds, and refusing to
        accept deliveries because an apply failed would drop events the
        queue could otherwise have held.  Keeping a stale node from
        *processing* work is the posture gate's job
        (:mod:`crewlet.db.config_plane`), not the receiver's.
        """
        if self._plane is None or self._store is None:
            return False
        target = await self._plane.target()
        if target is None or target.epoch <= self._epoch:
            return False

        revision = await self._store.get_revision(target.revision_id)
        if revision is None:
            logger.error(
                "api_activation_revision_missing",
                epoch=target.epoch,
                revision_id=str(target.revision_id),
            )
            return False
        # Same rule as the engine's apply path: re-read the secret store
        # before resolving this revision's ${VAR}s, so a rotated webhook
        # secret takes effect here without an API restart.
        await refresh_secret_snapshot()
        _apply_payload_to_app(self._app, revision.payload)
        self._epoch = target.epoch
        logger.info(
            "api_state_refreshed_from_revision",
            epoch=target.epoch,
            revision_id=str(target.revision_id),
        )
        return True

    async def run(self) -> None:
        """Poll the activation pointer until cancelled."""
        from crewlet.db.config_plane import reconcile_delay

        while True:
            try:
                with contextlib.suppress(TimeoutError):
                    await asyncio.wait_for(
                        self._nudge.wait(), timeout=reconcile_delay()
                    )
                self._nudge.clear()
                await self.refresh_if_changed()
            except asyncio.CancelledError:
                raise
            except Exception:
                logger.exception("api_config_refresh_tick_failed")

    async def start(self, event_queue: Any, *, poll: bool) -> None:
        """Attach the broadcast nudge and, when ``poll``, the loop.

        Without an activation log there is nothing a poll could observe,
        so the loop is skipped regardless of ``poll`` — the
        ``revision_applied`` handlers below still attach, since they are
        what keeps ``configured`` honest on such a process.
        """
        if self._plane is None:
            poll = False
        if event_queue is not None:
            self._unsubscribes.append(
                await event_queue.subscribe_stream(
                    "crewlet.config.revision_activated",
                    self._on_activated,
                )
            )
            self._unsubscribes.append(
                await event_queue.subscribe_stream(
                    "crewlet.config.revision_applied",
                    self._on_applied,
                )
            )
        if poll:
            self._task = asyncio.create_task(self.run())

    async def stop(self) -> None:
        if self._task is not None:
            self._task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._task
            self._task = None
        for unsubscribe in self._unsubscribes:
            with contextlib.suppress(Exception):
                await unsubscribe()
        self._unsubscribes.clear()

    # -- broadcast handlers (nudges + local tool refresh) --------------

    async def _on_activated(self, topic: str, event: Any) -> None:
        self.nudge()

    async def _on_applied(self, topic: str, event: ConfigRevisionApplied) -> None:
        """React to an apply outcome.

        Broadcast, so this fires for *every* node's outcome, not only the
        local one.  Both branches are safe under that: the tools refresh
        reads this process's own runtime (idempotent), and the
        ``configured`` reset is decided by a fleet-global query rather
        than by whose event it was.
        """
        if event.status == "ok":
            # The spawn cascade finished — tool_registry + per-role MCP
            # tools (atlassian, slack, github) are now populated.  With a
            # live runtime in this process, re-derive ``tools_data`` from
            # it so ``GET /tools`` reflects every discovered MCP tool,
            # not just the thin payload-derived list.
            runtime = getattr(self._app.state, "runtime", None)
            if runtime is None:
                return
            try:
                live_tools = runtime.tools_data()
                if live_tools:
                    self._app.state.tools_data = live_tools
                logger.info(
                    "api_tools_data_refreshed_from_engine",
                    revision_id=event.revision_id,
                    tool_count=len(self._app.state.tools_data),
                )
            except Exception as exc:
                logger.warning(
                    "api_tools_data_refresh_failed",
                    revision_id=event.revision_id,
                    error=str(exc),
                )
            return
        # An apply failed and rolled back.  If there's no active revision
        # in the DB the company is unconfigured; flip ``configured`` back
        # so /health and the webhook drop are honest.  When a revision is
        # still active this was a second-or-later apply that failed and
        # the node rolled back to the prior working state — leave it.
        if self._store is None:
            return
        active = await self._store.get_active()
        if active is None:
            self._app.state.configured = False
            logger.warning(
                "api_configured_reset_after_apply_error",
                revision_id=event.revision_id,
                error=event.error,
            )


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
