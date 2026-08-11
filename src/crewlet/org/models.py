"""Organization hierarchy models — flexible, recursively nested OrgUnits."""

import re
from enum import StrEnum
from typing import Literal
from zoneinfo import ZoneInfo

from pydantic import BaseModel, ConfigDict, Field, model_validator

from crewlet._logging import get_logger
from crewlet.env_refs import env_var_reference
from crewlet.sandbox.setup import SandboxSetupStep

logger = get_logger("org.models")

#: Canonical handle shape — lowercase alphanumerics + hyphens.  The
#: single source for handle validation: ``Role`` enforces it on
#: explicit handles (``slugify`` output always conforms) and the
#: notifications ``HandleRegistry`` enforces it on registration.
HANDLE_RE = re.compile(r"^[a-z0-9][a-z0-9-]*$")


def slugify(name: str) -> str:
    """Convert a role name to a URL/email-safe handle slug.

    ``"Sarah Chen"`` → ``"sarah-chen"``; ``"QA/Test Lead"`` →
    ``"qa-test-lead"``.  This is the single canonical handle-derivation
    used by :meth:`Role.get_handle` and the deterministic agent-id
    derivation; the API layer imports it so a role's dashboard /
    webhook handle matches the one the engine spawns under (any
    divergent reimplementation produces a different
    ``derive_agent_id`` and silently orphans per-agent memory).
    """
    s = name.lower().strip()
    s = re.sub(r"[^a-z0-9]+", "-", s)
    return s.strip("-")


class ScheduleTarget(StrEnum):
    """Delivery targets for a *unit* schedule — ``each`` member or ``lead``.

    ``target`` is ignored for role-scoped schedules (the runner is always
    that role).  There is intentionally no "specific role" target: a
    static role pin adds nothing over a role schedule, so to run a
    recurring task as one person, define a role schedule on that role.
    ``lead`` is kept because it's *dynamic* — it follows whoever leads
    the unit (including inherited leads), so the schedule doesn't change
    when leadership does.
    """

    EACH = "each"
    """Fan out one task per **direct** role of the unit — the default.

    Each member runs its own turn with its own idempotency key, so a
    slow or failing member never blocks the others.
    """

    LEAD = "lead"
    """Route a single task to the unit's effective lead.

    The lead carries the team roster in its Plan prompt and owns the
    unit Slack channel, so lead-coordinated rituals (gather-and-post
    standups, weekly reports) are a natural fit.
    """


class Schedule(BaseModel):
    """A recurring task an agent or unit owns, fired on a cron expression.

    Schedules live in the YAML org config (``Role.schedules`` and
    ``OrgUnit.schedules``) — one source of truth, hot-reloadable via
    ``engine.reload_config()``.  The engine's :class:`~crewlet.schedule.Scheduler`
    publishes a ``TaskAssigned`` to the resolved runner's inbox when a
    schedule is due; the agent runtime path is otherwise unchanged.

    See ``docs/concepts/scheduling.md`` for the full design.
    """

    model_config = ConfigDict(extra="forbid")

    name: str
    """Identifier for the schedule, unique within its role/unit.

    Forms part of the idempotency key, so renaming a schedule lets a
    fire at the same minute re-run once."""

    cron: str = Field(pattern=r"^\s*\S+(?:\s+\S+){4}\s*$")
    """Standard 5-field cron expression (``minute hour dom month dow``).

    Evaluated in ``timezone``.  Vixie semantics: when both day-of-month
    and day-of-week are restricted, a day matches if *either* matches.

    The ``pattern`` checks **shape only** — exactly five whitespace-
    separated fields.  Full semantic parsing happens in the model
    validator below; the pattern exists so the generated JSON Schema
    catches the common wrong-field-count mistake in an editor, without
    a regex that would have to encode all of cron's range/step grammar
    (and would inevitably reject something valid)."""

    task: str
    """The task prompt handed to the runner agent when the schedule fires."""

    timezone: str = ""
    """IANA timezone the ``cron`` expression is evaluated in.

    Empty falls back to ``scheduling.default_timezone`` (itself
    defaulting to ``UTC``).  Standups are local-time events, so set
    this to e.g. ``Europe/Amsterdam``."""

    target: str = ScheduleTarget.EACH.value
    """Who runs a *unit* schedule: ``each`` (default — every direct
    member) or ``lead`` (the effective unit lead).  Ignored for role
    schedules; to run recurring work as one specific person, define a
    role schedule on that role instead of a unit schedule."""

    enabled: bool = True
    """Set ``false`` to keep a schedule in config without firing it."""

    timeout_seconds: int = 180
    """Hard wall-clock cap on a single scheduled turn (3-minute
    default).  The runner's turn is cancelled if it exceeds this,
    surfacing a ``scheduled_timeout`` guard breach + ``TaskFailed``."""

    catchup: bool = True
    """When the engine starts after missing a tick, fire the single most
    recent missed run if it falls inside the catchup window (half the
    period, clamped to ``[catchup_min, catchup_max]``).  Older missed
    ticks are never backfilled."""

    @model_validator(mode="after")
    def _validate(self) -> "Schedule":
        if not self.name.strip():
            raise ValueError("schedule name must be non-empty")
        if not self.task.strip():
            raise ValueError(f"schedule '{self.name}': task must be non-empty")
        # Lazy import keeps crewlet.org free of a top-level dependency on
        # crewlet.schedule (which imports back into crewlet.org).
        from crewlet.schedule.cron import CronError, parse_cron

        try:
            parse_cron(self.cron)
        except CronError as exc:
            raise ValueError(
                f"schedule '{self.name}': invalid cron {self.cron!r}: {exc}"
            ) from exc
        if self.timezone:
            try:
                ZoneInfo(self.timezone)
            except Exception as exc:
                raise ValueError(
                    f"schedule '{self.name}': invalid timezone {self.timezone!r}"
                ) from exc
        if self.timeout_seconds <= 0:
            raise ValueError(f"schedule '{self.name}': timeout_seconds must be > 0")
        return self


