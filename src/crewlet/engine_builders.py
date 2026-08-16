"""Pure builders that take a :class:`CompanyConfig` (Tier B) and
produce per-subsystem inputs ready for the :class:`Engine`.

Extracted from the monolithic ``Engine.from_config`` so each piece
is independently testable and reusable by :meth:`Engine.apply_config`
when hot-applying a new revision over Pulsar.

Every builder is a pure function: same Tier B in → same outputs out,
no engine state mutation, no I/O.  Side effects (launching MCP
processes, opening HTTP clients) happen later when the engine
threads these values into its lifecycle.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from crewlet._logging import get_logger
from crewlet.config import (
    CompanyConfig,
    _resolve_env_recursive,
    _resolve_env_value,
    create_llm_providers,
    load_extensions_from_config,
    parse_extensions,
)


def _resolved_for_runtime(cfg: CompanyConfig) -> CompanyConfig:
    """Return a copy of ``cfg`` with every ``${VAR}`` reference
    resolved against the current environment.

    The on-disk YAML and the DB-stored payload keep ``${VAR}``
    strings verbatim so ``crewlet config export`` never leaks
    secrets.  The engine builders call this
    when they need real values for provider / transport / MCP env
    construction — once per ``apply_config`` invocation, not per
    field access.
    """
    return CompanyConfig.model_validate(
        _resolve_env_recursive(cfg.model_dump(mode="python", exclude_none=True))
    )


if TYPE_CHECKING:
    from crewlet.config import GitHubConfig, GitLabConfig, PlaneConfig
    from crewlet.db.protocol import StorageBackend
    from crewlet.extensions.protocol import Extension
    from crewlet.notifications.protocol import Transport
    from crewlet.org.models import Organization
    from crewlet.providers.llm.protocol import LLMProvider

logger = get_logger("engine.builders")


def build_llm_providers(cfg: CompanyConfig) -> dict[str, LLMProvider]:
    """Instantiate all LLM providers declared under ``providers.llm``."""
    if not cfg.providers.llm:
        return {}
    return create_llm_providers(cfg.providers)


def build_embedding_provider(cfg: CompanyConfig) -> Any | None:
    """Instantiate the embedding provider from ``providers.embeddings``."""
    if cfg.providers.embeddings is None:
        return None
    from crewlet.config import create_embedding_provider

    return create_embedding_provider(cfg.providers.embeddings)


def build_sandbox_manager(cfg: CompanyConfig) -> Any | None:
    """Build the ``SandboxManager`` from ``providers.sandbox``.

    Returns ``None`` when no sandbox provider is configured (the common
    case) or it is explicitly disabled (``type: none``); the turn engine
    then never selects the sandbox Execute backend regardless of plan or
    role gate.  ``${VAR}`` references in the provider block are resolved
    here (the on-disk / DB payload keeps them verbatim), mirroring
    :func:`build_llm_providers`.
    """
    sb = cfg.providers.sandbox
    if sb is None or sb.type == "none":
        return None

    from crewlet.sandbox import SandboxConfigError, SandboxManager

    sb = _resolved_for_runtime(cfg).providers.sandbox
    assert sb is not None  # type: narrowing — re-read of the same block

    if sb.type == "fake":
        from crewlet.sandbox.fake import FakeCodingAgentRunner, FakeSandboxProvider

        provider: Any = FakeSandboxProvider()
        runners: dict[str, Any] = {
            "claude-code": FakeCodingAgentRunner(name="claude-code"),
            "opencode": FakeCodingAgentRunner(name="opencode"),
        }
    elif sb.type == "e2b":
        from crewlet.sandbox.coding_agents.claude_code import ClaudeCodeRunner
        from crewlet.sandbox.coding_agents.opencode import OpenCodeRunner
        from crewlet.sandbox.e2b import E2BSandboxProvider

        provider = E2BSandboxProvider(
            api_key=sb.api_key, domain=sb.domain, template=sb.template
        )
        runners = {
            "claude-code": ClaudeCodeRunner(),
            "opencode": OpenCodeRunner(),
        }
    elif sb.type == "local":
        # Same runners as E2B: the local backend implements the Sandbox
        # protocol, so the coding-agent runners are unchanged — which is
        # the whole reason the protocol exists.
        from crewlet.sandbox.coding_agents.claude_code import ClaudeCodeRunner
        from crewlet.sandbox.coding_agents.opencode import OpenCodeRunner
        from crewlet.sandbox.local import LocalSandboxProvider

        local = sb.local
        assert local is not None  # guaranteed by SandboxProviderConfig
        provider = LocalSandboxProvider(
            containment=local.containment,
            state_dir=local.state_dir,
            image=local.image,
            runtime=local.runtime,
            network=local.network,
            run_args=list(local.run_args),
        )
        runners = {
            "claude-code": ClaudeCodeRunner(),
            "opencode": OpenCodeRunner(),
        }
        logger.info(
            "local_sandbox_provider_built",
            containment=local.containment,
            image=local.image or "(n/a)",
        )
    else:  # pragma: no cover — Literal already constrains the type
        raise SandboxConfigError(f"unknown sandbox provider type {sb.type!r}")

    logger.info(
        "sandbox_manager_built",
        provider=sb.type,
        default_coding_agent=sb.default_coding_agent,
    )
    # Setup steps come from the UNRESOLVED config, with only files+commands
    # ``${VAR}``-resolved here: a step's env must stay verbatim so it is
    # resolved exactly once — in ``build_sandbox_env`` at launch, like
    # ``role.sandbox.env`` — instead of twice (double resolution mangles a
    # secret whose real value contains a literal ``${...}``).
    from crewlet.config import resolve_setup_step_content

    raw_setup = cfg.providers.sandbox.setup if cfg.providers.sandbox else []
    return SandboxManager(
        provider=provider,
        runners=runners,
        default_coding_agent=sb.default_coding_agent,
        default_template=sb.template,
        default_timeout_s=sb.default_timeout_seconds,
        default_pause_ttl_s=sb.default_pause_ttl_seconds,
        default_setup=[resolve_setup_step_content(s) for s in raw_setup],
    )


def build_extensions(cfg: CompanyConfig) -> list[Extension]:
    """Resolve the extensions list from the config's ``extensions`` block."""
    if not cfg.extensions:
        return []
    ext_configs = parse_extensions(cfg.extensions)
    return load_extensions_from_config(ext_configs)


