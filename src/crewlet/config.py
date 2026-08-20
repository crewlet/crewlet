"""Config loading — parse YAML company configs into validated models."""

from __future__ import annotations

import os
import re
from collections.abc import Sequence
from pathlib import Path
from typing import Annotated, Any, Literal
from uuid import uuid4
from zoneinfo import ZoneInfo

import yaml
from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

from crewlet._logging import get_logger
from crewlet.env_refs import (
    ENV_VAR_PATTERN,
    env_var_reference,
    has_env_reference,
)
from crewlet.notifications.typing_status import StatusPhrases, WorkingStatusMode
from crewlet.org.hierarchy import get_effective_lead
from crewlet.org.models import (
    HumanContact,
    Organization,
    OrgUnit,
    Role,
    RoleKind,
    RolePlacementConfig,
    RoleSandboxConfig,
    Schedule,
)
from crewlet.providers.llm.cli_profiles import CLIAgentName
from crewlet.sandbox.setup import SandboxSetupStep
from crewlet.seat.placement import DEFAULT_NODE_ROLES, NodeRole
from crewlet.secrets.resolver import lookup_secret, resolve_env
from crewlet.tools.capabilities import ToolAnnotations

logger = get_logger("config")


ReasoningEffort = Literal["low", "medium", "high", "max"]

#: Built-in LLM provider implementations.  Kept in lockstep with the
#: dispatch in :func:`create_llm_providers` — see the test that asserts
#: every member constructs.
#:
#: ``cli-agent`` is the odd one out: instead of an HTTP endpoint it
#: drives a locally installed coding CLI (``claude``, ``codex``,
#: ``gemini``, …) authenticated by the operator's **subscription**
#: rather than a metered API key.  See
#: ``docs/concepts/subscription-llm-backends.md``.
LLMProviderType = Literal["openai", "anthropic", "openai-compatible", "cli-agent"]

#: Built-in embedding implementations (both served by the OpenAI client;
#: point at any OpenAI-compatible endpoint with ``base_url``).
EmbeddingProviderType = Literal["openai", "openai-compatible"]

#: A ``skill_variables`` key.  Must be a substitution identifier so the
#: operator key-space is provably identical to the ``${name}`` render
#: grammar in ``crewlet.agent.skills.models`` — a key like ``base-url``
#: would never be substituted.  Declared as a constrained key type (not
#: only a model validator) so the rule reaches the generated JSON Schema
#: as ``propertyNames``.
SkillVariableKey = Annotated[str, Field(pattern=r"^[A-Za-z_][A-Za-z0-9_]*$")]


class AuthoredModel(BaseModel):
    """Base for the founder-authored Tier B YAML models.

    Two rules that only make sense for hand-written (or AI-written)
    YAML, as opposed to the runtime models these are transformed into:

    **Unknown keys are errors.**  Every field below has a default, so a
    silently-ignored ``backstroy:`` would spawn the seat with an empty
    backstory and no diagnostic.  ``extra="forbid"`` turns a typo into a
    validation failure naming the exact path.

    **A valueless key means "unset", not ``None``.**  YAML parses
    ``lead:`` / ``roles:`` / ``manages:`` written with nothing under
    them as ``None``, which would otherwise fail as a type error
    against ``str`` / ``list[...]``.  That shape is ordinary authoring
    for "empty" — and what ``POST /config/roles`` can produce — so
    ``None`` values are dropped and the field default applies.  Fields
    whose default *is* ``None`` (``learning_enabled``, ``contact``,
    ``sandbox``) land on the same value either way.
    """

    model_config = ConfigDict(extra="forbid")

    @model_validator(mode="before")
    @classmethod
    def _valueless_key_is_unset(cls, data: Any) -> Any:
        if isinstance(data, dict):
            return {k: v for k, v in data.items() if v is not None}
        return data


class CredentialCooldownConfig(BaseModel):
    """TTL policy for the LLM credential pool.

    Each provider's pool keys can be marked cooled-down by error class
    -- a 429 typically clears in an hour, while a 401 / 403 often
    clears in minutes after a token refresh."""

    model_config = ConfigDict(extra="forbid")

    rate_limit_seconds: int = 3600
    auth_seconds: int = 300

    @model_validator(mode="after")
    def _validate_bounds(self) -> CredentialCooldownConfig:
        if not (60 <= self.rate_limit_seconds <= 86400):
            raise ValueError(
                "rate_limit_seconds must be between 60 and 86400 "
                f"(got {self.rate_limit_seconds})"
            )
        if not (60 <= self.auth_seconds <= 86400):
            raise ValueError(
                f"auth_seconds must be between 60 and 86400 (got {self.auth_seconds})"
            )
        return self


class CLIAgentAuthConfig(BaseModel):
    """How a ``cli-agent`` provider authenticates.

    See ``docs/concepts/subscription-llm-backends.md``; the login itself
    is performed by ``crewlet llm login``, not by the engine.
    """

    model_config = ConfigDict(extra="forbid")

    mode: Literal["subscription", "api-key", "inherit-env"] = "subscription"
    """``subscription`` (the default and the point of this backend) uses
    the operator's own CLI login — a token in the secret store, or the
    credential files ``crewlet llm login`` wrote.  ``api-key`` puts
    ``api_keys[0]`` into the CLI's key variable, billing the metered
    account.  ``inherit-env`` forwards whichever of the two variables
    the engine's own environment has — an escape hatch for an unusual
    CLI, and the only mode that lets a host credential reach the child.

    The default is deliberately *not* ``inherit-env``: a subscription
    backend that silently picked up a stray ``ANTHROPIC_API_KEY`` would
    bill the metered account while the operator believed they were on a
    flat-rate plan."""

    token: str = ""
    """``${VAR}`` reference to a long-lived subscription token (for
    Claude Code, what ``claude setup-token`` mints into
    ``CLAUDE_CODE_OAUTH_TOKEN``).  Empty falls back to the profile's own
    token variable in the secret store / environment.  This is the best
    headless path where a CLI offers one: no credential files to sync,
    no refresh-token rotation, and it survives an ephemeral container."""

    credential_bundle: str = ""
    """``${VAR}`` reference to a bundle exported by ``crewlet llm
    export`` — the CLI's credential files as one blob.  Restored into
    the provider's credential directory at boot when that directory is
    empty, so a fresh container comes up already logged in.  Empty falls
    back to the conventional name
    ``CREWLET_LLM_CLI_<KEY>_CREDENTIALS``."""


class CLIAgentConfig(BaseModel):
    """The ``cli-agent`` block of a ``providers.llm`` entry."""

    model_config = ConfigDict(extra="forbid")

    agent: CLIAgentName = "claude-code"
    """Which CLI to drive.  ``custom`` ships no defaults at all — supply
    everything under :attr:`overrides`."""

    state_dir: str = ""
    """Where this provider keeps its credential directory and its
    per-seat homes.  Empty uses ``$CREWLET_LLM_CLI_HOME/<key>``, falling
    back to ``~/.crewlet/llm-cli/<key>``.  Point it at a persistent
    volume when the engine runs in an ephemeral container, or the login
    is lost on every restart.

    **Entries pointing at the same directory share one login** — and one
    set of per-seat homes, and one generation, so a call on one entry
    never prunes a live call on another.  That is how per-phase models
    work off a single ``crewlet llm login``: an ``opus`` entry for Plan
    and a ``sonnet`` entry for Execute, both naming this directory.  The
    per-key default is the opposite behaviour on purpose, so unrelated
    providers never collide; entries that *should* share have to say so.
    Sharing requires the same :attr:`agent` — two CLIs disagree about
    which files are credentials and which are memory, and
    :class:`ProvidersConfig` rejects that pairing."""

    timeout_seconds: float = 300.0
    """Wall-clock cap on one CLI invocation.

    Separate from :attr:`LLMProviderConfig.timeout_seconds` because the
    transports are not comparable: that one is an HTTP client timeout,
    while this covers a process launch (a Node runtime costs seconds
    before the first byte), the model call, and the CLI's own internal
    retry/backoff.  120 s — right for HTTP — cuts off legitimate long
    reasoning turns here, so the default is 2.5x that.  On breach the
    process group is terminated and the call is reported as
    ``TIMEOUT``, which the role's fallback chain retries."""

    max_concurrent: int = 4
    """How many CLI processes this provider runs at once.

    Each is a full Node/Rust runtime at roughly 200-400 MB resident, so
    an unbounded fleet of seats entering Plan together can exhaust the
    engine host; 4 keeps peak usage near 1.5 GB, which fits the smallest
    realistic host while still overlapping the calls' long I/O waits.
    Subscription plans also throttle concurrency well below what an API
    key allows, so a higher number mostly buys rate-limit errors.
    Raise it on a large host with a plan that permits it."""

    env: dict[str, str] = Field(default_factory=dict)
    """Extra environment for every invocation, ``${VAR}``-resolved.

    The child process gets an **allowlisted** environment, not the
    engine's — so anything the CLI needs beyond PATH / locale / TLS /
    proxy (a vendor region variable, a self-hosted gateway URL) is
    declared here."""

    auth: CLIAgentAuthConfig = Field(default_factory=CLIAgentAuthConfig)

    overrides: dict[str, Any] = Field(default_factory=dict)
    """Field-wise overrides of the built-in profile
    (:class:`crewlet.providers.llm.cli_profiles.CLIAgentProfile`).

    CLI flags drift between releases, and a vendor renaming
    ``--output-format`` must be a config edit rather than a Crewlet
    release — so every profile field is replaceable here.  Lists replace
    wholesale (position matters in an argv), and validation runs against
    the profile model, so a typo fails at config-validation time."""

    @model_validator(mode="after")
    def _validate(self) -> CLIAgentConfig:
        if self.timeout_seconds <= 0:
            raise ValueError(
                f"cli.timeout_seconds must be positive (got {self.timeout_seconds})"
            )
        if self.max_concurrent < 1:
            raise ValueError(
                f"cli.max_concurrent must be at least 1 (got {self.max_concurrent})"
            )
        if self.overrides:
            # Validate the merge here rather than at engine boot: a
            # rejected override should fail `crewlet validate`, not the
            # first turn of the first agent.
            from crewlet.providers.llm.cli_profiles import resolve_profile

            try:
                resolve_profile(self.agent, self.overrides)
            except Exception as exc:
                raise ValueError(
                    f"cli.overrides are not a valid profile: {exc}"
                ) from exc
        return self

    def profile(self) -> Any:
        """The built-in profile with :attr:`overrides` applied."""
        from crewlet.providers.llm.cli_profiles import resolve_profile

        return resolve_profile(self.agent, self.overrides)


class LLMProviderConfig(BaseModel):
    """Configuration for an LLM provider."""

    model_config = ConfigDict(extra="forbid")

    type: LLMProviderType = "openai"
    """Which built-in provider implementation to construct.  A closed set
    rather than a free string: :func:`create_llm_providers` dispatches on
    it, so an unrecognised value used to fall through every branch and
    silently produce **no provider at all** for that key — with no error
    anywhere, leaving roles that named it without an LLM.  Constraining
    the type rejects the typo at validation time, and puts the valid
    values in the generated JSON Schema for editors."""
    model: str = "gpt-4o"
    api_keys: list[str] = Field(default_factory=list)
    """API keys for this provider. Always a list -- supply one entry
    for the single-key case, multiple for rotation. When more
    than one is configured the provider rotates on rate-limit / auth
    errors and only raises ``AllCredentialsExhausted`` when every key
    is in cooldown. The fallback wrapper catches that and
    tries the next provider in the role's chain.

    Each entry supports ``${ENV_VAR}`` interpolation. An empty list
    falls back to the provider's conventional env var
    (``OPENAI_API_KEY`` or ``ANTHROPIC_API_KEY``) at provider
    construction time, so a simple in-shell credential still works
    without YAML changes."""
    cooldowns: CredentialCooldownConfig = Field(
        default_factory=CredentialCooldownConfig
    )
    """TTL policy applied per error class when a key is marked
    cooled-down."""
    base_url: str = ""
    reasoning: bool = False
    """Enable reasoning/extended thinking for this provider."""
    reasoning_effort: ReasoningEffort = "medium"
    """OpenAI reasoning effort (low/medium/high). Used when reasoning=True."""
    reasoning_budget_tokens: int = 10000
    """Anthropic thinking budget in tokens. Only used when reasoning=True."""
    timeout_seconds: float = 120.0
    """Per-request wall-clock timeout (seconds) for a single LLM API call
    -- the HTTP client timeout passed to the provider SDK. Raise it for
    slow / large-output (e.g. reasoning) models that can otherwise time
    out mid-generation; lower it to fail fast. Applies to ``openai`` /
    ``openai-compatible`` / ``anthropic`` providers; the ``cli-agent``
    backend drives a subprocess rather than an HTTP client and has its
    own :attr:`CLIAgentConfig.timeout_seconds`."""
    cli: CLIAgentConfig | None = None
    """The ``cli-agent`` block: which coding CLI to drive, where its
    per-seat state lives, and how it authenticates.  Required when
    ``type`` is ``cli-agent`` and rejected otherwise."""

    @model_validator(mode="after")
    def _validate_reasoning(self) -> LLMProviderConfig:
        if not self.reasoning:
            return self
        if self.type == "openai-compatible":
            raise ValueError(
                "reasoning is not supported for openai-compatible providers. "
                "Use type 'openai' or 'anthropic' instead."
            )
        if self.type == "cli-agent":
            raise ValueError(
                "reasoning is not a Crewlet setting for cli-agent providers: "
                "the CLI's plan carries its own reasoning configuration and "
                "exposes no per-call switch. Drop `reasoning`, or pick the "
                "CLI's reasoning model via `model`."
            )
        return self

    @model_validator(mode="after")
    def _validate_cli_block(self) -> LLMProviderConfig:
        """Keep ``type`` and the ``cli`` block in agreement.

        Both halves matter.  A ``cli-agent`` entry with no ``cli`` block
        would silently construct the default Claude Code profile, which
        may be the wrong CLI entirely; a ``cli`` block on an HTTP
        provider would be read by nobody and quietly ignored — the
        classic "I configured it and nothing happened" bug.
        """
        if self.type == "cli-agent":
            if self.cli is None:
                raise ValueError(
                    "an LLM provider of type 'cli-agent' needs a `cli:` block "
                    "naming the CLI to drive, e.g. `cli: {agent: claude-code}`."
                )
        elif self.cli is not None:
            raise ValueError(
                f"`cli:` only applies to type 'cli-agent' (this entry is "
                f"type {self.type!r}). Remove the block, or change the type."
            )
        return self

    def resolved_keys(self) -> list[str]:
        """Return the deduplicated list of configured API keys.

        Empty strings are filtered out. Env-var interpolation
        (``"${LLM_API_KEY_1}"``) is handled by the caller via
        :func:`_resolve_env_value` before the keys reach the
        provider. Order is preserved from declaration.
        """
        keys: list[str] = []
        for raw in self.api_keys:
            value = (raw or "").strip()
            if value and value not in keys:
                keys.append(value)
        return keys


class EmbeddingProviderConfig(BaseModel):
    """Configuration for an embedding provider."""

    model_config = ConfigDict(extra="forbid")

    type: EmbeddingProviderType = "openai"
    """Which embedding implementation to construct.  Closed for the same
    reason as :attr:`LLMProviderConfig.type`; both values route to the
    OpenAI client, which reaches any OpenAI-compatible endpoint via
    ``base_url``."""
    model: str = "text-embedding-3-small"
    api_key: str = ""
    base_url: str = ""
    dimensions: int = 1536
    """Embedding vector dimensions. Must match the model's output size."""


class LocalSandboxConfig(BaseModel):
    """The ``local`` sandbox backend (``providers.sandbox.local``).

    Runs the coding agent on the **engine host** rather than a remote
    box, so it can use the subscription login ``crewlet llm login``
    already established — no E2B account, no API key. See
    ``docs/concepts/code-sandbox.md#local-sandboxes``.
    """

    model_config = ConfigDict(extra="forbid")

    containment: Literal["direct", "container"] = "direct"
    """How far the box is separated from the engine host.

    ``direct`` runs a process tree in a per-box directory with ``HOME``
    and the XDG variables pointed at it and an allowlisted environment.
    That isolates **state** — no box sees another's checkout, memory or
    credentials — but **not the host**: the coding agent runs as the
    engine user, so it can read what that user can read and reach what
    its credentials reach. Right for a workstation or a dedicated VM;
    wrong for a shared host.

    ``container`` runs each box in its own Docker/Podman container with
    the box directory bind-mounted at ``/home/user``, giving real host
    isolation and the same in-box paths E2B uses. It needs an
    :attr:`image` that has the coding-agent CLI installed."""

    image: str = ""
    """Container image for ``containment: container``. Required there —
    there is deliberately no default, because a box whose image lacks the
    coding-agent CLI fails only once an agent tries to use it."""

    runtime: Literal["auto", "docker", "podman"] = "auto"
    """Container CLI to drive. ``auto`` prefers ``docker`` and falls back
    to ``podman`` (the rootless default on Fedora/RHEL, same
    subcommands)."""

    state_dir: str = ""
    """Parent directory for box directories. Empty uses
    ``$CREWLET_SANDBOX_LOCAL_HOME``, falling back to
    ``~/.crewlet/sandboxes``. Boxes are removed at teardown, and orphans
    from a crashed engine are reaped on the next create."""

    network: str = ""
    """``--network`` for ``containment: container``. Empty uses the
    runtime's default. Set ``none`` to cut the box off from the network
    entirely — which also cuts off the coding agent's LLM, so only do it
    for a box whose work is purely local."""

    run_args: list[str] = Field(default_factory=list)
    """Extra arguments spliced into the container ``run`` command (
    ``--cpus``, ``--memory``, extra ``-v`` mounts, ``--user``). This is
    where a container box is *sized*, mirroring how an E2B box is sized
    by its template."""

    @model_validator(mode="after")
    def _validate(self) -> LocalSandboxConfig:
        if self.containment == "container" and not self.image:
            raise ValueError(
                "providers.sandbox.local.containment 'container' requires "
                "`image` — an image with the coding-agent CLI installed."
            )
        if self.containment == "direct" and self.image:
            raise ValueError(
                "providers.sandbox.local.image only applies to containment "
                "'container'. Remove it, or switch containment."
            )
        return self


class SandboxProviderConfig(BaseModel):
    """Engine-level sandbox provider config (``providers.sandbox``).

    Drives the [code sandbox](../concepts/code-sandbox.md): a
    sandbox-enabled role can run real code work as a coding agent
    (Claude Code / OpenCode) inside an isolated sandbox via the
    ``run_sandbox`` Execute tool. Sibling of
    ``providers.llm`` -- live-reloadable, secrets kept verbatim (``${ENV}``
    references) in the DB and resolved at construction time.
    """

    model_config = ConfigDict(extra="forbid")

    type: Literal["e2b", "local", "fake", "none"] = "e2b"
    """Provider backend. ``e2b`` (cloud, or self-hosted via ``domain``),
    ``local`` (the engine host — directly or in a container, see
    :class:`LocalSandboxConfig`; the way to run code work on a
    subscription CLI login with no E2B account), ``fake`` (in-process
    tests), or ``none`` (disabled)."""
    local: LocalSandboxConfig | None = None
    """The ``local`` backend's block. Required when ``type`` is ``local``
    and rejected otherwise."""
    api_key: str = ""
    """E2B API key (``${ENV}`` interpolated). Empty falls back to the
    ``E2B_API_KEY`` env var at construction time."""
    domain: str = ""
    """E2B domain. Empty = ``e2b.dev`` cloud; set to a self-hosted /
    local E2B cluster's domain (env ``E2B_DOMAIN``) for "local run"."""
    template: str = ""
    """Sandbox template. Empty = E2B's prebuilt ``claude`` / ``opencode``
    template per coding agent; or a derived image preinstalling the role's
    MCP servers + the ``ask`` shim.

    This is also **how sandbox resources are sized**. vCPU / RAM / disk are
    properties of the template, fixed when it is built
    (``Template.build(name=…, cpu_count=…, memory_mb=…)`` / ``e2b template
    build``); the sandbox-create API accepts no resource arguments at all.
    To give agents bigger boxes, build a template with the resources you
    want and name it here — there is deliberately no engine-side limits
    knob, because the engine could not honour one."""
    default_coding_agent: Literal["claude-code", "opencode"] = "claude-code"
    default_timeout_seconds: float = Field(default=900.0, gt=0)
    """Sandbox box TTL / keepalive window. **Not** a run-time limit — a coding
    job is never force-stopped on a timer; the waiter refreshes a running box's
    TTL to this value every tick so the clock never kills it. Effectively the
    orphan-reclaim grace: how long a box outlives an engine that stopped
    heart-beating before E2B reaps it."""
    default_pause_ttl_seconds: float = Field(default=1800.0, ge=0)
    """How long to hold a blocked sandbox paused before reaping → re-seed.

    A sandbox blocked on a human's answer is *paused*: E2B holds a paused
    box indefinitely — there is no provider-side TTL — and bills for the
    snapshot, so expiring it is the engine's job
    (:class:`~crewlet.sandbox.waiter.SandboxWaiter` reaps past this age and
    flips the run to ``reseed``, which re-seeds from the pushed git branch
    on the eventual answer). 30 minutes trades a bounded snapshot bill
    against exact conversational resume for the replies that do come back
    quickly; raise it if your teams routinely answer in hours and you would
    rather pay for the snapshot than re-seed.

    ``0`` = never pause: the box is torn down as soon as it blocks and the
    work always re-seeds from git, for zero snapshot cost. Negative values
    are rejected rather than read as "no expiry" — an unbounded pause is
    exactly the leak this knob exists to prevent, and a role that wants the
    provider default says so with ``role.sandbox.pause_ttl_seconds: -1``."""
    setup: list[SandboxSetupStep] = Field(default_factory=list)
    """Engine-wide sandbox setup steps (``crewlet.sandbox.setup``) applied
    to every sandbox before each role's ``role.sandbox.setup`` extras.
    Each step declares files to write, commands to run, env to merge
    (``${ENV}``-expanded with the rest of the sandbox env), and a brief
    paragraph telling the coding agent what the step made true about its
    box.  The engine ships NO steps of its own — git auth included: put the
    documented ``git-auth`` recipe here (see ``docs/concepts/code-sandbox.md``
    and ``examples/nimbus.company.yaml``) so headless clones authenticate with
    the injected ``$GITHUB_TOKEN`` and the coding agent's brief tells it
    so.

    **Containment matters here.** Steps that provision *system* paths —
    the shipped git-auth recipe writes ``/usr/local/bin/…`` — work on
    E2B and in a ``local`` **container** box, but are refused by
    ``local`` **direct** mode, which has no filesystem virtualisation and
    would otherwise write to the engine host itself. Direct mode wants
    the same recipe rooted under ``$HOME`` (see
    ``docs/concepts/code-sandbox.md#local-sandboxes``)."""

    @model_validator(mode="after")
    def _validate_local_block(self) -> SandboxProviderConfig:
        """Keep ``type`` and the ``local`` block in agreement.

        Both directions matter: ``type: local`` without the block would
        silently take the ``direct`` default (running the coding agent on
        the host, which must be a deliberate choice), and a ``local``
        block on an E2B provider would be read by nobody.
        """
        if self.type == "local":
            if self.local is None:
                raise ValueError(
                    "providers.sandbox.type 'local' needs a `local:` block "
                    "choosing a containment mode, e.g. "
                    "`local: {containment: direct}`. 'direct' runs the coding "
                    "agent as the engine user with no host isolation, so it "
                    "is never assumed."
                )
        elif self.local is not None:
            raise ValueError(
                f"`local:` only applies to providers.sandbox.type 'local' "
                f"(this is type {self.type!r}). Remove the block, or change "
                "the type."
            )
        return self