class RoleKind(StrEnum):
    """What kind of individual holds a seat in the org chart."""

    AGENT = "agent"
    """An AI agent — spawned as an :class:`AgentInstance` with an inbox,
    an LLM, and the full turn-engine runtime."""

    HUMAN = "human"
    """A human teammate — a first-class seat in the hierarchy that is
    addressable (rosters, ``lookup_colleague``, escalation targets,
    sender attribution) but never executable: no AgentInstance, no
    inbox topic, no LLM, no learning rows.  Agents reach human seats
    the same way they reach each other — their own colleague-surface
    tools (Slack / Jira / Confluence / GitHub) with an @-mention; the
    engine never sends as itself."""


#: Which :class:`HumanContact` field carries a given transport's
#: external ID.  Single source of truth — registration, party
#: resolution, and identity rendering all derive from this map.
CONTACT_FIELD_BY_TRANSPORT: dict[str, str] = {
    "slack": "slack_user_id",
    "jira": "atlassian_account_id",
    "confluence": "atlassian_account_id",
    "github": "github_login",
    "gitlab": "gitlab_username",
    "plane": "plane_user_id",
}


def _is_whole_env_ref(value: str) -> bool:
    """True when ``value`` is exactly one ``${VAR}`` config reference.

    Such a value is an indirection, not a literal identity: it is stored
    verbatim (never case-mangled — variable names are case-sensitive) and
    resolved at consumption time by
    :meth:`HumanContact.resolved_identities`.  Uses the shared grammar in
    :mod:`crewlet.env_refs` so "is a reference" means the same thing here
    as it does to the resolver.
    """
    return bool(env_var_reference(value))


#: Contact fields whose literal values are canonically lowercase.
#: Webhook parsers and agent-side identity resolution lowercase these
#: on their side (GitHub/GitLab logins, Plane user UUIDs), so a
#: mixed-case value here would never match at resolution time.
_NORMALIZED_CONTACT_FIELDS: frozenset[str] = frozenset(
    {"github_login", "gitlab_username", "plane_user_id"}
)


class HumanContact(BaseModel):
    """External identities for reaching a human seat.

    Agents reach humans through the same surfaces they use with each
    other (Slack mentions, Jira comments, Confluence mentions, GitHub
    reviews); these identifiers let prompts and the colleague-surface
    dispatcher address the right account.

    Every field accepts either a literal ID or exactly one whole-value
    ``${VAR}`` environment reference (e.g.
    ``plane_user_id: "${PLANE_FOUNDER_USER_ID}"`` — instance-specific
    IDs cannot ship in a config file).  Values are whitespace-stripped
    at validation; references are stored verbatim and resolved from the
    process environment at consumption time via
    :meth:`resolved_identities`.  A value that merely *embeds* a
    ``${VAR}`` inside a longer string (``"acme-${SUFFIX}"``) is rejected
    at validation — half-substituting it would silently register a wrong
    identity.
    """

    model_config = ConfigDict(extra="forbid")

    slack_user_id: str = ""
    """Slack member ID (e.g. ``U0123456789``) — used for ``<@…>``
    mentions and DM delivery."""

    atlassian_account_id: str = ""
    """Atlassian Cloud account ID — one ID covers both Jira and
    Confluence (assignments, ``<ri:user>`` mention markup, sender
    attribution on inbound webhooks)."""

    github_login: str = ""
    """GitHub username — review requests and sender attribution.
    Literal values are normalized to lowercase: webhook parsers and the
    agent-side MCP identity resolution both lowercase logins, so a
    mixed-case value here would never match at resolution time.  A
    ``${VAR}`` reference passes through verbatim (lowercased after
    resolution instead)."""

    gitlab_username: str = ""
    """GitLab username — assignment / review requests and sender
    attribution. Literal values are normalized to lowercase for the same
    reason as ``github_login``: the webhook parser and the boot-time
    ``GET /user`` identity resolution both lowercase usernames, so a
    mixed-case value here would never match at resolution time.  A
    ``${VAR}`` reference passes through verbatim (lowercased after
    resolution instead)."""

    plane_user_id: str = ""
    """Plane workspace-member user UUID — assignment, subscriber and
    ``<mention-component>`` attribution on inbound webhooks.  Discoverable
    via ``GET /api/v1/workspaces/{slug}/members/``.  Literal values are
    normalized to lowercase (webhook payloads and ``GET /users/me`` both
    carry lowercase UUIDs).  A ``${VAR}`` reference passes through
    verbatim (lowercased after resolution instead)."""

    @model_validator(mode="after")
    def _normalize(self) -> "HumanContact":
        """Strip, reference-check, and lowercase every identity value.

        Whitespace is stripped first — it is never part of any of these
        IDs, and a padded reference (``" ${VAR} "``) would otherwise
        dodge the whole-value match below and be silently mangled.

        A value that is exactly one ``${VAR}`` reference is left
        verbatim: env resolution is case-sensitive, so lowercasing the
        reference itself (``${PLANE_FOUNDER_USER_ID}`` →
        ``${plane_founder_user_id}``) would make it permanently
        unresolvable.  Such values are resolved — and only then
        lowercased — in :meth:`resolved_identities`.

        A value that *contains* ``${`` without being exactly one
        whole-value reference (``"acme-${SUFFIX}"``) is rejected: the
        env resolver substitutes embedded references too, so letting it
        through would silently register a truncated or case-mangled
        identity instead of failing loudly here.
        """
        for field in type(self).model_fields:
            value = getattr(self, field).strip()
            if "${" in value and not _is_whole_env_ref(value):
                raise ValueError(
                    f"contact.{field}: {value!r} embeds a ${{VAR}} reference"
                    " — contact fields take a literal ID or exactly one"
                    " whole-value ${VAR} reference"
                )
            if (
                value
                and field in _NORMALIZED_CONTACT_FIELDS
                and not _is_whole_env_ref(value)
            ):
                value = value.lower()
            setattr(self, field, value)
        return self

    def is_empty(self) -> bool:
        """True when no identity is set (``${VAR}`` references count as
        set — they are declared identities, resolved at use time)."""
        return not self._identities()

    def _identities(self) -> list[tuple[str, str]]:
        """Raw non-empty ``(transport, external_id)`` pairs — config-verbatim.

        Internal: values may still be unresolved ``${VAR}`` references,
        so anything that feeds routing, registration, prompts, or
        mention markup must go through :meth:`resolved_identities`
        instead.  Derived from :data:`CONTACT_FIELD_BY_TRANSPORT` —
        adding a field there extends every consumer at once.
        """
        return [
            (transport, value)
            for transport, field in CONTACT_FIELD_BY_TRANSPORT.items()
            if (value := getattr(self, field))
        ]

    def resolved_identities(self) -> list[tuple[str, str]]:
        """Non-empty ``(transport, external_id)`` pairs, ready to consume.

        The one enumeration registration and rendering should use.
        Each declared identity is ``${VAR}``-resolved from the process
        environment and then lowercased for the case-normalized
        transports.  An identity whose reference does not resolve is
        omitted with a debug log — emitting the raw ``${VAR}`` text
        would poison registration, prompts, and mention markup with a
        string no webhook payload or platform lookup can ever match.
        """
        # Lazy import: crewlet.config imports this module at top level.
        from crewlet.config import resolve_env_scalar

        pairs: list[tuple[str, str]] = []
        for transport, field in CONTACT_FIELD_BY_TRANSPORT.items():
            # Stored values are stripped + reference-checked by
            # ``_normalize``; the extra strip is defence in depth so a
            # padded value can never dodge the whole-value resolution.
            raw = getattr(self, field).strip()
            if not raw:
                continue
            resolved = resolve_env_scalar(raw).strip()
            if not resolved:
                logger.debug(
                    "contact_identity_unresolved",
                    transport=transport,
                    field=field,
                    reference=raw,
                )
                continue
            if field in _NORMALIZED_CONTACT_FIELDS:
                resolved = resolved.lower()
            pairs.append((transport, resolved))
        return pairs