def build_notification_transports(
    cfg: CompanyConfig, *, storage: StorageBackend | None
) -> list[Transport]:
    """Build the notification-transport list.

    One transport per configured integration — Jira / Confluence / Plane /
    Slack are derived from their own blocks; there is no separate transport
    list in config. Custom transports are passed to ``Engine`` directly
    (see docs/integrations/custom-transports.md) and are appended to
    whatever this builds.

    ``storage`` is required for the Slack transport (it persists
    thread-follow state).  When ``storage`` is not a ``Database``,
    the Slack transport is skipped (test environments).
    """
    cfg = _resolved_for_runtime(cfg)
    transports: list[Transport] = []

    if cfg.integrations.jira is not None:
        from crewlet.notifications.transports.jira import JiraTransport

        transports.append(JiraTransport(cfg.integrations.jira))
        logger.info("jira_transport_created", base_url=cfg.integrations.jira.base_url)

    if cfg.integrations.confluence is not None:
        from crewlet.notifications.transports.confluence import ConfluenceTransport

        transports.append(ConfluenceTransport(cfg.integrations.confluence))
        logger.info(
            "confluence_transport_created",
            base_url=cfg.integrations.confluence.base_url,
        )

    if cfg.integrations.plane is not None and cfg.integrations.plane.enabled:
        from crewlet.notifications.transports.plane import PlaneTransport

        transports.append(PlaneTransport(cfg.integrations.plane))
        logger.info(
            "plane_transport_created",
            url=cfg.integrations.plane.url,
            workspace=cfg.integrations.plane.workspace,
        )

    if cfg.integrations.slack is not None:
        try:
            from crewlet.db.client import Database
            from crewlet.db.slack_thread_follows import SlackThreadFollowRepository
            from crewlet.notifications.transports.slack import SlackTransport

            if not isinstance(storage, Database):
                raise RuntimeError(
                    "Slack thread routing requires a PostgreSQL database"
                )

            thread_follow_repo = SlackThreadFollowRepository(storage)
            transports.append(
                SlackTransport(
                    thread_follow_repo=thread_follow_repo,
                    typing_status_mode=cfg.integrations.slack.typing_status,
                    typing_status_phrases=(
                        cfg.integrations.slack.status_phrases.as_status_phrases()
                    ),
                )
            )
            logger.info(
                "slack_transport_created",
                typing_status=cfg.integrations.slack.typing_status.value,
            )
        except ImportError:
            logger.warning("slack_transport_missing_deps")

    return transports