class QueueConfig(BaseModel):
    """Configuration for the event queue backend."""

    model_config = ConfigDict(extra="forbid")

    type: Literal["pulsar"] = "pulsar"
    """Queue backend type. Currently only ``pulsar`` (Apache Pulsar) is supported."""
    url: str = "pulsar://localhost:6650"
    """Pulsar broker service URL (binary protocol)."""
    admin_url: str = ""
    """Pulsar admin HTTP endpoint. Empty = derived from ``url`` by
    Pulsar's own default port convention (``pulsar://host:6650`` →
    ``http://host:8080``, ``pulsar+ssl://host:6651`` →
    ``https://host:8443``).

    The engine needs it to keep **every seat's subscription alive
    whether or not any node owns the seat** — a durable subscription
    holds an unowned seat's mail, and creating one needs no consumer
    only through this endpoint. Set it explicitly whenever the admin
    endpoint is not on the broker's host at the default port (a proxy,
    a separate admin service, a non-default port)."""
    tenant: str = Field(default="public", pattern=r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
    """Pulsar tenant that holds all crewlet topics. Tenants are never
    auto-created — a non-default tenant must exist before start
    (``pulsar-admin tenants create <tenant>``)."""
    namespace: str = Field(default="default", pattern=r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
    """Namespace under ``tenant`` for all crewlet topics. Like the
    tenant, a non-default namespace is created out-of-band
    (``pulsar-admin namespaces create <tenant>/<namespace>``)."""
    auth_token: str = ""
    """JWT presented to the broker (token authentication), and to the
    admin endpoint as a bearer token. The token's ``sub`` claim is the
    engine's *role*; grant that role produce+consume on this engine's
    namespace only, so engines sharing a cluster cannot touch each
    other's tenants — plus the namespace-level permission the
    subscription lifecycle needs, since produce+consume alone does not
    authorize it. Use ``${VAR}`` to read it from the environment. Empty =
    connect unauthenticated."""
    tls_trust_certs_path: str = ""
    """CA bundle for ``pulsar+ssl://`` URLs (optional; empty = system
    defaults). Tokens are bearer credentials — use TLS whenever the
    broker is not on localhost."""


class DatabaseConfig(BaseModel):
    """Configuration for the PostgreSQL database."""

    model_config = ConfigDict(extra="forbid")

    dsn: str = ""
    """PostgreSQL connection string. Required for production use."""


class KnowledgeStoreConfig(BaseModel):
    """Configuration for the knowledge store backend (Tier A — bound to DB)."""

    model_config = ConfigDict(extra="forbid")

    type: Literal["pgvector", "memory"] = "pgvector"
    """Backend type — currently ``pgvector`` (the only production option)
    or ``memory`` (tests). Bound to the Tier A database choice."""


class ApiAuthTokenConfig(BaseModel):
    """One bearer token gating ``/config/*`` access."""

    model_config = ConfigDict(extra="forbid")

    id: str
    """Short label used in revision audit logs (``created_by``).
    Examples: ``"founder"``, ``"ops"``, ``"ci-pipeline"``."""

    token: str
    """Token value or ``${ENV_VAR}`` reference.  Resolved once at API
    startup; never stored in the DB."""


class ApiAuthConfig(BaseModel):
    """Bearer-token auth policy for the API.

    Writes and the whole ``/config`` surface always require a token.
    Reads are governed by :attr:`allow_anonymous_read`, which defaults
    to open — reading is what a dashboard does, and the page that would
    prompt for a token is itself served unauthenticated, so requiring
    one by default puts a modal in front of every first load.

    Exempt from auth entirely, because they authenticate by other means
    or must be reachable to obtain a token at all: ``/health``,
    ``/ready``, the dashboard shell and its static assets,
    ``/webhooks/*`` (HMAC-verified per source) and ``/otlp/*`` (signed
    per-run token).
    """

    model_config = ConfigDict(extra="forbid")

    tokens: list[ApiAuthTokenConfig] = Field(default_factory=list)
    """List of accepted bearer tokens.  Required unless
    ``disabled=True`` (local-dev escape hatch)."""

    disabled: bool = False
    """Local-development override.  When ``True``, the API serves
    every route without auth and logs a loud startup warning.
    Never use in production."""

    allow_anonymous_read: bool = True
    """Whether read-only routes (``GET`` / ``HEAD`` outside ``/config``)
    serve without a token.  Writes and the whole ``/config`` surface
    always require one.

    Default ``True``: the dashboard is a read surface, and a browser
    that has to be handed a bearer token before it will show an org
    chart is friction on the common case.  It is a real exposure
    nonetheless — the read surface carries LLM transcripts, agent diary
    entries and the whole event stream — so the API states which posture
    it took at startup, at WARNING when the bind host is not loopback.

    Set ``False`` to guard the entire surface.  Everything a browser
    needs then carries the token, including the ``/ws/stream``
    handshake, and the dashboard prompts for one when the engine refuses
    it."""

    allowed_origins: list[str] = Field(default_factory=list)
    """Browser origins permitted by CORS, e.g.
    ``["https://crewlet.example.com"]``.

    Empty (the default) means **same-origin only** — the dashboard is
    served by this process, so it needs no entry.  The previous default
    was ``*``, which let any site a logged-in operator visited read
    every unauthenticated endpoint."""


class ApiConfig(BaseModel):
    """Tier A API process settings — host, port, auth."""

    model_config = ConfigDict(extra="forbid")

    host: str = "0.0.0.0"
    """Bind host for the standalone API process."""

    port: int = 0
    """Bind port for the standalone API process.  ``0`` disables the
    embedded API; a positive integer enables the embedded variant
    inside the engine process (the standalone variant is run via
    ``crewlet run api`` and binds explicitly)."""

    auth: ApiAuthConfig = ApiAuthConfig()


class BootstrapProvidersConfig(BaseModel):
    """Tier A provider settings — queue, database, knowledge.

    Everything the engine needs to talk to its own infrastructure
    before reading the founder-owned Tier B config from the DB.
    """

    model_config = ConfigDict(extra="forbid")

    queue: QueueConfig = QueueConfig()
    database: DatabaseConfig = DatabaseConfig()
    knowledge: KnowledgeStoreConfig = KnowledgeStoreConfig()


class SecretKeyConfig(BaseModel):
    """One key in the Tier A encryption keyring.

    ``material`` is a base64-encoded 32-byte key (or a ``${VAR}``
    reference, resolved from the environment at bootstrap load time like
    every Tier A value). Generate one with ``crewlet secrets keygen``.
    See ``docs/concepts/configuration.md`` § Secrets.
    """

    model_config = ConfigDict(extra="forbid")

    id: str = Field(min_length=1, pattern=r"^[A-Za-z0-9._-]+$")
    """Stable key identifier, stamped into every envelope this key seals
    (``enc:v1:<id>:...``). Colon-free so the envelope parses; kept across
    a rotation so old ciphertext still names a live key."""

    material: str
    """Base64-encoded 32-byte key, or a ``${VAR}`` reference to one."""


class SecretsConfig(BaseModel):
    """Tier A encryption keyring for company-config secrets at rest.

    The keyring is the sole root of trust: the database holds only
    ciphertext, and the key material lives here (on disk / in env),
    never in the DB. Empty (the default) means secret encryption is
    disabled — the engine then fails closed only if the active revision
    actually contains sealed values.

    See ``docs/concepts/configuration.md`` § Secrets.
    """

    model_config = ConfigDict(extra="forbid")

    active_key_id: str = ""
    """Which key seals new writes. Required once ``keys`` is non-empty."""

    keys: list[SecretKeyConfig] = Field(default_factory=list)
    """The keyring. More than one entry supports online key rotation:
    the active key seals, every key decrypts."""

    @model_validator(mode="after")
    def _validate(self) -> SecretsConfig:
        ids = [k.id for k in self.keys]
        if len(ids) != len(set(ids)):
            raise ValueError("secrets.keys ids must be unique")
        if self.keys and not self.active_key_id:
            raise ValueError(
                "secrets.active_key_id is required when secrets.keys is set"
            )
        if self.active_key_id and self.active_key_id not in ids:
            raise ValueError(
                f"secrets.active_key_id {self.active_key_id!r} does not match "
                f"any configured key id"
            )
        return self


NODE_ID_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")

DEFAULT_NODE_ID = "node-0"


class NodeConfig(BaseModel):
    """Tier A identity of THIS process within the company.

    Every Crewlet process has one.  Today it labels every log line and
    the ``/health`` payload — already the difference between "a config
    apply failed" and "the config apply failed on node-2" once more than
    one process runs, and the only way a caller behind a load balancer
    can tell which process answered.  It is also the identity every
    cross-process mechanism is keyed on (lease owner, per-node
    subscription name, per-node config apply status); see
    ``docs/concepts/scaling.md``.

    **It must be stable across restarts**, which is why it comes from the
    deployment rather than being generated.  A fresh value per boot would
    orphan whatever the previous incarnation registered under the old one
    — the failure the ``hook-{id(callback)}`` subscription naming already
    demonstrates.  In Kubernetes use the pod name (a StatefulSet ordinal
    is ideal); under systemd, the host name.
    """

    model_config = ConfigDict(extra="forbid")

    id: str = ""
    """This process's identity. Empty resolves via ``CREWLET_NODE_ID``,
    then falls back to ``node-0`` (the single-process default, so nothing
    has to be set to run one engine)."""

    roles: list[NodeRole] = Field(default_factory=lambda: sorted(DEFAULT_NODE_ROLES))
    """What this process is willing to do: ``ingress`` (serve the HTTP
    API and its webhooks), ``seats`` (claim seat leases and run agents),
    ``workers`` (run the company-wide singleton duties). Defaults to all
    three — one process running a whole company, which is every existing
    deployment.

    Subtracting a role subtracts it from THIS node, never from the
    company, so the fleet as a whole still needs every role somewhere: a
    fleet with no ``workers`` node runs no scheduler, no retention sweep
    and no sandbox waiter, and one with no ``ingress`` node never hears a
    webhook. Neither is detectable from a single node's config, so the
    engine checks it against live node presence at runtime and says so
    (``fleet_role_unmanned``).

    A node that does not run seats is also excluded from the seat
    denominator its peers divide by — see
    :mod:`crewlet.seat.placement`."""

    labels: dict[str, str] = Field(default_factory=dict)
    """Free-form facts about where this process runs (``zone: eu``,
    ``gpu: "true"``), matched by a seat's ``role.placement`` selector.
    Advertised to peers on this node's presence lease, so a label change
    takes effect on the next heartbeat rather than at the next config
    activation.

    Values are strings; a selector compares them exactly. Nothing here
    means anything to the engine on its own — the org decides what to
    select on."""

    @field_validator("roles")
    @classmethod
    def _validate_roles(cls, v: list[NodeRole]) -> list[NodeRole]:
        if not v:
            raise ValueError(
                "node.roles must name at least one of "
                f"{', '.join(sorted(str(r) for r in NodeRole))} — a node "
                f"with no roles does nothing at all. Omit the key to run "
                f"every role, which is the single-process default."
            )
        return v

    @field_validator("labels")
    @classmethod
    def _validate_labels(cls, v: dict[str, str]) -> dict[str, str]:
        for key in v:
            if not key or not key.strip():
                raise ValueError("node.labels keys must be non-empty")
        return v

    @field_validator("id")
    @classmethod
    def _validate_id(cls, v: str) -> str:
        # A ``${VAR}`` reference is validated after resolution, in
        # ``resolve_node_id``.  Tier A substitutes references before
        # model_validate on the normal load path, so this only matters
        # for a config constructed programmatically — but rejecting the
        # reference form here would make that path fail on a value the
        # YAML path accepts.
        if v and not has_env_reference(v) and not NODE_ID_PATTERN.match(v):
            raise ValueError(
                f"node.id {v!r} must start alphanumeric and contain only "
                f"letters, digits, '.', '_' or '-' (max 64 chars) — it is "
                f"used in log fields and broker subscription names"
            )
        return v


class BootstrapConfig(BaseModel):
    """Tier A — ops-owned, on-disk, restart-only.

    Carries only the keys the engine needs to bring up its
    infrastructure (DB, Pulsar, API socket, auth, logging) plus the
    secret-encryption keyring (the root of trust for Tier B secrets at
    rest).  Everything else lives in the versioned ``company_config``
    table and is delivered to the engine via :meth:`Engine.apply_config`.

    See ``docs/concepts/configuration.md``.
    """

    model_config = ConfigDict(extra="forbid")

    providers: BootstrapProvidersConfig = BootstrapProvidersConfig()
    api: ApiConfig = ApiConfig()
    secrets: SecretsConfig = SecretsConfig()
    node: NodeConfig = NodeConfig()
    debug: bool = False


def resolve_node_id(bootstrap: BootstrapConfig | None = None) -> str:
    """Resolve this process's node id.

    Precedence: ``node.id`` in the Tier A file, then the
    ``CREWLET_NODE_ID`` environment variable (which is how a container
    orchestrator injects a pod name without templating the config file),
    then :data:`DEFAULT_NODE_ID`.

    Resolved through :func:`resolve_env_scalar`, so ``node.id:
    "${HOSTNAME}"`` works like every other Tier A reference.  An
    environment value that fails validation is rejected here rather than
    surfacing later as a malformed subscription name.
    """
    configured = bootstrap.node.id if bootstrap is not None else ""
    value = resolve_env_scalar(configured) if configured else ""
    if not value:
        value = os.environ.get("CREWLET_NODE_ID", "").strip()
    if not value:
        return DEFAULT_NODE_ID
    if not NODE_ID_PATTERN.match(value):
        raise ValueError(
            f"node id {value!r} must start alphanumeric and contain only "
            f"letters, digits, '.', '_' or '-' (max 64 chars)"
        )
    return value


_INCARNATION: str = ""


def resolve_node_incarnation(bootstrap: BootstrapConfig | None = None) -> str:
    """This process's unique holder identity: ``{node_id}:{random}``.

    The counterpart to :func:`resolve_node_id`, and deliberately the
    opposite property.  The node id is *stable across restarts* because
    it names a placement — a pod, a host — and things registered under it
    must survive a restart.  A lease holder needs the reverse: it names
    one running incarnation, so that a replacement process is never
    mistaken for its predecessor.

    Conflating the two is a real hole rather than a nicety.  A lease is
    renewable by its own owner string, so if a restarted pod reused the
    identity of a predecessor still draining a long turn, both would hold
    the seat at the same fencing epoch — and with the default id being
    the shared constant ``node-0``, so would any two engines started
    against one database.

    Computed once per process and cached: two calls return the same
    value, so a caller cannot accidentally fence itself out by
    re-resolving.

    That cache is right for the usual case — one engine per process —
    and wrong for the other one.  Two :class:`~crewlet.engine.Engine`
    objects in ONE process (an embedding, a fleet test) are two lease
    holders, and handing them the same identity recreates exactly the
    hole this function exists to close: each could renew the other's
    lease, and both would hold a seat at the same fencing epoch.  Those
    callers use :func:`new_node_incarnation`.
    """
    global _INCARNATION  # noqa: PLW0603
    if not _INCARNATION:
        _INCARNATION = new_node_incarnation(bootstrap)
    return _INCARNATION


def new_node_incarnation(bootstrap: BootstrapConfig | None = None) -> str:
    """Mint a FRESH holder identity, bypassing the process cache.

    For a caller that is itself one lease holder among several in this
    process.  Each engine holds its own for its whole life — minting a
    second one mid-run would fence the engine out of its own seats.
    """
    return f"{resolve_node_id(bootstrap)}:{uuid4().hex[:8]}"


class TurnEngineConfig(BaseModel):
    """Configuration for the Plan/Execute/Review turn engine.

    The turn engine replaces the single-shot LLM + tool loop with three
    phases per agent turn: Plan produces an ``ExecutionPlan``, Execute
    runs the plan's tool surface, and Review decides whether the turn
    is done, should iterate, or hand off to a colleague.

    See ``docs/concepts/turn-engine.md`` for details.
    """

    model_config = ConfigDict(extra="forbid")

    max_iterations: int = 3
    """Hard cap on Plan→Execute→Review iterations per turn.

    When Review returns ``self_iterate`` this counter increments; on
    breach the engine publishes a ``turn.guard_breach(kind="max_iter")``
    and terminates the turn as ``failed``.
    """

    subagent_max_turns: int = 20
    """Maximum tool-call rounds a single ephemeral sub-agent may run.

    The parent's ``spawn_subagent`` call may request ``max_turns <=``
    this value; requests above the cap are clamped.
    """

    subagent_timeout_seconds: float = 120.0
    """Wall-clock timeout for a single ``spawn_subagent`` invocation.

    The sub-agent runs inside ``asyncio.wait_for`` with this budget.
    ``float`` so callers can specify fractional seconds (e.g. ``0.5``
    in tests) without surprising integer truncation.
    """

    subagent_budget_fraction: float = 0.2
    """Fraction of the parent turn's remaining token budget a sub-agent
    may consume. ``0.2`` = 20 percent. The parent can request less, not
    more.

    For a batched ``spawn_subagent`` this fraction is the
    *total* slice shared across all children, not per-child.
    """

    subagent_max_parallel: int = 3
    """Maximum sub-agents a single batched ``spawn_subagent`` call runs
    concurrently. Children beyond this run as earlier ones finish.
    """

    subagent_batch_timeout_seconds: float = 120.0
    """Aggregate wall-clock timeout for one batched ``spawn_subagent``
    call. The whole batch (every child) must finish within this budget;
    each child additionally has its own ``subagent_timeout_seconds``.
    """

    subagent_min_per_child_tokens: int = 500
    """Floor on the per-child token slice in a batched ``spawn_subagent``.
    If the total budget slice divided across the requested children
    falls below this, the batch is rejected up front rather than
    starving every child.
    """

    executor_always_on_tools: list[str] = Field(
        default_factory=lambda: ["load_tool_skill"],
    )
    """Tools always available in the Execute phase regardless of what
    the plan named in ``plan.tools_needed``.

    Default: ``load_tool_skill`` (fetch the rich body of a Tool Skill
    mid-task) -- a read-only, side-effect-free read the executor
    frequently wants and can't get later if the planner forgot to
    name it.  Confluence search (``confluence_search``) is an MCP tool
    and cannot be force-added here; the planner names it in
    ``ExecutionPlan.tools_needed`` when Execute will need it.

    Adding to this list is easy and hard to reverse; prefer having the
    planner name the tool in its ``ExecutionPlan.tools_needed`` output.
    """

    delegation_depth_limit: int = 3
    """Maximum depth for colleague-to-colleague delegation chains.

    When Agent A asks Agent B through any surface (Slack, Jira,
    Confluence, GitHub, A2A), B's resulting turn inherits
    ``delegation_depth = A.delegation_depth + 1``. On breach the
    engine publishes a ``turn.guard_breach(kind="depth_cap")`` and
    terminates the turn as ``failed``.
    """

    max_tool_rounds: int = 20
    """Maximum tool-call rounds within a single Execute-phase run.

    Each round is one LLM call plus any tool calls it emits.  On cap
    breach the Execute loop exits and Review sees ``exhausted_rounds
    = True``, typically forcing a ``self_iterate``.
    """

    plan_max_tool_rounds: int = 16
    """Maximum tool-call rounds within a single Plan-phase run.

    Plan typically needs fewer rounds than Execute because it only
    calls meta-tools (``activate_tool``, ``submit_plan``); the
    default 16 is generous for the catalogue-exploration use case.
    On cap breach the rescue path forces ``submit_plan`` (or the
    extension judge fires first when ``extension_enabled``).
    """

    onboarding_max_tool_rounds: int = 10
    """Maximum tool-call rounds for the dedicated first-turn onboarding pass.

    First-turn onboarding (read the team's ``Onboarding`` pages, persist
    conventions via ``reflect_and_persist``, call ``mark_onboarded``) runs as
    its **own** phase before Plan, with this separate budget — so onboarding
    never competes with the Plan round budget for ``submit_plan``
    rounds on a first turn.  ``0`` disables the dedicated pass.
    """

    extension_enabled: bool = True
    """Master switch for the round-cap extension judge.

    When enabled, exhausting ``max_tool_rounds`` (Execute) or the Plan
    phase's own round cap routes through a cheap judge LLM that
    decides whether to grant more rounds or fall through to the
    existing rescue path.  See ``docs/concepts/turn-engine.md`` for
    the design and ``crewlet.agent.extension`` for the implementation.
    """

    plan_max_tool_rounds_ceiling: int = 32
    """Hard ceiling for total Plan-phase rounds across extensions.

    The judge can grant additional rounds up to (but not beyond) this
    cap.  Default 32 = 2x the Plan phase's base 16 rounds.  Set to the
    base round count to disable extensions for Plan without disabling
    them for Execute.
    """

    execute_max_tool_rounds_ceiling: int = 40
    """Hard ceiling for total Execute-phase rounds across extensions.

    The judge can grant additional rounds up to (but not beyond) this
    cap.  Default 40 = 2x the default ``max_tool_rounds`` of 20.  Set
    to ``max_tool_rounds`` to disable extensions for Execute without
    disabling them for Plan.
    """

    onboarding_max_tool_rounds_ceiling: int = 20
    """Hard ceiling for total onboarding-phase rounds across extensions.

    The dedicated first-turn onboarding pass starts at
    ``onboarding_max_tool_rounds`` and, like Plan/Execute, the round-cap
    extension judge can grant more rounds up to this cap when the agent is
    still making progress (reading a page, persisting a convention) rather
    than thrashing.  Default 20 = 2x the base.  Set equal to
    ``onboarding_max_tool_rounds`` to disable onboarding extensions.
    """

    extension_round_step: int = 8
    """Maximum rounds the judge may grant in a single extension call.

    Larger requests are clamped down; the judge is allowed to grant
    fewer if it has weaker confidence.  Repeated exhaustions during
    an extended run trigger a fresh judge call, so this is a
    per-extension cap, not a per-turn cap.
    """

    sandbox_budget_fraction: float = 0.5
    """Fraction of the agent's remaining token budget a sandboxed Execute
    may consume, used to derive the coding agent's in-agent caps
    (``--max-budget-usd`` / ``--max-turns``). Best-effort: the coding
    agent calls the LLM itself, so this is a cap + post-account, not the
    exact mid-stream gate the native loop has."""

    sandbox_min_budget_tokens: int = 2000
    """Pre-flight floor: refuse to launch a sandboxed Execute when the
    agent's remaining budget is below this."""


class EpisodicConfig(BaseModel):
    """Episodic-memory settings for the agent-learning subsystem."""

    model_config = ConfigDict(extra="forbid")

    retrieval_limit: int = 5
    """Default limit for ``query_episodes`` results (1-20)."""


class ReflectConfig(BaseModel):
    """Reflection/PersistDecider settings.

    Runs after every turn to decide whether any durable fact from the
    turn should be persisted to the knowledge store.  Enabled by
    default; the on-event dispatcher subscribes only when
    ``enabled=True``.
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = True
    """Master switch for the ReflectEngine subsystem."""

    persist_decider: bool = True
    """Whether to run the deterministic PersistDecider after each turn."""

    budget_tokens: int = 5000
    """Soft cap on tokens spent on the PersistDecider's LLM call per
    turn.  Zero disables the cap.  Respected by the provider's
    ``max_tokens`` argument."""

    summarize_episodes: bool = True
    """When true, ``query_episodes`` passes raw hits through the
    role's auxiliary/cheap model for a compact per-hit summary before
    returning.  Falls back to raw output when no auxiliary model is
    configured.  Turn off to return raw summaries verbatim."""

    summarize_max_tokens: int = 400
    """Soft cap on the summarisation LLM's response length."""


class SkillSynthesisConfig(BaseModel):
    """Skill-synthesis settings.

    Two triggers: single-turn (inline from the ReflectEngine when a
    turn used ``>= min_tool_calls`` tools and finished ``done``) and
    clustered (scheduled tick that groups recent successful turns by
    tool-sequence Jaccard similarity and drafts skills for recurring
    patterns).  Both paths write to the ``synthesized_skills`` table.
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = True
    """Master switch for phase-4 skill synthesis."""

    min_tool_calls: int = 5
    """Single-turn trigger threshold.  Turns that used fewer tools
    don't get a skill drafted inline — they can still contribute to
    clustered synthesis if the scheduler is enabled."""

    budget_tokens: int = 4000
    """Soft cap on the synthesizer's LLM call per draft attempt."""

    max_skills_per_agent: int = 50
    """Hard per-agent cap; once reached the synthesizer no-ops."""

    duplicate_jaccard_threshold: float = 0.7
    """Reject a new skill when its tool sequence has Jaccard >= this
    against any existing synthesized skill for the same agent."""

    # Scheduler — opt-in by default so CI and local dev don't spin up
    # background tasks unexpectedly.
    scheduler_enabled: bool = False
    scheduler_interval_seconds: int = 3600
    cluster_window_hours: int = 168
    cluster_min_size: int = 3
    cluster_jaccard_threshold: float = 0.6
    episode_fetch_limit: int = 200


class SkillRefinementConfig(BaseModel):
    """Skill-refinement settings.

    Runs as a fourth worker inside the ReflectEngine: whenever a turn
    used one or more synthesized skills, the refiner decides whether
    to append an observation (on success) or a counter-example (on
    failure) to the most-recently-used match.  Version history is
    archived automatically; rollback works via
    :meth:`SynthesizedSkillStore.rollback_to_version`.
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = True
    auto_refine_on_success: bool = True
    auto_refine_on_failure: bool = True
    budget_tokens: int = 3000
    max_body_chars: int = 20000
    max_versions_kept: int = 10


class SkillPromotionConfig(BaseModel):
    """Skill-promotion settings.

    Adds a second pass to the SkillClusteringScheduler: for each unit,
    cluster recent agent-scope synthesized skills across siblings by
    tool-sequence Jaccard; when ≥ ``min_sibling_count`` distinct
    agents share a cluster (and no unit-scope skill already covers
    it), draft a unit-scope skill via the auxiliary model.
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = True
    min_sibling_count: int = 3
    jaccard_threshold: float = 0.6
    budget_tokens: int = 4000


class EpisodeLifecycleConfig(BaseModel):
    """Episode-lifecycle settings.

    The ``EpisodeLifecycleWorker`` drains the ``episodes`` table by
    dropping low-value rows and LLM-compacting clusters of similar
    routine turns into single ``kind='compacted'`` summaries.  Trigger
    is write-side (``EpisodeStore.write`` publishes
    ``CompactionRequested`` once an agent's raw count exceeds
    ``max_raw_episodes_per_agent``); no daily cron, no caller-path
    latency on reads.

    See ``docs/concepts/agent-learning.md`` -- episode lifecycle
    section.
    """

    model_config = ConfigDict(extra="forbid")

    max_raw_episodes_per_agent: int = 500
    """Threshold that triggers a lifecycle pass when crossed.  The
    worker compacts the oldest batch (size ``compaction_batch_size``)
    bringing the count back down toward but not necessarily under the
    threshold -- the count drifts down naturally as routine work
    rotates through the consolidation funnel.  Default 500 means
    compaction kicks in after roughly 500 turns since the last pass."""

    write_check_every_n: int = 10
    """How often to actually run the count(*) check on the write
    path.  Every Nth write triggers a count; the rest just increment
    an in-memory per-agent counter.  Higher = lower write overhead,
    lower = sharper threshold detection."""

    non_terminal_max_age_days: int = 14
    """Drop ``self_iterate`` raw rows older than this.  This mid-state
    never feeds skill synthesis (the reflect engine's terminal-outcome
    gate filters it out) so the only consumer is ``query_episodes``
    recall, where it's noise."""

    consolidated_grace_days: int = 30
    """Drop raw rows past this age that have been marked
    ``consolidated_into_skill_id`` by the synthesizer.  Grace gives
    operators a chance to audit / detect bad consolidations before
    the source disappears."""

    compaction_min_age_days: int = 30
    """Only compact raw rows older than this.  Recent rows stay raw
    so the synthesizer + similar-prior-work recall still see them at
    full fidelity within the active learning windows
    (``cluster_window_hours = 168`` = 7 days; this default is 4× that)."""

    compaction_min_cluster_size: int = 3
    """Don't compact singletons / pairs.  Small clusters are noise --
    if a pattern occurred 3+ times it's worth summarising; less than
    that and the per-turn detail is more valuable than the aggregate."""

    compaction_jaccard_threshold: float = 0.6
    """Tool-sequence similarity required to pool two episodes into the
    same cluster.  Same default as the skill clustering scheduler so
    behaviour stays consistent across the two consolidation paths."""

    compaction_batch_size: int = 200
    """Max raw episodes pulled into one lifecycle pass for clustering.
    A single ``CompactionRequested`` event fires this batch size;
    subsequent events catch the next batch."""

    compaction_budget_tokens: int = 4000
    """Soft cap on the aux LLM call that summarises one cluster.
    Routes through ``role.llm_auxiliary``; falls back to ``role.llm``
    when the auxiliary slot is unset."""

    exemplar_count: int = 2
    """Number of original episodes to retain as raw rows after a
    cluster is compacted (referenced by the new compacted row's
    ``exemplar_turn_ids``).  Lets operators drill into typical
    examples of the pattern without losing fidelity entirely."""

    compacted_max_age_days: int = 0
    """Optional very-long-tail eviction for ``kind='compacted'`` rows.
    Default 0 (disabled) means compacted summaries stay
    indefinitely.  Set to 365+ if you want a hard storage cap years
    out -- accept that you'll lose long-horizon aggregate signal in
    exchange."""


class PersonalMemoryConfig(BaseModel):
    """Personal-memory read-side settings.

    Controls the Plan-phase ``## Personal memory`` digest and the
    LLM-callable ``refresh_memory`` tool.  Memory writes are governed
    by the ``reflect`` block above (PersistDecider) and the
    ``reflect_and_persist`` builtin -- they are not affected by these
    fields.
    """

    model_config = ConfigDict(extra="forbid")

    max_refreshes_per_turn: int = 3
    """Hard cap on how many distinct ``refresh_memory`` calls one
    turn may make.  Repeated calls with the same ``context_hint``
    are served from an idempotency cache and do NOT count against
    this cap.  Reached-cap calls return an error rather than silently
    no-op so the planner notices and stops trying."""


class CounterpartyConfig(BaseModel):
    """CounterpartyProfiler settings.

    Runs alongside the PersistDecider in the ReflectEngine.  For
    each turn triggered by an identifiable sender, updates a per-
    ``(observer, subject)`` profile on the auxiliary model.  Writes
    go to the ``counterparty_profiles`` table.
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = True
    """Master switch for per-counterparty profile observation."""

    budget_tokens: int = 3000
    """Soft cap on the profiler's LLM call per turn."""


class SkillCuratorConfig(BaseModel):
    """Skill-curator settings.

    The ``SkillCuratorWorker`` walks ``synthesized_skills`` on an
    interval and ages out unused skills (active → stale → archived).
    Pinned skills are exempt. Archived skills are still in the table
    -- restoration is ``UPDATE synthesized_skills SET state='active'``.

    The read-then-archive race against the Plan-phase prefetch is
    closed in the store: ``transition_state`` only fires the demote
    UPDATE if the row's ``last_used_at`` still matches the curator's
    snapshot, so no "safety window" knob is needed.
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = True
    interval_hours: int = 24
    stale_after_days: int = 30
    archive_after_days: int = 90

    @model_validator(mode="after")
    def _stale_before_archive(self) -> SkillCuratorConfig:
        if self.archive_after_days < self.stale_after_days:
            raise ValueError(
                "archive_after_days must be >= stale_after_days "
                f"(got archive={self.archive_after_days}, "
                f"stale={self.stale_after_days})"
            )
        return self


class LearningConfig(BaseModel):
    """Configuration for the agent-learning subsystem.

    See ``docs/concepts/agent-learning.md`` for the full design.
    Requires a configured database + embedding provider; when either
    is missing the engine logs a disabled-notice and skips registration
    of learning tools.
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = True
    """Master switch for the subsystem."""

    episodic: EpisodicConfig = EpisodicConfig()
    reflect: ReflectConfig = ReflectConfig()
    counterparty: CounterpartyConfig = CounterpartyConfig()
    skill_synthesis: SkillSynthesisConfig = SkillSynthesisConfig()
    skill_refinement: SkillRefinementConfig = SkillRefinementConfig()
    skill_promotion: SkillPromotionConfig = SkillPromotionConfig()
    skill_curator: SkillCuratorConfig = SkillCuratorConfig()
    personal_memory: PersonalMemoryConfig = PersonalMemoryConfig()
    episode_lifecycle: EpisodeLifecycleConfig = EpisodeLifecycleConfig()


class SchedulingConfig(BaseModel):
    """Configuration for the role/unit scheduler.

    The scheduler is auto-enabled when ``enabled`` is true, a database
    is configured (it needs the ``scheduled_runs`` ledger for
    at-most-once delivery), and at least one role or unit declares a
    ``schedules:`` block.  Orgs with no schedules never spin up the
    tick loop.

    See ``docs/concepts/scheduling.md``.
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = True
    """Master switch for the scheduler subsystem."""

    tick_seconds: int = 10
    """Poll interval for the scheduler loop.  Cron fires at minute
    granularity, so anything below 60s reliably catches every fire."""

    default_timezone: str = "UTC"
    """Timezone used for any ``Schedule`` that doesn't set its own."""

    jitter_seconds: int = 0
    """Max per-schedule deterministic jitter (seconds) used to spread
    load when many schedules share a popular cron minute (e.g.
    ``0 9 * * *``).  ``0`` (default) fires exactly on the minute; the
    ConcurrencyController already queues a burst fairly, so jitter is
    an opt-in smoothing knob, not a correctness requirement."""

    catchup_min_seconds: int = 120
    """Lower clamp on the missed-tick catchup window (half the period)."""

    catchup_max_seconds: int = 7200
    """Upper clamp on the missed-tick catchup window (half the period)."""

    @model_validator(mode="after")
    def _validate(self) -> SchedulingConfig:
        if self.tick_seconds < 1:
            raise ValueError("scheduling.tick_seconds must be >= 1")
        if self.jitter_seconds < 0:
            raise ValueError("scheduling.jitter_seconds must be >= 0")
        if self.catchup_min_seconds < 0:
            raise ValueError("scheduling.catchup_min_seconds must be >= 0")
        if self.catchup_max_seconds < self.catchup_min_seconds:
            raise ValueError(
                "scheduling.catchup_max_seconds must be >= catchup_min_seconds "
                f"(got max={self.catchup_max_seconds}, "
                f"min={self.catchup_min_seconds})"
            )
        if self.default_timezone:
            try:
                ZoneInfo(self.default_timezone)
            except Exception as exc:
                raise ValueError(
                    f"scheduling.default_timezone is invalid: {self.default_timezone!r}"
                ) from exc
        return self


class ProvidersConfig(BaseModel):
    """Tier B provider settings — LLM + embeddings.

    Tier A providers (queue, database, knowledge) live on
    :class:`BootstrapProvidersConfig` and load from ``config.yaml`` at
    engine startup.
    """

    model_config = ConfigDict(extra="forbid")

    llm: dict[str, LLMProviderConfig] = Field(default_factory=dict)
    embeddings: EmbeddingProviderConfig | None = None
    sandbox: SandboxProviderConfig | None = None
    """Optional code-runtime sandbox provider. When unset
    (or ``type: none``) no role can run a sandboxed Execute backend."""

    @model_validator(mode="after")
    def _validate_shared_cli_state_dirs(self) -> ProvidersConfig:
        """Entries sharing a ``cli.state_dir`` must drive the same CLI.

        Sharing one directory is the supported way to run several models
        off a **single** login — ``opus`` for Plan and ``sonnet`` for
        Execute are two entries, and pointing both at one ``state_dir``
        means one ``crewlet llm login`` instead of two. That works only
        because both entries then agree on which files are credentials
        and which are conversation memory.

        Two *different* CLIs over one directory do not agree on either,
        so each would prune the other's state and seed credentials the
        other cannot read. Caught here so it fails ``crewlet validate``,
        not at the first turn of whichever seat lost the race.
        """
        by_dir: dict[str, tuple[str, str]] = {}
        for key, cfg in self.llm.items():
            if cfg.type != "cli-agent" or cfg.cli is None or not cfg.cli.state_dir:
                continue
            seen = by_dir.get(cfg.cli.state_dir)
            if seen is not None and seen[1] != cfg.cli.agent:
                raise ValueError(
                    f"providers.llm.{key} and providers.llm.{seen[0]} share "
                    f"cli.state_dir {cfg.cli.state_dir!r} but drive different "
                    f"CLIs ({cfg.cli.agent!r} vs {seen[1]!r}). Sharing a state "
                    "directory shares one login and one set of per-seat "
                    "homes, which only works for the same CLI — give them "
                    "separate state_dirs."
                )
            by_dir.setdefault(cfg.cli.state_dir, (key, cfg.cli.agent))
        return self


class ExtensionConfig(BaseModel):
    """Configuration for an extension."""

    name: str
    settings: dict[str, Any] = Field(default_factory=dict)


def parse_tool_annotation_overrides(
    raw: dict[str, dict[str, Any]],
) -> dict[str, ToolAnnotations]:
    """Parse a ``tool_annotations`` override block into
    :class:`ToolAnnotations`, keyed by bare tool name.

    Shared by every MCP-server-bearing config (generic ``mcp_servers``,
    the Jira / Confluence / Slack integration MCP, and GitHub): it lets
    operators correct servers that under-advertise MCP behavioural hints
    without touching engine code.  See
    ``docs/concepts/tool-capabilities.md``.
    """
    return {name: ToolAnnotations.from_mcp(value) for name, value in raw.items()}


class MCPServerConfig(AuthoredModel):
    """Configuration for an MCP server integration.

    Supports two transports:
    - ``stdio`` (default): launches a local subprocess
    - ``http``: connects to a remote Streamable HTTP server

    When ``shared`` is True (default), a single global server instance
    is launched and its tools are available to all agents.  When False,
    the config is used only as a **template** for per-role instances
    (spawned from ``role.mcp_env`` overrides).  No global instance is
    launched.  Use ``shared: false`` for identity-bound servers where
    each agent needs its own credentials.

    Per-role instances work for **both** transports.  For each role that
    declares ``role.mcp_env[name]``, the engine launches a dedicated
    instance and applies the role's overrides as **environment
    variables** (``stdio``) or **HTTP headers** (``http``).  An ``http``
    server with ``shared: false`` is therefore how an identity-bound
    remote server (e.g. the GitHub Copilot MCP) gets a per-agent token.

    **All tool servers are configured here** — including Jira/Confluence
    (name them ``atlassian``), Slack, and GitHub.  The
    ``integrations.jira`` / ``.confluence`` / ``.slack`` / ``.github``
    sections carry only non-tool config (admin credentials, webhook
    secrets); the engine never derives MCP servers from them.  See
    ``docs/concepts/tool-capabilities.md``.

    Example YAML::

        mcp_servers:
          # stdio, shared by all agents
          - name: calculator
            command: uvx
            args: ["mcp-calculator"]
          # stdio, per-role (token from role.mcp_env.atlassian)
          - name: atlassian
            shared: false
            command: uvx
            args: ["mcp-atlassian"]
            env: { JIRA_URL: "https://co.atlassian.net" }
          # http, per-role (Authorization header from role.mcp_env.github)
          - name: github
            transport: http
            shared: false
            url: "https://api.githubcopilot.com/mcp/"
    """

    # ``forbid``: an unrecognised key here is a typo, and the fields
    # below all default to empty — a dropped ``commnad:`` would launch
    # the server with no command instead of failing validation.
    model_config = ConfigDict(extra="forbid")

    # Non-empty: the name flows through to the MCP client name and (for
    # per-role instances) into ``MCPToolWrapper.server_name``, where an
    # empty string would silently mis-categorize the role's MCP tools
    # into the prompt's "Builtin tools" block instead of "MCP servers".
    name: str = Field(min_length=1)
    transport: str = "stdio"
    shared: bool = True
    # stdio fields
    command: str = ""
    args: list[str] = Field(default_factory=list)
    env: dict[str, str] = Field(default_factory=dict)
    tool_prefix: str = ""
    # http fields
    url: str = ""
    headers: dict[str, str] = Field(default_factory=dict)
    tool_annotations: dict[str, dict[str, Any]] = Field(default_factory=dict)
    """Per-tool behavioural-hint overrides, keyed by **bare tool name**.

    The engine derives tool behaviour (e.g. whether a sub-agent may
    call a tool) from the MCP ``readOnlyHint`` / ``destructiveHint`` /
    ``openWorldHint`` annotations the server advertises.  Servers that
    *under*-annotate their tools can be corrected here without touching
    engine code::

        mcp_servers:
          - name: linear
            command: uvx
            args: ["mcp-linear"]
            tool_annotations:
              linear_create_comment: { read_only: false, open_world: true }
              linear_get_issue:      { read_only: true }

    Accepts snake_case (``read_only``) or the MCP camelCase
    (``readOnlyHint``) keys.  Overrides win over server-advertised
    hints."""

    def annotation_overrides(self) -> dict[str, ToolAnnotations]:
        """Parse :attr:`tool_annotations` into resolved
        :class:`ToolAnnotations`, keyed by bare tool name."""
        return parse_tool_annotation_overrides(self.tool_annotations)


_ATLASSIAN_GATEWAY = "https://api.atlassian.com/ex/jira"
_ATLASSIAN_CONFLUENCE_GATEWAY = "https://api.atlassian.com/ex/confluence"


class JiraConfig(BaseModel):
    """Organisation-level Jira configuration.

    Defines the admin / service account used for org-wide Jira
    API access.  This is **not** a per-agent identity — individual
    agents authenticate via their own ``mcp_env`` credentials.

    The admin account is used for:
    - Fetching ticket watchers for routing decisions
    - (Future) Provisioning per-role Jira service accounts

    Provide **either** ``url`` (direct instance URL) **or**
    ``cloud_id`` (Atlassian Cloud ID — the gateway URL is built
    automatically).  Providing both or neither is an error.

    This section is **non-tool config only** — admin credentials for
    the org-level REST calls above plus the inbound webhook secret.  It
    lives under the top-level ``integrations`` block.  The Jira/Confluence
    MCP *tool* server is declared separately in ``mcp_servers`` (name it
    ``atlassian``); see ``docs/concepts/tool-capabilities.md``.

    Example YAML::

        integrations:
          jira:
            url: "https://mycompany.atlassian.net"
            token: "${JIRA_ADMIN_TOKEN}"
            email: "${JIRA_ADMIN_EMAIL}"
            webhook_secret: "${JIRA_WEBHOOK_SECRET}"

        mcp_servers:
          - name: atlassian
            shared: false
            command: uvx
            args: ["mcp-atlassian"]
            env: { JIRA_URL: "https://mycompany.atlassian.net" }
    """

    model_config = ConfigDict(extra="forbid")

    url: str = ""
    """Base URL of the Jira instance
    (e.g. ``https://mycompany.atlassian.net``).
    Mutually exclusive with ``cloud_id``."""

    cloud_id: str = ""
    """Atlassian Cloud ID. When provided, the base URL is
    constructed as
    ``https://api.atlassian.com/ex/jira/{cloud_id}``.
    Mutually exclusive with ``url``."""

    token: str
    """API token (Cloud) or Personal Access Token (Data Center)
    for the admin/service account."""

    email: str = ""
    """Email address for Jira Cloud Basic Auth.

    When provided, authentication uses ``Basic base64(email:token)``.
    When omitted, uses ``Bearer token`` (for service accounts and
    Data Center PATs).
    """

    webhook_secret: str = ""
    """Shared secret for webhook HMAC-SHA256 signature verification.

    If empty, webhook signatures are not verified.
    """

    @model_validator(mode="after")
    def _validate_url_or_cloud_id(self) -> JiraConfig:
        has_url = bool(self.url)
        has_cloud_id = bool(self.cloud_id)
        if has_url and has_cloud_id:
            raise ValueError("Provide either 'url' or 'cloud_id', not both")
        if not has_url and not has_cloud_id:
            raise ValueError(
                "Provide either 'url' (e.g. "
                "'https://mycompany.atlassian.net') or "
                "'cloud_id' (Atlassian Cloud ID)"
            )
        return self

    @property
    def base_url(self) -> str:
        """Resolved base URL for the Jira REST API."""
        if self.cloud_id:
            return f"{_ATLASSIAN_GATEWAY}/{self.cloud_id}"
        return self.url


class SlackStatusPhrases(BaseModel):
    """Per-phase overrides for the Slack working-status lines.

    Slack renders each line suffixed to the agent's name, so every phrase
    must complete that sentence: ``"is crewleting..."`` reads as "*Agent
    SWE is crewleting…*".  Each phase draws from its whole list — the
    engine picks one per phase and holds it for that phase's duration —
    so a list of one is a fixed label and a longer list gives the
    indicator some variety across turns.

    A phase you omit (or give an empty list) keeps the built-in pool from
    :mod:`crewlet.notifications.typing_status`.  ``default`` covers any
    phase the engine adds later that has no pool of its own.

    Keep every phrase generic to the phase.  The engine's pick is
    arbitrary — it never inspects what the agent is doing — so a line
    naming real work ("is checking Jira...") is a claim that is false most
    of the time it shows; see the phrase rule in
    :mod:`crewlet.notifications.typing_status`.

    Example YAML::

        integrations:
          slack:
            status_phrases:
              plan: ["is nimbusing...", "is thinking very hard..."]
              execute: ["is nimbusing it...", "is on the case..."]
    """

    model_config = ConfigDict(extra="forbid")

    onboarding: list[str] = Field(default_factory=list)
    plan: list[str] = Field(default_factory=list)
    execute: list[str] = Field(default_factory=list)
    review: list[str] = Field(default_factory=list)
    default: list[str] = Field(default_factory=list)

    @field_validator("onboarding", "plan", "execute", "review", "default")
    @classmethod
    def _reject_blank_phrases(cls, value: list[str]) -> list[str]:
        """A blank entry is a typo, not a phrase.

        An empty *list* is the documented "keep the built-in pool", but a
        list containing an empty string would silently post a cleared
        indicator — the exact opposite of the intent — so it fails
        loudly at config load instead.
        """
        cleaned = [phrase.strip() for phrase in value]
        if any(not phrase for phrase in cleaned):
            raise ValueError(
                "status phrases must be non-empty; omit the phase (or use "
                "[]) to keep the built-in pool"
            )
        return cleaned

    def as_status_phrases(self) -> StatusPhrases:
        """Resolve to the runtime phrase book (built-ins + overrides)."""
        return StatusPhrases.with_overrides(self.model_dump())


class SlackConfig(BaseModel):
    """Organisation-level Slack configuration.

    Every *credential* is per-agent (``role.integrations.slack``); this
    block is the transport-enable marker plus the genuinely org-wide
    Slack behaviour — ``typing_status`` and ``status_phrases``.
    Declaring this section under the top-level ``integrations`` block
    (even as ``slack: {}``) turns on the outbound
    :class:`SlackTransport` and per-agent webhook routing; omit it to
    disable Slack entirely.

    ``typing_status`` controls the "*<agent> is thinking…*" working
    indicator an agent shows while it reasons about a Slack message (see
    :mod:`crewlet.notifications.typing_status`):

    - ``addressed`` (default) — only when a human is plausibly waiting on
      this agent: a DM / group DM, a direct ``@mention``, or a thread the
      agent already follows.
    - ``always`` — on every Slack-triggered turn, including passive
      top-level channel messages and ``@here`` broadcasts.
    - ``off`` — disabled.

    ``status_phrases`` replaces the words it shows — see
    :class:`SlackStatusPhrases`.

    The Slack *tool* server is declared separately in ``mcp_servers``
    (name it ``slack``, ``shared: false``).  A Slack-enabled agent needs
    its bot token in **two** places — they are two different consumers:
    the transport identity (``role.integrations.slack``, whose
    ``signing_secret`` also verifies inbound webhooks) and the Slack MCP
    subprocess (``role.mcp_env.slack.SLACK_MCP_XOXB_TOKEN``).  Both
    reference the same ``${VAR}``, so there is no secret duplication.
    See ``docs/concepts/tool-capabilities.md``.

    Example YAML::

        integrations:
          slack:
            typing_status: addressed   # addressed | always | off
            status_phrases:            # optional — built-ins otherwise
              plan: ["is nimbusing..."]

        mcp_servers:
          - name: slack
            shared: false
            command: npm
            args: ["exec", "--yes", "--", "slack-mcp-server@latest",
                   "--transport", "stdio"]
            tool_prefix: "slack_"

        roles:
          - name: Engineer
            integrations:                 # per-agent transport identity
              slack:
                bot_token: "${SLACK_BOT_TOKEN_ENGINEER}"
                signing_secret: "${SLACK_SIGNING_SECRET_ENGINEER}"
            mcp_env:                       # Slack MCP server identity
              slack:
                SLACK_MCP_XOXB_TOKEN: "${SLACK_BOT_TOKEN_ENGINEER}"
    """

    model_config = ConfigDict(extra="forbid")

    typing_status: WorkingStatusMode = WorkingStatusMode.ADDRESSED
    status_phrases: SlackStatusPhrases = Field(default_factory=SlackStatusPhrases)


class MattermostProvisioningConfig(BaseModel):
    """Inputs for ``crewlet mattermost provision`` — consumed by the CLI
    only.  The engine never reads this block; it must parse so a
    provisioning-ready config validates today."""

    model_config = ConfigDict(extra="forbid")

    username_prefix: str = ""
    """Optional prefix prepended to each agent handle to form the
    Mattermost username (e.g. ``agent-`` when the server is shared with
    humans and a handle could collide with a person's username)."""

    channels: list[str] = Field(default_factory=list)
    """Channel *names* (the URL slug, not the display name) every agent
    bot is added to, on top of whatever ``role.integrations.mattermost.channel``
    names.  A bot only receives messages from channels it is a member of."""

    display_name_suffix: str = ""
    """Optional suffix appended to each bot's display name (e.g. ``" (AI)"``)
    so humans can tell agents from colleagues at a glance."""


class MattermostConfig(BaseModel):
    """Organisation-level Mattermost integration.

    The self-hosted, open-source chat backend.  Shaped like
    :class:`GitLabConfig` / :class:`PlaneConfig` — ``enabled`` plus a
    required ``url`` and ``team`` — with one structural difference that
    shapes the whole integration: **Mattermost has no usable inbound
    webhook.**

    Its outgoing webhooks fire only in *public* channels (the server
    gates on ``ChannelTypeOpen``), and their payload carries no
    ``root_id``, no channel type and no mention list — so DMs, private
    channels and thread attribution are all unreachable through them.
    The supported path for an external service is the **WebSocket event
    API**, authenticated as each bot.  The engine therefore holds one
    connection per Mattermost-enabled agent seat
    (:mod:`crewlet.mattermost.events`) instead of receiving HTTP
    callbacks, and there is no ``webhook_secret`` here to configure.

    That also means an agent seat needs only **one** credential — the
    bot's personal access token drives the websocket, the REST calls and
    the MCP tool server alike.

    ``typing_status`` defaults to ``off``, unlike Slack's ``addressed``.
    Mattermost's indicator has a fixed vocabulary ("*is typing…*"), so it
    conveys only *busy* where Slack's carries the phase, and it has to be
    re-asserted every few seconds against Slack's 45 — a multi-minute
    turn costs one to two orders of magnitude more requests for strictly
    less information.  Operators who still want it opt in; the per-phase
    ``status_phrases`` setting has deliberately no analogue here, because
    no text this backend accepts would ever be rendered.

    The Mattermost *tool* server is declared in ``mcp_servers`` as a
    ``shared: false`` server; each agent supplies the same token in
    ``role.mcp_env.mattermost``.  See ``docs/integrations/mattermost.md``.

    Example YAML::

        integrations:
          mattermost:
            enabled: true
            url: "https://chat.nimbus.example"   # instance base URL (required)
            team: nimbus                         # team slug (required)
            typing_status: off                   # off (default) | addressed | always
            provisioning:          # consumed ONLY by the CLI, ignored by
                                   #   the engine
              username_prefix: ""  # e.g. "agent-" if humans share the server
              channels: [town-square, engineering]   # channels every bot joins
              display_name_suffix: " (AI)"

        mcp_servers:
          - name: mattermost
            shared: false
            command: uvx
            args: ["mcp-server-mattermost"]
            env:
              MATTERMOST_URL: "https://chat.nimbus.example"
            tool_prefix: "mattermost_"

        roles:
          - name: Agent SWE
            integrations:
              mattermost:
                bot_token: "${MATTERMOST_TOKEN_SWE}"
                channel: engineering
            mcp_env:
              mattermost:
                MATTERMOST_TOKEN: "${MATTERMOST_TOKEN_SWE}"   # same token
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = False
    url: str = ""
    """Base URL of the Mattermost instance
    (e.g. ``https://chat.nimbus.example``).  Required when enabled; used
    for the websocket endpoint, REST calls and provisioning."""

    team: str = ""
    """Team slug agents belong to.  Required when enabled — channels are
    team-scoped, so the provisioner cannot place a bot without it."""

    typing_status: WorkingStatusMode = WorkingStatusMode.OFF
    """When to raise the composer typing indicator.  Defaults ``off`` —
    see the class docstring for why this differs from Slack."""

    provisioning: MattermostProvisioningConfig | None = None
    """Inputs for the provisioning CLI. Ignored by the engine."""

    @model_validator(mode="after")
    def _require_url_and_team(self) -> MattermostConfig:
        # Normalise once, here, rather than letting each consumer strip a
        # trailing slash its own way — the value is string-compared
        # against the server's own SiteURL as well as concatenated with
        # paths, and two consumers disagreeing about the canonical form
        # is a mismatch report about two identical addresses.
        from crewlet.mattermost.client import normalize_base_url

        normalised = normalize_base_url(self.url)
        if normalised != self.url:
            object.__setattr__(self, "url", normalised)
        if self.enabled:
            if not self.url:
                raise ValueError(
                    "mattermost.url is required when mattermost is enabled"
                )
            # A schemeless URL is the mistake that produces no useful
            # error anywhere: ``httpx`` rejects the base at the first
            # request and ``websockets`` rejects a URI with no ws scheme,
            # both long after ``crewlet validate`` has passed.  An
            # unresolved ``${VAR}`` is let through — the reference is
            # resolved later, and rejecting it here would forbid
            # configuring the URL from the environment.
            if (
                not self.url.startswith(("http://", "https://"))
                and "${" not in self.url
            ):
                raise ValueError(
                    f"mattermost.url must start with http:// or https:// "
                    f"(got {self.url!r}). It is the instance URL browsers "
                    f"use, e.g. https://chat.example.com"
                )
            if not self.team:
                raise ValueError(
                    "mattermost.team is required when mattermost is enabled"
                )
        return self

    @property
    def api_base(self) -> str:
        """The ``/api/v4`` REST base derived from ``url``."""
        return f"{self.url}/api/v4"

    @property
    def websocket_url(self) -> str:
        """The ``/api/v4/websocket`` endpoint, with the ws(s) scheme.

        Delegates to :func:`crewlet.mattermost.client.websocket_url` so
        the config model, the transport that builds the fleet and
        ``crewlet mattermost doctor`` all derive it the same way.
        """
        from crewlet.mattermost.client import websocket_url

        return websocket_url(self.url)


class GitHubConfig(BaseModel):
    """Organisation-level GitHub integration configuration — webhook side.

    This section is **non-tool config only**: it enables inbound GitHub
    webhook handling (``webhook_secret``) and the per-agent GitHub
    identity registration used to route those webhooks.

    The GitHub *tool* server (the remote Copilot MCP at
    ``https://api.githubcopilot.com/mcp/``) is declared in
    ``mcp_servers`` as a ``shared: false`` ``http`` server.  Each agent
    supplies its token in ``role.mcp_env.github`` as an ``Authorization:
    Bearer <token>`` header (the per-role ``http`` overrides become
    request headers).  See ``docs/concepts/tool-capabilities.md``.

    Example YAML::

        integrations:
          github:
            enabled: true
            webhook_secret: "${GITHUB_WEBHOOK_SECRET}"

        mcp_servers:
          - name: github
            transport: http
            shared: false
            url: "https://api.githubcopilot.com/mcp/"

        roles:
          - name: Senior Engineer
            mcp_env:
              github:
                Authorization: "Bearer ${GITHUB_TOKEN_ENGINEER}"
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = False
    webhook_secret: str | None = None

    @model_validator(mode="after")
    def _require_webhook_secret(self) -> GitHubConfig:
        if self.enabled and not self.webhook_secret:
            raise ValueError("github.webhook_secret is required when github is enabled")
        return self


class GitLabProvisioningConfig(BaseModel):
    """Inputs for ``crewlet gitlab provision`` — consumed by the CLI only.

    The engine never reads this block: it drives the reconcile that
    creates one service account per agent seat, adds group/project
    membership, mints per-agent PATs from the config's own ``${VAR}``
    references, and registers project webhooks. See
    ``docs/integrations/gitlab.md``.
    """

    model_config = ConfigDict(extra="forbid")

    group: str = ""
    """Top-level group (path or numeric id) the agent service accounts
    join. On GitLab.com a group Owner PAT is sufficient to create the
    accounts, their tokens, and memberships under this group."""

    access_level: str = "developer"
    """Default group membership level for each service account
    (``developer`` or ``maintainer``)."""

    access_levels: dict[str, str] = Field(default_factory=dict)
    """Per-handle membership overrides (``handle -> developer|maintainer``)."""

    username_prefix: str = ""
    """Optional prefix prepended to each agent handle to form the GitLab
    username (e.g. ``agent-`` when the group namespace is shared with
    humans)."""

    projects: list[str] = Field(default_factory=list)
    """Extra projects (path or numeric id) to add each service account to
    and to register webhooks on, beyond the group itself."""

    group_webhook: str = "auto"
    """``auto`` (one group hook when the instance accepts it — Premium —
    else per-project hooks), ``true`` (require a group hook), or ``false``
    (always per-project hooks)."""

    token_scopes: list[str] = Field(default_factory=lambda: ["api"])
    """Scopes minted on each service account PAT."""


class GitLabConfig(BaseModel):
    """Organisation-level GitLab integration configuration — webhook side.

    Symmetric with :class:`GitHubConfig`, with two differences: ``url``
    is **required** (webhook links, boot-time identity resolution via
    ``GET {url}/api/v4/user``, and provisioning all need the instance
    address), and inbound webhooks are verified by the GitLab **signing
    token** — a 19.1+ Standard-Webhooks HMAC over
    ``{webhook-id}.{webhook-timestamp}.{body}`` — so ``signing_secret``
    is **required** when enabled. (The weaker plain ``X-Gitlab-Token``
    scheme is intentionally unsupported; gitlab.com always runs ≥ 19.1
    and the docker-compose test instance runs ``gitlab-ee:latest``, so
    the signing token is always available. Self-managed GitLab older
    than 19.1 is not supported.)

    The GitLab *tool* server is declared in ``mcp_servers`` as a
    ``shared: false`` server; each agent supplies its service-account PAT
    in ``role.mcp_env.gitlab``. The default is the official ``glab`` CLI's
    stdio MCP server (``glab mcp serve``), with the token in the
    ``GITLAB_TOKEN`` env var — the engine's bridge spawns it per role, so
    there is no separate server to run. See ``docs/integrations/gitlab.md``.

    Example YAML::

        integrations:
          gitlab:
            enabled: true
            url: "https://gitlab.com"
            signing_secret: "${GITLAB_SIGNING_SECRET}"
            provisioning:
              group: nimbus-hq
              access_level: developer

        mcp_servers:
          - name: gitlab                       # official glab CLI MCP server (stdio)
            shared: false
            command: glab
            args: ["mcp", "serve"]

        roles:
          - name: Agent SWE
            mcp_env:
              gitlab:
                GITLAB_TOKEN: "${GITLAB_TOKEN_SWE}"
                GITLAB_HOST: gitlab.com
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = False
    url: str = ""
    """Base URL of the GitLab instance (e.g. ``https://gitlab.com``).
    Required when enabled; used for webhook links, ``GET /api/v4/user``
    identity resolution, and provisioning."""

    signing_secret: str | None = None
    """19.1+ signing token (``whsec_…``). Inbound webhooks are verified by
    the Standard-Webhooks HMAC-SHA256 over
    ``{webhook-id}.{webhook-timestamp}.{body}``. Required when enabled;
    the provisioner sets it as each hook's ``signing_token``."""

    token: str = ""
    """Optional read credential for **participants-based routing**.

    Webhook payloads carry assignees/reviewers but not the issue/MR
    *participants* list (author + assignees + reviewers + commenters +
    mentioned users), so mirroring GitLab's own notification semantics —
    everyone participating in a thread hears its activity — needs one
    ``GET …/participants`` REST call per comment / state-change event.
    Any PAT belonging to a group member with read access works
    (``read_api`` scope, Reporter+); the provisioner mints a dedicated
    ``crewlet-engine`` service account for the referenced ``${VAR}``
    automatically. When empty, routing degrades to the payload-derived
    targets only (assignees + explicit @mentions) — directed events are
    unaffected. This mirrors ``integrations.jira``'s admin token, which
    exists for the same reason (watcher lookups)."""

    provisioning: GitLabProvisioningConfig | None = None
    """Inputs for the provisioning CLI. Ignored by the engine."""

    @model_validator(mode="after")
    def _require_url_and_secret(self) -> GitLabConfig:
        if self.enabled:
            if not self.url:
                raise ValueError("gitlab.url is required when gitlab is enabled")
            if not self.signing_secret:
                raise ValueError(
                    "gitlab.signing_secret is required when gitlab is enabled"
                )
        return self

    @property
    def api_base(self) -> str:
        """The ``/api/v4`` REST base derived from ``url``."""
        return f"{self.url.rstrip('/')}/api/v4"


class PlaneProvisioningConfig(BaseModel):
    """Inputs for ``crewlet plane provision`` — consumed by the CLI only
    (slice 4).  The engine never reads this block; it must parse so a
    provisioning-ready config validates today."""

    model_config = ConfigDict(extra="forbid")

    role: str = "member"
    """Workspace role for agent service accounts
    (``admin`` | ``member`` | ``guest``)."""

    roles: dict[str, str] = Field(default_factory=dict)
    """Per-handle workspace-role overrides (``handle -> admin|member|guest``)."""

    username_prefix: str = ""
    """Optional prefix prepended to each agent handle to form the Plane
    username (e.g. ``agent-`` when the workspace is shared with humans)."""

    projects: list[str] = Field(default_factory=list)
    """Project identifiers the service accounts join (and that
    ``--create-projects`` creates)."""

    token_expiry_days: int = Field(default=364, ge=0)
    """Expiry minted on each service-account API token; ``0`` ⇒ the token
    NEVER expires (Plane semantics for an omitted/null ``expired_at``).
    Negative values are rejected — they would silently mean the same as
    ``0``, the inverse of a shorter-expiry intent."""


class PlaneConfig(BaseModel):
    """Organisation-level Plane integration — webhook + engine-read side.

    Symmetric with :class:`GitLabConfig`: ``enabled`` plus a required
    ``url`` when enabled, joined by ``workspace`` (every resource path the
    integration operates on is workspace-scoped — only ``GET /users/me/``
    and workspace creation aren't) and ``webhook_secret`` (the
    ``X-Plane-Signature`` HMAC-SHA256 key — the only verification mode
    Plane offers; generated *by* Plane at webhook creation and captured by
    the provisioner).

    The Plane *tool* server is declared in ``mcp_servers`` as a
    ``shared: false`` server; each agent supplies its service-account
    token in ``role.mcp_env.plane``.  See
    ``docs/integrations/plane.md``.

    Example YAML::

        integrations:
          plane:
            enabled: true
            url: "https://plane.nimbus.example"      # fork instance base URL (required)
            workspace: nimbus                        # workspace slug (required)
            webhook_secret: "${PLANE_WEBHOOK_SECRET}"  # X-Plane-Signature HMAC key —
                                                     #   generated BY Plane at webhook
                                                     #   creation; provisioner captures
                                                     #   it into this ${VAR}
            token: "${PLANE_ENGINE_TOKEN}"           # engine read credential
                                                     #   (subscribers, members, project
                                                     #   map, page fetch) — provisioned
                                                     #   crewlet-engine service account
            provisioning:            # consumed ONLY by the CLI, ignored by
                                     #   the engine
              role: member           # workspace role for agent accounts
                                     #   (admin|member|guest)
              roles: { tech-lead: admin }      # per-handle overrides
              username_prefix: ""    # e.g. "agent-" if the workspace is shared
              projects: [LEAD, ENG, PROD, TS]  # project identifiers: membership
                                               #   targets (+ created with
                                               #   --create-projects)
              token_expiry_days: 364 # omit expiry with 0 (Plane tokens:
                                     #   optional expiry)

        mcp_servers:
          - name: plane                       # official plane-mcp-server, stdio
            shared: false
            command: uvx
            args: ["plane-mcp-server", "stdio"]
            env:
              PLANE_BASE_URL: "https://plane.nimbus.example"
              PLANE_WORKSPACE_SLUG: "nimbus"

        roles:
          - name: Agent SWE
            mcp_env:
              plane:
                PLANE_API_KEY: "${PLANE_TOKEN_SWE}"   # per-agent service-account token
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = False
    url: str = ""
    """Base URL of the Plane fork instance
    (e.g. ``https://plane.nimbus.example``).  Required when enabled;
    used for webhook links, ``GET /api/v1/users/me/`` identity
    resolution, and provisioning."""

    workspace: str = ""
    """Workspace slug (e.g. ``nimbus``).  Required when enabled — every
    resource path the integration operates on is workspace-scoped."""

    webhook_secret: str | None = None
    """``X-Plane-Signature`` HMAC-SHA256 key.  Required when enabled (it
    is the only verification mode); generated by Plane at webhook
    creation and captured by the provisioner."""

    token: str = ""
    """Optional engine read credential (subscriber lookups, member /
    project resolution, webhook enrichment) — the provisioned
    ``crewlet-engine`` service account's token.  When empty, routing
    degrades to payload-derived targets only (assignees + explicit
    mentions) and the project id→identifier cache can only learn from
    ``project`` webhook payloads.  Mirrors ``integrations.gitlab.token``
    (participants lookups)."""

    provisioning: PlaneProvisioningConfig | None = None
    """Inputs for the provisioning CLI. Ignored by the engine."""

    @model_validator(mode="after")
    def _require_url_workspace_and_secret(self) -> PlaneConfig:
        if self.enabled:
            if not self.url:
                raise ValueError("plane.url is required when plane is enabled")
            if not self.workspace:
                raise ValueError("plane.workspace is required when plane is enabled")
            if not self.webhook_secret:
                raise ValueError(
                    "plane.webhook_secret is required when plane is enabled"
                )
        return self

    @property
    def api_base(self) -> str:
        """The ``/api/v1`` REST base derived from ``url``."""
        return f"{self.url.rstrip('/')}/api/v1"


class ConfluenceConfig(BaseModel):
    """Organisation-level Confluence configuration.

    Defines the admin / service account used for org-wide Confluence
    API access.  Individual agents authenticate via their own
    ``mcp_env`` credentials.

    Cloud webhooks are handled by the Forge app.  Data Center
    webhooks use ``webhook_secret`` for HMAC-SHA256 verification.

    Provide **either** ``url`` (direct instance URL) **or**
    ``cloud_id`` (Atlassian Cloud ID).  Providing both or neither
    is an error.

    This section is **non-tool config only** — admin credentials for
    org-level REST calls and the inbound webhook secret.  It lives under
    the top-level ``integrations`` block.  Org-wide knowledge spaces are
    configured in ``knowledge.confluence_spaces``, and the Confluence MCP
    *tool* server is declared in ``mcp_servers`` (name it ``atlassian``,
    shared with Jira); see ``docs/concepts/tool-capabilities.md``.

    Example YAML::

        integrations:
          confluence:
            url: "https://mycompany.atlassian.net/wiki"
            token: "${CONFLUENCE_ADMIN_TOKEN}"
            email: "${CONFLUENCE_ADMIN_EMAIL}"

        knowledge:
          confluence_spaces: ["HANDBOOK"]

        mcp_servers:
          - name: atlassian
            shared: false
            command: uvx
            args: ["mcp-atlassian"]
            env: { CONFLUENCE_URL: "https://mycompany.atlassian.net/wiki" }
    """

    model_config = ConfigDict(extra="forbid")

    url: str = ""
    """Base URL of the Confluence instance
    (e.g. ``https://mycompany.atlassian.net/wiki``).
    Mutually exclusive with ``cloud_id``."""

    cloud_id: str = ""
    """Atlassian Cloud ID. When provided, the base URL is
    constructed as
    ``https://api.atlassian.com/ex/confluence/{cloud_id}``.
    Mutually exclusive with ``url``."""

    site_url: str = ""
    """Human-readable site URL for constructing shareable links
    (e.g. ``https://mycompany.atlassian.net/wiki``).

    Only needed when using ``cloud_id`` (where the API gateway
    URL is not user-friendly).  When using ``url`` directly,
    this defaults to the ``url`` value."""

    token: str
    """API token (Cloud) or Personal Access Token (Data Center)
    for the admin/service account."""

    email: str = ""
    """Email address for Confluence Cloud Basic Auth.

    When provided, authentication uses ``Basic base64(email:token)``.
    When omitted, uses ``Bearer token`` (for Data Center PATs).
    """

    webhook_secret: str = ""
    """Shared secret for Data Center webhook HMAC-SHA256 verification."""

    @model_validator(mode="after")
    def _validate_url_or_cloud_id(self) -> ConfluenceConfig:
        has_url = bool(self.url)
        has_cloud_id = bool(self.cloud_id)
        if has_url and has_cloud_id:
            raise ValueError("Provide either 'url' or 'cloud_id', not both")
        if not has_url and not has_cloud_id:
            raise ValueError(
                "Provide either 'url' (e.g. "
                "'https://mycompany.atlassian.net/wiki') or "
                "'cloud_id' (Atlassian Cloud ID)"
            )
        return self

    @property
    def base_url(self) -> str:
        """Resolved base URL for the Confluence REST API."""
        if self.cloud_id:
            return f"{_ATLASSIAN_CONFLUENCE_GATEWAY}/{self.cloud_id}"
        return self.url

    @property
    def shareable_base_url(self) -> str:
        """Human-readable base URL for constructing shareable links.

        Returns ``site_url`` if set, otherwise falls back to ``url``.
        When using ``cloud_id`` without ``site_url``, returns empty
        string (shareable links cannot be constructed).
        """
        return self.site_url or self.url


class IntegrationsConfig(BaseModel):
    """External-system integrations — the inbound / notification side.

    Groups the **non-tool** integration config: the Atlassian (Jira +
    Confluence) admin accounts used for webhook routing and watcher
    lookups, the Slack transport marker, the GitHub / GitLab / Plane
    webhook configs, the Forge app id, and the outbound notification
    ``transports`` list.

    Agent *tools* are configured separately under
    :attr:`CompanyConfig.mcp_servers`; this block is purely how external
    events reach agents and how notifications are delivered.  It is the
    inbound counterpart to ``mcp_servers`` (outbound tool actions).
    """

    model_config = ConfigDict(extra="forbid")

    jira: JiraConfig | None = None
    confluence: ConfluenceConfig | None = None
    slack: SlackConfig | None = None
    mattermost: MattermostConfig | None = None
    github: GitHubConfig | None = None
    gitlab: GitLabConfig | None = None
    plane: PlaneConfig | None = None
    forge_app_id: str = ""
    """Forge app ID for verifying Forge Invocation Tokens (FIT).

    The Forge app sends a JWT in the ``Authorization: Bearer``
    header, verified against Atlassian's JWKS endpoint.  The
    ``aud`` claim must match this app ID.  Required when using
    the Forge app — the endpoint rejects all requests if this
    is not set.
    """


class KnowledgeConfig(BaseModel):
    """Shared-knowledge config (Tier B).

    Currently the org-wide Confluence spaces every agent can search (the
    Plan-phase ``## Relevant knowledge`` prefetch scope).  Distinct from
    the Tier A :class:`KnowledgeStoreConfig` (the vector-store backend
    bound to the database).
    """

    model_config = ConfigDict(extra="forbid")

    confluence_spaces: list[str] = Field(default_factory=list)
    """Org-wide Confluence read scope — the **only** thing that narrows
    the query-time ``## Relevant knowledge`` search.

    This list (and nothing else) becomes the ``space IN (...)`` clause of
    the search CQL — see
    :func:`~crewlet.knowledge.accessibility.accessible_spaces`.  It is
    **role-independent**: a unit's own Confluence space
    (``integrations.confluence.space``) is *integration identity*
    (webhook routing + write home) and deliberately does **not** scope
    reads.

    Empty by default and **optional**: with no spaces listed, an agent
    that authenticates with its own Atlassian credentials searches
    *unscoped* — Confluence's own page ACLs bound the results, so it sees
    every space its account can read.  (A credential-less agent, which
    falls back to the org admin account, searches nothing without an
    explicit scope, rather than reading the whole instance.)  Set this
    only to *narrow* the search to a curated set (e.g. a company handbook
    floor).  (Was ``confluence.org_spaces``.)"""

    plane_projects: list[str] = Field(default_factory=list)
    """Org-wide Plane read scope for the knowledge search (slice 3
    consumer).  The ``confluence_spaces`` analog: empty = unscoped,
    Plane membership/ACLs bound the search.  Materialised onto
    :attr:`~crewlet.org.models.Organization.plane_projects`.  Requires
    ``integrations.plane`` enabled (a scope list for a disabled backend
    is rejected — see the knowledge-backend exclusivity validator on
    :class:`CompanyConfig`)."""


class RoleSlackIdentity(BaseModel):
    """Per-agent Slack *transport* identity (one Slack app per agent).

    The credentials the :class:`SlackTransport` uses for this agent — its
    bot token (outbound Web API + ``send()`` fallback), signing secret
    (inbound webhook signature verification), and optional default
    channel.  This is **not** the Slack MCP credential: the Slack MCP
    server reads its own ``SLACK_MCP_XOXB_TOKEN`` from
    ``role.mcp_env.slack`` — name the same ``${VAR}`` in both (two
    consumers, one secret).
    """

    model_config = ConfigDict(extra="forbid")

    bot_token: str = ""
    signing_secret: str = ""
    channel: str = ""

    @model_validator(mode="after")
    def _validate_credential_shape(self) -> RoleSlackIdentity:
        """Reject credential shapes that are always authoring mistakes.

        - ``signing_secret`` without ``bot_token`` is dead config:
          ``register_slack_apps_from_org`` skips tokenless roles
          entirely, so the secret would verify nothing.
          (``bot_token`` WITHOUT a signing secret stays legal — an
          outbound-only app that never receives webhooks.)
        - When both are set, both must be the same *kind* — whole-value
          ``${VAR}`` placeholders (provisionable by ``crewlet slack
          provision``) or literals (a manually managed app).  A mixed
          pair leaves one credential the provisioner can't fill and
          surfaces later as a silent webhook-verification failure.
        """
        if self.signing_secret and not self.bot_token:
            raise ValueError(
                "role slack identity has a signing_secret but no bot_token "
                "— the app is never registered without a token, so the "
                "secret would verify nothing"
            )
        if (
            self.bot_token
            and self.signing_secret
            and bool(env_var_reference(self.bot_token))
            != bool(env_var_reference(self.signing_secret))
        ):
            raise ValueError(
                "bot_token and signing_secret must both be whole-value "
                "${VAR} placeholders or both be literal values — a mixed "
                "pair leaves one credential unprovisionable"
            )
        return self


#: Mattermost usernames are lowercase and limited to alphanumerics plus
#: ``.``, ``-`` and ``_``; the server rejects anything else outright.
#: Validated at config load so a bad username fails on the line that
#: authored it, not midway through a provisioning run that has already
#: created half the fleet.
_MATTERMOST_USERNAME_RE = re.compile(r"^[a-z0-9][a-z0-9._-]*$")


class RoleMattermostIdentity(BaseModel):
    """Per-agent Mattermost identity (one bot account per agent).

    Unlike Slack, a seat needs exactly **one** credential: the bot's
    personal access token authenticates the inbound websocket, the
    outbound REST calls and the Mattermost MCP server alike.  There is no
    signing secret because there is no inbound webhook to verify — see
    :class:`MattermostConfig`.

    Name the same ``${VAR}`` here and under
    ``role.mcp_env.mattermost.MATTERMOST_TOKEN``: two consumers, one
    secret, minted once by ``crewlet mattermost provision``.
    """

    model_config = ConfigDict(extra="forbid")

    bot_token: str = ""
    """The bot account's personal access token.  A whole-value
    ``${VAR}`` placeholder is what makes the seat provisionable; a
    literal marks a manually managed bot, reported and left untouched."""

    username: str = ""
    """Mattermost username of the bot account.  Defaults to the agent's
    handle (with ``provisioning.username_prefix`` applied).  Set it
    explicitly only when the account already exists under another name."""

    channel: str = ""
    """Optional default channel *name* for this agent's outbound
    notifications."""

    @field_validator("username")
    @classmethod
    def _validate_username(cls, value: str) -> str:
        """Reject usernames Mattermost itself would refuse.

        A ``${VAR}`` reference is allowed through: the value is resolved
        later, and rejecting the unresolved form here would forbid
        configuring the username from the environment.
        """
        if not value or env_var_reference(value):
            return value
        if not _MATTERMOST_USERNAME_RE.match(value):
            raise ValueError(
                f"invalid Mattermost username {value!r} — usernames must be "
                "lowercase and contain only letters, numbers, '.', '-' and "
                "'_', starting with a letter or number"
            )
        return value


class JiraIdentity(BaseModel):
    """The Jira project a unit / root-role owns.

    Used for inbound webhook routing — project activity with no better
    recipient routes to the unit lead (:func:`build_project_key_lead_map`)
    — and as the project agents file work under.  Not a tool credential
    (the Jira MCP server authenticates from ``mcp_env.atlassian`` and
    scopes via its own ``JIRA_PROJECTS_FILTER``) and irrelevant to
    knowledge reads.
    """

    model_config = ConfigDict(extra="forbid")

    project: str = ""


class ConfluenceIdentity(BaseModel):
    """The Confluence space a unit / root-role owns.

    Used for inbound webhook routing — page activity routes to the unit
    lead (:func:`build_space_key_lead_map`) — and as the team's write /
    skill-promotion home.  Not a tool credential, and **does not** scope
    knowledge reads: read scope is the org-wide
    :attr:`KnowledgeConfig.confluence_spaces` only.
    """

    model_config = ConfigDict(extra="forbid")

    space: str = ""


class PlaneIdentity(BaseModel):
    """The Plane project a unit / root-role owns.

    Integration identity only — the inbound webhook fallback-routing
    target (:func:`build_plane_project_lead_map`) and the project agents
    file work under.  Not a tool credential (the Plane MCP server
    authenticates from ``mcp_env.plane``), and it does **not** scope
    knowledge reads (read scope is the org-wide
    :attr:`KnowledgeConfig.plane_projects` only).
    """

    model_config = ConfigDict(extra="forbid")

    project: str = ""


class RoleIntegrationsConfig(BaseModel):
    """Per-agent integration identity — the inbound / transport side.

    The per-agent counterpart to the org-level ``integrations`` block:
    where an agent's *non-tool* identity for an external system lives —
    the Slack transport app (``slack``) and, for root-level roles, the
    Jira project / Confluence space / Plane project the role owns for
    webhook routing (``jira`` / ``confluence`` / ``plane``; units carry
    these too — see :class:`UnitIntegrationsConfig`).

    Per-agent *tool* credentials are configured separately under
    ``role.mcp_env`` (Atlassian / GitHub / Plane tokens, the Slack MCP's
    ``SLACK_MCP_XOXB_TOKEN``) — those go straight to the MCP server that
    consumes them.  The ``jira`` / ``confluence`` / ``plane`` identity
    here is **not** a tool credential and **does not** scope knowledge
    reads (read scope is the org-wide
    :attr:`KnowledgeConfig.confluence_spaces` /
    :attr:`KnowledgeConfig.plane_projects` only).
    """

    model_config = ConfigDict(extra="forbid")

    slack: RoleSlackIdentity | None = None
    mattermost: RoleMattermostIdentity | None = None
    jira: JiraIdentity | None = None
    confluence: ConfluenceIdentity | None = None
    plane: PlaneIdentity | None = None


class UnitIntegrationsConfig(BaseModel):
    """Per-unit integration identity — the Jira project + Confluence space
    + Plane project a unit owns.

    Drives inbound webhook routing (Jira project / Confluence space /
    Plane project activity with no better recipient routes to the unit
    lead — see :func:`build_project_key_lead_map` /
    :func:`build_space_key_lead_map` /
    :func:`build_plane_project_lead_map`) and serves as the team's write
    / skill-promotion home.  Slack at the unit level is the
    ``slack_channel`` field, so it is intentionally not part of this
    block.  No field here is a tool credential, and none scopes
    knowledge reads.
    """

    model_config = ConfigDict(extra="forbid")

    jira: JiraIdentity | None = None
    confluence: ConfluenceIdentity | None = None
    plane: PlaneIdentity | None = None


class RolePhaseLLMConfig(AuthoredModel):
    """Dict form of ``role.llm`` — one provider key per turn phase.

    The scalar form (``llm: fast``) points every phase at one named
    provider.  This mapping form splits them, with any omitted phase
    falling back to :attr:`default`.  Equivalent to the flat
    ``llm_plan`` / ``llm_execute`` / … fields on :class:`RoleConfig`;
    when both are given the flat field wins.  See
    ``docs/concepts/turn-engine.md`` § Per-phase LLM models.
    """

    model_config = ConfigDict(extra="forbid")

    default: str | list[str] = "default"
    plan: str | list[str] | None = None
    execute: str | list[str] | None = None
    review: str | list[str] | None = None
    subagent: str | list[str] | None = None
    auxiliary: str | list[str] | None = None
    judge: str | list[str] | None = None
    sandbox: str | list[str] | None = None


class RoleConfig(AuthoredModel):
    """The authored YAML shape of one seat — a ``roles:`` entry.

    This is the *wire format* a founder writes, not the runtime model:
    :func:`_parse_role` transforms it into a
    :class:`~crewlet.org.models.Role` (materialising
    ``integrations.slack`` into ``Role.slack``, collapsing the ``llm``
    dict form into per-phase fields, and mapping the integration
    identities onto ``jira_project`` / ``confluence_space`` /
    ``plane_project``).

    ``extra="forbid"``: an unrecognised key here is a typo, and a
    silently-ignored ``backstroy:`` would spawn the seat with an empty
    backstory and no diagnostic.  Fields are rejected at
    ``crewlet validate`` time instead.
    """

    model_config = ConfigDict(
        extra="forbid",
        # Mirrors ``Role._validate_kind_fields`` for schema-only
        # consumers — see the note on :class:`CompanyConfig`.  A human
        # seat with no way to reach the person is the most common
        # mistake here, and the one an editor can catch inline.
        json_schema_extra={
            "if": {
                "properties": {"kind": {"const": "human"}},
                "required": ["kind"],
            },
            "then": {
                "$comment": (
                    "A human seat is addressable-only and needs at least "
                    "one contact identity."
                ),
                "required": ["contact"],
            },
        },
    )

    name: str
    """Unique seat identity.  Also the source of the auto-derived
    :attr:`handle` — see the rename caveat there."""

    kind: RoleKind = RoleKind.AGENT
    """``agent`` (default) spawns a runtime seat; ``human`` marks an
    addressable-only :doc:`human seat </concepts/humans-in-the-org>`
    that requires at least one :attr:`contact` identity and rejects the
    runtime-only fields (validated on
    :class:`~crewlet.org.models.Role`)."""

    contact: HumanContact | None = None
    """Human seats — external identities (Slack / Atlassian / GitHub /
    GitLab / Plane) used to address the person."""

    availability: str = ""
    """Human seats — free-text availability rendered into rosters."""

    handle: str = Field(default="", pattern=r"^$|^[a-z0-9][a-z0-9-]*$")
    """Canonical identity slug; auto-derived from :attr:`name` when
    empty (the ``^$`` branch of the pattern).  Mirrors ``HANDLE_RE`` on
    :class:`~crewlet.org.models.Role`, declared here as a field
    constraint so the generated JSON Schema flags a malformed handle in
    the editor rather than only at load time.

    **Effectively permanent**: the agent's durable id is
    ``uuid5(ns, f"{org.name}:{handle}")``, so changing this (or the
    company name) orphans that seat's diary, onboarding markers, and
    counterparty profiles.  See
    :func:`~crewlet.db.agents.derive_agent_id`."""

    email: str = ""
    unit: str = ""
    """Home-unit reference for a root-level role — resolved into that
    unit's ``roles`` before parsing so the seat picks up the unit's
    ``mcp_env`` and lead auto-management.  Written by
    ``POST /config/roles``; hand-authored configs normally nest the
    role under the unit directly."""

    goal: str = ""
    backstory: str = ""
    responsibilities: list[str] = Field(default_factory=list)
    manages: list[str] = Field(default_factory=list)
    """Role names **or** unit names this seat manages; a unit name
    expands to every role in that unit and its descendants."""
    behavioral_guidelines: list[str] = Field(default_factory=list)
    token_budget: int = Field(default=0, ge=0)
    """Per-agent token ceiling (0 = unlimited).  Per-engine-run — the
    counter is in memory and resets on restart."""

    llm: str | list[str] | RolePhaseLLMConfig = "default"
    """Named ``providers.llm`` key (or the per-phase mapping form)."""
    llm_plan: str | list[str] | None = None
    llm_execute: str | list[str] | None = None
    llm_review: str | list[str] | None = None
    llm_subagent: str | list[str] | None = None
    llm_auxiliary: str | list[str] | None = None
    llm_judge: str | list[str] | None = None
    llm_sandbox: str | list[str] | None = None

    learning_enabled: bool | None = None
    mcp_env: dict[str, dict[str, str]] = Field(default_factory=dict)
    """Per-agent MCP credentials keyed by server name — env vars for
    ``stdio`` servers, HTTP headers for ``http`` servers.  Tool
    credentials only; the team's tracker/wiki identity lives under
    :attr:`integrations`."""

    sandbox: RoleSandboxConfig | None = None
    """Per-role :doc:`code sandbox </concepts/code-sandbox>` gate.
    Absent → the seat is never offered the ``run_sandbox`` tool."""

    placement: RolePlacementConfig | None = None
    """Which nodes in the fleet may run this seat (see
    :doc:`the fleet guide </guides/fleet>`). Absent → any node that runs
    seats, which is what a single-process deployment always means."""

    integrations: RoleIntegrationsConfig = Field(default_factory=RoleIntegrationsConfig)
    schedules: list[Schedule] = Field(default_factory=list)


class OrgUnitConfig(AuthoredModel):
    """The authored YAML shape of one ``units:`` entry (recursive).

    Transformed into an :class:`~crewlet.org.models.OrgUnit` by
    :func:`_parse_unit`.  Like :class:`RoleConfig` this forbids extra
    keys so a mistyped field fails validation rather than vanishing.
    """

    model_config = ConfigDict(extra="forbid")

    name: str
    type: str = "team"
    """Informational label — ``department`` / ``team`` / ``squad`` /
    ``pod`` / anything else.  Does not affect behaviour."""
    purpose: str = ""
    lead: str = ""
    """Role name leading this unit; inherited from the parent unit when
    empty.  The lead routes work and auto-manages otherwise-unmanaged
    direct members."""
    goals: list[str] = Field(default_factory=list)
    slack_channel: str = ""
    knowledge: list[str] = Field(default_factory=list)
    """Free-text knowledge references for this unit (materialised onto
    ``OrgUnit.knowledge_refs``).  Not a read scope — org-wide read
    scope is ``knowledge.confluence_spaces`` / ``knowledge.plane_projects``."""
    mcp_env: dict[str, dict[str, str]] = Field(default_factory=dict)
    """Per-agent MCP credentials inherited by this unit's direct roles
    (a role's own value for the same server key wins)."""
    integrations: UnitIntegrationsConfig = Field(default_factory=UnitIntegrationsConfig)
    roles: list[RoleConfig] = Field(default_factory=list)
    children: list[OrgUnitConfig] = Field(default_factory=list)
    schedules: list[Schedule] = Field(default_factory=list)


class CompanyConfig(AuthoredModel):
    """Tier B — founder-owned, live-editable, versioned in PostgreSQL.

    Everything that defines *what the company is and how its agents
    behave*: identity, providers, integrations, MCP servers, roles,
    units, learning + turn-engine settings, budgets, extensions.

    Stored as a JSONB document in the ``company_config`` table; one
    row marked active at a time.  Delivered to the engine via
    :meth:`Engine.apply_config` either at boot (from the active DB
    row) or at runtime (over Pulsar after a ``PUT /config``).

    See ``docs/concepts/configuration.md``.
    """

    model_config = ConfigDict(
        extra="forbid",
        # Second encoding of the knowledge-backend rules enforced by
        # ``_validate_knowledge_backend_exclusivity`` below, for
        # consumers that only have the JSON Schema: an editor, a CI
        # linter, or an AI authoring a config without crewlet installed
        # (docs/getting-started/ai-authoring.md).  Pydantic cannot
        # derive this from a model validator, so it is written out here
        # — deliberately adjacent to the validator it mirrors, with
        # ``test_json_schema_rules_agree_with_validators`` asserting the
        # two accept and reject exactly the same configs.
        json_schema_extra={
            "allOf": [
                {
                    "$comment": (
                        "The knowledge backend is single-homed: "
                        "integrations.confluence and an enabled "
                        "integrations.plane are mutually exclusive."
                    ),
                    "not": {
                        "properties": {
                            "integrations": {
                                "properties": {
                                    "confluence": {"type": "object"},
                                    "plane": {
                                        "type": "object",
                                        "properties": {"enabled": {"const": True}},
                                        "required": ["enabled"],
                                    },
                                },
                                "required": ["confluence", "plane"],
                            }
                        },
                        "required": ["integrations"],
                    },
                },
                {
                    "$comment": (
                        "knowledge.plane_projects requires an enabled "
                        "integrations.plane."
                    ),
                    "if": {
                        "properties": {
                            "knowledge": {
                                "properties": {
                                    "plane_projects": {"minItems": 1},
                                },
                                "required": ["plane_projects"],
                            }
                        },
                        "required": ["knowledge"],
                    },
                    "then": {
                        "properties": {
                            "integrations": {
                                "properties": {
                                    "plane": {
                                        "properties": {"enabled": {"const": True}},
                                        "required": ["enabled"],
                                    }
                                },
                                "required": ["plane"],
                            }
                        },
                        "required": ["integrations"],
                    },
                },
                {
                    "$comment": (
                        "knowledge.confluence_spaces is a scope list for "
                        "the Confluence backend; it is rejected when "
                        "integrations.plane is enabled."
                    ),
                    "if": {
                        "properties": {
                            "knowledge": {
                                "properties": {
                                    "confluence_spaces": {"minItems": 1},
                                },
                                "required": ["confluence_spaces"],
                            }
                        },
                        "required": ["knowledge"],
                    },
                    "then": {
                        "not": {
                            "properties": {
                                "integrations": {
                                    "properties": {
                                        "plane": {
                                            "properties": {"enabled": {"const": True}},
                                            "required": ["enabled"],
                                        }
                                    },
                                    "required": ["plane"],
                                }
                            },
                            "required": ["integrations"],
                        }
                    },
                },
            ]
        },
    )

    name: str
    mission: str = ""
    vision: str = ""
    policies: list[str] = Field(default_factory=list)
    integrations: IntegrationsConfig = Field(default_factory=IntegrationsConfig)
    """Inbound / notification integrations (Jira, Confluence, Slack,
    GitHub, Forge, outbound transports).  See :class:`IntegrationsConfig`.
    Agent *tools* live under :attr:`mcp_servers`."""
    knowledge: KnowledgeConfig = Field(default_factory=KnowledgeConfig)
    """Shared-knowledge config — org-wide Confluence search spaces.
    See :class:`KnowledgeConfig`."""
    skill_variables: dict[SkillVariableKey, str] = Field(
        default_factory=dict,
        # Pydantic renders a constrained key type as ``patternProperties``
        # alone, which only *types* matching keys — a non-matching
        # ``base-url`` stays unconstrained and passes.  Pairing it with
        # ``additionalProperties: false`` is what makes a JSON Schema
        # validator reject the key, matching what Pydantic itself does.
        json_schema_extra={"additionalProperties": False},
    )
    """Operator-defined ``name -> value`` map substituted into tool-skill
    text wherever a skill writes ``${name}``.

    Lets a static, Confluence-authored [tool skill](tool-skills.md)
    reference a per-org fact its body can't know — e.g. the tenant's
    Confluence/Jira base URL so agents share
    ``https://acme.atlassian.net/wiki/...`` links rather than the
    ``api.atlassian.com`` gateway URLs the MCP tools return.  The engine
    carries no integration-specific knowledge: operators name the
    variables, skills reference them.

    Keys must be substitution identifiers (``^[A-Za-z_][A-Za-z0-9_]*$``)
    so the operator key-space matches the ``${name}`` render grammar
    exactly.  Values support ``${ENV}`` resolution like the rest of
    Tier B config, and an empty/unset value leaves any ``${name}``
    reference un-substituted (visibly broken) rather than blanked.
    Values render into LLM prompts (and the event store / dashboard like
    any other prompt content) — treat them like a policy or backstory
    string and do not reference secrets.  Hot-reloadable."""
    providers: ProvidersConfig = ProvidersConfig()
    turn_engine: TurnEngineConfig = TurnEngineConfig()
    learning: LearningConfig = LearningConfig()
    scheduling: SchedulingConfig = SchedulingConfig()
    mcp_servers: list[MCPServerConfig] = Field(default_factory=list)

    token_budget: int = 0  # org-wide token budget (0 = unlimited)
    notification_rate_limit: int = 0
    notification_coalesce_window_seconds: float = Field(default=0.0, ge=0.0, le=60.0)
    """Linger window (seconds) for inbox batching: how long after the
    first pending event an idle agent's inbox waits to absorb a burst
    before dispatching.  ``0`` (default) adds no latency — backlog that
    piled up while the agent was busy still coalesces, because the
    drain collects everything already queued.  Capped at 60s: the
    window counts against the broker ack-timeout budget that bounds a
    drained message's unacked lifetime (collection + one turn must fit
    inside it), so an unbounded linger would reintroduce mid-drain
    redelivery.  Hot-reloadable.  See
    ``docs/concepts/event-system.md`` § Inbox batching."""
    notification_coalesce_max_batch: int = Field(default=20, ge=1)
    """Cap on events merged into one coalesced digest trigger.  A
    larger backlog arrives as successive capped batches rather than
    one unbounded megaprompt.  Hot-reloadable."""
    roles: list[RoleConfig] = Field(default_factory=list)
    """Root-level roles not scoped to any OrgUnit."""
    units: list[OrgUnitConfig] = Field(default_factory=list)
    extensions: list[dict[str, Any]] = Field(default_factory=list)

    @model_validator(mode="after")
    def _validate_skill_variable_keys(self) -> CompanyConfig:
        # Keys must be substitution identifiers so the operator key-space
        # is provably identical to the ``${name}`` render grammar in
        # ``crewlet.agent.skills.models`` — a key like ``base-url`` or
        # ``a.b`` would never be substituted, so reject it up front
        # rather than let it silently no-op at render time.  Imported
        # lazily to avoid an import cycle (config <- skills <- ...).
        from crewlet.agent.skills.models import SKILL_VARIABLE_KEY_PATTERN

        bad = [
            k for k in self.skill_variables if not SKILL_VARIABLE_KEY_PATTERN.match(k)
        ]
        if bad:
            raise ValueError(
                "skill_variables keys must be identifiers matching "
                f"[A-Za-z_][A-Za-z0-9_]* (offending: {', '.join(sorted(bad))})"
            )
        return self

    @model_validator(mode="after")
    def _validate_knowledge_backend_exclusivity(self) -> CompanyConfig:
        """The knowledge backend is single-homed: Confluence XOR Plane.

        Backend selection keys on integration-block *presence* (the scope
        lists default to empty = unscoped, so they can't be the signal).
        Enabling both is rejected — a Confluence→Plane migration is a
        cut-over — and a scope list for the disabled backend is rejected
        too.  A ``plane`` block with ``enabled: false`` alongside
        Confluence is allowed (inert), as is ``confluence_spaces``
        without an ``integrations.confluence`` block (today's behavior).
        """
        plane_active = self.integrations.plane is not None and (
            self.integrations.plane.enabled
        )
        confluence_active = self.integrations.confluence is not None
        if plane_active and confluence_active:
            raise ValueError(
                "integrations.confluence and integrations.plane are mutually "
                "exclusive — the knowledge backend is single-homed; a "
                "Confluence→Plane migration is a cut-over (disable one)"
            )
        if plane_active and self.knowledge.confluence_spaces:
            raise ValueError(
                "knowledge.confluence_spaces is a scope list for the "
                "disabled Confluence backend — remove it when "
                "integrations.plane is enabled (use knowledge.plane_projects)"
            )
        if confluence_active and self.knowledge.plane_projects:
            raise ValueError(
                "knowledge.plane_projects is a scope list for the disabled "
                "Plane backend — remove it when integrations.confluence is "
                "configured (use knowledge.confluence_spaces)"
            )
        if self.knowledge.plane_projects and not plane_active:
            raise ValueError(
                "knowledge.plane_projects requires integrations.plane enabled"
            )
        return self


def _slack_dict(cfg: RoleIntegrationsConfig) -> dict[str, str]:
    """Materialise ``role.integrations.slack`` into the ``role.slack``
    transport dict the runtime reads.

    No fan-out: the Slack MCP token is configured explicitly in
    ``mcp_env.slack``.
    """
    slack: dict[str, str] = {}
    if cfg.slack is not None:
        if cfg.slack.bot_token:
            slack["bot_token"] = cfg.slack.bot_token
        if cfg.slack.signing_secret:
            slack["signing_secret"] = cfg.slack.signing_secret
        if cfg.slack.channel:
            slack["channel"] = cfg.slack.channel
    return slack


def _mattermost_dict(cfg: RoleIntegrationsConfig) -> dict[str, str]:
    """Materialise ``role.integrations.mattermost`` into the
    ``role.mattermost`` transport dict the runtime reads.

    No fan-out: the Mattermost MCP token is configured explicitly in
    ``mcp_env.mattermost`` — the same ``${VAR}`` by convention, but the
    loader never writes one block from the other, so an operator who
    deliberately splits them (a read-only MCP token, say) keeps that
    split.
    """
    mattermost: dict[str, str] = {}
    if cfg.mattermost is not None:
        if cfg.mattermost.bot_token:
            mattermost["bot_token"] = cfg.mattermost.bot_token
        if cfg.mattermost.username:
            mattermost["username"] = cfg.mattermost.username
        if cfg.mattermost.channel:
            mattermost["channel"] = cfg.mattermost.channel
    return mattermost


def _integration_identities(
    cfg: RoleIntegrationsConfig | UnitIntegrationsConfig,
) -> tuple[str, str, str]:
    """Extract ``(jira_project, confluence_space, plane_project)`` from a
    validated integrations config — the unit's / root-role's integration
    *identity* (webhook routing + write home), distinct from MCP tool
    credentials and from knowledge read scope.  ``${VAR}`` refs are left
    verbatim and resolved at use time (Tier B values are not resolved at
    load)."""
    project = (cfg.jira.project if cfg.jira else "").strip()
    space = (cfg.confluence.space if cfg.confluence else "").strip()
    plane_project = (cfg.plane.project if cfg.plane else "").strip()
    return project, space, plane_project


def _parse_role(data: RoleConfig | dict[str, Any]) -> Role:
    """Transform an authored :class:`RoleConfig` into a runtime
    :class:`~crewlet.org.models.Role`.

    ``llm`` may be either a scalar (one provider key for every phase) or
    the :class:`RolePhaseLLMConfig` mapping form with ``plan`` /
    ``execute`` / ``review`` / ``subagent`` / ``auxiliary`` / ``judge``
    / ``sandbox`` keys — any missing phase falls back to ``default`` (if
    set) or to ``"default"``.  A flat ``llm_<phase>`` field wins over
    the same phase inside the mapping.

    Per-agent *tool* credentials live in ``mcp_env`` (one entry per
    ``shared: false`` MCP server — e.g. ``atlassian`` / ``github`` / the
    Slack MCP's ``SLACK_MCP_XOXB_TOKEN``).  The per-agent Slack
    *transport* identity lives under ``integrations.slack`` and is
    materialised into ``role.slack``.  A Slack-enabled agent names its
    bot token in both ``integrations.slack.bot_token`` and
    ``mcp_env.slack.SLACK_MCP_XOXB_TOKEN`` (same ``${VAR}``) — two
    consumers, one secret.

    A raw mapping is accepted and validated through :class:`RoleConfig`
    first, so callers holding un-parsed YAML get the same
    unknown-key rejection the config loader applies.
    """
    cfg = data if isinstance(data, RoleConfig) else RoleConfig.model_validate(data)

    slack = _slack_dict(cfg.integrations)
    mattermost = _mattermost_dict(cfg.integrations)
    jira_project, confluence_space, plane_project = _integration_identities(
        cfg.integrations
    )

    llm_plan = cfg.llm_plan
    llm_execute = cfg.llm_execute
    llm_review = cfg.llm_review
    llm_subagent = cfg.llm_subagent
    llm_auxiliary = cfg.llm_auxiliary
    llm_judge = cfg.llm_judge
    llm_sandbox = cfg.llm_sandbox

    if isinstance(cfg.llm, RolePhaseLLMConfig):
        llm_plan = llm_plan or cfg.llm.plan
        llm_execute = llm_execute or cfg.llm.execute
        llm_review = llm_review or cfg.llm.review
        llm_subagent = llm_subagent or cfg.llm.subagent
        llm_auxiliary = llm_auxiliary or cfg.llm.auxiliary
        llm_judge = llm_judge or cfg.llm.judge
        llm_sandbox = llm_sandbox or cfg.llm.sandbox
        llm = cfg.llm.default
    else:
        llm = cfg.llm

    return Role(
        name=cfg.name,
        kind=cfg.kind,
        contact=cfg.contact,
        availability=cfg.availability,
        handle=cfg.handle,
        email=cfg.email,
        unit=cfg.unit,
        goal=cfg.goal,
        backstory=cfg.backstory,
        responsibilities=list(cfg.responsibilities),
        manages=list(cfg.manages),
        behavioral_guidelines=list(cfg.behavioral_guidelines),
        token_budget=cfg.token_budget,
        llm=llm,
        llm_plan=llm_plan,
        llm_execute=llm_execute,
        llm_review=llm_review,
        llm_subagent=llm_subagent,
        llm_auxiliary=llm_auxiliary,
        llm_judge=llm_judge,
        llm_sandbox=llm_sandbox,
        learning_enabled=cfg.learning_enabled,
        sandbox=cfg.sandbox,
        placement=cfg.placement,
        mcp_env={k: dict(v) for k, v in cfg.mcp_env.items()},
        slack=slack,
        mattermost=mattermost,
        jira_project=jira_project,
        confluence_space=confluence_space,
        plane_project=plane_project,
        schedules=list(cfg.schedules),
    )


def _parse_unit(data: OrgUnitConfig | dict[str, Any]) -> OrgUnit:
    """Transform an authored :class:`OrgUnitConfig` into a runtime
    :class:`~crewlet.org.models.OrgUnit` (recursive).

    Supports nested ``children`` for arbitrary depth.  ``mcp_env`` on a
    unit is inherited by all direct roles (role values override / take
    priority) — per-agent MCP tool credentials shared across the unit.
    The unit's Jira project / Confluence space / Plane project *identity*
    (webhook routing + write home) is authored under
    ``integrations.jira.project`` / ``integrations.confluence.space`` /
    ``integrations.plane.project`` and materialised onto
    ``OrgUnit.jira_project`` / ``OrgUnit.confluence_space`` /
    ``OrgUnit.plane_project``; it is not an MCP env var and is not
    inherited into role ``mcp_env``.
    """
    cfg = (
        data if isinstance(data, OrgUnitConfig) else OrgUnitConfig.model_validate(data)
    )

    unit_mcp_env = {k: dict(v) for k, v in cfg.mcp_env.items()}
    roles = [_parse_role(r) for r in cfg.roles]

    # Inherit: unit mcp_env is the base, role overrides on top.
    if unit_mcp_env:
        for role in roles:
            for server, unit_vars in unit_mcp_env.items():
                role_vars = role.mcp_env.get(server, {})
                merged = {**unit_vars, **role_vars}
                role.mcp_env[server] = merged

    children = [_parse_unit(c) for c in cfg.children]

    jira_project, confluence_space, plane_project = _integration_identities(
        cfg.integrations
    )

    return OrgUnit(
        name=cfg.name,
        type=cfg.type,
        purpose=cfg.purpose,
        lead=cfg.lead,
        goals=list(cfg.goals),
        slack_channel=cfg.slack_channel,
        knowledge_refs=list(cfg.knowledge),
        roles=roles,
        children=children,
        mcp_env=unit_mcp_env,
        jira_project=jira_project,
        confluence_space=confluence_space,
        plane_project=plane_project,
        schedules=list(cfg.schedules),
    )


def parse_extensions(data: list[dict[str, Any]]) -> list[ExtensionConfig]:
    """Parse extensions configuration from YAML data."""
    extensions = []
    for item in data:
        for name, settings in item.items():
            ext_settings = settings if isinstance(settings, dict) else {}
            extensions.append(ExtensionConfig(name=name, settings=ext_settings))
    return extensions


def load_bootstrap_config(path: str | Path) -> BootstrapConfig:
    """Load and validate Tier A from a YAML file.

    Tier A carries only ops-owned, restart-only settings — DB DSN,
    Pulsar URL, API host/port/auth, debug.  Everything else lives in
    Tier B (the ``company_config`` DB table) and is read by the
    engine after Tier A boots its infrastructure.

    Args:
        path: Path to the Tier A YAML file (typically ``config.yaml``).

    Returns:
        Validated :class:`BootstrapConfig`.

    Raises:
        FileNotFoundError: If the file doesn't exist.
        ValueError: If the YAML is invalid or contains Tier B keys
            (which would silently be ignored — caught explicitly by
            ``extra="forbid"`` to surface mistakes).
    """
    config_path = Path(path)
    logger.info("loading_bootstrap_config", path=str(config_path))
    if not config_path.exists():
        msg = f"Bootstrap config file not found: {config_path}"
        raise FileNotFoundError(msg)

    with open(config_path) as f:
        raw = yaml.safe_load(f) or {}

    if not isinstance(raw, dict):
        msg = "Bootstrap config file must contain a YAML mapping"
        raise ValueError(msg)

    # Env-var interpolation runs on the bootstrap YAML at load time
    # because the resolved values (DSN, Pulsar URL, auth tokens) are
    # needed immediately to bring infrastructure up.  Tier B keeps
    # ``${VAR}`` strings verbatim in the DB; the engine resolves at
    # provider/transport construction time so ``crewlet config export``
    # never leaks secrets.
    #
    # ``use_store=False`` is the root-of-trust boundary: this file carries
    # the DB DSN and the encryption keyring, i.e. exactly what is needed
    # to *open and decrypt* the secret store. Tier A can therefore never
    # source a value from it, and saying so explicitly keeps that true
    # even if boot ordering is ever rearranged.
    resolved = _resolve_env_recursive(raw, use_store=False)
    config = BootstrapConfig.model_validate(resolved)
    logger.debug("bootstrap_config_loaded_and_validated")
    return config


def load_company_config(path: str | Path) -> CompanyConfig:
    """Load and validate Tier B from a YAML file.

    Used by ``crewlet config import`` to populate the
    ``company_config`` DB table from a YAML on disk.  At runtime the
    engine reads Tier B from the DB, never from a YAML file.

    Args:
        path: Path to a Tier B YAML file (typically ``company.yaml``).

    Returns:
        Validated :class:`CompanyConfig`.

    Raises:
        FileNotFoundError: If the file doesn't exist.
        ValueError: If the YAML is invalid, missing ``name``, or
            contains Tier A keys (caught via ``extra="forbid"``).

    Note:
        ``${VAR}`` references are preserved verbatim in the loaded
        :class:`CompanyConfig` and in the DB-stored payload.
        Resolution happens at provider / transport / handle-resolver
        construction time inside the engine builders.  This keeps the
        in-memory ``CompanyConfig`` (and any DB snapshot or YAML
        export) free of resolved secrets — see
        ``docs/concepts/configuration.md`` § Secrets.
    """
    config_path = Path(path)
    logger.info("loading_company_config", path=str(config_path))
    if not config_path.exists():
        msg = f"Company config file not found: {config_path}"
        raise FileNotFoundError(msg)

    with open(config_path) as f:
        raw = yaml.safe_load(f)

    if not isinstance(raw, dict):
        msg = "Company config file must contain a YAML mapping"
        raise ValueError(msg)

    if "name" not in raw:
        msg = "Company config file must have a 'name' field"
        raise ValueError(msg)

    config = CompanyConfig.model_validate(raw)
    logger.debug("company_config_loaded_and_validated")
    return config


def _find_unit_dict_by_name(
    units: list[OrgUnitConfig], name: str
) -> OrgUnitConfig | None:
    """Depth-first lookup for a unit by name (incl. children)."""
    for unit in units:
        if unit.name == name:
            return unit
        found = _find_unit_dict_by_name(unit.children, name)
        if found is not None:
            return found
    return None


def _attach_root_roles_to_units(
    root_roles: list[RoleConfig], units: list[OrgUnitConfig]
) -> list[RoleConfig]:
    """Move root roles carrying a ``unit:`` reference into the named
    unit's ``roles`` list **before** the unit is parsed.

    Roles added via ``POST /config/roles`` live at the
    :class:`CompanyConfig.roles` root but may carry a ``unit:`` reference
    to their organisational home.  Without resolution, the role misses
    the unit's ``mcp_env`` inheritance (so per-role MCP instances never
    spawn) and ``unit.get_lead()`` can't see it (so Confluence
    space-key routing fails).

    Crucially the move happens on the **authored** :class:`RoleConfig` /
    :class:`OrgUnitConfig`, before :func:`_parse_unit` constructs each
    :class:`OrgUnit` and its ``_validate_lead`` model-validator runs.
    Appending the role to an already-parsed ``OrgUnit.roles`` would run
    *after* lead auto-management, so the unit lead would never gain a
    ``manages`` entry for the moved role — silently orphaning it from
    the hierarchy.  Moving it first lets ``_parse_unit`` apply both
    ``mcp_env`` inheritance and lead auto-management uniformly.

    Roles whose ``unit`` is empty or names a non-existent unit are kept
    at root.  A warning is logged for unresolved references so the
    operator sees the mismatch in the engine startup log.
    """
    remaining: list[RoleConfig] = []
    for role in root_roles:
        if not role.unit:
            remaining.append(role)
            continue
        target = _find_unit_dict_by_name(units, role.unit)
        if target is None:
            logger.warning(
                "role_unit_ref_not_found",
                role=role.name,
                unit=role.unit,
            )
            remaining.append(role)
            continue
        target.roles.append(role)
    return remaining


def config_to_organization(config: CompanyConfig) -> Organization:
    """Convert a CompanyConfig into an Organization model."""
    logger.debug("converting_config_to_org")

    # Resolve ``role.unit`` references on root roles BEFORE parsing the
    # units so that the OrgUnit lead-validation (auto-manage) and the
    # unit ``mcp_env`` inheritance both see the moved roles as direct
    # members.  Work on deep copies so the source CompanyConfig (kept as
    # ``self._active_config`` / a DB revision payload) is never mutated.
    root_role_configs = [r.model_copy(deep=True) for r in config.roles]
    unit_configs = [u.model_copy(deep=True) for u in config.units]
    root_role_configs = _attach_root_roles_to_units(root_role_configs, unit_configs)

    root_roles = [_parse_role(r) for r in root_role_configs]
    units = [_parse_unit(u) for u in unit_configs]

    org = Organization(
        name=config.name,
        mission=config.mission,
        vision=config.vision,
        policies=config.policies,
        roles=root_roles,
        units=units,
        token_budget=config.token_budget,
        confluence_spaces=list(config.knowledge.confluence_spaces),
        plane_projects=list(config.knowledge.plane_projects),
    )
    total_roles = len(org.all_roles())
    logger.debug(
        "org_parsed",
        name=config.name,
        units=len(units),
        root_roles=len(root_roles),
        total_roles=total_roles,
    )
    return org


def load_organization(path: str | Path) -> Organization:
    """Load a Tier B YAML file and return the Organization model.

    Convenience function combining :func:`load_company_config` and
    :func:`config_to_organization`.  Used by ``crewlet validate``
    and by examples; runtime code goes through ``CompanyConfigStore``
    → ``apply_config``.
    """
    logger.info("loading_organization", path=str(path))
    config = load_company_config(path)
    return config_to_organization(config)


def create_llm_providers(
    providers_config: ProvidersConfig,
) -> dict[str, Any]:
    """Instantiate LLM providers from config.

    Returns a dict mapping provider keys (e.g. "default", "budget")
    to LLMProvider instances.
    """
    from crewlet.providers.llm.openai import OpenAIProvider

    provider_names = list(providers_config.llm.keys())
    logger.info(
        "creating_llm_providers",
        providers=", ".join(
            f"{k} ({providers_config.llm[k].type})" for k in provider_names
        ),
    )
    result: dict[str, Any] = {}
    # The conventional env var the provider falls back to when
    # ``api_keys`` is empty -- keyed by provider type.
    _conventional_env = {
        "openai": "OPENAI_API_KEY",
        "openai-compatible": "OPENAI_API_KEY",
        "anthropic": "ANTHROPIC_API_KEY",
    }
    for key, cfg in providers_config.llm.items():
        # Resolve env-var references on each key. An empty
        # list is fine *only* when the provider's conventional env var
        # (``OPENAI_API_KEY`` / ``ANTHROPIC_API_KEY``) is set; otherwise
        # fail fast here with a clear message rather than letting a
        # keyless client surface an AuthenticationError deep inside the
        # first turn's first phase.
        resolved_keys = [
            str(k) for k in (_resolve_env_value(k) for k in cfg.resolved_keys()) if k
        ]
        if cfg.type == "cli-agent":
            # No API key by construction: this backend authenticates
            # with the operator's CLI subscription, so the key check
            # below would reject exactly the configuration the feature
            # exists to support.  Its own credential validation lives in
            # ``_create_cli_agent_provider`` / ``crewlet llm doctor``.
            result[key] = _create_cli_agent_provider(key, cfg, resolved_keys)
            continue
        if not resolved_keys:
            env_var = _conventional_env.get(cfg.type, "")
            # Same two sources the provider constructor will use, so this
            # check can't reject a key the provider would have found — a
            # credential held only in the secret store counts as set.
            if not env_var or not resolve_env(env_var):
                raise ValueError(
                    f"LLM provider '{key}' (type '{cfg.type}') has no API key: "
                    f"providers.llm.{key}.api_keys is empty and the conventional "
                    f"credential {env_var or '(none for this type)'} is not in "
                    "the environment or the secret store. Set api_keys in the "
                    "config, export the env var, or `crewlet secrets set` it."
                )
        base_url = str(_resolve_env_value(cfg.base_url)) if cfg.base_url else ""
        cooldown_kwargs: dict[str, Any] = {
            "rate_limit_seconds": cfg.cooldowns.rate_limit_seconds,
            "auth_seconds": cfg.cooldowns.auth_seconds,
        }
        if cfg.type in ("openai", "openai-compatible"):
            kwargs: dict[str, Any] = {
                "model": cfg.model,
                "api_keys": resolved_keys,
                "cooldowns": cooldown_kwargs,
                "timeout": cfg.timeout_seconds,
            }
            if base_url:
                kwargs["base_url"] = base_url
            if cfg.reasoning:
                kwargs["reasoning"] = True
                kwargs["reasoning_effort"] = cfg.reasoning_effort
            result[key] = OpenAIProvider(**kwargs)
            logger.debug(
                "openai_provider_created",
                key=key,
                model=cfg.model,
                key_count=len(resolved_keys),
            )
        elif cfg.type == "anthropic":
            from crewlet.providers.llm.anthropic import AnthropicProvider

            kwargs = {
                "model": cfg.model,
                "api_keys": resolved_keys,
                "cooldowns": cooldown_kwargs,
                "timeout": cfg.timeout_seconds,
            }
            if base_url:
                kwargs["base_url"] = base_url
            if cfg.reasoning:
                kwargs["reasoning"] = True
                kwargs["reasoning_budget_tokens"] = cfg.reasoning_budget_tokens
            result[key] = AnthropicProvider(**kwargs)
            logger.debug(
                "anthropic_provider_created",
                key=key,
                model=cfg.model,
                key_count=len(resolved_keys),
            )
        else:
            # Unreachable from a validated config (``type`` is a
            # ``Literal``), but this function is callable directly with a
            # hand-built ProvidersConfig.  Without the raise, an
            # unrecognised type falls through both branches and the key
            # is simply MISSING from the result — no provider, no error,
            # and every role naming it left without an LLM.  Mirrors the
            # raise that ``create_embedding_provider`` already had.
            raise ValueError(
                f"Unsupported LLM provider type: {cfg.type!r} (provider "
                f"{key!r}). Supported types: 'openai', 'anthropic', "
                "'openai-compatible', 'cli-agent'."
            )
    return result


def _create_cli_agent_provider(
    key: str, cfg: LLMProviderConfig, resolved_keys: list[str]
) -> Any:
    """Build one ``cli-agent`` provider, restoring its login if needed.

    Split out of :func:`create_llm_providers` because it has a real
    side effect the other branches do not: when the provider's
    credential directory is empty and a bundle is reachable, the login
    is materialised onto disk here.  That is what lets an engine in an
    ephemeral container come up already authenticated instead of every
    seat failing its first turn with "not logged in".
    """
    from crewlet.providers.llm.cli_agent import CLIAgentProvider
    from crewlet.providers.llm.cli_login import bundle_env_name, restore_from_secret

    assert cfg.cli is not None  # guaranteed by LLMProviderConfig validation
    cli = cfg.cli
    profile = cli.profile()

    # The subscription token: the explicit ``${VAR}`` reference if the
    # operator wrote one, else the profile's own conventional variable
    # (``CLAUDE_CODE_OAUTH_TOKEN``) from the secret store / environment
    # — same shape as the API-key providers' conventional fallback.
    token = str(_resolve_env_value(cli.auth.token)) if cli.auth.token else ""
    if not token and profile.token_env:
        token = resolve_env(profile.token_env)

    provider = CLIAgentProvider(
        provider_key=key,
        profile=profile,
        model=cfg.model,
        state_dir=str(_resolve_env_value(cli.state_dir)) if cli.state_dir else "",
        timeout=cli.timeout_seconds,
        max_concurrent=cli.max_concurrent,
        auth_mode=cli.auth.mode,
        api_key=resolved_keys[0] if resolved_keys else "",
        subscription_token=token,
        env={k: str(_resolve_env_value(v)) for k, v in cli.env.items()},
    )

    bundle = (
        str(_resolve_env_value(cli.auth.credential_bundle))
        if cli.auth.credential_bundle
        else resolve_env(bundle_env_name(key))
    )
    if bundle:
        restore_from_secret(provider.workspace, profile, bundle)

    logger.debug(
        "cli_agent_provider_created",
        key=key,
        agent=profile.name,
        model=cfg.model,
        auth_mode=cli.auth.mode,
        state_dir=str(provider.workspace.state_dir),
        logged_in=provider.workspace.has_credentials() or bool(token),
    )
    return provider


def create_embedding_provider(
    config: EmbeddingProviderConfig,
) -> Any:
    """Instantiate an embedding provider from config.

    Returns an EmbeddingProvider implementation based on ``config.type``.
    Currently supports ``openai`` (also works with any OpenAI-compatible
    endpoint via ``base_url``).
    """
    api_key = str(_resolve_env_value(config.api_key)) if config.api_key else ""
    base_url = str(_resolve_env_value(config.base_url)) if config.base_url else ""

    logger.info(
        "creating_embedding_provider",
        type=config.type,
        model=config.model,
    )

    if config.type in ("openai", "openai-compatible"):
        from crewlet.providers.embeddings.openai import OpenAIEmbeddingProvider

        kwargs: dict[str, Any] = {
            "model": config.model,
            "dimensions": config.dimensions,
        }
        if api_key:
            kwargs["api_key"] = api_key
        if base_url:
            kwargs["base_url"] = base_url
        provider = OpenAIEmbeddingProvider(**kwargs)
        logger.debug(
            "openai_embedding_provider_created",
            model=config.model,
            dimensions=provider.dimensions,
        )
        return provider

    raise ValueError(
        f"Unsupported embedding provider type: {config.type!r}. "
        "Supported types: 'openai', 'openai-compatible'."
    )


def load_extensions_from_config(
    extension_configs: list[ExtensionConfig],
) -> list[Any]:
    """Instantiate extensions from config.

    Supports external extension packages.  The REST API is not an
    extension — it runs as its own process via ``crewlet run api``.
    """
    logger.info("loading_extensions", count=len(extension_configs))
    extensions: list[Any] = []
    for ext_cfg in extension_configs:
        if ext_cfg.name == "crewlet_api":
            logger.warning(
                "crewlet_api_extension_removed",
                hint="Use 'crewlet run api <config>' to start the standalone API",
            )
        else:
            # Try importing as a Python module
            try:
                import importlib

                mod = importlib.import_module(ext_cfg.name)
                if hasattr(mod, "Extension"):
                    ext_cls = mod.Extension
                    extensions.append(ext_cls(**ext_cfg.settings))
                elif hasattr(mod, "create_extension"):
                    extensions.append(mod.create_extension(**ext_cfg.settings))
            except ImportError as exc:
                logger.warning(
                    "extension_import_failed",
                    extension=ext_cfg.name,
                    error=str(exc),
                )
    return extensions


def parse_mcp_servers(
    raw: Sequence[MCPServerConfig | dict[str, Any]],
) -> list[MCPServerConfig]:
    """Normalise an ``mcp_servers`` list to :class:`MCPServerConfig`.

    :attr:`CompanyConfig.mcp_servers` is already typed, so entries
    arriving from a validated config pass straight through; raw mappings
    (a hand-built list in a test or an extension) are validated here.
    """
    configs = [
        item if isinstance(item, MCPServerConfig) else MCPServerConfig(**item)
        for item in raw
    ]
    logger.debug("mcp_servers_parsed", count=len(configs))
    return configs


# The one ``${VAR}`` grammar, shared with the redaction registry, the
# provisioning sinks, and ``Role.contact`` — see ``crewlet.env_refs`` for
# why it must not be re-spelled per module. Re-exported under the old
# private name because call sites across the engine import it from here.
_ENV_VAR_PATTERN = ENV_VAR_PATTERN


def _resolve_env_value(value: Any, *, use_store: bool = True) -> Any:
    """Resolve ``${VAR}`` references in a string value.

    Substitutes **every** ``${VAR}`` occurrence, so both a whole-value
    reference (``"${TOKEN}"``) and an embedded one (``"Bearer ${TOKEN}"``
    — used for per-role HTTP ``Authorization`` headers) resolve.
    Non-string values pass through unchanged.

    Two sources, in order: the process-wide **secret store** (the
    encrypted ``secret_values`` table, loaded once at boot) and then
    ``os.environ`` (empty string when unset).  Store-first is deliberate —
    env-first would let a stale ``.env`` shadow a freshly rotated secret,
    which is exactly the failure the store exists to remove.  With no
    store installed this is byte-for-byte the previous ``os.getenv``
    behaviour, so every deployment that has never stored a secret is
    unaffected.

    ``use_store=False`` forces the environment-only path.  That is the
    **root of trust boundary**: the Tier A bootstrap carries the DB DSN
    and the encryption keyring, and the store cannot answer for values
    needed to open and decrypt the store itself.  Passing the flag makes
    that circularity structural rather than a happy accident of boot
    ordering.
    """
    if not isinstance(value, str) or "${" not in value:
        return value

    def _sub(match: re.Match[str]) -> str:
        var_name = match.group(1)
        logger.debug("resolving_env_var", var_name=var_name)
        if use_store:
            stored = lookup_secret(var_name)
            if stored is not None:
                return stored
        return os.getenv(var_name, "")

    return _ENV_VAR_PATTERN.sub(_sub, value)


def resolve_env_vars(env: dict[str, str]) -> dict[str, str]:
    """Resolve ``${VAR}`` references in env dicts (secret store, then env)."""
    logger.debug("resolving_env_vars", count=len(env))
    return {k: str(_resolve_env_value(v)) for k, v in env.items()}


def resolve_env_scalar(value: str) -> str:
    """Resolve ``${VAR}`` references in a single config string.

    The scalar counterpart to :func:`resolve_env_vars`, used for Tier B
    values stored verbatim and resolved at use time (e.g. a unit's
    ``jira_project`` / ``confluence_space`` identity)."""
    return str(_resolve_env_value(value or ""))


def env_reference_is_resolvable(value: str) -> bool:
    """True when every ``${VAR}`` in ``value`` resolves to something non-empty.

    The presence check callers need before launching a subprocess with a
    credential: an embedded ``"Bearer ${TOKEN}"`` resolves to the
    truthy-but-broken ``"Bearer "`` when the var is unset, so testing the
    resolved value would miss exactly the composite shapes config allows.
    Consults the same two sources as :func:`_resolve_env_value`, so a
    secret that lives only in the store does not read as missing.
    """
    if not isinstance(value, str):
        return True
    return all(
        bool(_resolve_env_value("${" + name + "}"))
        for name in _ENV_VAR_PATTERN.findall(value)
    )


def _resolve_env_recursive(data: Any, *, use_store: bool = True) -> Any:
    """Recursively resolve ``${VAR}`` references in nested dicts/lists."""
    if isinstance(data, dict):
        return {
            k: _resolve_env_recursive(v, use_store=use_store) for k, v in data.items()
        }
    if isinstance(data, list):
        return [_resolve_env_recursive(item, use_store=use_store) for item in data]
    return _resolve_env_value(data, use_store=use_store)


def resolve_setup_step_content(step: SandboxSetupStep) -> SandboxSetupStep:
    """Resolve ``${VAR}`` references in a setup step's files + commands ONLY.

    A step's ``env`` is deliberately left verbatim: it is resolved exactly
    once, together with the rest of the sandbox env, in
    ``build_sandbox_env`` — same semantics as ``role.sandbox.env``.
    Resolving it here too would double-resolve, silently mangling a secret
    whose real value contains a literal ``${...}``. ``brief`` and ``name``
    are never resolved (agent-facing text / an identifier — substituting
    engine-host env into them would be surprising at best).
    """
    return step.model_copy(
        update={
            "files": {k: str(_resolve_env_value(v)) for k, v in step.files.items()},
            "commands": [str(_resolve_env_value(c)) for c in step.commands],
        }
    )


def register_slack_apps_from_org(
    transport: Any,
    org: Any,
) -> None:
    """Register per-agent Slack apps on a SlackTransport from org roles.

    Scans all roles in the organization for ``slack`` config dicts
    and calls ``transport.register_app()`` for each one. Environment
    variable references (``${VAR}``) are resolved.

    Args:
        transport: A SlackTransport instance.
        org: An Organization model with roles that may have ``slack`` config.
    """
    from crewlet.notifications.transports.slack import SlackAppConfig

    for role in org.all_roles():
        if not role.slack:
            continue
        resolved = resolve_env_vars(role.slack)
        bot_token = resolved.get("bot_token", "")
        if not bot_token:
            logger.warning("slack_bot_token_empty", role=role.name)
            continue
        handle = role.get_handle()
        config = SlackAppConfig(
            bot_token=bot_token,
            signing_secret=resolved.get("signing_secret", ""),
            channel=resolved.get("channel", ""),
        )
        transport.register_app(handle, config)
        logger.debug("slack_app_registered", handle=handle)


def register_mattermost_bots_from_org(
    transport: Any,
    org: Any,
) -> None:
    """Register per-agent Mattermost bots on a MattermostTransport.

    Scans all roles for materialised ``mattermost`` config dicts and
    calls ``transport.register_bot()`` for each.  ``${VAR}`` references
    are resolved here; the username defaults to the agent's handle,
    which is also what the provisioner names the account.

    Args:
        transport: A MattermostTransport instance.
        org: An Organization whose roles may carry ``mattermost`` config.
    """
    from crewlet.notifications.transports.mattermost import MattermostBotConfig

    for role in org.all_roles():
        if not role.mattermost:
            continue
        resolved = resolve_env_vars(role.mattermost)
        bot_token = resolved.get("bot_token", "")
        handle = role.get_handle()
        if not bot_token:
            logger.warning("mattermost_bot_token_empty", role=role.name)
            continue
        config = MattermostBotConfig(
            bot_token=bot_token,
            username=resolved.get("username", "") or handle,
            channel=resolved.get("channel", ""),
        )
        transport.register_bot(handle, config)
        logger.debug("mattermost_bot_registered", handle=handle)


def build_project_key_lead_map(
    org: Organization,
) -> dict[str, str]:
    """Build a mapping from Jira project key to agent handle.

    Scans two sources for the project *identity*
    (``integrations.jira.project`` → ``jira_project``):

    1. **OrgUnit ``jira_project``** — maps the project key to the unit
       lead's handle.
    2. **Root-level role ``jira_project``** — maps the project key
       directly to the role's handle (root roles have no unit).

    If multiple sources share the same project key, the first one wins;
    a warning is logged ONLY when the sources resolve to DIFFERENT
    leads — several units sharing one project under one lead is the
    documented Jira-parity pattern and stays silent.

    Returns:
        ``{project_key: handle}`` for all discovered mappings.
    """
    # (label, handle) tuples grouped by normalised project key
    key_to_sources: dict[str, list[tuple[str, str]]] = {}

    # 1. Unit-level: project key → unit lead
    for unit in org.all_units():
        project_key = resolve_env_scalar(unit.jira_project)
        if not project_key:
            continue
        lead = get_effective_lead(unit, org)
        if lead is None:
            logger.warning(
                "jira_project_key_no_lead",
                unit=unit.name,
                project_key=project_key,
            )
            continue
        lead_handle = lead.get_handle()
        norm_key = project_key.upper()
        key_to_sources.setdefault(norm_key, []).append((unit.name, lead_handle))

    # 2. Root-level roles: project key → role handle directly
    for role in org.roles:
        project_key = resolve_env_scalar(role.jira_project)
        if not project_key:
            continue
        norm_key = project_key.upper()
        key_to_sources.setdefault(norm_key, []).append(
            (f"role:{role.name}", role.get_handle())
        )

    result: dict[str, str] = {}
    for key, sources in key_to_sources.items():
        if len({s[1] for s in sources}) > 1:
            # Same key, DIFFERENT leads — a genuine routing ambiguity.
            # Duplicates resolving to one lead are the documented
            # shared-project pattern and stay silent.
            source_labels = [s[0] for s in sources]
            logger.warning(
                "jira_project_key_shared",
                project_key=key,
                sources=", ".join(source_labels),
                routed_to=sources[0][1],
            )
        result[key] = sources[0][1]

    if result:
        logger.info(
            "project_key_lead_map_built",
            mapping=", ".join(f"{k}→{v}" for k, v in result.items()),
        )
    return result


def build_space_key_lead_map(
    org: Organization,
) -> dict[str, list[str]]:
    """Build a mapping from Confluence space key to agent handles.

    Scans two sources for the space *identity*
    (``integrations.confluence.space`` → ``confluence_space``):

    1. **OrgUnit ``confluence_space``** — maps the space key to the unit
       lead's handle.
    2. **Root-level role ``confluence_space``** — maps the space key
       directly to the role's handle (root roles have no unit).

    Multiple units can share the same space key — all their leads
    are included so each receives webhook notifications.

    Inherited leads count: a unit whose lead cascades down from an
    ancestor resolves via
    :func:`~crewlet.org.hierarchy.get_effective_lead` (``unit.get_lead()``
    alone returns ``None`` when the lead role lives outside the unit's
    subtree, which silently dropped page routing for such units).

    Returns:
        ``{space_key: [handle, ...]}`` for all discovered mappings.
    """
    result: dict[str, list[str]] = {}

    # 1. Unit-level: space key → unit lead
    for unit in org.all_units():
        space_key = resolve_env_scalar(unit.confluence_space)
        if not space_key:
            continue
        lead = get_effective_lead(unit, org)
        if lead is None:
            logger.warning(
                "confluence_space_key_no_lead",
                unit=unit.name,
                space_key=space_key,
            )
            continue
        lead_handle = lead.get_handle()
        norm_key = space_key.upper()
        if lead_handle not in result.setdefault(norm_key, []):
            result[norm_key].append(lead_handle)

    # 2. Root-level roles: space key → role handle directly
    for role in org.roles:
        space_key = resolve_env_scalar(role.confluence_space)
        if not space_key:
            continue
        norm_key = space_key.upper()
        handle = role.get_handle()
        if handle not in result.setdefault(norm_key, []):
            result[norm_key].append(handle)

    if result:
        logger.info(
            "space_key_lead_map_built",
            mapping=", ".join(f"{k}→[{', '.join(v)}]" for k, v in result.items()),
        )
    return result


def build_plane_project_lead_map(
    org: Organization,
) -> dict[str, str]:
    """Build a mapping from Plane project identifier to agent handle.

    Scans two sources for the project *identity*
    (``integrations.plane.project`` → ``plane_project``):

    1. **OrgUnit ``plane_project``** — maps the project identifier to the
       unit lead's handle (inherited leads resolve via
       :func:`~crewlet.org.hierarchy.get_effective_lead`).
    2. **Root-level role ``plane_project``** — maps the project
       identifier directly to the role's handle (root roles have no
       unit).

    If multiple sources share the same project identifier, the first
    one wins; a warning is logged ONLY when the sources resolve to
    DIFFERENT leads — several units sharing one project under one lead
    is the documented Jira-parity pattern and stays silent.

    Returns:
        ``{project_identifier: handle}`` for all discovered mappings.
    """
    # (label, handle) tuples grouped by normalised project identifier
    key_to_sources: dict[str, list[tuple[str, str]]] = {}

    # 1. Unit-level: project identifier → unit lead
    for unit in org.all_units():
        project_key = resolve_env_scalar(unit.plane_project)
        if not project_key:
            continue
        lead = get_effective_lead(unit, org)
        if lead is None:
            logger.warning(
                "plane_project_key_no_lead",
                unit=unit.name,
                project_key=project_key,
            )
            continue
        lead_handle = lead.get_handle()
        norm_key = project_key.upper()
        key_to_sources.setdefault(norm_key, []).append((unit.name, lead_handle))

    # 2. Root-level roles: project identifier → role handle directly
    for role in org.roles:
        project_key = resolve_env_scalar(role.plane_project)
        if not project_key:
            continue
        norm_key = project_key.upper()
        key_to_sources.setdefault(norm_key, []).append(
            (f"role:{role.name}", role.get_handle())
        )

    result: dict[str, str] = {}
    for key, sources in key_to_sources.items():
        if len({s[1] for s in sources}) > 1:
            # Same identifier, DIFFERENT leads — a genuine routing
            # ambiguity.  Duplicates resolving to one lead are the
            # documented shared-project pattern and stay silent.
            source_labels = [s[0] for s in sources]
            logger.warning(
                "plane_project_key_shared",
                project_key=key,
                sources=", ".join(source_labels),
                routed_to=sources[0][1],
            )
        result[key] = sources[0][1]

    if result:
        logger.info(
            "plane_project_lead_map_built",
            mapping=", ".join(f"{k}→{v}" for k, v in result.items()),
        )
    return result


async def _resolve_jira_account_via_mcp(
    mcp_bridge: Any,
    instance_name: str,
    jira_username: str = "",
) -> str:
    """Resolve a Jira account ID via MCP ``jira_get_user_profile``.

    Args:
        mcp_bridge: The MCP tool bridge.
        instance_name: MCP server instance name (e.g. ``atlassian::Agent_PM``).
        jira_username: The ``JIRA_USERNAME`` (email) for this role's
            atlassian env.  Passed as ``user_identifier`` to the
            ``jira_get_user_profile`` MCP tool.

    Returns the ``account_id`` from the profile, or ``""`` on failure.
    """
    import json

    client = mcp_bridge.get_client(instance_name)
    if client is None:
        logger.warning("mcp_client_not_found", instance=instance_name)
        return ""

    if not jira_username:
        logger.warning(
            "jira_username_missing",
            instance=instance_name,
        )
        return ""

    try:
        content_blocks = await client.call_tool(
            "jira_get_user_profile",
            {"user_identifier": jira_username},
        )
    except Exception as exc:
        logger.error(
            "jira_profile_lookup_failed",
            instance=instance_name,
            error=str(exc),
        )
        return ""

    # Extract account_id from the response text.
    # mcp-atlassian wraps the result as:
    #   {"success": true, "user": {<simplified dict>}}
    # The simplified dict may contain: display_name, name, email,
    # avatar_url, key.  On Cloud, account_id may be present (if the
    # fork / future versions include it); on Data Center, ``key`` or
    # ``name`` serves as the user identifier.
    for block in content_blocks:
        if block.get("type") != "text":
            continue
        text = block.get("text", "")
        try:
            data = json.loads(text)
        except (json.JSONDecodeError, AttributeError):
            # Fall back to looking for account_id in plain text
            for line in text.splitlines():
                for key in ("account_id:", "accountId:"):
                    if key in line:
                        return line.split(key, 1)[1].strip()
            continue

        # Unwrap the {"success": ..., "user": {...}} envelope
        user = data.get("user") if isinstance(data.get("user"), dict) else data

        account_id = (
            user.get("account_id")
            or user.get("accountId")
            or user.get("key")
            or user.get("name", "")
        )
        if account_id:
            return str(account_id)
    return ""


async def register_jira_accounts_from_org(
    handle_registry: Any,
    org: Any,
    mcp_bridge: Any,
    only_roles: set[str] | None = None,
) -> int:
    """Resolve Jira accountIds via MCP and register them for webhook routing.

    For each role with an ``atlassian`` mcp_env, calls
    ``jira_get_user_profile`` on the role's per-instance MCP server to obtain
    the account ID, then registers ``(jira, accountId) → agent handle``
    in the HandleRegistry.

    Roles that share the same accountId are skipped (can't disambiguate).

    ``only_roles`` restricts the MCP lookups to the named roles (by
    ``Role.name``).  Live role additions pass the just-added set so the
    refresh issues one profile lookup per new agent instead of
    re-resolving every previously-registered role on every
    ``POST /config/roles`` (which is O(n²) over a per-entity bootstrap).

    Returns the number of successfully registered mappings.
    """
    import asyncio

    from crewlet.mcp.bridge import mcp_instance_name

    # Collect (handle, label, instance_name, jira_username) for roles
    # with atlassian env.
    RoleInfo = tuple[str, str, str, str]
    role_infos: list[RoleInfo] = []

    for role in org.all_roles():
        if only_roles is not None and role.name not in only_roles:
            continue
        if "atlassian" not in role.mcp_env:
            continue

        handle = role.get_handle()
        instance_name = mcp_instance_name("atlassian", role.name)
        jira_username = resolve_env_vars(role.mcp_env["atlassian"]).get(
            "JIRA_USERNAME", ""
        )
        role_infos.append((handle, role.name, instance_name, jira_username))

    if not role_infos:
        return 0

    # Resolve all identities concurrently via MCP jira_get_user_profile
    results = await asyncio.gather(
        *(
            _resolve_jira_account_via_mcp(mcp_bridge, instance_name, jira_username)
            for _, _, instance_name, jira_username in role_infos
        ),
        return_exceptions=True,
    )

    # Group handles by resolved identity
    account_to_handles: dict[str, list[str]] = {}
    for (handle, label, _, _), result in zip(role_infos, results, strict=True):
        if isinstance(result, Exception):
            logger.warning(
                "jira_identity_resolution_failed",
                role=label,
                error=str(result),
            )
            continue
        account_id: str = result
        if not account_id:
            logger.warning("jira_identity_unresolved", role=label)
            continue
        account_to_handles.setdefault(account_id, []).append(handle)

    registered = 0
    for account_id, handles in account_to_handles.items():
        if len(handles) > 1:
            logger.warning(
                "jira_account_shared",
                account_id=account_id,
                handles=", ".join(handles),
            )
            continue

        handle = handles[0]
        handle_registry.register_external_id("jira", account_id, handle)
        # Jira and Confluence share Atlassian account IDs — register
        # under "confluence" too so ConfluenceTransport can resolve
        # watchers and self-ignore checks.
        handle_registry.register_external_id("confluence", account_id, handle)
        registered += 1
        logger.info(
            "jira_account_mapped",
            account_id=account_id,
            handle=handle,
        )

    if registered:
        logger.info("jira_accounts_registered", count=registered)
    return registered


async def _resolve_github_login_via_mcp(
    mcp_bridge: Any,
    instance_name: str,
) -> str:
    """Resolve the authenticated GitHub login via MCP ``get_me``.

    Args:
        mcp_bridge: The MCP tool bridge.
        instance_name: MCP server instance name
            (e.g. ``github::Agent_SWE``).

    Returns the ``login`` from the profile, or ``""`` on failure.
    """
    import json

    client = mcp_bridge.get_client(instance_name)
    if client is None:
        logger.warning("mcp_client_not_found", instance=instance_name)
        return ""

    try:
        content_blocks = await client.call_tool("get_me", {})
    except Exception as exc:
        logger.error(
            "github_get_me_failed",
            instance=instance_name,
            error=str(exc),
        )
        return ""

    for block in content_blocks:
        if block.get("type") != "text":
            continue
        text = block.get("text", "")
        try:
            data = json.loads(text)
        except (json.JSONDecodeError, AttributeError):
            continue
        if isinstance(data, dict) and data.get("login"):
            return data["login"].lower()

    return ""


async def register_github_accounts_from_org(
    handle_registry: Any,
    org: Any,
    *,
    mcp_bridge: Any,
    only_roles: set[str] | None = None,
) -> int:
    """Resolve GitHub usernames via MCP and register them for routing.

    For each role with a ``github`` entry in ``mcp_env`` (the per-agent
    GitHub MCP credentials), calls ``get_me`` on the role's per-instance
    GitHub MCP server to obtain the authenticated username, then registers
    ``(github, login) → agent handle`` in the HandleRegistry.

    ``only_roles`` restricts the MCP lookups to the named roles (by
    ``Role.name``) — see :func:`register_jira_accounts_from_org` for the
    O(n²) bootstrap motivation.

    Returns the number of successfully registered mappings.
    """
    import asyncio

    from crewlet.mcp.bridge import mcp_instance_name

    RoleInfo = tuple[str, str, str]  # (handle, role_name, instance_name)
    role_infos: list[RoleInfo] = []

    for role in org.all_roles():
        if only_roles is not None and role.name not in only_roles:
            continue
        github_env = role.mcp_env.get("github")
        if not github_env:
            continue
        # Skip roles whose credentials don't resolve — no MCP server was
        # started for them in _start_mcp_servers(), so get_client() fails.
        if not any(resolve_env_vars(github_env).values()):
            continue
        handle = role.get_handle()
        instance_name = mcp_instance_name("github", role.name)
        role_infos.append((handle, role.name, instance_name))

    if not role_infos:
        return 0

    results = await asyncio.gather(
        *(_resolve_github_login_via_mcp(mcp_bridge, inst) for _, _, inst in role_infos),
        return_exceptions=True,
    )

    login_to_handles: dict[str, list[str]] = {}
    for (handle, label, _), result in zip(role_infos, results, strict=True):
        if isinstance(result, Exception):
            logger.warning(
                "github_identity_resolution_failed",
                role=label,
                error=str(result),
            )
            continue
        login: str = result
        if not login:
            logger.warning("github_identity_unresolved", role=label)
            continue
        login_to_handles.setdefault(login, []).append(handle)

    registered = 0
    for login, handles in login_to_handles.items():
        if len(handles) > 1:
            logger.warning(
                "github_account_shared",
                login=login,
                handles=", ".join(handles),
            )
            continue

        handle = handles[0]
        handle_registry.register_external_id("github", login, handle)
        registered += 1
        logger.info(
            "github_account_mapped",
            login=login,
            handle=handle,
        )

    if registered:
        logger.info("github_accounts_registered", count=registered)
    return registered


def gitlab_token_from_mcp_env(gitlab_env: dict[str, str]) -> str:
    """Extract a GitLab PAT from a role's ``mcp_env.gitlab`` block.

    The block may carry the token as an http header (``Private-Token`` or
    ``Authorization: Bearer <pat>``) for a remote MCP server, or as a stdio
    env var — ``GITLAB_TOKEN`` (the ``glab`` CLI's variable) or
    ``GITLAB_PERSONAL_ACCESS_TOKEN`` (zereight's). ``${VAR}`` references are
    resolved. Returns ``""`` when no token is present or it resolves empty.
    """
    resolved = resolve_env_vars(gitlab_env)
    token = (
        resolved.get("Private-Token")
        or resolved.get("PRIVATE-TOKEN")
        or resolved.get("GITLAB_TOKEN")
        or resolved.get("GITLAB_PERSONAL_ACCESS_TOKEN")
        or ""
    )
    if not token:
        auth = resolved.get("Authorization") or resolved.get("authorization") or ""
        if auth.lower().startswith("bearer "):
            token = auth[7:].strip()
    return token.strip()


async def _resolve_gitlab_username_via_rest(
    api_base: str,
    token: str,
) -> str:
    """Resolve the authenticated GitLab username via ``GET /api/v4/user``.

    Uses REST rather than an MCP round-trip because the official GitLab
    MCP server exposes no ``whoami`` tool and community servers disagree
    on its name — ``GET /user`` is stable core API and needs only the
    role's own PAT. Returns the lowercased ``username`` or ``""`` on
    failure.
    """
    import httpx

    url = f"{api_base.rstrip('/')}/user"
    try:
        async with httpx.AsyncClient(timeout=15.0) as client:
            resp = await client.get(url, headers={"Private-Token": token})
    except Exception as exc:
        logger.error("gitlab_get_user_failed", url=url, error=str(exc))
        return ""
    if resp.status_code != 200:
        logger.error(
            "gitlab_get_user_status",
            url=url,
            status=resp.status_code,
        )
        return ""
    try:
        data = resp.json()
    except (ValueError, TypeError):
        return ""
    username = data.get("username", "") if isinstance(data, dict) else ""
    return username.lower() if isinstance(username, str) else ""


async def register_gitlab_accounts_from_org(
    handle_registry: Any,
    org: Any,
    *,
    gitlab_config: Any,
    only_roles: set[str] | None = None,
) -> int:
    """Resolve GitLab usernames via REST and register them for routing.

    For each role with a ``gitlab`` entry in ``mcp_env`` (the per-agent
    service-account PAT), calls ``GET {gitlab.url}/api/v4/user`` with that
    token to obtain the authenticated username, then registers
    ``(gitlab, username) → agent handle`` in the HandleRegistry — the
    mapping the webhook router resolves an assignee / reviewer / mentioned
    ``@username`` against.

    Unlike :func:`register_github_accounts_from_org` this needs no MCP
    bridge: the engine already holds the instance URL, and ``GET /user``
    is core API. ``only_roles`` restricts the lookups to the named roles
    (by ``Role.name``) for the live-reload per-entity refresh path.

    Returns the number of successfully registered mappings.
    """
    import asyncio

    if gitlab_config is None or not getattr(gitlab_config, "url", ""):
        return 0
    api_base = gitlab_config.api_base

    RoleInfo = tuple[str, str, str]  # (handle, role_name, token)
    role_infos: list[RoleInfo] = []

    for role in org.all_roles():
        if only_roles is not None and role.name not in only_roles:
            continue
        gitlab_env = role.mcp_env.get("gitlab")
        if not gitlab_env:
            continue
        token = gitlab_token_from_mcp_env(gitlab_env)
        if not token:
            continue
        role_infos.append((role.get_handle(), role.name, token))

    if not role_infos:
        return 0

    results = await asyncio.gather(
        *(
            _resolve_gitlab_username_via_rest(api_base, token)
            for _, _, token in role_infos
        ),
        return_exceptions=True,
    )

    username_to_handles: dict[str, list[str]] = {}
    for (handle, label, _), result in zip(role_infos, results, strict=True):
        if isinstance(result, Exception):
            logger.warning(
                "gitlab_identity_resolution_failed",
                role=label,
                error=str(result),
            )
            continue
        username: str = result
        if not username:
            logger.warning("gitlab_identity_unresolved", role=label)
            continue
        username_to_handles.setdefault(username, []).append(handle)

    registered = 0
    for username, handles in username_to_handles.items():
        if len(handles) > 1:
            logger.warning(
                "gitlab_account_shared",
                username=username,
                handles=", ".join(handles),
            )
            continue
        handle_registry.register_external_id("gitlab", username, handles[0])
        registered += 1
        logger.info("gitlab_account_mapped", username=username, handle=handles[0])

    if registered:
        logger.info("gitlab_accounts_registered", count=registered)
    return registered


def plane_token_from_mcp_env(plane_env: dict[str, str]) -> str:
    """Extract a Plane API token from a role's ``mcp_env.plane`` block.

    The block may carry the token as a stdio env var — ``PLANE_API_KEY``
    (the official plane-mcp-server's variable) — or as an ``X-API-Key``
    http header for a remote MCP server. ``${VAR}`` references are
    resolved. Returns ``""`` when no token is present or it resolves
    empty.
    """
    resolved = resolve_env_vars(plane_env)
    token = (
        resolved.get("PLANE_API_KEY")
        or resolved.get("X-API-Key")
        or resolved.get("x-api-key")
        or ""
    )
    return token.strip()


async def _resolve_plane_user_id_via_rest(
    url: str,
    token: str,
) -> str:
    """Resolve the authenticated Plane user UUID via ``GET /api/v1/users/me/``.

    Uses REST rather than an MCP round-trip for the same reason as
    :func:`_resolve_gitlab_username_via_rest`: ``GET /users/me/`` is
    stable public API and needs only the role's own token, while an MCP
    round-trip would couple the engine to one server's tool naming.
    Returns the lowercased user ``id`` or ``""`` on failure.
    """
    import httpx

    me_url = f"{url.rstrip('/')}/api/v1/users/me/"
    try:
        async with httpx.AsyncClient(timeout=15.0) as client:
            resp = await client.get(me_url, headers={"X-API-Key": token})
    except Exception as exc:
        logger.error("plane_get_me_failed", url=me_url, error=str(exc))
        return ""
    if resp.status_code != 200:
        logger.error(
            "plane_get_me_status",
            url=me_url,
            status=resp.status_code,
        )
        return ""
    try:
        data = resp.json()
    except (ValueError, TypeError):
        return ""
    if not isinstance(data, dict):
        return ""
    return str(data.get("id", "")).lower()


async def register_plane_accounts_from_org(
    handle_registry: Any,
    org: Any,
    *,
    plane_config: Any,
    only_roles: set[str] | None = None,
) -> int:
    """Resolve Plane user UUIDs via REST and register them for routing.

    For each role with a ``plane`` entry in ``mcp_env`` (the per-agent
    service-account token), calls ``GET {plane.url}/api/v1/users/me/``
    with that token to obtain the authenticated user UUID, then registers
    ``(plane, user_id) → agent handle`` in the HandleRegistry — the
    mapping the webhook router resolves an assignee / subscriber /
    mentioned ``entity_identifier`` against.

    Like :func:`register_gitlab_accounts_from_org` this needs no MCP
    bridge: the engine already holds the instance URL, and
    ``GET /users/me/`` is core API. ``only_roles`` restricts the lookups
    to the named roles (by ``Role.name``) for the live-reload per-entity
    refresh path.

    Returns the number of successfully registered mappings.
    """
    import asyncio

    if plane_config is None or not getattr(plane_config, "url", ""):
        return 0
    url = plane_config.url

    RoleInfo = tuple[str, str, str]  # (handle, role_name, token)
    role_infos: list[RoleInfo] = []

    for role in org.all_roles():
        if only_roles is not None and role.name not in only_roles:
            continue
        plane_env = role.mcp_env.get("plane")
        if not plane_env:
            continue
        token = plane_token_from_mcp_env(plane_env)
        if not token:
            continue
        role_infos.append((role.get_handle(), role.name, token))

    if not role_infos:
        return 0

    results = await asyncio.gather(
        *(_resolve_plane_user_id_via_rest(url, token) for _, _, token in role_infos),
        return_exceptions=True,
    )

    user_id_to_handles: dict[str, list[str]] = {}
    for (handle, label, _), result in zip(role_infos, results, strict=True):
        if isinstance(result, Exception):
            logger.warning(
                "plane_identity_resolution_failed",
                role=label,
                error=str(result),
            )
            continue
        user_id: str = result
        if not user_id:
            logger.warning("plane_identity_unresolved", role=label)
            continue
        user_id_to_handles.setdefault(user_id, []).append(handle)

    registered = 0
    for user_id, handles in user_id_to_handles.items():
        if len(handles) > 1:
            logger.warning(
                "plane_account_shared",
                user_id=user_id,
                handles=", ".join(handles),
            )
            continue
        handle_registry.register_external_id("plane", user_id, handles[0])
        registered += 1
        logger.info("plane_account_mapped", user_id=user_id, handle=handles[0])

    if registered:
        logger.info("plane_accounts_registered", count=registered)
    return registered