class RoleSandboxMCPConfig(BaseModel):
    """Which of the role's MCP servers to render into the sandbox.

    Scoping is at the **server** level only. There is no per-tool allowlist:
    the coding agent gets every tool the configured servers expose. OpenCode
    has no allowlist flag, and Claude Code already runs ``bypassPermissions``
    headless, so a curated allowlist couldn't be enforced uniformly — we let
    each platform deal with the servers it's given."""

    model_config = ConfigDict(extra="forbid")

    servers: list[str] = Field(default_factory=list)
    """Server names (from ``mcp_servers``) to expose to the coding agent."""


class RoleSandboxConfig(BaseModel):
    """Per-role code-runtime gate (``role.sandbox``).

    Absent → the role never sees the sandbox option: the
    ``run_sandbox`` Execute tool is not offered to it.
    """

    model_config = ConfigDict(extra="forbid")

    enabled: bool = False
    coding_agent: Literal["", "claude-code", "opencode"] = ""
    """Coding agent to run inside the sandbox. Empty (the default) →
    inherit ``providers.sandbox.default_coding_agent``, resolved at
    launch; set to ``claude-code`` / ``opencode`` to override the
    provider default for this role only."""
    pause_ttl_seconds: float = -1.0
    """Blocked-sandbox pause TTL before reaping → re-seed. ``< 0`` →
    provider default; ``0`` → never pause, always re-seed."""
    mcp: RoleSandboxMCPConfig = Field(default_factory=RoleSandboxMCPConfig)
    env: dict[str, str] = Field(default_factory=dict)
    """Env injected into this seat's sandbox run, ``${ENV}``-expanded like
    ``mcp_env``.  This is where external-service tokens are DECLARED
    (``GITHUB_TOKEN: "${GITHUB_TOKEN_X}"`` for the git-auth recipe) plus
    any extras the coding task needs (a private-registry token, a test
    ``DATABASE_URL``) — the engine names no tool-specific variables of its
    own; only LLM creds derive automatically."""
    setup: list[SandboxSetupStep] = Field(default_factory=list)
    """Per-role sandbox setup steps (``crewlet.sandbox.setup``), applied
    after the engine-wide ``providers.sandbox.setup`` steps (the engine
    ships no steps of its own).  Each step declares files to write,
    commands to run, env to merge, and a brief paragraph telling the coding
    agent what the step made true about its box."""