def build_github_integration(
    cfg: CompanyConfig, org: Organization
) -> GitHubConfig | None:
    """Return the GitHub webhook config (``${VAR}`` refs resolved) when
    enabled, else ``None``.

    GitHub *tools* come from a ``shared: false`` ``http`` server in
    ``mcp_servers`` (per-agent token via ``role.mcp_env.github``); this
    function only surfaces the webhook-side config the engine needs to
    verify inbound GitHub webhooks and register per-agent GitHub
    identities for routing.  Side-effect-free; the engine acts on the
    returned value during ``start()``.
    """
    if cfg.integrations.github is None or not cfg.integrations.github.enabled:
        return None
    cfg = _resolved_for_runtime(cfg)
    gh_roles = [r.name for r in org.all_roles() if r.mcp_env.get("github")]
    logger.info(
        "github_integration_enabled",
        roles=", ".join(gh_roles) or "(none)",
    )
    return cfg.integrations.github


def build_gitlab_integration(
    cfg: CompanyConfig, org: Organization
) -> GitLabConfig | None:
    """Return the GitLab webhook config (``${VAR}`` refs resolved) when
    enabled, else ``None``.

    GitLab *tools* come from a ``shared: false`` server in ``mcp_servers``
    (per-agent service-account PAT via ``role.mcp_env.gitlab``); this
    function only surfaces the webhook-side config the engine needs to
    verify inbound GitLab webhooks and register per-agent GitLab
    identities for routing. Side-effect-free; the engine acts on the
    returned value during ``start()``.
    """
    if cfg.integrations.gitlab is None or not cfg.integrations.gitlab.enabled:
        return None
    cfg = _resolved_for_runtime(cfg)
    gl_roles = [r.name for r in org.all_roles() if r.mcp_env.get("gitlab")]
    logger.info(
        "gitlab_integration_enabled",
        url=cfg.integrations.gitlab.url,
        roles=", ".join(gl_roles) or "(none)",
    )
    return cfg.integrations.gitlab


def build_plane_integration(
    cfg: CompanyConfig, org: Organization
) -> PlaneConfig | None:
    """Return the Plane webhook/read config (``${VAR}`` refs resolved) when
    enabled, else ``None``.

    Plane *tools* come from a ``shared: false`` server in ``mcp_servers``
    (per-agent service-account token via ``role.mcp_env.plane``); this
    function only surfaces the webhook-side config the engine needs to
    verify inbound Plane webhooks and register per-agent Plane
    identities for routing. Side-effect-free; the engine acts on the
    returned value during ``start()``.
    """
    if cfg.integrations.plane is None or not cfg.integrations.plane.enabled:
        return None
    cfg = _resolved_for_runtime(cfg)
    plane = cfg.integrations.plane
    assert plane is not None  # type narrowing — re-read of the same block
    pl_roles = [r.name for r in org.all_roles() if r.mcp_env.get("plane")]
    logger.info(
        "plane_integration_enabled",
        url=plane.url,
        roles=", ".join(pl_roles) or "(none)",
    )
    if not plane.token:
        # Subscriber-routing quality degrades without the engine read credential:
        # no subscriber fan-out, and the project id→identifier cache can
        # only learn from ``project`` webhook payloads.  Surfaced here
        # (engine boot logs) and again at ``PlaneTransport.start()``.
        logger.warning("plane_engine_token_missing", url=plane.url)
    return plane


def resolve_forge_app_id(cfg: CompanyConfig) -> str:
    """Resolve the Forge app id (``${VAR}`` → real value) for FIT JWT
    validation."""
    forge = cfg.integrations.forge_app_id
    return str(_resolve_env_value(forge)) if forge else ""


def resolve_skill_variables(cfg: CompanyConfig) -> dict[str, str]:
    """Resolve ``${VAR}`` references in the operator-defined ``skill_variables``.

    ``skill_variables`` is a generic ``name -> value`` map the engine
    substitutes into tool-skill text wherever a skill writes ``${name}``
    (see :meth:`PromptSkillRegistry.render`).  It lets a static,
    Confluence-authored skill reference a per-org fact the skill body
    cannot know — e.g. the tenant's Confluence/Jira base URL so agents
    share ``https://acme.atlassian.net/wiki/...`` links instead of the
    ``api.atlassian.com`` gateway URLs the MCP tools return.  The engine
    holds no integration-specific knowledge: operators name the variables
    and skills reference them.

    Values support ``${ENV}`` resolution like the rest of Tier B config.
    These values render into LLM prompts (and the event store / dashboard
    like any other prompt content), so treat them as you would a policy
    or backstory string — do not reference secrets.

    Entries that resolve to empty (unset ``${ENV}`` or a blank value) are
    dropped, so a ``${name}`` reference to a missing variable renders as
    the literal ``${name}`` (visibly broken, debuggable) rather than
    silently collapsing into a malformed value such as a hostless URL.
    """
    resolved = {
        name: str(_resolve_env_value(value))
        for name, value in cfg.skill_variables.items()
    }
    return {name: value for name, value in resolved.items() if value}