class Role(BaseModel):
    """A seat in the org chart — held by an AI agent or a human.

    Each Role defines one unique individual with its own identity,
    expertise, and position in the hierarchy.  Seats with
    ``kind: agent`` (the default) map 1:1 to an AgentInstance — agents
    are not interchangeable.  Seats with ``kind: human`` participate in
    the same hierarchy (``manages``, unit ``lead``, rosters, lookup,
    escalation) but are never spawned: agents reach them over the
    external surfaces (Slack, Jira, Confluence, GitHub) with an
    @-mention, exactly as they reach each other, and the human replies
    natively — the engine never sends as itself.
    """

    name: str
    kind: RoleKind = RoleKind.AGENT
    """Whether this seat is held by an AI agent (default) or a human.

    Human seats keep the identity/hierarchy fields (``name``,
    ``handle``, ``email``, ``goal``, ``backstory``,
    ``responsibilities``, ``manages``) and gain ``contact`` /
    ``availability``; every runtime-only field (LLM keys, budgets,
    schedules, bot credentials, ``mcp_env``, ``behavioral_guidelines``)
    is rejected for them at validation time.
    """
    contact: HumanContact | None = None
    """External identities for a human seat (Slack member ID,
    Atlassian account ID, GitHub login).  These are how agents mention
    and reach the person and how inbound webhooks attribute their
    activity by name.  Human seats only; at least one identity is
    required."""
    availability: str = ""
    """Free-text availability note for a human seat, rendered into
    colleague rosters and ``lookup_colleague`` results — e.g.
    ``"CET business hours; replies within ~4h"``.  Human seats only."""
    handle: str = ""
    email: str = ""
    unit: str = ""
    """Name of the :class:`OrgUnit` this role belongs to.

    A soft reference used when the role is declared at the
    :class:`CompanyConfig.roles` root (e.g. by ``POST /config/roles``
    with a ``unit:`` field) instead of physically nested inside the
    unit's ``roles`` array.  :func:`config_to_organization` resolves
    the reference at build time: it moves the role into the named
    unit and applies the unit's ``mcp_env`` inheritance, so all
    downstream code can treat the role as unit-scoped.

    Leave empty for top-level roles that don't belong to any unit.
    """
    goal: str = ""
    backstory: str = ""
    responsibilities: list[str] = Field(default_factory=list)
    manages: list[str] = Field(default_factory=list)
    behavioral_guidelines: list[str] = Field(default_factory=list)
    token_budget: int = 0  # 0 = unlimited
    llm: str | list[str] = "default"
    """LLM provider key used when a per-phase key is not set.

    Accepts a single string (e.g. ``"claude-sonnet"``) for the
    single-provider case, or a list of keys (e.g.
    ``["claude-sonnet", "claude-haiku", "gpt-4o"]``) for fallback
    -- the runtime tries each entry in order when the previous one
    raises a retryable error.
    """
    llm_plan: str | list[str] | None = None
    """LLM provider key(s) for the Plan phase. Falls back to ``llm``.
    A list form enables per-phase fallback."""
    llm_execute: str | list[str] | None = None
    """LLM provider key(s) for the Execute phase. Falls back to ``llm``."""
    llm_review: str | list[str] | None = None
    """LLM provider key(s) for the Review phase. Falls back to ``llm``."""
    llm_subagent: str | list[str] | None = None
    """LLM provider key(s) for ephemeral sub-agents spawned from this role.
    Falls back to ``llm``.
    """
    llm_auxiliary: str | list[str] | None = None
    """LLM provider key(s) for auxiliary/cheap-model work driven by the
    agent-learning subsystem — episode-hit summarization and the
    PersistDecider's post-turn reflection call.  Falls back to ``llm``.
    Set this to a fast/cheap model (e.g. ``gpt-4o-mini``) so reflection
    does not inflate turn cost."""
    llm_judge: str | list[str] | None = None
    """LLM provider key(s) for the round-cap extension judge.

    The judge is invoked when the Plan or Execute phase exhausts its
    tool-round cap; it decides whether to extend (the agent is making
    progress) or fall through to the existing rescue path (the agent
    is thrashing).  Runs on a small, fast model by preference (the
    decision is cheap and the prompt is bounded).  Falls back to
    ``llm`` then to ``"default"`` via the standard phase-chain
    resolution."""
    llm_sandbox: str | list[str] | None = None
    """LLM provider key(s) the sandboxed Execute backend's coding agent
    runs on. Falls back to ``llm_execute`` then ``llm``."""
    learning_enabled: bool | None = None
    """Per-role override for the agent-learning subsystem.

    When ``None`` (default), the role inherits the system-level
    ``learning.enabled`` setting.  When ``False``, the ReflectEngine
    skips every worker for turns from this role — no PersistDecider,
    no CounterpartyProfiler, no SkillSynthesizer, no SkillRefiner.
    Episodes are still written (they are cheap and useful for humans
    regardless of whether the reflection loop consumes them).

    Lets operators opt specific roles out of reflection without
    disabling the whole subsystem — useful for roles whose turns are
    low-signal (e.g. a noisy poller) or carry sensitive data."""
    mcp_env: dict[str, dict[str, str]] = Field(default_factory=dict)
    """Per-agent overrides for MCP servers, keyed by server name.

    For each ``shared: false`` server in ``mcp_servers`` the engine
    launches a dedicated per-role instance and applies these overrides
    as **environment variables** (``stdio`` transport) or **HTTP
    headers** (``http`` transport).  This is how each agent authenticates
    as a distinct identity — e.g. a per-agent Jira token
    (``mcp_env.atlassian.JIRA_API_TOKEN``), the Slack MCP bot token
    (``mcp_env.slack.SLACK_MCP_XOXB_TOKEN``), or the GitHub Copilot MCP
    auth header (``mcp_env.github.Authorization``).
    """
    sandbox: RoleSandboxConfig | None = None
    """Optional code-runtime gate. Absent → no sandboxed
    Execute backend for this role."""
    slack: dict[str, str] = Field(default_factory=dict)
    """Per-agent Slack app credentials for the SlackTransport.

    The resolved transport identity, authored under the role's
    ``integrations.slack`` block and materialised here by the config
    loader.  Each agent has its own Slack app for inbound webhook
    verification and outbound notification delivery::

        integrations:
          slack:
            bot_token: "${SLACK_BOT_TOKEN_ENGINEER}"      # xoxb-*
            signing_secret: "${SLACK_SIGNING_SECRET_ENGINEER}"
            channel: C_ENGINEER  # optional default channel

    These credentials drive the transport only.  The Slack *tool*
    server is a separate ``mcp_servers`` entry; give the agent its MCP
    bot token via ``mcp_env.slack.SLACK_MCP_XOXB_TOKEN`` (typically the
    same ``${...}`` reference as ``bot_token`` here).
    """
    jira_project: str = ""
    """Jira project key this role owns (``integrations.jira.project``).

    Meaningful for **root-level** roles (no containing unit): inbound
    Jira webhook activity with no better recipient routes to this role
    (see :func:`~crewlet.config.build_project_key_lead_map`).  For
    unit-nested roles the unit carries the identity, so this stays empty.
    Integration identity only — not an MCP credential, and it does not
    scope knowledge reads.
    """
    confluence_space: str = ""
    """Confluence space key this role owns (``integrations.confluence.space``).

    Meaningful for **root-level** roles: inbound page webhook activity
    routes to this role (see
    :func:`~crewlet.config.build_space_key_lead_map`).  Integration
    identity only — not an MCP credential, and it does **not** scope
    knowledge reads (read scope is the org-wide
    :attr:`Organization.confluence_spaces`).
    """
    plane_project: str = ""
    """Plane project identifier this role owns (``integrations.plane.project``).

    Meaningful for **root-level** roles (no containing unit): inbound
    Plane webhook activity with no better recipient routes to this role
    (see :func:`~crewlet.config.build_plane_project_lead_map`).  For
    unit-nested roles the unit carries the identity, so this stays empty.
    Integration identity only — not an MCP credential, and it does
    **not** scope knowledge reads (read scope is the org-wide
    :attr:`Organization.plane_projects`).
    """
    schedules: list[Schedule] = Field(default_factory=list)
    """Role-scoped recurring tasks (cron analogue).

    Each schedule fires a ``TaskAssigned`` to this role's own inbox on
    its cron expression.  ``Schedule.target`` is ignored here — the
    runner is always this role.  See ``docs/concepts/scheduling.md``.
    """

    @model_validator(mode="after")
    def _validate_schedules(self) -> "Role":
        seen: set[str] = set()
        for sch in self.schedules:
            if sch.name in seen:
                raise ValueError(
                    f"role '{self.name}': duplicate schedule name '{sch.name}'"
                )
            seen.add(sch.name)
        return self

    @model_validator(mode="after")
    def _validate_handle_format(self) -> "Role":
        """Explicit handles must be registry-safe.

        Handles flow into inbox topic names, plus-addressed emails,
        and ``HandleRegistry.register_external_id`` (which raises on
        malformed handles at runtime — e.g. a human seat's contact
        registration during engine start).  Reject at config time
        with an actionable message instead.  Auto-derived handles
        (``slugify``) always conform.
        """
        if self.handle and not HANDLE_RE.match(self.handle):
            raise ValueError(
                f"role '{self.name}': handle '{self.handle}' must match "
                f"[a-z0-9][a-z0-9-]* (lowercase alphanumerics and "
                f"hyphens) — e.g. '{slugify(self.handle) or 'my-handle'}'"
            )
        return self

    @model_validator(mode="after")
    def _validate_kind_fields(self) -> "Role":
        """Enforce the seat-kind field contract.

        Human seats must not carry runtime-only config (it would be
        dead config at best, silently misleading at worst), and agent
        seats must not carry the human-only contact/notify fields.
        """
        if self.kind == RoleKind.HUMAN:
            forbidden: list[tuple[str, bool]] = [
                ("llm", self.llm != "default"),
                ("llm_plan", self.llm_plan is not None),
                ("llm_execute", self.llm_execute is not None),
                ("llm_review", self.llm_review is not None),
                ("llm_subagent", self.llm_subagent is not None),
                ("llm_auxiliary", self.llm_auxiliary is not None),
                ("llm_judge", self.llm_judge is not None),
                ("llm_sandbox", self.llm_sandbox is not None),
                ("sandbox", self.sandbox is not None),
                ("token_budget", self.token_budget != 0),
                ("learning_enabled", self.learning_enabled is not None),
                ("schedules", bool(self.schedules)),
                ("slack", bool(self.slack)),
                ("integrations.jira", bool(self.jira_project)),
                ("integrations.confluence", bool(self.confluence_space)),
                ("integrations.plane", bool(self.plane_project)),
                ("mcp_env", bool(self.mcp_env)),
                ("behavioral_guidelines", bool(self.behavioral_guidelines)),
            ]
            offending = [field for field, is_set in forbidden if is_set]
            if offending:
                raise ValueError(
                    f"role '{self.name}' is a human seat; the following "
                    f"agent-only fields must not be set: {', '.join(offending)}"
                )
            if self.contact is None or self.contact.is_empty():
                raise ValueError(
                    f"role '{self.name}' is a human seat and needs at least "
                    f"one 'contact' identity (slack_user_id / "
                    f"atlassian_account_id / github_login / gitlab_username / "
                    f"plane_user_id) so agents can mention and reach them"
                )
        else:
            human_only = [
                field
                for field, is_set in (
                    ("contact", self.contact is not None),
                    ("availability", bool(self.availability)),
                )
                if is_set
            ]
            if human_only:
                raise ValueError(
                    f"role '{self.name}' is an agent seat; the following "
                    f"human-only fields must not be set: "
                    f"{', '.join(human_only)} (did you mean 'kind: human'?)"
                )
        return self

    @property
    def is_human(self) -> bool:
        """True when this seat is held by a human."""
        return self.kind == RoleKind.HUMAN

    def get_handle(self) -> str:
        """Get the canonical handle for this seat.

        Returns ``handle`` if set, otherwise slugifies the name.
        """
        return self.handle or slugify(self.name)


class OrgUnitType(StrEnum):
    """Well-known organization unit types.

    These are provided for convenience and documentation. Custom
    type strings are also accepted — the ``OrgUnit.type`` field
    is a plain ``str``, not restricted to this enum.
    """

    DIVISION = "division"
    DEPARTMENT = "department"
    GROUP = "group"
    TEAM = "team"
    SQUAD = "squad"
    POD = "pod"
    GUILD = "guild"
    CHAPTER = "chapter"
    UNIT = "unit"


class OrgUnit(BaseModel):
    """A flexible organizational unit that can nest recursively.

    An OrgUnit can represent any grouping level — division, department,
    team, squad, pod, guild, chapter, or any custom type. Units can
    contain roles directly and/or child units, supporting arbitrary
    org structures.

    Examples::

        # Flat team
        OrgUnit(name="Backend", type="team", lead="Lead Dev",
                roles=[Role(name="Lead Dev"), Role(name="Dev")])

        # Department with child teams
        OrgUnit(name="Engineering", type="department", lead="VP Eng",
                children=[
                    OrgUnit(name="Backend", type="team", ...),
                    OrgUnit(name="Frontend", type="team", ...),
                ])

        # Deep nesting: Division > Department > Team
        OrgUnit(name="Technology", type="division", children=[
            OrgUnit(name="Engineering", type="department", children=[
                OrgUnit(name="Platform", type="team", roles=[...]),
            ]),
        ])
    """

    name: str
    type: str = "team"
    purpose: str = ""
    lead: str = ""
    """Name of the role designated as unit lead.

    The value must reference a role that exists somewhere in the
    organization — either in this unit's own roles, in a descendant,
    or in an ancestor unit (when inherited).

    When a unit has no lead set, ``Organization._inherit_unit_leads``
    copies the parent unit's lead down automatically.

    The lead is responsible for task routing, coordination within the
    unit, and acting as the single point of contact. The lead
    auto-manages any direct role in this unit not already managed
    by another role.
    """
    goals: list[str] = Field(default_factory=list)
    slack_channel: str = ""
    """Slack channel ID for this unit (e.g. ``C0123456789``).

    Agents in this unit are told to use this channel for team
    discussions and decisions.  Inherited by child units that don't
    set their own.
    """
    jira_project: str = ""
    """Jira project key this unit owns (``integrations.jira.project``).

    Inbound Jira webhook activity with no better recipient routes to the
    unit lead (see :func:`~crewlet.config.build_project_key_lead_map`),
    and it's the project this unit's agents file work under.  Integration
    identity only — not an MCP credential, and it does not scope
    knowledge reads.
    """
    confluence_space: str = ""
    """Confluence space key this unit owns (``integrations.confluence.space``).

    Inbound page webhook activity routes to the unit lead (see
    :func:`~crewlet.config.build_space_key_lead_map`), and it's the
    unit's write / skill-promotion home.  Integration identity only —
    not an MCP credential, and it does **not** scope knowledge reads
    (read scope is the org-wide :attr:`Organization.confluence_spaces`).
    """
    plane_project: str = ""
    """Plane project identifier this unit owns (``integrations.plane.project``).

    Inbound Plane webhook activity with no better recipient routes to
    the unit lead (see :func:`~crewlet.config.build_plane_project_lead_map`),
    and it's the project this unit's agents file work under.  Integration
    identity only — not an MCP credential, and it does **not** scope
    knowledge reads (read scope is the org-wide
    :attr:`Organization.plane_projects`).
    """
    knowledge_refs: list[str] = Field(default_factory=list)
    mcp_env: dict[str, dict[str, str]] = Field(default_factory=dict)
    roles: list[Role] = Field(default_factory=list)
    children: list["OrgUnit"] = Field(default_factory=list)
    schedules: list[Schedule] = Field(default_factory=list)
    """Unit-scoped recurring tasks (cron analogue).

    Each schedule fires on its cron expression; ``Schedule.target``
    selects the runner(s): ``each`` (default — every direct member),
    ``lead`` (the effective unit lead), or a role name/handle in this
    unit.  Schedules are **not** inherited by child units.  See
    ``docs/concepts/scheduling.md``.
    """

    def get_role(self, name: str) -> Role | None:
        """Find a role by name within this unit's direct roles."""
        for role in self.roles:
            if role.name == name:
                return role
        return None

    def all_roles(self) -> list[Role]:
        """Return a flat list of all roles in this unit and descendants."""
        roles = list(self.roles)
        for child in self.children:
            roles.extend(child.all_roles())
        return roles

    def get_child(self, name: str) -> "OrgUnit | None":
        """Find a direct child unit by name."""
        for child in self.children:
            if child.name == name:
                return child
        return None

    def get_lead(self) -> Role | None:
        """Return the lead role, searching direct roles then descendants."""
        if not self.lead:
            return None
        # Check direct roles first
        role = self.get_role(self.lead)
        if role is not None:
            return role
        # Search descendants
        for child in self.children:
            role = child._find_role_recursive(self.lead)
            if role is not None:
                return role
        return None

    def to_api_dict(self) -> dict:
        """Serialize this unit and descendants for API responses."""
        return {
            "name": self.name,
            "type": self.type,
            "purpose": self.purpose,
            "lead": self.lead,
            "goals": self.goals,
            "roles": [
                {
                    "name": r.name,
                    "kind": r.kind.value,
                    "handle": r.get_handle(),
                    "goal": r.goal,
                    "manages": r.manages,
                }
                for r in self.roles
            ],
            "children": [c.to_api_dict() for c in self.children],
        }

    def _find_role_recursive(self, name: str) -> Role | None:
        """Find a role by name anywhere in this subtree."""
        for role in self.roles:
            if role.name == name:
                return role
        for child in self.children:
            found = child._find_role_recursive(name)
            if found is not None:
                return found
        return None

    @model_validator(mode="after")
    def _validate_schedules(self) -> "OrgUnit":
        """Validate schedule names are unique and targets are ``each``/``lead``.

        ``lead`` resolvability is validated at the ``Organization`` level
        alongside the other lead checks (an inherited lead lives in an
        ancestor unit).  Pinning a schedule to one specific role is not a
        unit-schedule target — define a role schedule on that role.
        """
        if not self.schedules:
            return self
        seen: set[str] = set()
        for sch in self.schedules:
            if sch.name in seen:
                raise ValueError(
                    f"OrgUnit '{self.name}': duplicate schedule name '{sch.name}'"
                )
            seen.add(sch.name)
            target = sch.target
            if target == ScheduleTarget.EACH.value:
                # ``each`` fans out to DIRECT roles only — never descendants
                # — and only to agent seats (humans run no turns), so on a
                # unit with no direct agent roles it can never fire.  Fail
                # at load rather than no-op silently at runtime.
                if not any(r.kind == RoleKind.AGENT for r in self.roles):
                    raise ValueError(
                        f"OrgUnit '{self.name}': schedule '{sch.name}' uses "
                        f"target 'each' but the unit has no direct agent "
                        f"roles (each fans out to direct agent roles only — "
                        f"never descendants, never human seats)"
                    )
                continue
            if target != ScheduleTarget.LEAD.value:
                raise ValueError(
                    f"OrgUnit '{self.name}': schedule '{sch.name}' target "
                    f"'{target}' must be 'each' or 'lead'. To run a recurring "
                    f"task as one specific role, define a role schedule on "
                    f"that role instead."
                )
        return self

    @model_validator(mode="after")
    def _validate_lead(self) -> "OrgUnit":
        """Validate and auto-manage for leads that are local to this unit.

        When the lead role exists in this unit's direct roles or
        descendants, validation and auto-management happen here.
        When the lead is *not* found locally it is left alone — it may
        be inherited from a parent unit, which is validated at the
        ``Organization`` level by ``_inherit_unit_leads``.
        """
        if not self.lead:
            return self

        # Check whether the lead exists in this unit or descendants.
        direct_names = {r.name for r in self.roles}
        found_locally = self.lead in direct_names
        if not found_locally:
            all_role_names = direct_names | {
                r.name for c in self.children for r in c.all_roles()
            }
            found_locally = self.lead in all_role_names

        if not found_locally:
            # Lead is not in this subtree — it may be inherited from a
            # parent unit.  Organization-level validation will catch
            # truly invalid references.
            return self

        # Auto-infer manages for the lead when it's a direct role.
        # Any role in this unit that isn't already managed by another
        # role defaults to being managed by the lead. Skip roles that
        # directly manage the lead to avoid cycles.
        if self.roles and self.get_role(self.lead) is not None:
            already_managed: set[str] = set()
            for role in self.roles:
                already_managed.update(role.manages)
            lead_role = self.get_role(self.lead)
            if lead_role is not None:
                for role in self.roles:
                    if (
                        role.name != self.lead
                        and role.name not in already_managed
                        and self.lead not in role.manages
                    ):
                        lead_role.manages.append(role.name)
        return self


class Organization(BaseModel):
    """The root model representing an entire company.

    The organization contains a flexible hierarchy of ``OrgUnit``
    nodes. Units can nest to any depth, supporting flat teams,
    departments with sub-teams, divisions, or any custom structure
    the founder designs.

    Roles can live at two levels:

    - **Inside an OrgUnit** — scoped to that unit for knowledge,
      MCP env inheritance, and lead auto-management.
    - **At the root level** (``roles``) — org-wide agents with no
      unit affiliation.  Their knowledge is scoped to the org.
      They participate in the ``manages[]`` hierarchy like any
      other role and are fully visible to task routing.
    """

    name: str
    mission: str = ""
    vision: str = ""
    policies: list[str] = Field(default_factory=list)
    roles: list[Role] = Field(default_factory=list)
    """Root-level roles that are not scoped to any OrgUnit.

    These are org-wide agents (e.g. a CEO, CTO, or cross-cutting
    advisor) that don't belong to a specific team or department.
    They still participate in the ``manages[]`` hierarchy and are
    discoverable via ``get_role()`` / ``all_roles()``.
    """
    units: list[OrgUnit] = Field(default_factory=list)
    confluence_spaces: list[str] = Field(default_factory=list)
    """Org-wide Confluence read scope for the ``## Relevant knowledge``
    search — the only thing that narrows it.

    Mirrored at parse time from
    :class:`~crewlet.config.KnowledgeConfig.confluence_spaces`.  Returned
    by :func:`~crewlet.knowledge.accessibility.accessible_spaces` as the
    ``space IN (...)`` clause; empty ⇒ unscoped/ACL-bound.  Role- and
    unit-independent: a unit's ``confluence_space`` *identity* does not
    scope reads.

    Domain-only -- carries no auth / URL / MCP fields; that lives on
    :class:`~crewlet.config.ConfluenceConfig` in the config layer.
    """
    plane_projects: list[str] = Field(default_factory=list)
    """Org-wide Plane read scope for the ``## Relevant knowledge``
    search — the only thing that narrows it (consumed by the
    Plane knowledge searcher).

    Mirrored at parse time from
    :class:`~crewlet.config.KnowledgeConfig.plane_projects`.  Empty ⇒
    unscoped — Plane membership/ACLs bound the search.  Role- and
    unit-independent: a unit's ``plane_project`` *identity* does not
    scope reads.

    Domain-only -- carries no auth / URL / MCP fields; that lives on
    :class:`~crewlet.config.PlaneConfig` in the config layer.
    """

    @model_validator(mode="after")
    def _inherit_unit_leads(self) -> "Organization":
        """Propagate lead from parent to child units that have no lead.

        When a child unit has no ``lead`` set, it inherits its parent's
        lead.  For inherited leads (where the lead role lives in an
        ancestor unit rather than the current one), auto-management is
        applied: the inherited lead role gains ``manages`` entries for
        any unmanaged direct roles in the child unit.

        Lead references that don't resolve to any role are logged as
        a warning rather than raised.  Live config management bootstraps
        the org in pieces (``POST /config/units`` is allowed to land
        before the matching ``POST /config/roles``), and the engine
        applies every intermediate revision; rejecting partially-wired
        revisions would make per-entity bootstrap impossible.  Once
        every required role arrives, no warning fires.  Downstream code
        (``unit.get_lead`` / ``get_effective_lead``) already treats an
        unresolved lead as ``None``.
        """
        all_role_names = {r.name for r in self.all_roles()}

        for unit in self.units:
            _propagate_lead(unit, parent_lead="")

        # Validate all lead references and auto-manage for inherited leads.
        for unit in self.all_units():
            if not unit.lead:
                continue

            if unit.lead not in all_role_names:
                logger.warning(
                    "orgunit_lead_unresolved",
                    unit=unit.name,
                    lead=unit.lead,
                    hint=(
                        "lead role not yet present in the org -- expected "
                        "during per-entity bootstrap; persistent warnings "
                        "indicate a misspelled lead reference"
                    ),
                )
                continue

            if not unit.roles:
                continue
            # Skip if lead is a direct role — _validate_lead already handled it.
            if unit.get_role(unit.lead) is not None:
                continue
            lead_role = self.get_role(unit.lead)
            if lead_role is None:
                continue
            already_managed: set[str] = set()
            for role in unit.roles:
                already_managed.update(role.manages)
            lead_manages_set = set(lead_role.manages)
            for role in unit.roles:
                if (
                    role.name != unit.lead
                    and role.name not in already_managed
                    and role.name not in lead_manages_set
                    and unit.lead not in role.manages
                ):
                    lead_role.manages.append(role.name)
                    lead_manages_set.add(role.name)
        return self

    @model_validator(mode="after")
    def _expand_unit_manages(self) -> "Organization":
        """Expand unit names in ``manages`` to individual role names.

        When a ``manages`` entry matches an OrgUnit name (and does not
        match any role name), it is replaced with the names of all
        roles contained in that unit (including descendants).  This
        lets config authors write ``manages: ["Backend"]`` instead of
        listing every role in the Backend unit individually.
        """
        all_roles = self.all_roles()
        all_role_names = {r.name for r in all_roles}
        # Pre-compute unit role names so repeated references don't
        # re-traverse the subtree each time.
        unit_roles: dict[str, list[str]] = {
            u.name: [r.name for r in u.all_roles()] for u in self.all_units()
        }

        for role in all_roles:
            expanded: list[str] = []
            seen: set[str] = set()
            for entry in role.manages:
                if entry in all_role_names:
                    # Explicit role name — keep as-is.
                    expanded.append(entry)
                    seen.add(entry)
                elif entry in unit_roles:
                    # Unit name — expand to all roles in that unit,
                    # excluding the managing role itself.
                    for name in unit_roles[entry]:
                        if name != role.name and name not in seen:
                            expanded.append(name)
                            seen.add(name)
                else:
                    # Unknown reference — keep as-is (validation
                    # happens elsewhere if needed).
                    expanded.append(entry)
                    seen.add(entry)
            role.manages = expanded
        return self

    @model_validator(mode="after")
    def _validate_seats(self) -> "Organization":
        """Org-wide seat checks that need the fully-wired hierarchy.

        Runs after lead inheritance (validators execute in definition
        order), so inherited leads are resolvable here.

        - Handles are unique org-wide.
        - A unit schedule with ``target: lead`` whose resolved
          effective lead is a human seat is a hard error: the schedule
          could never fire (humans run no turns), and no later revision
          fixes it without changing one of the two entities.
        """
        all_roles = self.all_roles()

        # Handles are the canonical identity key (inbox topics, party
        # resolution, external-ID registration) — a collision makes
        # one seat silently unreachable.  Agent/agent collisions were
        # always broken (shared inbox topic); agent/human collisions
        # would misattribute the person's external activity to the
        # agent.  Hard error either way.
        seen_handles: dict[str, str] = {}
        for role in all_roles:
            handle = role.get_handle()
            if handle in seen_handles:
                raise ValueError(
                    f"duplicate handle '{handle}': roles "
                    f"'{seen_handles[handle]}' and '{role.name}' — handles "
                    f"are the canonical seat identity and must be unique"
                )
            seen_handles[handle] = role.name

        # Lazy import: hierarchy type-checks against this module.
        from crewlet.org.hierarchy import get_effective_lead

        for unit in self.all_units():
            lead_schedules = [
                s
                for s in unit.schedules
                if s.enabled and s.target == ScheduleTarget.LEAD.value
            ]
            if not lead_schedules:
                continue
            lead = get_effective_lead(unit, self)
            if lead is not None and lead.kind == RoleKind.HUMAN:
                raise ValueError(
                    f"OrgUnit '{unit.name}': schedule "
                    f"'{lead_schedules[0].name}' targets the unit lead, "
                    f"but the effective lead '{lead.name}' is a human "
                    f"seat — humans run no scheduled turns. Target a "
                    f"role schedule on an agent seat instead."
                )
        return self

    def all_roles(self) -> list[Role]:
        """Return a flat list of all roles across the entire org.

        Includes both root-level roles and roles nested inside
        OrgUnits.
        """
        roles = list(self.roles)
        for unit in self.units:
            roles.extend(unit.all_roles())
        return roles

    def get_role(self, name: str) -> Role | None:
        """Find a role by name across the entire organization.

        Searches root-level roles first, then OrgUnit trees.
        """
        # Check root-level roles first (fast path)
        for role in self.roles:
            if role.name == name:
                return role
        # Then search OrgUnit trees
        for unit in self.units:
            found = unit._find_role_recursive(name)
            if found is not None:
                return found
        return None

    def get_unit(self, name: str) -> OrgUnit | None:
        """Find a unit by name anywhere in the org tree."""
        for unit in self.units:
            found = _find_unit_recursive(unit, name)
            if found is not None:
                return found
        return None

    def all_units(self) -> list[OrgUnit]:
        """Return a flat list of all units in the org tree."""
        units: list[OrgUnit] = []
        for unit in self.units:
            _collect_units(unit, units)
        return units

    def to_api_dict(self) -> dict:
        """Serialize the org hierarchy for API responses."""
        return {
            "name": self.name,
            "mission": self.mission,
            "roles": [
                {
                    "name": r.name,
                    "kind": r.kind.value,
                    "handle": r.get_handle(),
                    "goal": r.goal,
                    "manages": r.manages,
                }
                for r in self.roles
            ],
            "units": [u.to_api_dict() for u in self.units],
        }


def _propagate_lead(
    unit: OrgUnit, parent_lead: str, parent_slack_channel: str = ""
) -> None:
    """Recursively inherit lead and slack_channel from parent."""
    effective_lead = unit.lead or parent_lead
    if not unit.lead and parent_lead:
        unit.lead = parent_lead
    effective_channel = unit.slack_channel or parent_slack_channel
    if not unit.slack_channel and parent_slack_channel:
        unit.slack_channel = parent_slack_channel
    for child in unit.children:
        _propagate_lead(child, effective_lead, effective_channel)


def _find_unit_recursive(unit: OrgUnit, name: str) -> OrgUnit | None:
    """Find a unit by name in a subtree."""
    if unit.name == name:
        return unit
    for child in unit.children:
        found = _find_unit_recursive(child, name)
        if found is not None:
            return found
    return None


def _collect_units(unit: OrgUnit, out: list[OrgUnit]) -> None:
    """Collect all units in a subtree (DFS)."""
    out.append(unit)
    for child in unit.children:
        _collect_units(child, out)
